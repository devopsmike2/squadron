// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package recommendations is the v0.25 cost-optimization advice
// engine. It reads from the v0.24 insights surface, runs a set of
// pure-Go heuristic "recipes" against the latest fleet snapshot,
// and returns a ranked list of actionable Recommendations.
//
// Every recommendation carries:
//
//   - A stable ID (hashed from recipe + scope) so dismissals stick
//     across re-evaluations.
//   - A category + severity so the UI can group and color.
//   - A YAML snippet that, dropped into a collector config, would
//     plausibly enact the suggestion.
//   - An estimated bytes-saved figure so operators can prioritize
//     by impact rather than guess.
//
// The engine is pure heuristic — no LLM, no historical-data store.
// That's a deliberate v0.25 choice: heuristics are auditable,
// repeatable, and fast (<10 ms per evaluation in benchmarks). The
// "AI-assisted config" arc is its own track.
package recommendations

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/insights"
)

// Category groups recipes into UI buckets. Stable string values so
// the UI can color/icon them without a switch on every render.
type Category string

const (
	// CategoryNoisyAttribute — an attribute key is contributing a
	// disproportionate share of a signal's byte budget. Suggested fix:
	// drop or hash it via attributes processor.
	CategoryNoisyAttribute Category = "noisy_attribute"

	// CategoryOutlierAgent — one agent is producing dramatically
	// more telemetry than its peers. Suggested fix: investigate the
	// config; the agent may be missing a sampling step.
	CategoryOutlierAgent Category = "outlier_agent"

	// CategoryDropHotspot — a signal is being rejected at the
	// receiver or dead-lettered after retries. Suggested fix:
	// inspect rate limits / capacity.
	CategoryDropHotspot Category = "drop_hotspot"

	// CategoryEmptySignal — an agent reports a signal type with
	// zero bytes for the entire window. Suggested fix: drop the
	// pipeline branch on the source side.
	CategoryEmptySignal Category = "empty_signal"

	// CategoryHighCardinality (v0.28) — a metric has so many
	// distinct label-set combinations that it's almost certainly
	// driving outsized storage cost in the metric backend.
	// Suggested fix: metricstransform/aggregate_labels to drop the
	// highest-cardinality dimension.
	CategoryHighCardinality Category = "high_cardinality"
)

// Severity is a coarse three-step scale. We resist the urge to
// invent more — operators are scanning, not reading.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarn     Severity = "warn"
	SeverityInfo     Severity = "info"
)

