// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// --- response types -----------------------------------------------------
//
// Mirrors docs/proposals/span-quality-slice1.md §6.1 (per-provider
// aggregate) and §6.2 (per-resource detail). Provider keys are
// deterministic — the four cloud strings, always populated — so the
// dashboard renders the same four columns regardless of which clouds
// the deployment has actually wired. The "unknown" bucket catches any
// quality observation whose key/provider can't be classified (tier-5
// host-name-only / tier-6 service-name-only observations); the
// dashboard hides it but its count contributes to the totals.

// ProviderSpanQuality is the per-provider aggregate. ResourceCount is
// the number of distinct quality keys observed for the provider in the
// rolling window; ResourcesWithIssues counts keys where AT LEAST ONE of
// the five pathology percentages is non-zero. The percentages are mean
// across keys (not weighted by span count) — a single noisy resource
// can't dominate a fleet of clean ones. Slice 2 (v0.89.110) extends
// with MalformedTraceparentPct + MissingTraceparentOnChildPct; their
// means use honest denominators per span-quality-slice2.md §3.3: only
// keys with SpansWithTraceparent > 0 contribute to the malformed mean,
// and only keys with ChildSpans > 0 contribute to the missing-on-child
// mean. A resource with no child spans can't be missing-on-child, so
// it shouldn't dilute the rate.
type ProviderSpanQuality struct {
	ResourceCount       int     `json:"resource_count"`
	ResourcesWithIssues int     `json:"resources_with_issues"`
	OrphanPct           float64 `json:"orphan_pct"`
	MissingAttrPct      float64 `json:"missing_attr_pct"`
	AttrMismatchPct     float64 `json:"attr_mismatch_pct"`

	// Slice 2 (v0.89.110) — W3C trace context parsing. Aggregated mean
	// across resources with > 0 SpansWithTraceparent (for malformed)
	// and > 0 ChildSpans (for missing-on-child). The denominators stay
	// honest so a fleet with mostly root spans doesn't dilute the
	// missing-on-child rate.
	MalformedTraceparentPct      float64 `json:"malformed_traceparent_pct"`
	MissingTraceparentOnChildPct float64 `json:"missing_traceparent_on_child_pct"`

	// SamplingTooAggressivePct — Sampling rate slice 1 (v0.89.124).
	// Per-provider percentage of serverless resources whose latest
	// sampling-rate detection result fires the
	// span-quality-sampling-too-aggressive recommendation (ratio below
	// the 5% floor AND invocation count at or above the 1000 statistical
	// minimum). Denominator: total serverless resource count contributed
	// by the SamplingInventoryReader (resources without sufficient
	// invocations don't dilute — they're not statistically meaningful).
	// Zero when no SamplingInventoryReader is wired (the cold-start
	// posture before the per-cloud MetricQuerier substrate runs).
	SamplingTooAggressivePct float64 `json:"sampling_too_aggressive_pct"`
}

// SpanQualityTotals is the cross-provider roll-up. Sums ResourceCount
// + ResourcesWithIssues; the percentages re-mean across every observed
// key (not a re-mean of the per-provider means — that would
// double-bucket the unknown observations). Slice 2 additions match the
// honest-denominator semantics of ProviderSpanQuality.
type SpanQualityTotals struct {
	ResourceCount       int     `json:"resource_count"`
	ResourcesWithIssues int     `json:"resources_with_issues"`
	OrphanPct           float64 `json:"orphan_pct"`
	MissingAttrPct      float64 `json:"missing_attr_pct"`
	AttrMismatchPct     float64 `json:"attr_mismatch_pct"`

	// Slice 2 (v0.89.110) additions, same denominator semantics as
	// ProviderSpanQuality.
	MalformedTraceparentPct      float64 `json:"malformed_traceparent_pct"`
	MissingTraceparentOnChildPct float64 `json:"missing_traceparent_on_child_pct"`

	// Sampling rate slice 1 (v0.89.124). Cross-provider sum of
	// per-provider sampling-too-aggressive counts divided by the
	// cross-provider sum of qualifying serverless resource counts.
	// Honest denominator semantics: resources below the minimum
	// invocation count don't enter either side of the ratio.
	SamplingTooAggressivePct float64 `json:"sampling_too_aggressive_pct"`
}

