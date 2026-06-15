// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package insights is the v0.24+ Telemetry Volume Insights query
// layer. It answers "where are my telemetry bytes going?" by
// aggregating ingest-side accounting from the otlp_batches table
// and (for per-attribute breakdown) sampling the row tables.
//
// The service is deliberately read-only and stateless. Caching of
// expensive aggregates happens here (short-TTL, in-process) so the
// UI's polling refresh interval doesn't hammer DuckDB. The v0.25
// recommendation engine reads from this same surface — keep the
// return shapes stable.
package insights

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	telemetrytypes "github.com/devopsmike2/squadron/internal/storage/telemetrystore/types"
)

// Window enumerates the supported time windows. v0.24 keeps it
// small on purpose — the three windows operators reach for in
// practice are "the last minute" (for hot debugging), "the last
// hour" (for the dashboard), and "the last day" (for trend
// review). More granular slicing waits for explicit demand.
type Window string

const (
	Window5m  Window = "5m"
	Window1h  Window = "1h"
	Window24h Window = "24h"
)

// AsDuration maps a Window to a time.Duration. Returns 0 + an
// error if the value is unrecognized; callers are expected to
// 400 on that.
func (w Window) AsDuration() (time.Duration, error) {
	switch w {
	case Window5m:
		return 5 * time.Minute, nil
	case Window1h:
		return 1 * time.Hour, nil
	case Window24h:
		return 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported window %q (allowed: 5m, 1h, 24h)", string(w))
	}
}

// Signal mirrors the otlp_batches.signal_type column.
type Signal string

const (
	SignalTraces  Signal = "traces"
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
)

// Service is the public query surface. Everything returns ready-to-
// serialize structs; the HTTP handler is a thin shell.
type Service struct {
	reader telemetrytypes.Reader
	logger *zap.Logger

	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[string]cacheEntry
}

type cacheEntry struct {
	storedAt time.Time
	value    any
}

// NewService constructs a Service. cacheTTL controls how long
// computed aggregates are kept; 15s is a reasonable default that
// matches the UI's refresh interval and keeps DuckDB busy with
// fewer redundant queries.
func NewService(reader telemetrytypes.Reader, logger *zap.Logger) *Service {
	return &Service{
		reader:   reader,
		logger:   logger,
		cacheTTL: 15 * time.Second,
		cache:    make(map[string]cacheEntry),
	}
}

// ----------------------------------------------------------------
// Public surfaces. The shapes here are wire-stable — v0.25 reads
// from them.
// ----------------------------------------------------------------

// SignalVolume is the per-signal breakdown nested inside a fleet
// or agent summary. Bytes are the measured wire-size attribution
// from otlp_batches; ItemCount is the count of spans / data
// points / log records; DroppedCount is items the worker pool
// refused or that dead-lettered after exhausting retries.
type SignalVolume struct {
	Signal       Signal `json:"signal"`
	Bytes        int64  `json:"bytes"`
	ItemCount    int64  `json:"item_count"`
	DroppedCount int64  `json:"dropped_count"`
}

// FleetSummary is the top-line "where are my bytes going" answer
// for the whole fleet within a window.
type FleetSummary struct {
	Window     Window         `json:"window"`
	StartTime  time.Time      `json:"start_time"`
	EndTime    time.Time      `json:"end_time"`
	Totals     SignalVolume   `json:"totals"` // Signal field is "" on this aggregate row
	BySignal   []SignalVolume `json:"by_signal"`
	AgentCount int            `json:"agent_count"`
}

// AgentVolume is the per-agent breakdown — the row shape on the
// Cost Insights outliers table and the source for the agent-detail
// drawer's Volume panel.
type AgentVolume struct {
	AgentID    string         `json:"agent_id"`
	AgentName  string         `json:"agent_name,omitempty"` // populated by the handler if it has the name
	TotalBytes int64          `json:"total_bytes"`
	BySignal   []SignalVolume `json:"by_signal"`
}