// Recommendation is one piece of advice the engine produced. The
// JSON shape is wire-stable — UI clients and the v0.25 squadronctl
// commands depend on these field names.
type Recommendation struct {
	// ID is deterministic from (recipe, scope). Same inputs → same
	// ID, so dismissals survive re-evaluations.
	ID string `json:"id"`

	Category Category `json:"category"`
	Severity Severity `json:"severity"`

	// Title is the headline shown in lists. Imperative voice,
	// <80 chars: "Drop attribute http.url from metrics".
	Title string `json:"title"`

	// Detail is the longer-form explanation rendered when the
	// operator expands the card. Multi-line OK.
	Detail string `json:"detail"`

	// AgentID is set when the recommendation is scoped to a single
	// agent; empty for fleet-wide advice.
	AgentID   string `json:"agent_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`

	// Signal narrows the recommendation to one telemetry type when
	// applicable (most do). Empty for cross-signal advice.
	Signal insights.Signal `json:"signal,omitempty"`

	// EstSavingsBytes is the bytes-per-window the suggested fix
	// would plausibly avoid. -1 when not estimable (e.g. drop
	// hotspots where the "saving" is reliability, not bytes).
	EstSavingsBytes int64 `json:"est_savings_bytes"`

	// EstSavingsPerMonthUSD is the bytes saving translated through
	// the v0.27 pricing model. 0 when the engine wasn't given a
	// pricing projector, or when EstSavingsBytes is non-positive.
	// This is the operator-friendly number the Savings dashboard
	// renders prominently; the byte figure stays for accountability.
	EstSavingsPerMonthUSD float64 `json:"est_savings_per_month_usd,omitempty"`

	// PctOfSignal is the share of the signal's byte budget the
	// recommendation targets. Useful for the UI's progress bars.
	PctOfSignal float64 `json:"pct_of_signal,omitempty"`

	// Snippet is a small valid OpenTelemetry Collector YAML
	// fragment the operator can paste into their config. Includes
	// a header comment explaining the source of the advice.
	Snippet string `json:"snippet,omitempty"`

	// GeneratedAt is the snapshot time the recommendation reflects.
	// Clients can show "as of 2 min ago" if they care.
	GeneratedAt time.Time `json:"generated_at"`

	// Source — v0.85 — typed source for the recommendation.
	// Distinguishes recommendations produced by the cost-spike
	// pipeline (existing JARVIS arc) from discovery scans
	// (universal observation arc) and from operator manual
	// creation. The Source field is purely descriptive — it
	// doesn't change how the engine ranks, dismisses, or displays
	// the recommendation. Existing producers leave this nil; the
	// JSON tag uses omitempty so v0.84-era wire shapes are
	// unchanged on the wire.
	Source *RecommendationSource `json:"source,omitempty"`

	// Action — v0.85 — typed action payload. When the
	// recommendation has a concrete action the operator can take
	// (start a rollout, create a plan, apply a discovery-action),
	// the typed payload carries the information the UI needs to
	// render the correct button + confirmation flow. When Action
	// is nil, the recommendation is advisory only — operator
	// copies the Snippet and applies it out-of-band.
	Action *RecommendationAction `json:"action,omitempty"`

	// IaC — v0.85 — Infrastructure-as-Code snippet for cloud-side
	// changes. When present, the operator runs this through their
	// existing IaC pipeline (Terraform, CDK, Pulumi). Squadron
	// does NOT execute it. Empty for collector-side
	// recommendations whose remediation is captured by the
	// existing Snippet field (collector YAML).
	IaC *IaCSnippet `json:"iac,omitempty"`

	// ResourceKind — v0.89.3 #603 Stream 19 Phase 4 — keys this
	// recommendation against the IaC connection's placement map.
	// Set ONLY on discovery-source recommendations whose snippet
	// targets a known kind from the slice-1 placement-map list
	// (ec2-otel-layer, lambda-otel-layer, rds-pi-em,
	// s3-access-logging, alb-access-logs, eks-cluster-logging,
	// eks-observability-addon). Empty when the recommendation is
	// not Open-PR-eligible (collector-side advice, or a discovery
	// step whose snippet didn't classify into a known kind). The
	// Recommendations tab's Open-PR button reads this field to
	// decide whether the Open-PR action is available, and to send
	// it on the POST /iac/github/connections/:id/open-pr payload.
	ResourceKind string `json:"resource_kind,omitempty"`

	// AffectedResources — v0.89.4 #611 Stream 19 Phase 4 follow-on
	// — list of resource identifiers (ARNs for AWS where
	// available, otherwise the canonical id Squadron uses
	// internally) that this recommendation's plan step instruments.
	// Sourced from the discovery proposer's per-step
	// affected_resources output. The Recommendations tab forwards
	// this list to the Open-PR backend, which uses len() in the PR
	// title ("instrument <kind> for <N> resources") and renders the
	// list as the "Affected resources" bullet list in the PR body.
	// Empty when the proposer model didn't emit the field — the PR
	// title falls back to "for 0 resources" (same as Phase 4
	// behavior) rather than erroring; the body's section is
	// omitted. The Recommendations tab UI does NOT render this
	// list to the operator in slice 1.5; it is metadata for the
	// backend's PR-text construction.
	AffectedResources []string `json:"affected_resources,omitempty"`

	// Disposition — v0.89.11 #626 Stream 27 (slice 1.5) — names
	// HOW the Open-PR handler should land the snippet in the
	// operator's IaC repo. Two values, from internal/iac:
	//
	//   - "new_file" — the snippet is a NET-NEW top-level resource;
	//     Squadron writes a sibling file
	//     squadron_<resource_kind>.tf in the placement file's
	//     directory. Clean drop-in; merge-clean.
	//   - "patch_existing" — the snippet modifies an EXISTING
	//     top-level resource block; Squadron appends to the
	//     placement file (slice-1 behavior) and labels the PR
	//     "[needs manual merge]" so the operator knows hand
	//     integration is required.
	//
	// The classification is STRUCTURAL (per the per-kind table in
	// internal/iac/dispositions.go) — the proposer outputs it but
	// the Open-PR handler OVERRIDES the model's choice with the
	// canonical lookup on every request. The UI reads this field
	// to render a "Needs manual merge" badge next to the Open PR
	// button for patch_existing kinds.
	//
	// Empty for non-IaC recommendations (collector-side advice,
	// cost-spike outputs) and for discovery recommendations whose
	// ResourceKind is empty.
	Disposition string `json:"disposition,omitempty"`
}

// SourceKind is the typed enum carried on RecommendationSource.
// Stable string values so the UI can route on them without a
// switch on every render.
type SourceKind string

const (
	// SourceCostSpike — produced by the JARVIS cost-spike
	// pipeline. The RefID typically points at the cost-spike
	// detection record.
	SourceCostSpike SourceKind = "cost_spike"

	// SourceDiscoveryScan — produced by a discovery scan (AWS /
	// GCP / on-prem). The RefID typically points at the
	// discovery_scan record.
	SourceDiscoveryScan SourceKind = "discovery_scan"

	// SourceManual — created by an operator via the manual
	// creation flow. The RefID typically points at the actor user
	// id.
	SourceManual SourceKind = "manual"
)

// RecommendationSource is the typed "where did this come from"
// pointer. RefID is descriptive only — the engine does not
// dereference it.
type RecommendationSource struct {
	Kind  SourceKind `json:"kind"`
	RefID string     `json:"ref_id,omitempty"` // cost_spike_id / discovery_scan_id / actor id
}

// ActionKind is the typed enum carried on RecommendationAction.
// The UI matches Kind to unmarshal Payload into the right shape.
type ActionKind string