// SpanQualityResponse is the JSON wire shape for GET
// /api/v1/discovery/span_quality. Providers is keyed by the four cloud
// strings ("aws", "gcp", "azure", "oci") so the dashboard renders
// deterministically.
type SpanQualityResponse struct {
	Providers map[string]ProviderSpanQuality `json:"providers"`
	Totals    SpanQualityTotals              `json:"totals"`
}

// ResourceSpanQuality is the JSON wire shape for GET
// /api/v1/discovery/{provider}/inventory/{kind}/{id}/span_quality.
// Placeholders is the bounded slice of recent {attr, placeholder}
// pairs the traceindex Quality observer recorded; the operator can
// see what specific sentinel value the SDK emitted.
//
// Slice 2 (v0.89.110) extends with four W3C trace context fields:
// MalformedTraceparentPct + MissingTraceparentOnChildPct (the per-
// resource percentages) plus SpansWithTraceparent + ChildSpans (the
// raw denominators) so the UI drill-down can render honest "N of M"
// framing alongside the percentage. HasIssues extends to any of the
// five percentages being non-zero.
type ResourceSpanQuality struct {
	ResourceID      string    `json:"resource_id"`
	Provider        string    `json:"provider"`
	Kind            string    `json:"kind"`
	TotalSpans      uint64    `json:"total_spans"`
	WindowStart     time.Time `json:"window_start"`
	OrphanPct       float64   `json:"orphan_pct"`
	MissingAttrPct  float64   `json:"missing_attr_pct"`
	AttrMismatchPct float64   `json:"attr_mismatch_pct"`

	// Slice 2 (v0.89.110) — W3C trace context parsing per
	// span-quality-slice2.md §6.1.
	MalformedTraceparentPct      float64 `json:"malformed_traceparent_pct"`
	MissingTraceparentOnChildPct float64 `json:"missing_traceparent_on_child_pct"`
	SpansWithTraceparent         uint64  `json:"spans_with_traceparent"`
	ChildSpans                   uint64  `json:"child_spans"`

	Placeholders []traceindex.PlaceholderObservation `json:"placeholders"`
	HasIssues    bool                                `json:"has_issues"`
}

// --- store + index interfaces ------------------------------------------

// QualitySnapshotIndex is the slim surface the handler needs from the
// hot-path Quality observer. The real *traceindex.Quality satisfies
// this directly; tests substitute a stub returning canned snapshots.
//
// SnapshotKey returns ok=false when no observations exist for the key
// (the 404 branch on the per-resource endpoint). SnapshotAll returns
// one snapshot per observed key in arbitrary order — the aggregate
// pass treats the order as undefined.
type QualitySnapshotIndex interface {
	SnapshotAll() []traceindex.QualityCountersSnapshot
	SnapshotKey(key string) (traceindex.QualityCountersSnapshot, bool)
}

// ResourceKeyProjector resolves a (provider, kind, id) path-param
// tuple to the same quality key the OTLP hot path used. Production
// wires this through the inventory store: the same projection
// ComputeResourceKey would have produced from the inventory row's
// attributes. The interface keeps the handler from importing the
// inventory store directly; a nil projector leaves the per-resource
// endpoint serving 404 for every lookup (the cold-start posture on a
// deployment that hasn't wired the inventory projection yet).
type ResourceKeyProjector interface {
	ProjectKey(ctx context.Context, provider, kind, id string) (string, bool)
}