// AttributeVolume is a per-attribute-key row. Bytes is a *sampled
// estimate* — not measured precisely — and the response includes
// the Estimated flag so clients can render that caveat in the UI.
type AttributeVolume struct {
	Key       string `json:"key"`
	Bytes     int64  `json:"bytes"`
	Estimated bool   `json:"estimated"`
	// PctOfSignal is the share of the signal's total byte budget
	// this attribute contributes. Useful for the "http.url is 28%
	// of all log bytes" headline; clamps to [0, 1].
	PctOfSignal float64 `json:"pct_of_signal"`
}

// DestinationVolume is the bytes-per-destination breakdown. v0.24
// uses configured-exporter attribution (parse each agent's
// effective_config, pro-rate that agent's bytes across its
// destinations) — this is approximate; v0.26+ may add real egress
// tracking. The response includes Estimated=true so the UI labels
// it accordingly.
type DestinationVolume struct {
	DestinationKey string `json:"destination_key"` // e.g. "honeycomb:Honeycomb (api.honeycomb.io)"
	Label          string `json:"label"`
	Kind           string `json:"kind"` // bucket name from the UI's exporter-parser
	Bytes          int64  `json:"bytes"`
	Estimated      bool   `json:"estimated"`
}

// ----------------------------------------------------------------
// Implementations
// ----------------------------------------------------------------

// FleetVolume returns the fleet-wide totals + per-signal split for
// the given window. signalFilter limits to specific signals when
// non-empty; pass nil/empty for "all signals".
func (s *Service) FleetVolume(ctx context.Context, win Window, signalFilter []Signal) (*FleetSummary, error) {
	dur, err := win.AsDuration()
	if err != nil {
		return nil, err
	}
	cacheKey := fmt.Sprintf("fleet:%s:%s", win, joinSignals(signalFilter))
	if cached := s.fromCache(cacheKey); cached != nil {
		if v, ok := cached.(*FleetSummary); ok {
			return v, nil
		}
	}

	now := time.Now().UTC()
	start := now.Add(-dur)

	// Single GROUP BY signal_type. The receiver writes one row per
	// (batch, agent, signal), so summing here gives byte/item totals
	// per signal. Filtering by signal happens in WHERE for cheaper
	// scans when only one signal is requested.
	filter := ""
	args := []any{start}
	if len(signalFilter) > 0 {
		placeholders := make([]string, len(signalFilter))
		for i, sig := range signalFilter {
			placeholders[i] = "?"
			args = append(args, string(sig))
		}
		filter = fmt.Sprintf(" AND signal_type IN (%s)", strings.Join(placeholders, ","))
	}
	// CAST every SUM() to BIGINT: DuckDB promotes SUM(BIGINT) to
	// HUGEINT (128-bit) which the go-duckdb driver returns as
	// *big.Int. Without the cast, the row scanner sees a type the
	// caller doesn't recognize and the field reads as zero. This
	// caught us once already — keep the casts on every SUM site.
	q := fmt.Sprintf(`
		SELECT
			signal_type,
			CAST(COALESCE(SUM(payload_bytes), 0) AS BIGINT) AS bytes,
			CAST(COALESCE(SUM(item_count), 0)    AS BIGINT) AS items,
			CAST(COALESCE(SUM(dropped_count), 0) AS BIGINT) AS dropped,
			COUNT(DISTINCT agent_id)                        AS agents
		FROM otlp_batches
		WHERE timestamp >= ?%s
		GROUP BY signal_type
	`, filter)

	rows, err := s.reader.QueryRaw(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fleet volume query: %w", err)
	}

	out := &FleetSummary{
		Window:    win,
		StartTime: start,
		EndTime:   now,
		// Always-array so the UI's .by_signal.map(...) won't crash
		// on a window with no telemetry.
		BySignal: []SignalVolume{},
	}
	agentSet := map[string]struct{}{} // collected separately via second tiny query
	var totalBytes, totalItems, totalDropped int64
	for _, row := range rows {
		sig := stringOf(row["signal_type"])
		b := int64Of(row["bytes"])
		i := int64Of(row["items"])
		d := int64Of(row["dropped"])
		out.BySignal = append(out.BySignal, SignalVolume{
			Signal:       Signal(sig),
			Bytes:        b,
			ItemCount:    i,
			DroppedCount: d,
		})
		totalBytes += b
		totalItems += i
		totalDropped += d
	}
	// Stable order in the response so clients can rely on signal
	// position.
	sort.Slice(out.BySignal, func(i, j int) bool {
		return signalOrder(out.BySignal[i].Signal) < signalOrder(out.BySignal[j].Signal)
	})
	out.Totals = SignalVolume{
		Bytes:        totalBytes,
		ItemCount:    totalItems,
		DroppedCount: totalDropped,
	}
	// Distinct agent count for the window — cheap to count
	// separately rather than join in the main query.
	countRows, err := s.reader.QueryRaw(ctx,
		`SELECT COUNT(DISTINCT agent_id) AS n FROM otlp_batches WHERE timestamp >= ?`,
		start)
	if err == nil && len(countRows) > 0 {
		out.AgentCount = int(int64Of(countRows[0]["n"]))
	}
	_ = agentSet

	s.intoCache(cacheKey, out)
	return out, nil
}

