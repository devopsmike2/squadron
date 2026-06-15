// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package pipelinehealth

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Verdict classifies the operational state of a single agent based
// on the latest pipeline_health_samples row per metric. The verdict
// is deliberately coarse — three buckets that map cleanly to a
// colored badge in the UI. Operators who want detail look at the
// per-metric panel.
type Verdict string

const (
	// VerdictHealthy means every signal we look at is within bounds.
	VerdictHealthy Verdict = "healthy"
	// VerdictDegraded means at least one signal is concerning (queue
	// >50% full, non-zero send_failed in the latest sample, processor
	// drops > 0) but the collector is still working.
	VerdictDegraded Verdict = "degraded"
	// VerdictBroken means data is being dropped at the back of the
	// pipeline (queue saturated, send_failed_rate > sent_rate, or
	// continuous processor drops). The collector might be running
	// but it's not delivering reliably.
	VerdictBroken Verdict = "broken"
	// VerdictUnknown means we have no self-metric samples yet. New
	// agents start here and stay here until the collector reports
	// in. The UI treats Unknown distinctly from "no agent" — the
	// agent IS checking in via OpAMP, we just haven't seen its
	// otelcol_* metrics yet.
	VerdictUnknown Verdict = "unknown"
)

// Signal is one finding that contributed to the verdict. The UI
// renders these as a tidy bullet list under the badge.
type Signal struct {
	Kind     string  `json:"kind"`     // e.g. "queue_saturation", "send_failed", "processor_drops"
	Severity string  `json:"severity"` // "warn" | "critical"
	Message  string  `json:"message"`  // human-readable explanation
	Value    float64 `json:"value"`    // the offending number, for tooltips
}

// AgentSnapshot is the per-agent payload the UI consumes. It
// includes the overall verdict, the contributing signals, and the
// latest value of every captured metric so the dashboard can render
// gauges + sparklines without a second round trip.
type AgentSnapshot struct {
	AgentID    string                 `json:"agent_id"`
	Verdict    Verdict                `json:"verdict"`
	Signals    []Signal               `json:"signals"`
	Latest     map[string][]MetricRow `json:"latest"` // metric name → rows (one per label-set)
	LastSample time.Time              `json:"last_sample"`
}

// MetricRow is a single (label set, value) observation, used inside
// AgentSnapshot.Latest. We surface labels as a sorted slice so the
// UI can render "exporter=otlp/datadog" pairs in a stable order.
type MetricRow struct {
	Labels []KV    `json:"labels"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit,omitempty"`
}

// KV is a single label key + value pair. We use a slice of these in
// MetricRow rather than a map so the JSON output order is stable
// across requests — important because the UI compares snapshots to
// detect deltas.
type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// FleetSummary is the response for the fleet-wide overview: how
// many agents fall into each bucket. The Dashboard shows this as
// a stacked bar; the Fleet Map colorizes nodes by it.
type FleetSummary struct {
	Total     int                  `json:"total"`
	Healthy   int                  `json:"healthy"`
	Degraded  int                  `json:"degraded"`
	Broken    int                  `json:"broken"`
	Unknown   int                  `json:"unknown"`
	PerAgent  map[string]Verdict   `json:"per_agent"`
	UpdatedAt time.Time            `json:"updated_at"`
	Concerns  []string             `json:"concerns,omitempty"` // top 5 worst-offender agent IDs
	_         map[string]AgentSnapshot
}

// Reader is the minimal subset of telemetrytypes.Reader the service
// needs. Defined as a local interface so tests can stub with just
// QueryRaw without standing up the full telemetry store.
type Reader interface {
	QueryRaw(ctx context.Context, query string, args ...interface{}) ([]map[string]interface{}, error)
}

// AgentLister lets the service enumerate the agents Squadron knows
// about so the fleet summary can distinguish "no metrics yet"
// (Unknown) from "agent isn't even connected" (not in the response
// at all).
type AgentLister interface {
	AllAgentIDs(ctx context.Context) ([]string, error)
}

// Service answers pipeline-health queries. Construct one with
// NewService and pass the same instance to the API handler + any
// alert evaluator that wants verdict transitions.
type Service struct {
	reader Reader
	agents AgentLister
	logger *zap.Logger

	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[string]cacheEntry
}

type cacheEntry struct {
	storedAt time.Time
	value    any
}

// NewService constructs a pipeline-health service. A 10s cache TTL
// keeps DuckDB cool when the UI polls every 5s and the optional
// alert evaluator polls every 30s.
func NewService(reader Reader, agents AgentLister, logger *zap.Logger) *Service {
	return &Service{
		reader:   reader,
		agents:   agents,
		logger:   logger,
		cacheTTL: 10 * time.Second,
		cache:    map[string]cacheEntry{},
	}
}

