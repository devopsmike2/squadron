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
// orphan/missing/mismatch is non-zero. The three percentages are mean
// across keys (not weighted by span count) — a single noisy resource
// can't dominate a fleet of clean ones. Slice 2 may switch to weighted
// aggregation per design doc §12.
type ProviderSpanQuality struct {
	ResourceCount       int     `json:"resource_count"`
	ResourcesWithIssues int     `json:"resources_with_issues"`
	OrphanPct           float64 `json:"orphan_pct"`
	MissingAttrPct      float64 `json:"missing_attr_pct"`
	AttrMismatchPct     float64 `json:"attr_mismatch_pct"`
}

// SpanQualityTotals is the cross-provider roll-up. Sums ResourceCount
// + ResourcesWithIssues; the three percentages re-mean across every
// observed key (not a re-mean of the per-provider means — that would
// double-bucket the unknown observations).
type SpanQualityTotals struct {
	ResourceCount       int     `json:"resource_count"`
	ResourcesWithIssues int     `json:"resources_with_issues"`
	OrphanPct           float64 `json:"orphan_pct"`
	MissingAttrPct      float64 `json:"missing_attr_pct"`
	AttrMismatchPct     float64 `json:"attr_mismatch_pct"`
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
type ResourceSpanQuality struct {
	ResourceID      string                              `json:"resource_id"`
	Provider        string                              `json:"provider"`
	Kind            string                              `json:"kind"`
	TotalSpans      uint64                              `json:"total_spans"`
	WindowStart     time.Time                           `json:"window_start"`
	OrphanPct       float64                             `json:"orphan_pct"`
	MissingAttrPct  float64                             `json:"missing_attr_pct"`
	AttrMismatchPct float64                             `json:"attr_mismatch_pct"`
	Placeholders    []traceindex.PlaceholderObservation `json:"placeholders"`
	HasIssues       bool                                `json:"has_issues"`
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
	qualityIndex QualitySnapshotIndex
	keyProjector ResourceKeyProjector
	auditService services.AuditService
	cache        *spanQualityCache
	logger       *zap.Logger
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

	resp := h.compose()

	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:     "system",
			EventType: services.AuditEventSpanQualityRequested,
			Action:    "requested",
			Payload: map[string]any{
				"cache_status":                  "miss",
				"total_resource_count":          resp.Totals.ResourceCount,
				"total_resources_with_issues":   resp.Totals.ResourcesWithIssues,
				"recorded_at":                   time.Now().UTC(),
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
func (h *DiscoverySpanQualityHandlers) compose() *SpanQualityResponse {
	resp := &SpanQualityResponse{
		Providers: map[string]ProviderSpanQuality{
			"aws":   {},
			"gcp":   {},
			"azure": {},
			"oci":   {},
		},
	}
	if h.qualityIndex == nil {
		return resp
	}

	type bucket struct {
		count        int
		withIssues   int
		orphanSum    float64
		missingSum   float64
		mismatchSum  float64
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
		issues := snap.OrphanPct > 0 || snap.MissingAttrPct > 0 || snap.AttrMismatchPct > 0
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
	}

	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		b := buckets[p]
		resp.Providers[p] = ProviderSpanQuality{
			ResourceCount:       b.count,
			ResourcesWithIssues: b.withIssues,
			OrphanPct:           meanPct(b.orphanSum, b.count),
			MissingAttrPct:      meanPct(b.missingSum, b.count),
			AttrMismatchPct:     meanPct(b.mismatchSum, b.count),
		}
	}
	resp.Totals = SpanQualityTotals{
		ResourceCount:       totals.count,
		ResourcesWithIssues: totals.withIssues,
		OrphanPct:           meanPct(totals.orphanSum, totals.count),
		MissingAttrPct:      meanPct(totals.missingSum, totals.count),
		AttrMismatchPct:     meanPct(totals.mismatchSum, totals.count),
	}
	return resp
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
		ResourceID:      id,
		Provider:        provider,
		Kind:            kind,
		TotalSpans:      snap.TotalSpans,
		WindowStart:     snap.WindowStart,
		OrphanPct:       round1Quality(snap.OrphanPct),
		MissingAttrPct:  round1Quality(snap.MissingAttrPct),
		AttrMismatchPct: round1Quality(snap.AttrMismatchPct),
		Placeholders:    snap.Placeholders,
		HasIssues:       snap.OrphanPct > 0 || snap.MissingAttrPct > 0 || snap.AttrMismatchPct > 0,
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