const (
	// ActionRollout — Squadron should kick off a rollout. The
	// payload carries the rollout request (target group, config
	// version, canary policy, etc.).
	ActionRollout ActionKind = "rollout"

	// ActionPlan — Squadron should create a (multi-step) plan.
	// The payload carries the proposed plan shape.
	ActionPlan ActionKind = "plan"

	// ActionDiscoveryAction — operator should review a
	// discovery-action (cloud-side IaC change). The payload
	// carries the discovery-action handle the UI uses to deeplink.
	ActionDiscoveryAction ActionKind = "discovery_action"
)

// RecommendationAction is a typed action payload. Payload is
// action-specific JSON that the UI unmarshals based on Kind.
// Marshalling the Recommendation re-emits Payload as inline JSON
// rather than re-encoded escaped text.
type RecommendationAction struct {
	Kind ActionKind `json:"kind"`
	// Payload is action-specific JSON. The UI matches Kind to
	// unmarshal Payload into the right shape.
	Payload json.RawMessage `json:"payload"`
}

// IaCFormat is the typed enum carried on IaCSnippet. Slice 1
// ships Terraform; CDK and Pulumi land as later slices without a
// shape change.
type IaCFormat string

const (
	IaCTerraform IaCFormat = "terraform"
	IaCCDK       IaCFormat = "cdk"
	IaCPulumi    IaCFormat = "pulumi"
)

// IaCSnippet is the Infrastructure-as-Code snippet attached to a
// recommendation. Source is the actual Terraform/CDK/Pulumi code
// the operator pastes into their IaC pipeline. Squadron does NOT
// execute this — the thesis decision is explicit.
type IaCSnippet struct {
	Format IaCFormat `json:"format"`
	Source string    `json:"source"` // the actual Terraform/CDK/Pulumi code
}

// RecommendationOptions carries the v0.85 typed metadata
// producers attach when constructing a recommendation. Existing
// recipe code paths pass nil/zero; future producers (discovery
// scanner, manual creation flow) populate the relevant fields.
//
// Kept as a separate struct rather than threading three new
// arguments into every recipe so growth is additive: new
// metadata categories add a field to RecommendationOptions, not
// a parameter to every constructor.
type RecommendationOptions struct {
	Source *RecommendationSource
	Action *RecommendationAction
	IaC    *IaCSnippet
}

// applyOptions attaches the typed metadata (if any) to a
// recommendation. Returns the recommendation by value so call
// sites can keep their existing append-by-value pattern.
//
// Callers pass nil when they have nothing to attach; this is the
// hook existing recipe code uses (recipes pass nil). Future
// discovery / manual producers construct the options struct and
// pass it in.
func applyOptions(rec Recommendation, opts *RecommendationOptions) Recommendation {
	if opts == nil {
		return rec
	}
	if opts.Source != nil {
		rec.Source = opts.Source
	}
	if opts.Action != nil {
		rec.Action = opts.Action
	}
	if opts.IaC != nil {
		rec.IaC = opts.IaC
	}
	return rec
}

// Dismissals is the interface the engine uses to filter out
// recommendations operators have explicitly dismissed. Implemented
// by internal/services/recommendation_dismissals on top of the app
// store; faked in tests. Kept narrow on purpose so the engine
// doesn't depend on a particular storage layout.
type Dismissals interface {
	IsDismissed(ctx context.Context, recID string) (bool, error)
}

// noopDismissals is the safe default when the caller hasn't wired
// a dismissals store. Pass it explicitly via NewEngine to keep the
// interface non-optional at the call site.
type noopDismissals struct{}

func (noopDismissals) IsDismissed(context.Context, string) (bool, error) { return false, nil }

// NoopDismissals returns a Dismissals that never filters anything.
// Useful in tests and on first boot before the app DB is wired.
func NoopDismissals() Dismissals { return noopDismissals{} }

// AgentNameResolver lets the engine annotate per-agent
// recommendations with a human-readable name. We deliberately don't
// fetch the full agent record — that's wasteful when the engine
// already has the agent_id and just needs the label.
type AgentNameResolver func(ctx context.Context, agentID string) string

// Pricer is the narrow slice of pricing.Projector the engine uses.
// Extracted as an interface so internal/recommendations doesn't
// import internal/pricing (keeping the dependency arrow pointing
// from main.go outward); pricing.Projector satisfies it at runtime
// through a tiny adapter constructed in main.go.
//
// MonthlyForBytes is given a byte rate scoped to the engine's
// evaluation window (the engine normalizes from window to hours
// before calling). signal is the recommendation's signal; empty
// string asks the pricer to use the catch-all base rate.
type Pricer interface {
	MonthlyForBytesPerHour(bytesPerHour int64, signal string) float64
}

