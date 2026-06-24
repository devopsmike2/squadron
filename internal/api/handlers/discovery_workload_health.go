// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// discovery_workload_health.go — Workload Health dashboard panel
// slice 1 chunk 1 (v0.89.132, #772 Stream 170). Sibling of
// discovery_trace_coverage.go + discovery_span_quality.go. The
// handler walks the per-provider serverless inventory once per cache
// miss and rolls up three diagnostics — cold-start exceeded, sampling
// too aggressive, error rate spike — into a single response shape
// the new WORKLOAD HEALTH dashboard panel renders.
//
// Per docs/proposals/workload-health-panel-slice1.md §4 the response
// carries one ProviderWorkloadHealth per provider plus a Totals
// roll-up so the dashboard renders the per-provider math + the
// headline "any issue" footer in one round-trip.
//
// The handler is intentionally thin: the aggregation math is owned
// by the wired ServerlessHealthInventoryReader interface, not the
// handler. Tests against the handler stub the reader and exercise
// the cache / audit / response composition. The UNION semantics
// for any_issue_count are pinned by a separate helper test
// (WorkloadHealthAnyIssueCount) so the predicate stays single-
// sourced regardless of how the reader is wired.
//
// See docs/proposals/workload-health-panel-slice1.md §5 (UI panel),
// §6 (slice 1 contract), §8 (acceptance tests 1-15).

// --- response types -----------------------------------------------------

// ProviderWorkloadHealth is the per-provider workload health
// aggregate. All four diagnostic counts ARE absolute resource counts;
// the *_Pct fields are zero-safe percentages (returns 0 when
// serverless_resource_count is 0, NOT NaN — design doc §8
// acceptance test 7 wants the dashboard never to render NaN).
//
// AnyIssueCount uses UNION semantics: a resource that fires both
// cold-start AND sampling counts as 1 (design doc §4 + §8
// acceptance test 5). The reader is responsible for computing this
// from the per-resource detection outcomes; the handler does NOT
// double-count.
type ProviderWorkloadHealth struct {
	ServerlessResourceCount    int     `json:"serverless_resource_count"`
	ColdStartExceededCount     int     `json:"cold_start_exceeded_count"`
	ColdStartExceededPct       float64 `json:"cold_start_exceeded_pct"`
	SamplingTooAggressiveCount int     `json:"sampling_too_aggressive_count"`
	SamplingTooAggressivePct   float64 `json:"sampling_too_aggressive_pct"`
	ErrorRateSpikeCount        int     `json:"error_rate_spike_count"`
	ErrorRateSpikePct          float64 `json:"error_rate_spike_pct"`
	AnyIssueCount              int     `json:"any_issue_count"`
	AnyIssuePct                float64 `json:"any_issue_pct"`
}

// WorkloadHealthResponse is the JSON wire shape. Providers is always
// keyed by the four provider strings ("aws", "gcp", "azure", "oci")
// so the UI renders deterministically regardless of which providers
// are wired.
type WorkloadHealthResponse struct {
	Providers map[string]ProviderWorkloadHealth `json:"providers"`
	Totals    ProviderWorkloadHealth            `json:"totals"`
}

// --- reader interface ---------------------------------------------------

// ServerlessHealthInventoryReader is the slim, optional surface the
// compose pass uses to populate the per-provider workload health
// counts. Production wires a reader that walks the in-memory
// serverless inventory (the same one the cold-start / sampling /
// error-rate annotators fill at scan time) and counts, per provider:
//
//   - ServerlessResourceCount: total serverless resources observed
//     in the most recent scan. The honest denominator for every
//     percentage.
//   - ColdStartExceededCount: resources whose latest cold_start_
//     observation passes the substrate 1.5x ratio + 500ms floor +
//     >=50 baseline samples gates.
//   - SamplingTooAggressiveCount: resources whose latest sampling
//     ratio is below the 5% floor AND invocation count >=1000.
//   - ErrorRateSpikeCount: resources whose latest error rate
//     observation passes the 2.0x ratio + >=1000 invocations + >=50
//     errors gates.
//   - AnyIssueCount: UNION over the three above — a resource that
//     fires both cold-start AND sampling counts as 1, not 2 (design
//     doc §4 + §8 acceptance test 5).
//
// Cold-start posture: a nil reader leaves every count at zero across
// every provider + totals, which matches the "panel hides when all 3
// percentages are zero" UI contract on a deployment that hasn't yet
// wired the per-provider serverless inventory annotators. Slice 1
// of Workload Health intentionally ships the endpoint + panel ahead
// of the production wiring; the wiring lands in a follow-on chunk
// alongside per-provider scan integration.
type ServerlessHealthInventoryReader interface {
	WorkloadHealthCounts(ctx context.Context) map[string]WorkloadHealthProviderCounts
}

// WorkloadHealthProviderCounts is the per-provider tuple
// ServerlessHealthInventoryReader returns. Each count must satisfy
// the per-row invariants the WorkloadHealthAnyIssueCount helper pins
// (all sub-counts <= ServerlessResourceCount; AnyIssueCount <=
// ServerlessResourceCount AND >= max(sub-counts)). Compose silently
// clamps if it isn't (defensive against an out-of-sync inventory
// snapshot).
type WorkloadHealthProviderCounts struct {
	ServerlessResourceCount    int
	ColdStartExceededCount     int
	SamplingTooAggressiveCount int
	ErrorRateSpikeCount        int
	AnyIssueCount              int
}