// SamplingInventoryReader is the slim, optional surface the compose
// pass uses to populate ProviderSpanQuality.SamplingTooAggressivePct
// (Sampling rate slice 1, v0.89.124). Production wires a reader that
// walks the in-memory serverless inventory (the same one
// AnnotateServerlessWithSampling fills) and counts, per provider:
//
//   - QualifyingCount: serverless resources with SamplingRatio set AND
//     ExpectedInvocationCount at or above the 1000 statistical
//     minimum. This is the honest denominator — resources with
//     insufficient invocations don't enter either side of the ratio.
//   - TooAggressiveCount: subset of QualifyingCount whose detection
//     fires (SamplingExceedsFloor true).
//
// Cold-start posture: a nil reader leaves
// SamplingTooAggressivePct at zero across every provider + totals,
// which matches the "panel hides when all 6 zero" UI contract on a
// deployment that hasn't wired the per-cloud MetricQuerier substrate.
type SamplingInventoryReader interface {
	SamplingQualifyingCounts(ctx context.Context) map[string]SamplingProviderCount
}

// SamplingProviderCount is the per-provider tuple SamplingInventoryReader
// returns. TooAggressiveCount must always be <= QualifyingCount;
// compose silently clamps if it isn't (defensive against an
// out-of-sync inventory snapshot).
type SamplingProviderCount struct {
	QualifyingCount    int
	TooAggressiveCount int
}

// --- cache --------------------------------------------------------------
//
// spanQualityCache mirrors the v0.89.61 summaryCache / v0.89.76
// traceCoverageCache pattern: a TTL cache around the composed
// SpanQualityResponse so subsequent dashboard polls inside the 30s
// window short-circuit the full SnapshotAll walk + per-provider
// aggregation. The detail endpoint is NOT cached — its per-resource
// lookup is already a single map read on the Quality observer.

type spanQualityCache struct {
	mu       sync.Mutex
	cached   *SpanQualityResponse
	cachedAt time.Time
	ttl      time.Duration
	clock    func() time.Time
}

func newSpanQualityCache(ttl time.Duration, clock func() time.Time) *spanQualityCache {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &spanQualityCache{ttl: ttl, clock: clock}
}

// Get returns the cached response and true if within the TTL window;
// otherwise (nil, false).
func (c *spanQualityCache) Get() (*SpanQualityResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == nil {
		return nil, false
	}
	if c.clock().Sub(c.cachedAt) >= c.ttl {
		return nil, false
	}
	return c.cached, true
}

// Set replaces the cached response and stamps cachedAt to now.
func (c *spanQualityCache) Set(r *SpanQualityResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = r
	c.cachedAt = c.clock()
}

// --- handler ------------------------------------------------------------

// DefaultSpanQualityCacheTTL is the production cache TTL per design
// doc §6.1 ("30s in-memory cache, mirrors the v0.89.61 summary cache
// pattern"). Mirrors DefaultTraceCoverageCacheTTL so the dashboard's
// quality and coverage polls share one staleness budget.
const DefaultSpanQualityCacheTTL = 30 * time.Second

// DiscoverySpanQualityHandlers serves the two §6 endpoints. Any of
// the three wires may be nil — that's the cold-start posture for a
// deployment that hasn't observed any spans / wired the inventory
// projection / wired the audit service yet:
//
//   - qualityIndex nil → aggregate endpoint returns all-zero counts
//     for every provider, detail endpoint 404s every lookup.
//   - keyProjector nil → detail endpoint 404s every lookup (no way
//     to derive the quality key from path params). Aggregate
//     endpoint still serves SnapshotAll-derived totals.
//   - auditService nil → no audit row on cache miss.
type DiscoverySpanQualityHandlers struct {
	qualityIndex      QualitySnapshotIndex
	keyProjector      ResourceKeyProjector
	samplingInventory SamplingInventoryReader
	auditService      services.AuditService
	cache             *spanQualityCache
	logger            *zap.Logger
}