// AgentVolume returns the per-signal split for a single agent.
// Used by the agent-detail drawer's Volume panel.
func (s *Service) AgentVolume(ctx context.Context, agentID string, win Window) (*AgentVolume, error) {
	dur, err := win.AsDuration()
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, fmt.Errorf("agent_id required")
	}
	cacheKey := fmt.Sprintf("agent:%s:%s", agentID, win)
	if cached := s.fromCache(cacheKey); cached != nil {
		if v, ok := cached.(*AgentVolume); ok {
			return v, nil
		}
	}

	start := time.Now().UTC().Add(-dur)
	rows, err := s.reader.QueryRaw(ctx, `
		SELECT signal_type,
		       CAST(COALESCE(SUM(payload_bytes), 0) AS BIGINT) AS bytes,
		       CAST(COALESCE(SUM(item_count), 0)    AS BIGINT) AS items,
		       CAST(COALESCE(SUM(dropped_count), 0) AS BIGINT) AS dropped
		FROM otlp_batches
		WHERE agent_id = ? AND timestamp >= ?
		GROUP BY signal_type
	`, agentID, start)
	if err != nil {
		return nil, fmt.Errorf("agent volume query: %w", err)
	}

	out := &AgentVolume{AgentID: agentID, BySignal: []SignalVolume{}}
	for _, row := range rows {
		b := int64Of(row["bytes"])
		out.BySignal = append(out.BySignal, SignalVolume{
			Signal:       Signal(stringOf(row["signal_type"])),
			Bytes:        b,
			ItemCount:    int64Of(row["items"]),
			DroppedCount: int64Of(row["dropped"]),
		})
		out.TotalBytes += b
	}
	sort.Slice(out.BySignal, func(i, j int) bool {
		return signalOrder(out.BySignal[i].Signal) < signalOrder(out.BySignal[j].Signal)
	})

	s.intoCache(cacheKey, out)
	return out, nil
}