// --- per-row predicate helper ------------------------------------------

// WorkloadHealthAnyIssueCount returns the count of resources where AT
// LEAST ONE of the three diagnostics fires (UNION semantics). The
// three input slices MUST be the same length — one bool per
// serverless resource, indexed identically across the three. Indices
// not satisfying that invariant silently default to false.
//
// This helper is the single source of truth for the UNION rule
// (design doc §4 + §8 acceptance test 5). Both the production
// reader (wire layer) AND the chunk-1 test suite call it so the
// predicate stays consistent regardless of wiring.
func WorkloadHealthAnyIssueCount(
	coldStart, sampling, errorRate []bool,
) int {
	n := len(coldStart)
	if len(sampling) < n {
		n = len(sampling)
	}
	if len(errorRate) < n {
		n = len(errorRate)
	}
	count := 0
	for i := 0; i < n; i++ {
		if coldStart[i] || sampling[i] || errorRate[i] {
			count++
		}
	}
	return count
}

// --- cache --------------------------------------------------------------
//
// workloadHealthCache mirrors the v0.89.76 traceCoverageCache pattern:
// a small TTL cache around the composed WorkloadHealthResponse so
// subsequent dashboard polls inside the 30s window short-circuit the
// four-provider walk. Production TTL is 30s per design doc §6 ("same
// 30s in-memory cache pattern as the v0.89.61 summary endpoint");
// tests pin a shorter TTL or an injectable clock to deterministically
// exercise the expire-then-refetch path.

type workloadHealthCache struct {
	mu       sync.Mutex
	cached   *WorkloadHealthResponse
	cachedAt time.Time
	ttl      time.Duration
	clock    func() time.Time
}

func newWorkloadHealthCache(ttl time.Duration, clock func() time.Time) *workloadHealthCache {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &workloadHealthCache{ttl: ttl, clock: clock}
}

// Get returns the cached response and true if within the TTL window;
// otherwise (nil, false). A nil cached value always returns (nil,
// false).
func (c *workloadHealthCache) Get() (*WorkloadHealthResponse, bool) {
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
func (c *workloadHealthCache) Set(r *WorkloadHealthResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = r
	c.cachedAt = c.clock()
}

// --- handler ------------------------------------------------------------

// DefaultWorkloadHealthCacheTTL is the production cache TTL per design
// doc §6. Mirrors DefaultTraceCoverageCacheTTL + DefaultSummaryCacheTTL
// so the dashboard's three polling endpoints carry identical staleness
// budgets.
const DefaultWorkloadHealthCacheTTL = 30 * time.Second

// workloadHealthAggregationTimeout caps the per-request reader walk so
// a slow inventory reader can't hang the dashboard.
const workloadHealthAggregationTimeout = 10 * time.Second

// workloadHealthProviderOrder pins the four-provider key set so the
// pre-populated Providers map renders deterministically. Sorted for
// stable audit payloads.
var workloadHealthProviderOrder = []string{"aws", "azure", "gcp", "oci"}

// DiscoveryWorkloadHealthHandlers serves
// GET /api/v1/discovery/workload_health. The reader is OPTIONAL — a
// nil reader yields a zero-count ProviderWorkloadHealth for every
// provider so a deployment that hasn't wired the serverless inventory
// annotators renders the panel hidden (design doc §5.3 hide
// conditions). auditService nil disables the cache-miss emit.
type DiscoveryWorkloadHealthHandlers struct {
	reader       ServerlessHealthInventoryReader
	auditService services.AuditService
	cache        *workloadHealthCache
	logger       *zap.Logger
}

// NewDiscoveryWorkloadHealthHandlers builds the handler. ttl <= 0
// falls through to DefaultWorkloadHealthCacheTTL; a nil clock falls
// through to time.Now (production posture). A nil reader is
// acceptable — see the type godoc above.
func NewDiscoveryWorkloadHealthHandlers(
	reader ServerlessHealthInventoryReader,
	auditService services.AuditService,
	ttl time.Duration,
	clock func() time.Time,
	logger *zap.Logger,
) *DiscoveryWorkloadHealthHandlers {
	if ttl <= 0 {
		ttl = DefaultWorkloadHealthCacheTTL
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DiscoveryWorkloadHealthHandlers{
		reader:       reader,
		auditService: auditService,
		cache:        newWorkloadHealthCache(ttl, clock),
		logger:       logger,
	}
}

// HandleWorkloadHealth serves
// GET /api/v1/discovery/workload_health.
//
// Cache hit: return the cached response immediately (no audit emit).
// Cache miss: walk the reader, compose the response, cache it, and
// emit one discovery.workload_health.requested audit row. Per design
// doc §6 the route sits under the existing auth middleware (registered
// in server.go under ScopeAgentsRead — same scope as the rest of the
// read-side discovery surface).
func (h *DiscoveryWorkloadHealthHandlers) HandleWorkloadHealth(c *gin.Context) {
	if cached, ok := h.cache.Get(); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), workloadHealthAggregationTimeout)
	defer cancel()

	resp := h.compose(ctx)

	if h.auditService != nil {
		_ = h.auditService.Record(ctx, services.AuditEntry{
			Actor:     "system",
			EventType: services.AuditEventDiscoveryWorkloadHealthRequested,
			Action:    "requested",
			Payload: map[string]any{
				"cache_status":               "miss",
				"total_serverless_resources": resp.Totals.ServerlessResourceCount,
				"total_any_issue_count":      resp.Totals.AnyIssueCount,
				"total_any_issue_pct":        resp.Totals.AnyIssuePct,
				"recorded_at":                time.Now().UTC(),
			},
		})
	}

	h.cache.Set(resp)
	c.JSON(http.StatusOK, resp)
}