// InsightsQuerier is the narrow slice of insights.Service that
// the engine needs. Extracting it as an interface lets us pass a
// fake in tests without spinning up DuckDB; *insights.Service
// satisfies it at runtime.
type InsightsQuerier interface {
	FleetVolume(ctx context.Context, win insights.Window, signalFilter []insights.Signal) (*insights.FleetSummary, error)
	TopAgents(ctx context.Context, win insights.Window, limit int) ([]insights.AgentVolume, error)
	TopAttributes(ctx context.Context, win insights.Window, signal insights.Signal, limit int) ([]insights.AttributeVolume, error)
	// v0.28 — high-cardinality detection. Optional: implementations
	// that don't have a metric-name × distinct-combos query can
	// return (nil, nil) and the recipe will skip cleanly.
	TopMetricCardinality(ctx context.Context, win insights.Window, limit int, minCombos int64) ([]insights.MetricCardinality, error)
}

// Engine is the public surface. Construct with NewEngine, call
// Evaluate. Thread-safe; safe to share across requests.
type Engine struct {
	insights InsightsQuerier
	dismiss  Dismissals
	resolve  AgentNameResolver
	pricer   Pricer // nil → no $ projection on recommendations
	logger   *zap.Logger

	// Heuristic thresholds. Exposed as fields so a future
	// configuration surface (env vars, per-tenant overrides) can
	// tweak without touching recipe code. v0.25 ships with these
	// defaults; v0.25.x may make them runtime-configurable.
	NoisyAttributeMinPct float64 // default 0.15 (15%)
	OutlierAgentRatio    float64 // default 2.0  (2× fleet median)
	DropHotspotMinPct    float64 // default 0.01 (1%)
	EmptySignalMinAgents int     // default 3 — avoid noise on tiny fleets

	// Evaluation cache (insights service has its own cache, but
	// this engine adds a thin layer so concurrent UI polls don't
	// re-rank). 10s TTL pairs with the UI's 30s polling.
	mu       sync.Mutex
	cache    *cachedSnapshot
	cacheTTL time.Duration
}

type cachedSnapshot struct {
	storedAt time.Time
	value    []Recommendation
	key      string
}

// SetPricer wires the (optional) pricing projector after
// construction. Setter pattern so the v0.25 NewEngine signature
// stays back-compat and tests that don't care about pricing don't
// have to thread a nil pricer through.
func (e *Engine) SetPricer(p Pricer) { e.pricer = p }

// NewEngine builds the engine. Pass NoopDismissals() if you don't
// have a dismissals store yet; the resolve fn can be nil and the
// engine will leave AgentName empty. svc accepts any
// InsightsQuerier so tests can pass a fake; *insights.Service
// satisfies the interface at runtime.
func NewEngine(svc InsightsQuerier, dismiss Dismissals, resolve AgentNameResolver, logger *zap.Logger) *Engine {
	if dismiss == nil {
		dismiss = NoopDismissals()
	}
	return &Engine{
		insights:             svc,
		dismiss:              dismiss,
		resolve:              resolve,
		logger:               logger,
		NoisyAttributeMinPct: 0.15,
		OutlierAgentRatio:    2.0,
		DropHotspotMinPct:    0.01,
		EmptySignalMinAgents: 3,
		cacheTTL:             10 * time.Second,
	}
}

// Evaluate runs every recipe against the fleet snapshot for the
// given window and returns the surviving recommendations, ranked
// by severity then estimated savings.
//
// Returns an empty slice (not nil) when there's nothing to say —
// caller can json.Marshal without a nil-check.
func (e *Engine) Evaluate(ctx context.Context, win insights.Window) ([]Recommendation, error) {
	// Cache check. The key is just the window — recipe outputs are
	// pure functions of the insights snapshot, so two requests for
	// the same window within the TTL get the same answer.
	cacheKey := "win:" + string(win)
	e.mu.Lock()
	if e.cache != nil && e.cache.key == cacheKey && time.Since(e.cache.storedAt) < e.cacheTTL {
		out := append([]Recommendation(nil), e.cache.value...)
		e.mu.Unlock()
		return out, nil
	}
	e.mu.Unlock()

	now := time.Now().UTC()
	out := make([]Recommendation, 0, 16)

	// Fan out to the insights service. Each recipe reads what it
	// needs; we don't pre-aggregate because the insights service
	// caches its own queries.
	fleet, err := e.insights.FleetVolume(ctx, win, nil)
	if err != nil {
		return nil, fmt.Errorf("fleet snapshot: %w", err)
	}
	// Recipes — order matters only insofar as duplicate IDs across
	// recipes would collide. We use distinct ID prefixes per recipe
	// so collisions can't happen.
	out = append(out, e.recipeNoisyAttribute(ctx, win, fleet, now)...)
	out = append(out, e.recipeOutlierAgent(ctx, win, fleet, now)...)
	out = append(out, e.recipeDropHotspot(ctx, win, fleet, now)...)
	out = append(out, e.recipeEmptySignal(ctx, win, fleet, now)...)
	out = append(out, e.recipeHighCardinality(ctx, win, now)...)

	// Filter dismissals. Done after generation so a future
	// "restore" path doesn't have to remember what was filtered.
	kept := out[:0]
	for _, r := range out {
		dismissed, err := e.dismiss.IsDismissed(ctx, r.ID)
		if err != nil {
			// Conservative: surface the recommendation. Log so the
			// dismissals storage misbehavior is visible without
			// being silently lost.
			e.logger.Warn("dismissal check failed",
				zap.String("rec_id", r.ID), zap.Error(err))
			kept = append(kept, r)
			continue
		}
		if !dismissed {
			kept = append(kept, r)
		}
	}

	// v0.27: annotate each recommendation with a $/month figure
	// before ranking. Done in a single pass so recipes stay
	// pricing-agnostic and the engine can swap the pricer at
	// runtime without touching recipe code. The window for the
	// engine is the insights window (5m/1h/24h); normalize to
	// bytes-per-hour before asking the pricer.
	if e.pricer != nil {
		windowSeconds := int64(0)
		if dur, err := win.AsDuration(); err == nil {
			windowSeconds = int64(dur.Seconds())
		}
		for i := range kept {
			if kept[i].EstSavingsBytes <= 0 || windowSeconds <= 0 {
				continue
			}
			bytesPerHour := int64(float64(kept[i].EstSavingsBytes) * 3600.0 / float64(windowSeconds))
			kept[i].EstSavingsPerMonthUSD = e.pricer.MonthlyForBytesPerHour(
				bytesPerHour, string(kept[i].Signal))
		}
	}

	// Rank: severity DESC, then estimated savings DESC. Stable
	// sort so deterministic given identical inputs (helps testing).
	sort.SliceStable(kept, func(i, j int) bool {
		if severityRank(kept[i].Severity) != severityRank(kept[j].Severity) {
			return severityRank(kept[i].Severity) > severityRank(kept[j].Severity)
		}
		return kept[i].EstSavingsBytes > kept[j].EstSavingsBytes
	})

	e.mu.Lock()
	e.cache = &cachedSnapshot{storedAt: now, value: kept, key: cacheKey}
	e.mu.Unlock()

	return kept, nil
}