// TopAgents returns the top-N agents by total bytes within the
// window. Drives the Cost Insights outliers panel.
func (s *Service) TopAgents(ctx context.Context, win Window, limit int) ([]AgentVolume, error) {
	dur, err := win.AsDuration()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	cacheKey := fmt.Sprintf("topAgents:%s:%d", win, limit)
	if cached := s.fromCache(cacheKey); cached != nil {
		if v, ok := cached.([]AgentVolume); ok {
			return v, nil
		}
	}

	start := time.Now().UTC().Add(-dur)
	// One query to grab the top-N agents by total bytes. Per-signal
	// breakdown comes from a second window over the same set.
	rows, err := s.reader.QueryRaw(ctx, `
		SELECT agent_id,
		       CAST(COALESCE(SUM(payload_bytes), 0) AS BIGINT) AS bytes
		FROM otlp_batches
		WHERE timestamp >= ?
		GROUP BY agent_id
		ORDER BY bytes DESC
		LIMIT ?
	`, start, limit)
	if err != nil {
		return nil, fmt.Errorf("top agents query: %w", err)
	}
	if len(rows) == 0 {
		s.intoCache(cacheKey, []AgentVolume{})
		return []AgentVolume{}, nil
	}

	agentIDs := make([]string, 0, len(rows))
	totalsByAgent := make(map[string]int64, len(rows))
	for _, r := range rows {
		id := stringOf(r["agent_id"])
		totalsByAgent[id] = int64Of(r["bytes"])
		agentIDs = append(agentIDs, id)
	}

	// Per-signal split. One query, GROUP BY (agent_id, signal_type),
	// filtered to the top agents we just selected.
	placeholders := make([]string, len(agentIDs))
	args := []any{start}
	for i, id := range agentIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	splitQ := fmt.Sprintf(`
		SELECT agent_id, signal_type,
		       CAST(COALESCE(SUM(payload_bytes), 0) AS BIGINT) AS bytes,
		       CAST(COALESCE(SUM(item_count), 0)    AS BIGINT) AS items,
		       CAST(COALESCE(SUM(dropped_count), 0) AS BIGINT) AS dropped
		FROM otlp_batches
		WHERE timestamp >= ? AND agent_id IN (%s)
		GROUP BY agent_id, signal_type
	`, strings.Join(placeholders, ","))
	splitRows, err := s.reader.QueryRaw(ctx, splitQ, args...)
	if err != nil {
		return nil, fmt.Errorf("top agents signal split: %w", err)
	}
	bySig := map[string][]SignalVolume{}
	for _, r := range splitRows {
		id := stringOf(r["agent_id"])
		bySig[id] = append(bySig[id], SignalVolume{
			Signal:       Signal(stringOf(r["signal_type"])),
			Bytes:        int64Of(r["bytes"]),
			ItemCount:    int64Of(r["items"]),
			DroppedCount: int64Of(r["dropped"]),
		})
	}

	out := make([]AgentVolume, 0, len(agentIDs))
	for _, id := range agentIDs {
		sigs := bySig[id]
		sort.Slice(sigs, func(i, j int) bool {
			return signalOrder(sigs[i].Signal) < signalOrder(sigs[j].Signal)
		})
		out = append(out, AgentVolume{
			AgentID:    id,
			TotalBytes: totalsByAgent[id],
			BySignal:   sigs,
		})
	}

	s.intoCache(cacheKey, out)
	return out, nil
}