// NewDiscoverySpanQualityHandlers builds the handler. ttl <= 0 falls
// through to DefaultSpanQualityCacheTTL; a nil clock falls through to
// time.Now (production posture). Any wire may be nil per the type
// comment's cold-start posture.
func NewDiscoverySpanQualityHandlers(
	qualityIndex QualitySnapshotIndex,
	keyProjector ResourceKeyProjector,
	auditService services.AuditService,
	ttl time.Duration,
	clock func() time.Time,
	logger *zap.Logger,
) *DiscoverySpanQualityHandlers {
	if ttl <= 0 {
		ttl = DefaultSpanQualityCacheTTL
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DiscoverySpanQualityHandlers{
		qualityIndex: qualityIndex,
		keyProjector: keyProjector,
		auditService: auditService,
		cache:        newSpanQualityCache(ttl, clock),
		logger:       logger,
	}
}

// SetSamplingInventoryReader wires the optional Sampling rate slice 1
// reader (v0.89.124). The reader contributes
// SamplingTooAggressivePct on both the per-provider and the totals
// rows of the SpanQualityResponse. Nil reader leaves the new field at
// zero across the board — the cold-start posture for a deployment
// without the per-cloud MetricQuerier substrate.
//
// Late-bound on purpose: the per-cloud serverless inventory + the
// MetricQuerier substrate land via separate wire-throughs from
// Server, so the handler is constructed first and the reader is
// attached afterwards (mirrors the SetResourceKeyProjectorForDiscovery
// pattern on Server).
func (h *DiscoverySpanQualityHandlers) SetSamplingInventoryReader(r SamplingInventoryReader) {
	h.samplingInventory = r
}

// HandleSpanQuality serves GET /api/v1/discovery/span_quality.
//
// Cache hit: return the cached response immediately (no audit emit).
// Cache miss: walk SnapshotAll, bucket per provider, compose, cache,
// emit one discovery.span_quality.requested audit row.
func (h *DiscoverySpanQualityHandlers) HandleSpanQuality(c *gin.Context) {
	if cached, ok := h.cache.Get(); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	resp := h.compose(c.Request.Context())

	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:     "system",
			EventType: services.AuditEventSpanQualityRequested,
			Action:    "requested",
			Payload: map[string]any{
				"cache_status":                "miss",
				"total_resource_count":        resp.Totals.ResourceCount,
				"total_resources_with_issues": resp.Totals.ResourcesWithIssues,
				"recorded_at":                 time.Now().UTC(),
			},
		})
	}

	h.cache.Set(resp)
	c.JSON(http.StatusOK, resp)
}