// InvalidateCache drops the engine's cached snapshot. Call after
// any state change that should be reflected in the next Evaluate:
// dismissing or restoring a recommendation, mostly. Cheap; safe to
// call frequently.
func (e *Engine) InvalidateCache() {
	e.mu.Lock()
	e.cache = nil
	e.mu.Unlock()
}

// EvaluateForAgent narrows to recommendations whose AgentID matches.
// Convenience for the agent-detail drawer; runs the full evaluation
// (the cache covers the cost) and filters down.
func (e *Engine) EvaluateForAgent(ctx context.Context, win insights.Window, agentID string) ([]Recommendation, error) {
	all, err := e.Evaluate(ctx, win)
	if err != nil {
		return nil, err
	}
	out := make([]Recommendation, 0, 4)
	for _, r := range all {
		if r.AgentID == agentID {
			out = append(out, r)
		}
	}
	return out, nil
}

// ----------------------------------------------------------------
// Recipes — each is a small pure function over the insights
// snapshot. Adding new recipes is the most common kind of v0.25.x
// change; the pattern is: take fleet + insights handle, emit zero
// or more Recommendations.
// ----------------------------------------------------------------

// recipeNoisyAttribute — per signal, fetch the top-attributes
// estimate and emit one recommendation per key whose PctOfSignal
// exceeds the threshold. The "drop the attribute" advice is the
// single highest-ROI optimization in production OTel deployments,
// so it gets prime placement and Critical severity at high pct.
func (e *Engine) recipeNoisyAttribute(ctx context.Context, win insights.Window, fleet *insights.FleetSummary, now time.Time) []Recommendation {
	out := make([]Recommendation, 0, 4)
	for _, sv := range fleet.BySignal {
		if sv.Bytes == 0 {
			continue
		}
		attrs, err := e.insights.TopAttributes(ctx, win, sv.Signal, 20)
		if err != nil {
			e.logger.Warn("topAttributes failed; skipping recipe",
				zap.String("signal", string(sv.Signal)), zap.Error(err))
			continue
		}
		for _, a := range attrs {
			if a.PctOfSignal < e.NoisyAttributeMinPct {
				continue
			}
			sev := SeverityWarn
			if a.PctOfSignal >= 0.30 {
				sev = SeverityCritical
			}
			// Estimated savings: the attribute's byte share of the
			// signal's total. The TopAttributes Bytes field is
			// already extrapolated from the sample, but we re-base
			// against the signal total so the figure aligns with
			// the volume panel the operator just saw.
			est := int64(float64(sv.Bytes) * a.PctOfSignal)
			out = append(out, Recommendation{
				ID: idFor("noisy_attr", string(sv.Signal), a.Key),
				Category:        CategoryNoisyAttribute,
				Severity:        sev,
				Title:           fmt.Sprintf("Drop attribute %q from %s", a.Key, sv.Signal),
				Detail:          noisyAttributeDetail(a, sv),
				Signal:          sv.Signal,
				EstSavingsBytes: est,
				PctOfSignal:     a.PctOfSignal,
				Snippet:         attributeDeleteSnippet(a.Key, sv.Signal),
				GeneratedAt:     now,
			})
		}
	}
	return out
}