// AgentSnapshot returns the latest sample of every captured metric
// for an agent, plus the computed verdict and contributing signals.
//
// Returns a VerdictUnknown snapshot with no signals when the agent
// has no rows in pipeline_health_samples — the caller can tell the
// agent apart from a missing one because it's still in the agents
// table.
func (s *Service) AgentSnapshot(ctx context.Context, agentID string) (*AgentSnapshot, error) {
	if cached := s.cacheGet("agent:" + agentID); cached != nil {
		return cached.(*AgentSnapshot), nil
	}

	const query = `
		WITH ranked AS (
			SELECT
				metric_name,
				labels_json,
				labels_hash,
				value,
				unit,
				timestamp,
				ROW_NUMBER() OVER (
					PARTITION BY metric_name, labels_hash
					ORDER BY timestamp DESC
				) AS rn
			FROM pipeline_health_samples
			WHERE agent_id = ?
		)
		SELECT metric_name, labels_json, value, unit, timestamp
		FROM ranked WHERE rn = 1
	`

	rows, err := s.reader.QueryRaw(ctx, query, agentID)
	if err != nil {
		return nil, fmt.Errorf("pipeline-health agent snapshot: %w", err)
	}

	snap := &AgentSnapshot{
		AgentID: agentID,
		Verdict: VerdictUnknown,
		Latest:  map[string][]MetricRow{},
		// Signals always serialized as a JSON array, never null —
		// the UI does .signals.length unconditionally and would
		// crash on null.
		Signals: []Signal{},
	}
	if len(rows) == 0 {
		s.cachePut("agent:"+agentID, snap)
		return snap, nil
	}

	var latest time.Time
	for _, r := range rows {
		name := stringOf(r["metric_name"])
		value := floatOf(r["value"])
		unit := stringOf(r["unit"])
		labels := parseLabels(r["labels_json"])
		ts := timeOf(r["timestamp"])
		if ts.After(latest) {
			latest = ts
		}
		snap.Latest[name] = append(snap.Latest[name], MetricRow{
			Labels: sortedKV(labels),
			Value:  value,
			Unit:   unit,
		})
	}
	snap.LastSample = latest
	snap.Verdict, snap.Signals = ComputeVerdict(snap.Latest)

	s.cachePut("agent:"+agentID, snap)
	return snap, nil
}

// FleetSummary returns per-agent verdicts in aggregate. Used by the
// Dashboard stacked bar + Fleet Map color coding.
func (s *Service) FleetSummary(ctx context.Context) (*FleetSummary, error) {
	if cached := s.cacheGet("fleet"); cached != nil {
		return cached.(*FleetSummary), nil
	}

	// One query: per-agent latest sample of each (metric, label_hash).
	// We use the same row-number window as AgentSnapshot but without
	// the agent_id filter — the read cost is bounded by
	// (agents × captured_metric_names × label_combinations), which is
	// small even at fleet scales of low-thousands.
	const query = `
		WITH ranked AS (
			SELECT
				agent_id,
				metric_name,
				labels_json,
				labels_hash,
				value,
				unit,
				timestamp,
				ROW_NUMBER() OVER (
					PARTITION BY agent_id, metric_name, labels_hash
					ORDER BY timestamp DESC
				) AS rn
			FROM pipeline_health_samples
		)
		SELECT agent_id, metric_name, labels_json, value, unit, timestamp
		FROM ranked WHERE rn = 1
	`

	rows, err := s.reader.QueryRaw(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("pipeline-health fleet summary: %w", err)
	}

	perAgentLatest := map[string]map[string][]MetricRow{}
	for _, r := range rows {
		agentID := stringOf(r["agent_id"])
		name := stringOf(r["metric_name"])
		value := floatOf(r["value"])
		unit := stringOf(r["unit"])
		labels := parseLabels(r["labels_json"])
		if perAgentLatest[agentID] == nil {
			perAgentLatest[agentID] = map[string][]MetricRow{}
		}
		perAgentLatest[agentID][name] = append(perAgentLatest[agentID][name], MetricRow{
			Labels: sortedKV(labels),
			Value:  value,
			Unit:   unit,
		})
	}

	out := &FleetSummary{
		PerAgent:  map[string]Verdict{},
		UpdatedAt: time.Now().UTC(),
	}

	// First, every agent with samples gets a real verdict
	type concern struct {
		agentID string
		verdict Verdict
	}
	concerns := []concern{}
	for agentID, latest := range perAgentLatest {
		verdict, _ := ComputeVerdict(latest)
		out.PerAgent[agentID] = verdict
		switch verdict {
		case VerdictHealthy:
			out.Healthy++
		case VerdictDegraded:
			out.Degraded++
			concerns = append(concerns, concern{agentID, verdict})
		case VerdictBroken:
			out.Broken++
			concerns = append(concerns, concern{agentID, verdict})
		default:
			out.Unknown++
		}
	}

	// Then add agents we know about but have no metrics for —
	// those are Unknown. This lets the dashboard distinguish
	// "the collector hasn't started reporting yet" from "we have
	// nothing connected at all".
	if s.agents != nil {
		ids, err := s.agents.AllAgentIDs(ctx)
		if err != nil {
			s.logger.Warn("pipeline-health: AllAgentIDs failed (non-fatal)",
				zap.Error(err))
		} else {
			for _, id := range ids {
				if _, ok := out.PerAgent[id]; !ok {
					out.PerAgent[id] = VerdictUnknown
					out.Unknown++
				}
			}
		}
	}
	out.Total = out.Healthy + out.Degraded + out.Broken + out.Unknown

	// Top-5 concerns: broken first, then degraded. Stable order so
	// the dashboard doesn't flicker between identical updates.
	sort.Slice(concerns, func(i, j int) bool {
		if concerns[i].verdict != concerns[j].verdict {
			return concerns[i].verdict == VerdictBroken
		}
		return concerns[i].agentID < concerns[j].agentID
	})
	for i, c := range concerns {
		if i >= 5 {
			break
		}
		out.Concerns = append(out.Concerns, c.agentID)
	}

	s.cachePut("fleet", out)
	return out, nil
}