// compose walks SnapshotAll once, buckets each snapshot under its
// provider (snapshot.Provider populated by the chunk-2 SpanObservation
// pass-through; falls back to inferProviderFromKey for any tier-5 /
// tier-6 / verbatim-ARN keys that didn't carry the attribute). The
// Providers map is pre-populated with the four cloud keys so the wire
// shape stays stable across deployments.
func (h *DiscoverySpanQualityHandlers) compose(ctx context.Context) *SpanQualityResponse {
	resp := &SpanQualityResponse{
		Providers: map[string]ProviderSpanQuality{
			"aws":   {},
			"gcp":   {},
			"azure": {},
			"oci":   {},
		},
	}
	// Sampling rate slice 1 (v0.89.124): walk the sampling inventory
	// FIRST so the per-provider counts are ready to overlay onto the
	// quality-derived shape. A nil reader leaves every entry at zero
	// — the panel hides itself in that case.
	samplingCounts := h.gatherSamplingCounts(ctx)
	if h.qualityIndex == nil {
		// Without quality observations we still want the sampling
		// totals to surface — the panel may still appear if at least
		// one provider has a non-zero sampling pct even when no spans
		// have been observed yet.
		applySamplingCounts(resp, samplingCounts)
		return resp
	}

	// Slice 2 (v0.89.110): the malformed + missing-on-child counts use
	// their own denominators per span-quality-slice2.md §3.3. A
	// resource with zero spans-carrying-traceparent shouldn't dilute
	// the malformed mean (its 0% is artificial — nothing to be
	// malformed). Same for missing-on-child: a resource with zero
	// child spans is trivially 0% missing-on-child. The
	// malformedDenom / missingChildDenom bucket fields track how many
	// resources actually contributed to those means so meanPct can
	// divide honestly.
	type bucket struct {
		count             int
		withIssues        int
		orphanSum         float64
		missingSum        float64
		mismatchSum       float64
		malformedSum      float64
		malformedDenom    int
		missingChildSum   float64
		missingChildDenom int
	}
	buckets := map[string]*bucket{
		"aws":     {},
		"gcp":     {},
		"azure":   {},
		"oci":     {},
		"unknown": {},
	}
	totals := &bucket{}

	for _, snap := range h.qualityIndex.SnapshotAll() {
		provider := snap.Provider
		if provider == "" || provider == "unknown" {
			provider = inferProviderFromKey(snap.Key)
		}
		if _, ok := buckets[provider]; !ok {
			provider = "unknown"
		}
		b := buckets[provider]
		b.count++
		totals.count++
		// "Has issues" extends to any of the five percentages being
		// non-zero so the resource lands in the panel-visible count
		// when only a traceparent pathology is present.
		issues := snap.OrphanPct > 0 ||
			snap.MissingAttrPct > 0 ||
			snap.AttrMismatchPct > 0 ||
			snap.MalformedTraceparentPct > 0 ||
			snap.MissingTraceparentOnChildPct > 0
		if issues {
			b.withIssues++
			totals.withIssues++
		}
		b.orphanSum += snap.OrphanPct
		b.missingSum += snap.MissingAttrPct
		b.mismatchSum += snap.AttrMismatchPct
		totals.orphanSum += snap.OrphanPct
		totals.missingSum += snap.MissingAttrPct
		totals.mismatchSum += snap.AttrMismatchPct

		// Honest denominators for the two new traceparent rates.
		if snap.SpansWithTraceparent > 0 {
			b.malformedSum += snap.MalformedTraceparentPct
			b.malformedDenom++
			totals.malformedSum += snap.MalformedTraceparentPct
			totals.malformedDenom++
		}
		if snap.ChildSpans > 0 {
			b.missingChildSum += snap.MissingTraceparentOnChildPct
			b.missingChildDenom++
			totals.missingChildSum += snap.MissingTraceparentOnChildPct
			totals.missingChildDenom++
		}
	}

	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		b := buckets[p]
		resp.Providers[p] = ProviderSpanQuality{
			ResourceCount:                b.count,
			ResourcesWithIssues:          b.withIssues,
			OrphanPct:                    meanPct(b.orphanSum, b.count),
			MissingAttrPct:               meanPct(b.missingSum, b.count),
			AttrMismatchPct:              meanPct(b.mismatchSum, b.count),
			MalformedTraceparentPct:      meanPct(b.malformedSum, b.malformedDenom),
			MissingTraceparentOnChildPct: meanPct(b.missingChildSum, b.missingChildDenom),
		}
	}
	resp.Totals = SpanQualityTotals{
		ResourceCount:                totals.count,
		ResourcesWithIssues:          totals.withIssues,
		OrphanPct:                    meanPct(totals.orphanSum, totals.count),
		MissingAttrPct:               meanPct(totals.missingSum, totals.count),
		AttrMismatchPct:              meanPct(totals.mismatchSum, totals.count),
		MalformedTraceparentPct:      meanPct(totals.malformedSum, totals.malformedDenom),
		MissingTraceparentOnChildPct: meanPct(totals.missingChildSum, totals.missingChildDenom),
	}
	applySamplingCounts(resp, samplingCounts)
	return resp
}