// recipeOutlierAgent — surface agents producing >= ratio× the
// median agent's bytes. Median (not mean) so a single absurd
// outlier doesn't pull the threshold up and hide the next-worst.
func (e *Engine) recipeOutlierAgent(ctx context.Context, win insights.Window, _ *insights.FleetSummary, now time.Time) []Recommendation {
	agents, err := e.insights.TopAgents(ctx, win, 50)
	if err != nil {
		e.logger.Warn("topAgents failed; skipping recipe", zap.Error(err))
		return nil
	}
	if len(agents) < 4 {
		// Tiny fleets — outlier detection is meaningless when N=2.
		return nil
	}
	// Median of TotalBytes across the top-N. Use top-N rather than
	// full fleet because the full fleet may have many zero-traffic
	// agents that drag the median down and produce false positives.
	sorted := append([]insights.AgentVolume(nil), agents...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TotalBytes < sorted[j].TotalBytes })
	median := sorted[len(sorted)/2].TotalBytes
	if median == 0 {
		return nil
	}
	threshold := int64(float64(median) * e.OutlierAgentRatio)

	out := make([]Recommendation, 0, 2)
	for _, a := range agents {
		if a.TotalBytes < threshold {
			continue
		}
		ratio := float64(a.TotalBytes) / float64(median)
		sev := SeverityWarn
		if ratio >= 5 {
			sev = SeverityCritical
		}
		name := a.AgentName
		if name == "" && e.resolve != nil {
			name = e.resolve(ctx, a.AgentID)
		}
		out = append(out, Recommendation{
			ID:              idFor("outlier_agent", a.AgentID),
			Category:        CategoryOutlierAgent,
			Severity:        sev,
			Title:           fmt.Sprintf("Agent %s produces %.1f× the fleet median", shortAgentLabel(name, a.AgentID), ratio),
			Detail:          outlierAgentDetail(a, ratio, median),
			AgentID:         a.AgentID,
			AgentName:       name,
			EstSavingsBytes: a.TotalBytes - median, // savings if it dropped to median
			Snippet:         "", // no single snippet — operator needs to review the agent's config
			GeneratedAt:     now,
		})
	}
	return out
}

// recipeDropHotspot — any signal with non-zero drops in the window.
// Severity scales with the drop rate. Snippet suggests bumping the
// batch-processor send_batch_size; that's the single most common
// fix for dead-letter pressure.
func (e *Engine) recipeDropHotspot(ctx context.Context, win insights.Window, fleet *insights.FleetSummary, now time.Time) []Recommendation {
	out := make([]Recommendation, 0, 2)
	for _, sv := range fleet.BySignal {
		if sv.DroppedCount == 0 || sv.ItemCount == 0 {
			continue
		}
		dropPct := float64(sv.DroppedCount) / float64(sv.ItemCount+sv.DroppedCount)
		if dropPct < e.DropHotspotMinPct {
			continue
		}
		sev := SeverityWarn
		if dropPct >= 0.05 {
			sev = SeverityCritical
		}
		out = append(out, Recommendation{
			ID:              idFor("drop_hotspot", string(sv.Signal)),
			Category:        CategoryDropHotspot,
			Severity:        sev,
			Title:           fmt.Sprintf("%.2f%% of %s items are being dropped", dropPct*100, sv.Signal),
			Detail:          dropHotspotDetail(sv, dropPct),
			Signal:          sv.Signal,
			EstSavingsBytes: -1, // reliability, not bytes
			Snippet:         batchProcessorSnippet(sv.Signal),
			GeneratedAt:     now,
		})
	}
	return out
}

// recipeEmptySignal — any agent whose total bytes is non-zero but
// has zero bytes for at least one signal. Suggests the agent's
// pipeline ships a branch that produces nothing — easy to prune.
// We require >= EmptySignalMinAgents in the fleet so a 1-agent dev
// run doesn't spam recommendations.
func (e *Engine) recipeEmptySignal(ctx context.Context, win insights.Window, fleet *insights.FleetSummary, now time.Time) []Recommendation {
	if fleet.AgentCount < e.EmptySignalMinAgents {
		return nil
	}
	agents, err := e.insights.TopAgents(ctx, win, 100)
	if err != nil {
		e.logger.Warn("topAgents failed; skipping empty-signal recipe", zap.Error(err))
		return nil
	}
	// Figure out which signals the fleet actually produces. An
	// agent's "empty signal" only counts if other agents are
	// emitting that signal — otherwise it's "nobody uses this
	// signal", which is fleet-wide and a different recipe.
	fleetHas := map[insights.Signal]bool{}
	for _, sv := range fleet.BySignal {
		if sv.Bytes > 0 {
			fleetHas[sv.Signal] = true
		}
	}

	out := make([]Recommendation, 0, 4)
	for _, a := range agents {
		if a.TotalBytes == 0 {
			continue
		}
		have := map[insights.Signal]bool{}
		for _, sv := range a.BySignal {
			if sv.Bytes > 0 {
				have[sv.Signal] = true
			}
		}
		for sig := range fleetHas {
			if have[sig] {
				continue
			}
			// Only emit when the agent has non-trivial volume on
			// other signals — otherwise it's probably just lightly
			// loaded, not misconfigured.
			if a.TotalBytes < 10_000 {
				continue
			}
			name := a.AgentName
			if name == "" && e.resolve != nil {
				name = e.resolve(ctx, a.AgentID)
			}
			out = append(out, Recommendation{
				ID:              idFor("empty_signal", a.AgentID, string(sig)),
				Category:        CategoryEmptySignal,
				Severity:        SeverityInfo,
				Title:           fmt.Sprintf("Agent %s reports no %s; consider pruning the pipeline", shortAgentLabel(name, a.AgentID), sig),
				Detail:          emptySignalDetail(a, sig),
				AgentID:         a.AgentID,
				AgentName:       name,
				Signal:          sig,
				EstSavingsBytes: 0, // no byte savings — operational hygiene
				Snippet:         "", // no single snippet — operator deletes the pipeline branch
				GeneratedAt:     now,
			})
		}
	}
	return out
}