// compose walks the reader, populates every provider key, and rolls
// the totals row. Same cold-start posture as the trace coverage
// handler: a nil reader yields every provider populated as zero
// counts (the panel will hide itself per design doc §5.3).
func (h *DiscoveryWorkloadHealthHandlers) compose(ctx context.Context) *WorkloadHealthResponse {
	resp := &WorkloadHealthResponse{
		Providers: map[string]ProviderWorkloadHealth{
			"aws":   {},
			"gcp":   {},
			"azure": {},
			"oci":   {},
		},
	}

	if h.reader == nil {
		return resp
	}

	raw := h.reader.WorkloadHealthCounts(ctx)
	if raw == nil {
		return resp
	}

	for _, name := range workloadHealthProviderOrder {
		counts := raw[name]
		resp.Providers[name] = projectProviderWorkloadHealth(counts)
	}

	// Totals roll up the absolute counts; the percentages are derived
	// once at the end against the totals' serverless_resource_count
	// denominator. This matches the design doc §4 example where the
	// per-provider percentages and the totals percentages are
	// independent rollups, not weighted averages of per-provider
	// percentages.
	var tCounts WorkloadHealthProviderCounts
	for _, name := range workloadHealthProviderOrder {
		p := resp.Providers[name]
		tCounts.ServerlessResourceCount += p.ServerlessResourceCount
		tCounts.ColdStartExceededCount += p.ColdStartExceededCount
		tCounts.SamplingTooAggressiveCount += p.SamplingTooAggressiveCount
		tCounts.ErrorRateSpikeCount += p.ErrorRateSpikeCount
		tCounts.AnyIssueCount += p.AnyIssueCount
	}
	resp.Totals = projectProviderWorkloadHealth(tCounts)

	return resp
}

// projectProviderWorkloadHealth turns the per-provider counts tuple
// into the JSON-wire ProviderWorkloadHealth struct with all five
// percentage fields populated against the serverless_resource_count
// denominator. Defensive: clamps sub-counts that exceed the
// denominator to the denominator (an out-of-sync inventory snapshot
// must NEVER make any percentage exceed 100.0).
func projectProviderWorkloadHealth(c WorkloadHealthProviderCounts) ProviderWorkloadHealth {
	den := c.ServerlessResourceCount
	clamp := func(n int) int {
		if n < 0 {
			return 0
		}
		if den > 0 && n > den {
			return den
		}
		return n
	}
	cs := clamp(c.ColdStartExceededCount)
	sp := clamp(c.SamplingTooAggressiveCount)
	er := clamp(c.ErrorRateSpikeCount)
	any := clamp(c.AnyIssueCount)
	// The UNION any-issue can't be smaller than the max of its parts.
	// Defensive against a reader that under-reports the UNION on
	// stale data.
	for _, sub := range []int{cs, sp, er} {
		if sub > any {
			any = sub
		}
	}
	return ProviderWorkloadHealth{
		ServerlessResourceCount:    den,
		ColdStartExceededCount:     cs,
		ColdStartExceededPct:       workloadHealthPct(cs, den),
		SamplingTooAggressiveCount: sp,
		SamplingTooAggressivePct:   workloadHealthPct(sp, den),
		ErrorRateSpikeCount:        er,
		ErrorRateSpikePct:          workloadHealthPct(er, den),
		AnyIssueCount:              any,
		AnyIssuePct:                workloadHealthPct(any, den),
	}
}

// workloadHealthPct returns num / den * 100 rounded to one decimal.
// Zero-safe: returns 0 when den is 0. Mirrors round1 from
// discovery_trace_coverage.go so the dashboard's three polling
// endpoints use one rounding rule.
func workloadHealthPct(num, den int) float64 {
	if den <= 0 {
		return 0
	}
	return math.Round((float64(num)/float64(den)*100)*10) / 10
}

// workloadHealthProviderKeys returns the four-provider key set in a
// sorted slice for stable audit payloads + iteration. Tests use this
// to assert deterministic ordering of any per-provider rollups.
func workloadHealthProviderKeys() []string {
	out := make([]string, len(workloadHealthProviderOrder))
	copy(out, workloadHealthProviderOrder)
	sort.Strings(out)
	return out
}