// gatherSamplingCounts pulls per-provider sampling counts from the
// optional inventory reader. Returns a zero-value map (all four
// providers keyed with zero counts) when the reader is nil OR when
// the reader panics — defensive against an in-progress wire-through
// that hasn't fully landed.
func (h *DiscoverySpanQualityHandlers) gatherSamplingCounts(ctx context.Context) map[string]SamplingProviderCount {
	out := map[string]SamplingProviderCount{
		"aws":   {},
		"gcp":   {},
		"azure": {},
		"oci":   {},
	}
	if h.samplingInventory == nil {
		return out
	}
	counts := h.samplingInventory.SamplingQualifyingCounts(ctx)
	for p, v := range counts {
		// Clamp the too-aggressive count to the qualifying count so
		// an out-of-sync inventory snapshot can't surface > 100%.
		if v.TooAggressiveCount > v.QualifyingCount {
			v.TooAggressiveCount = v.QualifyingCount
		}
		if v.QualifyingCount < 0 {
			v.QualifyingCount = 0
		}
		if v.TooAggressiveCount < 0 {
			v.TooAggressiveCount = 0
		}
		out[p] = v
	}
	return out
}

// applySamplingCounts mutates the response in-place, populating
// SamplingTooAggressivePct on each provider row + the totals row.
// Per-provider pct: TooAggressiveCount / QualifyingCount * 100.
// Totals pct: sum(TooAggressiveCount) / sum(QualifyingCount) * 100
// over the four canonical providers — the cross-provider mean uses
// raw counts (not a re-mean of per-provider pcts) so a deployment
// with 1 of 1 too-aggressive Lambdas in AWS doesn't drown out 5 of
// 100 too-aggressive Cloud Run services in GCP.
//
// Honest denominator: a provider with QualifyingCount == 0
// contributes zero to BOTH sides of the totals ratio, so an
// all-zero deployment surfaces 0% (and the panel stays hidden).
func applySamplingCounts(resp *SpanQualityResponse, counts map[string]SamplingProviderCount) {
	var totalQualifying, totalTooAggressive int
	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		c := counts[p]
		row := resp.Providers[p]
		row.SamplingTooAggressivePct = samplingPct(c.TooAggressiveCount, c.QualifyingCount)
		resp.Providers[p] = row
		totalQualifying += c.QualifyingCount
		totalTooAggressive += c.TooAggressiveCount
	}
	resp.Totals.SamplingTooAggressivePct = samplingPct(totalTooAggressive, totalQualifying)
}

// samplingPct returns numerator/denominator * 100, rounded to one
// decimal. Zero-safe on denominator=0 (returns 0 rather than NaN),
// matching meanPct's contract so all percentages on the response
// share one rounding rule.
func samplingPct(numerator, denominator int) float64 {
	if denominator <= 0 {
		return 0
	}
	pct := float64(numerator) / float64(denominator) * 100
	return math.Round(pct*10) / 10
}