// recipeHighCardinality (v0.28) — flag metrics with absurd numbers
// of distinct label-set combinations. The threshold is intentionally
// SMB-friendly:
//
//   >= 10,000 distinct combos → critical (almost certainly costing
//     significant $ in metric storage)
//   >= 2,000 distinct combos  → warn (worth reviewing)
//
// Backends differ wildly in how cardinality maps to cost. Datadog
// charges per "custom metric" (a unique metric+tagset combo);
// Honeycomb's event model is more forgiving; Prometheus-style
// (Mimir/Cortex) gets expensive at series counts. The recipe
// surfaces the count + the highest-cardinality label so the operator
// can decide whether to keep it; the snippet shows the
// metricstransform/aggregate_labels pattern.
//
// We don't compute a $/month estimate because the byte cost isn't
// the right unit here (cardinality cost is per-series, not
// per-byte). EstSavingsBytes stays 0 so the engine's pricing pass
// doesn't overpromise.
func (e *Engine) recipeHighCardinality(ctx context.Context, win insights.Window, now time.Time) []Recommendation {
	// minCombos = 2000 matches the recipe's warn threshold.
	cards, err := e.insights.TopMetricCardinality(ctx, win, 20, 2000)
	if err != nil {
		e.logger.Warn("topMetricCardinality failed; skipping recipe", zap.Error(err))
		return nil
	}
	out := make([]Recommendation, 0, len(cards))
	for _, c := range cards {
		sev := SeverityWarn
		if c.DistinctCombos >= 10_000 {
			sev = SeverityCritical
		}
		out = append(out, Recommendation{
			ID:              idFor("high_card", c.MetricName),
			Category:        CategoryHighCardinality,
			Severity:        sev,
			Title:           fmt.Sprintf("Metric %q has %s distinct label combinations", c.MetricName, humanCount(c.DistinctCombos)),
			Detail:          highCardinalityDetail(c),
			Signal:          insights.SignalMetrics,
			EstSavingsBytes: 0, // not byte-bounded; cost is per-series
			Snippet:         metricstransformSnippet(c),
			GeneratedAt:     now,
		})
	}
	return out
}

// ----------------------------------------------------------------
// Snippet builders. Each returns a small, valid YAML fragment with
// a header comment explaining what to do with it.
// ----------------------------------------------------------------

// attributeDeleteSnippet builds an attributesprocessor block that
// drops the named key. The header explains the merge step the
// operator has to do — we can't merge into an unknown config for
// them.
func attributeDeleteSnippet(key string, sig insights.Signal) string {
	procName := "attributes/drop_" + sanitizeYAMLKey(key)
	return strings.TrimSpace(fmt.Sprintf(`
# Recommendation: drop attribute %q from %s pipelines.
# Merge into your collector config: add the processor under
# processors:, then list it in service.pipelines.%s.processors.
processors:
  %s:
    actions:
      - key: %q
        action: delete

# Then, in your existing %s pipeline:
service:
  pipelines:
    %s:
      processors: [%s, batch]  # keep any existing processors after this
`, key, sig, sig, procName, key, sig, sig, procName))
}

// metricstransformSnippet builds a metricstransform processor
// block that aggregates over the highest-cardinality label,
// effectively dropping it from the time-series identity. The
// `include: <metric>` clause narrows the rule so we don't
// accidentally aggregate other metrics. The aggregation_type is
// sum — safe for counters and most gauges; operators using
// distributions may want to swap to mean or max.
func metricstransformSnippet(c insights.MetricCardinality) string {
	procName := "metricstransform/cap_" + sanitizeYAMLKey(c.MetricName)
	label := c.HighestCardLabel
	if label == "" {
		label = "<the high-cardinality label>"
	}
	return strings.TrimSpace(fmt.Sprintf(`
# Recommendation: metric %q has high cardinality (%s distinct combinations).
# The dominant driver looks like the %q label.
#
# This metricstransform block aggregates over %q, collapsing
# every combo that differs only in that label into one series.
# Saves significant $$ on metric backends that charge per series
# (Datadog custom metrics, Prometheus-style stores).
#
# Review BEFORE applying: aggregating loses the ability to filter
# or alert on %q values. If you need those, consider an
# attributes/delete that drops %q on a SAMPLED subset instead.
processors:
  %s:
    transforms:
      - include: %s
        action: update
        operations:
          - action: aggregate_labels
            aggregation_type: sum
            # Keep these labels; anything else is dropped, including %s.
            label_set: []     # FIXME: list the labels you want to keep

# Then in service.pipelines.metrics.processors, add %s before batch.
`, c.MetricName, humanCount(c.DistinctCombos), label, label, label, label, procName, c.MetricName, label, procName))
}