// TopAttributes returns the top-N attribute keys by approximate
// byte contribution for a given signal type within the window.
// Estimation strategy: sample at most `sampleSize` rows, compute
// per-attribute value length × occurrence, then extrapolate to
// the full row population. Marked Estimated=true on every row so
// the UI can render the caveat.
//
// Caveats explicitly captured: rows with NULL attributes JSON
// don't contribute; key cardinality is unbounded but we only
// return the top-N, so memory stays small.
func (s *Service) TopAttributes(ctx context.Context, win Window, signal Signal, limit int) ([]AttributeVolume, error) {
	dur, err := win.AsDuration()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	cacheKey := fmt.Sprintf("topAttrs:%s:%s:%d", win, signal, limit)
	if cached := s.fromCache(cacheKey); cached != nil {
		if v, ok := cached.([]AttributeVolume); ok {
			return v, nil
		}
	}

	const sampleSize = 2000

	// Different signal types live in different tables. Pick the
	// appropriate (table, attribute_column) pair.
	var table, attrCol string
	switch signal {
	case SignalTraces:
		table, attrCol = "traces", "span_attributes"
	case SignalLogs:
		table, attrCol = "logs", "log_attributes"
	case SignalMetrics:
		// Metrics' attributes live across three tables (sum/gauge/
		// histogram). We pick metrics_sum for v0.24 — extending
		// requires a UNION ALL which is left to v0.25 if operators
		// ask for it.
		table, attrCol = "metrics_sum", "metric_attributes"
	default:
		return nil, fmt.Errorf("unsupported signal %q", signal)
	}

	start := time.Now().UTC().Add(-dur)

	// First, the population size: total rows in the window. Skews
	// the extrapolation factor.
	popRows, err := s.reader.QueryRaw(ctx,
		fmt.Sprintf(`SELECT COUNT(*) AS n FROM %s WHERE timestamp >= ?`, table),
		start)
	if err != nil {
		return nil, fmt.Errorf("attr population count: %w", err)
	}
	population := int64Of(popRows[0]["n"])
	if population == 0 {
		s.intoCache(cacheKey, []AttributeVolume{})
		return []AttributeVolume{}, nil
	}

	// Pull a sample. ORDER BY random() is cheap on DuckDB and
	// produces an i.i.d. sample. If population is small, the
	// sample IS the population.
	sampleQ := fmt.Sprintf(`
		SELECT %s AS attrs FROM %s
		WHERE timestamp >= ? AND %s IS NOT NULL
		ORDER BY random()
		LIMIT ?
	`, attrCol, table, attrCol)
	sample, err := s.reader.QueryRaw(ctx, sampleQ, start, sampleSize)
	if err != nil {
		return nil, fmt.Errorf("attr sample: %w", err)
	}

	// Aggregate the sample: for each key, sum byte contribution.
	// The byte contribution for one row is roughly len(value as
	// string); for nested objects we'd need recursive sizing,
	// but in practice attribute values are scalars and the linear
	// approximation is fine.
	contribByKey := map[string]int64{}
	for _, row := range sample {
		// row["attrs"] is a JSON object reified by DuckDB as a string
		// (when JSON column) or map[string]any (when typed). Handle
		// both.
		switch v := row["attrs"].(type) {
		case string:
			accumulateKeyBytes(v, contribByKey)
		case map[string]any:
			for k, val := range v {
				contribByKey[k] += int64(len(k)) + int64(stringLen(val))
			}
		}
	}

	// Sort and trim to top-N.
	type kv struct {
		k string
		v int64
	}
	flat := make([]kv, 0, len(contribByKey))
	for k, v := range contribByKey {
		flat = append(flat, kv{k, v})
	}
	sort.Slice(flat, func(i, j int) bool { return flat[i].v > flat[j].v })
	if len(flat) > limit {
		flat = flat[:limit]
	}

	// Extrapolate from sample to population. Total bytes for the
	// signal = sum of contribByKey × (population / sampleSize).
	// Each row's per-key bytes scale by the same factor.
	sampleN := int64(len(sample))
	if sampleN == 0 {
		s.intoCache(cacheKey, []AttributeVolume{})
		return []AttributeVolume{}, nil
	}
	scale := float64(population) / float64(sampleN)

	// Compute total across all keys (in sample, before truncation)
	// so PctOfSignal is honest about the full attribute footprint
	// of this signal — not just the top-N.
	var sampleTotal int64
	for _, v := range contribByKey {
		sampleTotal += v
	}
	if sampleTotal == 0 {
		s.intoCache(cacheKey, []AttributeVolume{})
		return []AttributeVolume{}, nil
	}

	out := make([]AttributeVolume, 0, len(flat))
	for _, e := range flat {
		out = append(out, AttributeVolume{
			Key:         e.k,
			Bytes:       int64(float64(e.v) * scale),
			Estimated:   true,
			PctOfSignal: float64(e.v) / float64(sampleTotal),
		})
	}

	s.intoCache(cacheKey, out)
	return out, nil
}

// MetricCardinality is one row of the v0.28 high-cardinality
// surface: per metric name, how many DISTINCT label-set combinations
// exist in the window. Metrics with huge cardinality are the OTHER
// big OTel cost killer alongside verbose attributes, because every
// distinct combo becomes its own time-series in the metric backend.
type MetricCardinality struct {
	MetricName     string `json:"metric_name"`
	DistinctCombos int64  `json:"distinct_combos"`
	TotalSamples   int64  `json:"total_samples"`
	// EstimatedHighCardLabels is a hint at WHICH label is driving
	// the cardinality. Sampled — we look at the first ~100 rows of
	// this metric, count distinct values per attribute key, and
	// return the top one. The metricstransform snippet the
	// recommendation generates uses this to suggest which label
	// to drop / aggregate over.
	HighestCardLabel string `json:"highest_card_label,omitempty"`
}