// Timeseries returns 1-minute bucketed values for a single
// (agent, metric) over the last `window`. Used by the agent detail
// sparklines. The label hash is optional — when empty we sum across
// label sets (e.g. total send_failed across all exporters).
func (s *Service) Timeseries(
	ctx context.Context,
	agentID, metricName, labelsHash string,
	window time.Duration,
) ([]TimePoint, error) {
	cacheKey := fmt.Sprintf("ts:%s:%s:%s:%s", agentID, metricName, labelsHash, window)
	if cached := s.cacheGet(cacheKey); cached != nil {
		return cached.([]TimePoint), nil
	}

	startedAt := time.Now().Add(-window).UTC()
	args := []interface{}{agentID, metricName, startedAt}
	labelFilter := ""
	if labelsHash != "" {
		labelFilter = "AND labels_hash = ?"
		args = append(args, labelsHash)
	}

	q := fmt.Sprintf(`
		SELECT
			date_trunc('minute', timestamp) AS bucket,
			AVG(value)                       AS value
		FROM pipeline_health_samples
		WHERE agent_id = ? AND metric_name = ? AND timestamp >= ? %s
		GROUP BY bucket
		ORDER BY bucket
	`, labelFilter)

	rows, err := s.reader.QueryRaw(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pipeline-health timeseries: %w", err)
	}
	out := make([]TimePoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, TimePoint{
			Time:  timeOf(r["bucket"]),
			Value: floatOf(r["value"]),
		})
	}
	s.cachePut(cacheKey, out)
	return out, nil
}

// TimePoint is one bucketed (time, value) for the sparkline API.
type TimePoint struct {
	Time  time.Time `json:"t"`
	Value float64   `json:"v"`
}

// cacheGet looks up a key, treating expired entries as misses.
func (s *Service) cacheGet(key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok {
		return nil
	}
	if time.Since(e.storedAt) > s.cacheTTL {
		delete(s.cache, key)
		return nil
	}
	return e.value
}

// cachePut writes a cache entry with the current timestamp.
func (s *Service) cachePut(key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = cacheEntry{storedAt: time.Now(), value: v}
}

// ---- adapters from DuckDB row maps -----------------------------------------

func stringOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return ""
	}
}

func floatOf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	}
	return 0
}

func timeOf(v any) time.Time {
	switch x := v.(type) {
	case time.Time:
		return x
	}
	return time.Time{}
}

func parseLabels(v any) map[string]string {
	s := stringOf(v)
	if s == "" || s == "null" {
		return nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func sortedKV(labels map[string]string) []KV {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]KV, 0, len(keys))
	for _, k := range keys {
		out = append(out, KV{Key: k, Value: labels[k]})
	}
	return out
}

// LabelHashFromQuery turns a `key=value;key=value` URL query
// parameter into a labels-hash compatible with HashLabels in the
// extractor. The query form lets the UI request a specific
// time series without us inventing a separate ID scheme.
func LabelHashFromQuery(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, ";")
	labels := map[string]string{}
	for _, pair := range pairs {
		if pair == "" {
			continue
		}
		idx := strings.IndexByte(pair, '=')
		if idx <= 0 {
			continue
		}
		labels[pair[:idx]] = pair[idx+1:]
	}
	return HashLabels(labels)
}