// HandleResourceSpanQuality serves
// GET /api/v1/discovery/:provider/inventory/:kind/:id/span_quality.
//
// 404 cases:
//   - keyProjector wire not present (deployment hasn't wired the
//     inventory projection yet),
//   - projector returned ok=false (no inventory row matches the path
//     params),
//   - SnapshotKey returned ok=false (the resource exists in inventory
//     but no spans have been observed for it yet — slice 1 §3.4 cold
//     start).
//
// 200 case includes the placeholder slice verbatim from the snapshot
// so the operator can see WHICH placeholders the SDK emitted.
func (h *DiscoverySpanQualityHandlers) HandleResourceSpanQuality(c *gin.Context) {
	provider := strings.ToLower(strings.TrimSpace(c.Param("provider")))
	kind := strings.ToLower(strings.TrimSpace(c.Param("kind")))
	id := strings.TrimSpace(c.Param("id"))
	if provider == "" || kind == "" || id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider, kind, id are required"})
		return
	}
	if h.keyProjector == nil || h.qualityIndex == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no quality observations recorded for resource"})
		return
	}
	key, ok := h.keyProjector.ProjectKey(c.Request.Context(), provider, kind, id)
	if !ok || key == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "no inventory row matches the supplied identifiers"})
		return
	}
	snap, ok := h.qualityIndex.SnapshotKey(key)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no quality observations recorded for resource"})
		return
	}
	resp := ResourceSpanQuality{
		ResourceID:                   id,
		Provider:                     provider,
		Kind:                         kind,
		TotalSpans:                   snap.TotalSpans,
		WindowStart:                  snap.WindowStart,
		OrphanPct:                    round1Quality(snap.OrphanPct),
		MissingAttrPct:               round1Quality(snap.MissingAttrPct),
		AttrMismatchPct:              round1Quality(snap.AttrMismatchPct),
		MalformedTraceparentPct:      round1Quality(snap.MalformedTraceparentPct),
		MissingTraceparentOnChildPct: round1Quality(snap.MissingTraceparentOnChildPct),
		SpansWithTraceparent:         snap.SpansWithTraceparent,
		ChildSpans:                   snap.ChildSpans,
		Placeholders:                 snap.Placeholders,
		// Slice 2 (v0.89.110): any of the five percentages > 0 → issues.
		HasIssues: snap.OrphanPct > 0 ||
			snap.MissingAttrPct > 0 ||
			snap.AttrMismatchPct > 0 ||
			snap.MalformedTraceparentPct > 0 ||
			snap.MissingTraceparentOnChildPct > 0,
	}
	if resp.Placeholders == nil {
		// Keep the JSON shape stable: never serialize null for the
		// list field — the UI's drill-down expects an array always.
		resp.Placeholders = []traceindex.PlaceholderObservation{}
	}
	c.JSON(http.StatusOK, resp)
}

// meanPct returns sum/count rounded to one decimal. Zero-safe on
// count=0 (returns 0 rather than NaN), matching the trace_coverage
// handler's round1 / computeTraceCoveragePct contract so the
// dashboard's two read endpoints share one rounding rule.
func meanPct(sum float64, count int) float64 {
	if count <= 0 {
		return 0
	}
	pct := sum / float64(count)
	return math.Round(pct*10) / 10
}

// round1Quality is the per-resource detail percentage rounder; it
// exists separately from the trace_coverage handler's round1 to keep
// the two packages from cross-importing the rounding helper.
func round1Quality(v float64) float64 {
	return math.Round(v*10) / 10
}

// inferProviderFromKey is the fallback bucketing used when a snapshot's
// Provider field is unset (legacy chunk-1 observations made before the
// chunk-2 receiver wire-through landed) OR when ComputeResourceKey
// classified the provider as "unknown" but the key shape still encodes
// a hint. Looks at the key prefix for the canonical
// "<provider>:<account>:<...>" shape; falls back to ARN / GCP URI /
// Azure ARM / OCI OCID conventions for tier-1 raw cloud.resource_id
// keys; otherwise returns "unknown".
func inferProviderFromKey(key string) string {
	if key == "" {
		return "unknown"
	}
	switch {
	case strings.HasPrefix(key, "aws:"):
		return "aws"
	case strings.HasPrefix(key, "gcp:"):
		return "gcp"
	case strings.HasPrefix(key, "azure:"):
		return "azure"
	case strings.HasPrefix(key, "oci:"):
		return "oci"
	case strings.HasPrefix(key, "arn:aws:"):
		return "aws"
	case strings.HasPrefix(key, "//") && strings.Contains(key, "googleapis.com"):
		return "gcp"
	case strings.HasPrefix(key, "/subscriptions/"):
		return "azure"
	case strings.HasPrefix(key, "ocid1."):
		return "oci"
	}
	return "unknown"
}