// TopMetricCardinality returns the metrics with the highest
// distinct-combo counts in the window, ordered descending. limit
// caps the response; default 20. minCombos filters out small
// metrics whose cardinality isn't operationally interesting.
//
// Implementation notes:
//   - DuckDB's COUNT(DISTINCT metric_attributes) on a JSON column
//     is expensive but feasible at v0.24 fleet sizes. We cache
//     aggressively (same cacheTTL as the other insights queries).
//   - We only scan metrics_sum for v0.28; metrics_gauge and
//     metrics_histogram add complexity (UNION ALL with different
//     metric-name columns) and the v0.28 recipe is the same advice
//     regardless. If operators ask, v0.28.x extends to all three.
func (s *Service) TopMetricCardinality(ctx context.Context, win Window, limit int, minCombos int64) ([]MetricCardinality, error) {
	dur, err := win.AsDuration()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if minCombos <= 0 {
		minCombos = 2000
	}
	cacheKey := fmt.Sprintf("metricCardinality:%s:%d:%d", win, limit, minCombos)
	if cached := s.fromCache(cacheKey); cached != nil {
		if v, ok := cached.([]MetricCardinality); ok {
			return v, nil
		}
	}

	start := time.Now().UTC().Add(-dur)
	q := `
		SELECT
			metric_name,
			CAST(COUNT(DISTINCT metric_attributes) AS BIGINT) AS combos,
			CAST(COUNT(*) AS BIGINT)                          AS samples
		FROM metrics_sum
		WHERE timestamp >= ?
		GROUP BY metric_name
		HAVING combos >= ?
		ORDER BY combos DESC
		LIMIT ?
	`
	rows, err := s.reader.QueryRaw(ctx, q, start, minCombos, limit)
	if err != nil {
		return nil, fmt.Errorf("top metric cardinality: %w", err)
	}

	out := make([]MetricCardinality, 0, len(rows))
	for _, r := range rows {
		mc := MetricCardinality{
			MetricName:     stringOf(r["metric_name"]),
			DistinctCombos: int64Of(r["combos"]),
			TotalSamples:   int64Of(r["samples"]),
		}
		// Best-effort "which label drives this" hint. Sample 200
		// rows from the metric, scan their attributes JSON, count
		// distinct values per key, pick the highest. Cheap because
		// it's bounded by the sample size, not the population.
		mc.HighestCardLabel = s.findHighestCardLabel(ctx, mc.MetricName, start)
		out = append(out, mc)
	}

	s.intoCache(cacheKey, out)
	return out, nil
}

// findHighestCardLabel samples a metric's recent rows and returns
// the attribute key with the most distinct values. Returns "" when
// the sample is empty or all rows share a single label set. Best-
// effort; correctness of the hint isn't safety-critical (the
// recommendation snippet still surfaces the metric name so the
// operator can confirm).
func (s *Service) findHighestCardLabel(ctx context.Context, metricName string, since time.Time) string {
	rows, err := s.reader.QueryRaw(ctx,
		`SELECT metric_attributes FROM metrics_sum
		 WHERE timestamp >= ? AND metric_name = ?
		 ORDER BY random()
		 LIMIT 200`, since, metricName)
	if err != nil || len(rows) == 0 {
		return ""
	}
	distinctVals := map[string]map[string]struct{}{}
	for _, row := range rows {
		var attrs string
		switch v := row["metric_attributes"].(type) {
		case string:
			attrs = v
		case []byte:
			attrs = string(v)
		default:
			continue
		}
		// Reuse the minimal JSON scanner from accumulateKeyBytes
		// to walk top-level keys + capture values. We only need
		// the (key, value) pair, so a tiny adapter does.
		walkJSONKeys(attrs, func(k, v string) {
			set, ok := distinctVals[k]
			if !ok {
				set = map[string]struct{}{}
				distinctVals[k] = set
			}
			set[v] = struct{}{}
		})
	}
	var bestKey string
	var bestCount int
	for k, set := range distinctVals {
		if len(set) > bestCount {
			bestCount = len(set)
			bestKey = k
		}
	}
	return bestKey
}