// batchProcessorSnippet suggests larger batches for drop-hotspot
// signals. Doesn't blindly raise send_batch_size — also adds
// send_batch_max_size so the operator gets a bounded payload.
func batchProcessorSnippet(sig insights.Signal) string {
	return strings.TrimSpace(fmt.Sprintf(`
# Recommendation: drops on %s suggest the batch processor isn't
# absorbing your ingest rate. Try larger batches with a hard ceiling.
processors:
  batch/larger:
    timeout: 5s
    send_batch_size: 4096
    send_batch_max_size: 8192

# Then in service.pipelines.%s.processors, replace 'batch' with
# 'batch/larger' (or add it if you don't have batching yet).
`, sig, sig))
}

// ----------------------------------------------------------------
// Detail-text builders. Kept separate so the recipe code stays
// readable.
// ----------------------------------------------------------------

func noisyAttributeDetail(a insights.AttributeVolume, sv insights.SignalVolume) string {
	return fmt.Sprintf(
		`The attribute %q accounts for an estimated %.1f%% of your %s byte budget (~%s). `+
			`Dropping it via an attributesprocessor is the single most common cost optimization. `+
			`Estimate is sampled (~2000 rows per query); validate against your own bill before adopting.`,
		a.Key, a.PctOfSignal*100, sv.Signal, humanBytes(a.Bytes),
	)
}

func outlierAgentDetail(a insights.AgentVolume, ratio float64, median int64) string {
	return fmt.Sprintf(
		`This agent emitted %s in the window — %.1f× the fleet median of %s. `+
			`Common causes: missing sampling step, an exporter retry loop, or a verbose log severity. `+
			`Open the agent's config to investigate; no single snippet fixes this category.`,
		humanBytes(a.TotalBytes), ratio, humanBytes(median),
	)
}

func dropHotspotDetail(sv insights.SignalVolume, dropPct float64) string {
	return fmt.Sprintf(
		`%d of %d %s items were dropped or dead-lettered in the window (%.2f%%). `+
			`That's data the agent intended to send but couldn't — usually a sign the batch `+
			`processor or exporter queue is undersized.`,
		sv.DroppedCount, sv.ItemCount+sv.DroppedCount, sv.Signal, dropPct*100,
	)
}

func highCardinalityDetail(c insights.MetricCardinality) string {
	hint := ""
	if c.HighestCardLabel != "" {
		hint = fmt.Sprintf(" Sampled inspection suggests the %q label is the dominant driver.", c.HighestCardLabel)
	}
	return fmt.Sprintf(
		`%q has %s distinct label-set combinations in the window across %s samples. `+
			`Each unique combination is a separate time-series in your metric backend, `+
			`which is the most common driver of unexpected metric storage costs.%s `+
			`A metricstransform/aggregate_labels rule is the standard fix — review the `+
			`snippet, list the labels you actually need to filter/alert on, and drop the rest.`,
		c.MetricName, humanCount(c.DistinctCombos), humanCount(c.TotalSamples), hint,
	)
}

func emptySignalDetail(a insights.AgentVolume, sig insights.Signal) string {
	return fmt.Sprintf(
		`This agent ships a %s pipeline but emitted zero bytes during the window, while the rest `+
			`of the fleet did. Prune the %s receiver/exporter pair from this agent's config to reduce `+
			`memory and connection overhead.`,
		sig, sig,
	)
}

// ----------------------------------------------------------------
// Pure helpers
// ----------------------------------------------------------------

// idFor produces a stable, URL-safe ID from a recipe name + scope
// fields. SHA1 truncated to 16 hex chars — collision-resistant
// enough for an N≈100 recommendation list and stable across
// process restarts.
func idFor(parts ...string) string {
	h := sha1.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0x1f}) // unit separator
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarn:
		return 2
	case SeverityInfo:
		return 1
	}
	return 0
}

// shortAgentLabel returns a 12-char-ish label preferring the
// agent's name and falling back to the first chunk of its UUID.
func shortAgentLabel(name, id string) string {
	if name != "" {
		return name
	}
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// sanitizeYAMLKey turns an OTel attribute key (which may contain
// dots, slashes, dashes) into a snake_case-ish identifier safe to
// drop into a YAML key. Doesn't need to be reversible.
func sanitizeYAMLKey(k string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "attr"
	}
	return out
}

// humanCount is a small humanizer for cardinality numbers. 12,500
// becomes "12.5k"; 2,300,000 becomes "2.3M". Reads better than raw
// integers when the recipe is talking about distinct-combo counts.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// humanBytes is a small humanizer used only in detail text. Kept
// local so the package doesn't depend on a third-party formatter.
func humanBytes(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