// walkJSONKeys is a minimal scanner that calls fn for each
// top-level "key": value pair in a JSON object. Values are passed
// verbatim (including quotes for strings). Same defensive parsing
// as accumulateKeyBytes — failures silently no-op.
func walkJSONKeys(jsonStr string, fn func(key, value string)) {
	in := []byte(jsonStr)
	i := 0
	n := len(in)
	for i < n && (in[i] == ' ' || in[i] == '\t' || in[i] == '\n') {
		i++
	}
	if i >= n || in[i] != '{' {
		return
	}
	i++
	for i < n {
		for i < n && (in[i] == ' ' || in[i] == '\t' || in[i] == '\n' || in[i] == ',') {
			i++
		}
		if i >= n || in[i] == '}' {
			return
		}
		if in[i] != '"' {
			return
		}
		keyStart := i + 1
		i++
		for i < n && in[i] != '"' {
			if in[i] == '\\' && i+1 < n {
				i += 2
				continue
			}
			i++
		}
		if i >= n {
			return
		}
		key := string(in[keyStart:i])
		i++
		for i < n && (in[i] == ':' || in[i] == ' ' || in[i] == '\t') {
			i++
		}
		valStart := i
		switch {
		case i < n && in[i] == '"':
			i++
			for i < n && in[i] != '"' {
				if in[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				i++
			}
			if i < n {
				i++
			}
		case i < n && (in[i] == '{' || in[i] == '['):
			open := in[i]
			closeByte := byte('}')
			if open == '[' {
				closeByte = ']'
			}
			depth := 1
			i++
			for i < n && depth > 0 {
				if in[i] == '"' {
					i++
					for i < n && in[i] != '"' {
						if in[i] == '\\' && i+1 < n {
							i += 2
							continue
						}
						i++
					}
				} else if in[i] == open {
					depth++
				} else if in[i] == closeByte {
					depth--
				}
				i++
			}
		default:
			for i < n && in[i] != ',' && in[i] != '}' && in[i] != '\n' {
				i++
			}
		}
		fn(key, string(in[valStart:i]))
	}
}

// Drops returns the drop count per signal across the window.
// Currently a thin wrapper around the same otlp_batches table —
// drops live alongside successful ingest counts so we don't need
// a separate path.
func (s *Service) Drops(ctx context.Context, win Window) ([]SignalVolume, error) {
	dur, err := win.AsDuration()
	if err != nil {
		return nil, err
	}
	start := time.Now().UTC().Add(-dur)
	rows, err := s.reader.QueryRaw(ctx, `
		SELECT signal_type,
		       CAST(COALESCE(SUM(dropped_count), 0) AS BIGINT) AS dropped,
		       CAST(COALESCE(SUM(item_count), 0)    AS BIGINT) AS items,
		       CAST(COALESCE(SUM(payload_bytes), 0) AS BIGINT) AS bytes
		FROM otlp_batches
		WHERE timestamp >= ? AND status != 'ok'
		GROUP BY signal_type
	`, start)
	if err != nil {
		return nil, fmt.Errorf("drops query: %w", err)
	}
	out := make([]SignalVolume, 0, len(rows))
	for _, r := range rows {
		out = append(out, SignalVolume{
			Signal:       Signal(stringOf(r["signal_type"])),
			Bytes:        int64Of(r["bytes"]),
			ItemCount:    int64Of(r["items"]),
			DroppedCount: int64Of(r["dropped"]),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return signalOrder(out[i].Signal) < signalOrder(out[j].Signal)
	})
	return out, nil
}

// ----------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------

// accumulateKeyBytes parses a JSON object literal and adds each
// (key, value) contribution to the running tally. Conservative —
// failures to parse are silent (the row just doesn't contribute).
// Uses a minimal scanner rather than encoding/json to avoid
// allocating intermediate maps; we only need keys + scalar value
// lengths.
func accumulateKeyBytes(jsonStr string, dst map[string]int64) {
	// Minimal-but-correct: find top-level "key": value pairs and
	// count the byte length of the value. For nested objects,
	// fall back to a coarse "length of the substring" approx by
	// scanning to the matching brace.
	//
	// This isn't a fully spec-compliant JSON parser — it's a
	// best-effort attribution sampler. Wrong bytes here only
	// affect the estimate, never the underlying telemetry.
	in := []byte(jsonStr)
	i := 0
	n := len(in)
	// Skip leading whitespace + opening brace.
	for i < n && (in[i] == ' ' || in[i] == '\t' || in[i] == '\n') {
		i++
	}
	if i >= n || in[i] != '{' {
		return
	}
	i++
	for i < n {
		// Skip whitespace + commas.
		for i < n && (in[i] == ' ' || in[i] == '\t' || in[i] == '\n' || in[i] == ',') {
			i++
		}
		if i >= n || in[i] == '}' {
			return
		}
		if in[i] != '"' {
			return
		}
		// Read the key.
		keyStart := i + 1
		i++
		for i < n && in[i] != '"' {
			if in[i] == '\\' && i+1 < n {
				i += 2
				continue
			}
			i++
		}
		if i >= n {
			return
		}
		key := string(in[keyStart:i])
		i++ // consume closing quote
		// Skip ":" and whitespace.
		for i < n && (in[i] == ':' || in[i] == ' ' || in[i] == '\t') {
			i++
		}
		// Read the value's byte length. Three cases: string,
		// number, object/array. For string we include the quotes
		// in the size (mirrors wire cost). For numbers/objects we
		// scan to the next top-level separator.
		valStart := i
		switch {
		case i < n && in[i] == '"':
			i++
			for i < n && in[i] != '"' {
				if in[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				i++
			}
			if i < n {
				i++
			}
		case i < n && (in[i] == '{' || in[i] == '['):
			open := in[i]
			close := byte('}')
			if open == '[' {
				close = ']'
			}
			depth := 1
			i++
			for i < n && depth > 0 {
				if in[i] == '"' {
					i++
					for i < n && in[i] != '"' {
						if in[i] == '\\' && i+1 < n {
							i += 2
							continue
						}
						i++
					}
				} else if in[i] == open {
					depth++
				} else if in[i] == close {
					depth--
				}
				i++
			}
		default:
			for i < n && in[i] != ',' && in[i] != '}' && in[i] != '\n' {
				i++
			}
		}
		valLen := i - valStart
		dst[key] += int64(len(key)) + int64(valLen)
	}
}

// stringLen returns a reasonable byte length for an arbitrary
// JSON-decoded value. Strings: len(s). Numbers: a fixed 8 (an
// approximation that's good enough for an attribute byte sampler).
// Maps/slices: a coarse JSON-encoded length proxy via reflection
// (avoided here by returning a small constant — accurate sizing
// only matters when those types dominate, which is rare for
// attribute values).
func stringLen(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case float64:
		return 8
	case bool:
		return 4
	case nil:
		return 0
	default:
		return 16
	}
}

func (s *Service) fromCache(key string) any {
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

func (s *Service) intoCache(key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = cacheEntry{storedAt: time.Now(), value: v}
}

func joinSignals(sigs []Signal) string {
	if len(sigs) == 0 {
		return "*"
	}
	parts := make([]string, len(sigs))
	for i, s := range sigs {
		parts[i] = string(s)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func signalOrder(s Signal) int {
	switch s {
	case SignalTraces:
		return 0
	case SignalMetrics:
		return 1
	case SignalLogs:
		return 2
	}
	return 3
}

// stringOf / int64Of are tiny tolerant coercions for the
// map[string]any rows DuckDB returns. The reader's driver hands
// back different concrete types depending on column type and
// nullability; we accept the common ones rather than panic.
func stringOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		if x == nil {
			return ""
		}
		return fmt.Sprintf("%v", x)
	}
}

func int64Of(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case uint64:
		return int64(x)
	case nil:
		return 0
	case *big.Int:
		// DuckDB widens SUM(BIGINT) to HUGEINT (128-bit), which the
		// marcboeker/go-duckdb driver reifies as *big.Int. We cast in
		// SQL too (defense in depth), but accept this here so the
		// query layer is robust to driver upgrades or query shapes we
		// haven't cast.
		if x == nil {
			return 0
		}
		if x.IsInt64() {
			return x.Int64()
		}
		// Larger than int64 — saturate. In practice telemetry byte
		// counts don't exceed int64 anytime this century, so this is
		// a paranoia clamp.
		return 1<<63 - 1
	case big.Int:
		if x.IsInt64() {
			return x.Int64()
		}
		return 1<<63 - 1
	default:
		return 0
	}
}
