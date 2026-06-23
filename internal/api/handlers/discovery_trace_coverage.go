// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// --- response types -----------------------------------------------------
//
// ProviderTraceCoverage / TraceCoverageTotals / TraceCoverageResponse
// are the wire shape returned by GET /api/v1/discovery/trace_coverage.
// Per docs/proposals/trace-integration-slice1.md §7 the response carries
// one ProviderTraceCoverage per provider plus a Totals roll-up so the
// Discovery dashboard can render the per-provider chips AND the headline
// number in one round-trip.
//
// CoveragePct is zero-safe: a provider with no inventory yet (a fresh
// install before any scan ran) returns 0.0 NOT NaN. Acceptance test 11
// ("cold-start parity preserved") pins this. StrongMatchPct +
// WeakMatchPct surface unchanged from the traceindex.Summary so the
// dashboard's caveat-icon logic for weak-match providers reads them
// directly without recomputing.

// ProviderTraceCoverage is the per-provider trace coverage aggregate.
type ProviderTraceCoverage struct {
	InventoryCount    int        `json:"inventory_count"`
	EmittingCount     int        `json:"emitting_count"`
	CoveragePct       float64    `json:"coverage_pct"`
	StrongMatchPct    float64    `json:"strong_match_pct"`
	WeakMatchPct      float64    `json:"weak_match_pct"`
	LastIndexUpdateAt *time.Time `json:"last_index_update_at,omitempty"`
}

// TraceCoverageTotals is the cross-provider roll-up.
type TraceCoverageTotals struct {
	InventoryCount int     `json:"inventory_count"`
	EmittingCount  int     `json:"emitting_count"`
	CoveragePct    float64 `json:"coverage_pct"`
}

// TraceCoverageResponse is the JSON wire shape. Providers is always
// keyed by the four provider strings ("aws", "gcp", "azure", "oci") so
// the UI renders deterministically regardless of which providers are
// wired in the deployment.
type TraceCoverageResponse struct {
	Providers map[string]ProviderTraceCoverage `json:"providers"`
	Totals    TraceCoverageTotals              `json:"totals"`
}

// --- store + index interfaces ------------------------------------------
//
// The handler reuses the four AWSSummaryStore / GCPSummaryStore /
// AzureSummaryStore / OCISummaryStore connection-list adapters from
// discovery_summary.go so production wiring composes against the same
// list-only stores both handlers consume.
//
// InventoryCountQuery returns the slice-1 inventory_count proxy per
// (provider, scopeID). The production adapter (see
// discovery_trace_coverage_wire.go) reads the most recent
// scan_completed event from the audit log — identical to how
// discovery_summary.go projects inventory. Tests substitute a stub
// returning canned counts so the assertions stay focused on the
// trace-coverage aggregation, not on the audit projection logic.

// InventoryCountQuery answers "how many resources did the most recent
// scan see for this (provider, scopeID) pair." Returns 0 on cold start
// (no scan_completed event yet) — zero-safe by design.
type InventoryCountQuery interface {
	InventoryCountForScope(ctx context.Context, provider, scopeID string) (int, error)
}

// TraceIndex is the slim surface DiscoveryTraceCoverageHandlers uses to
// project per-scope trace coverage. The real *traceindex.Index from
// internal/traceindex satisfies this directly; tests substitute a stub
// returning canned Summary values.
//
// LastIndexUpdateAt is NOT a separate method here — the design
// alternative (a fleet-wide max(last_seen_at) call) would have required
// a new method on the traceindex.Index, expanding the slice-1 surface
// for a value the per-scope Coverage call already returns inside its
// Summary.LastIndexUpdateAt. The handler aggregates the per-scope
// timestamps into a per-provider max at compose time; the per-provider
// last_index_update_at the dashboard renders is the latest update seen
// across that provider's scopes.
type TraceIndex interface {
	Coverage(ctx context.Context, provider, scopeID string, inventoryCount int) (traceindex.Summary, error)
}

// --- cache --------------------------------------------------------------
//
// traceCoverageCache mirrors the v0.89.61 summaryCache pattern: a small
// TTL cache around the composed TraceCoverageResponse so subsequent
// dashboard polls inside the 30s window short-circuit the four-provider
// walk. Production TTL is 30s per design doc §7; tests pin a shorter
// TTL or an injectable clock to deterministically exercise the
// expire-then-refetch path.

type traceCoverageCache struct {
	mu       sync.Mutex
	cached   *TraceCoverageResponse
	cachedAt time.Time
	ttl      time.Duration
	clock    func() time.Time
}

func newTraceCoverageCache(ttl time.Duration, clock func() time.Time) *traceCoverageCache {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &traceCoverageCache{ttl: ttl, clock: clock}
}

// Get returns the cached response and true if within the TTL window;
// otherwise (nil, false). A nil cached value always returns (nil, false).
func (c *traceCoverageCache) Get() (*TraceCoverageResponse, bool) {
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
func (c *traceCoverageCache) Set(r *TraceCoverageResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = r
	c.cachedAt = c.clock()
}

// --- handler ------------------------------------------------------------

// DefaultTraceCoverageCacheTTL is the production cache TTL per design
// doc §7. Mirrors DefaultSummaryCacheTTL so the dashboard's two polling
// endpoints carry identical staleness budgets.
const DefaultTraceCoverageCacheTTL = 30 * time.Second

// traceCoverageAggregationTimeout caps the per-request fan-out across
// the four provider stores so a slow store can't hang the dashboard.
const traceCoverageAggregationTimeout = 10 * time.Second

// DiscoveryTraceCoverageHandlers serves
// GET /api/v1/discovery/trace_coverage. Each per-provider store is
// OPTIONAL — a nil store yields a zero-count ProviderTraceCoverage so
// a deployment that wired only two clouds renders the other two
// providers as empty rather than failing. traceIndex nil short-circuits
// every provider to all-zero counts (the same posture as a deployment
// that hasn't observed any spans yet); inventoryQuery nil short-circuits
// the inventory_count lookup to 0 for every scope.
type DiscoveryTraceCoverageHandlers struct {
	awsStore       AWSSummaryStore
	gcpStore       GCPSummaryStore
	azureStore     AzureSummaryStore
	ociStore       OCISummaryStore
	traceIndex     TraceIndex
	inventoryQuery InventoryCountQuery
	auditService   services.AuditService
	cache          *traceCoverageCache
	logger         *zap.Logger
}

// NewDiscoveryTraceCoverageHandlers builds the handler. ttl <= 0 falls
// through to DefaultTraceCoverageCacheTTL; a nil clock falls through to
// time.Now (production posture). Any store may be nil to express
// "this provider isn't wired in this deployment" — the handler still
// returns 200 with the corresponding provider key populated as a
// zero-count ProviderTraceCoverage.
func NewDiscoveryTraceCoverageHandlers(
	aws AWSSummaryStore,
	gcp GCPSummaryStore,
	azure AzureSummaryStore,
	oci OCISummaryStore,
	traceIndex TraceIndex,
	inventoryQuery InventoryCountQuery,
	auditService services.AuditService,
	ttl time.Duration,
	clock func() time.Time,
	logger *zap.Logger,
) *DiscoveryTraceCoverageHandlers {
	if ttl <= 0 {
		ttl = DefaultTraceCoverageCacheTTL
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DiscoveryTraceCoverageHandlers{
		awsStore:       aws,
		gcpStore:       gcp,
		azureStore:     azure,
		ociStore:       oci,
		traceIndex:     traceIndex,
		inventoryQuery: inventoryQuery,
		auditService:   auditService,
		cache:          newTraceCoverageCache(ttl, clock),
		logger:         logger,
	}
}

// HandleTraceCoverage serves GET /api/v1/discovery/trace_coverage.
//
// Cache hit: return the cached response immediately (no audit emit).
// Cache miss: walk each enabled provider, project the per-scope
// coverage against the traceindex, compose the response, cache it, and
// emit one discovery.trace_coverage.requested audit row. Per design doc
// §7 contract item 6 the route sits under the existing auth middleware
// (registered in server.go under ScopeAgentsRead).
func (h *DiscoveryTraceCoverageHandlers) HandleTraceCoverage(c *gin.Context) {
	if cached, ok := h.cache.Get(); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), traceCoverageAggregationTimeout)
	defer cancel()

	resp := h.compose(ctx)

	if h.auditService != nil {
		_ = h.auditService.Record(ctx, services.AuditEntry{
			Actor:     "system",
			EventType: services.AuditEventTraceCoverageRequested,
			Action:    "requested",
			Payload: map[string]any{
				"cache_status":           "miss",
				"total_inventory_count":  resp.Totals.InventoryCount,
				"total_emitting_count":   resp.Totals.EmittingCount,
				"total_coverage_pct":     resp.Totals.CoveragePct,
				"recorded_at":            time.Now().UTC(),
			},
		})
	}

	h.cache.Set(resp)
	c.JSON(http.StatusOK, resp)
}

// compose walks each provider's store, projects per-scope coverage
// against the traceindex, and rolls per-provider + cross-provider
// totals. One provider's failure does not sink the others — it surfaces
// as a zero-count entry plus a logged warning. The Providers map is
// pre-populated with all four provider keys so the response shape is
// stable across deployments.
func (h *DiscoveryTraceCoverageHandlers) compose(ctx context.Context) *TraceCoverageResponse {
	resp := &TraceCoverageResponse{
		Providers: map[string]ProviderTraceCoverage{
			"aws":   {},
			"gcp":   {},
			"azure": {},
			"oci":   {},
		},
	}

	type job struct {
		name   string
		scopes []string
	}
	var jobs []job

	if h.awsStore != nil {
		ids, err := h.awsStore.ListAWSAccountIDs(ctx)
		if err != nil {
			h.logger.Warn("trace coverage: aws list failed", zap.Error(err))
		} else {
			jobs = append(jobs, job{name: "aws", scopes: ids})
		}
	}
	if h.gcpStore != nil {
		ids, err := h.gcpStore.ListGCPConnectionIDs(ctx)
		if err != nil {
			h.logger.Warn("trace coverage: gcp list failed", zap.Error(err))
		} else {
			jobs = append(jobs, job{name: "gcp", scopes: ids})
		}
	}
	if h.azureStore != nil {
		ids, err := h.azureStore.ListAzureConnectionIDs(ctx)
		if err != nil {
			h.logger.Warn("trace coverage: azure list failed", zap.Error(err))
		} else {
			jobs = append(jobs, job{name: "azure", scopes: ids})
		}
	}
	if h.ociStore != nil {
		ids, err := h.ociStore.ListOCIConnectionIDs(ctx)
		if err != nil {
			h.logger.Warn("trace coverage: oci list failed", zap.Error(err))
		} else {
			jobs = append(jobs, job{name: "oci", scopes: ids})
		}
	}

	for _, j := range jobs {
		resp.Providers[j.name] = h.aggregateProvider(ctx, j.name, j.scopes)
	}

	// Cross-provider totals + zero-safe coverage_pct.
	for _, p := range resp.Providers {
		resp.Totals.InventoryCount += p.InventoryCount
		resp.Totals.EmittingCount += p.EmittingCount
	}
	resp.Totals.CoveragePct = computeTraceCoveragePct(resp.Totals.EmittingCount, resp.Totals.InventoryCount)

	return resp
}

// aggregateProvider walks every scope (account_id / project_id /
// subscription_id / tenancy_ocid) the provider's store reports,
// looks up inventory_count via the InventoryCountQuery, and projects
// per-scope coverage through the traceindex. The per-provider
// aggregate sums emitting and inventory counts, weighted-averages the
// strong/weak match percentages, and forwards the latest
// LastIndexUpdateAt across all scopes.
func (h *DiscoveryTraceCoverageHandlers) aggregateProvider(
	ctx context.Context, provider string, scopes []string,
) ProviderTraceCoverage {
	out := ProviderTraceCoverage{}
	var strongWeighted, weakWeighted, weightTotal float64
	var lastUpdate time.Time

	for _, scope := range scopes {
		if scope == "" {
			continue
		}
		invCount := 0
		if h.inventoryQuery != nil {
			n, err := h.inventoryQuery.InventoryCountForScope(ctx, provider, scope)
			if err != nil {
				h.logger.Warn("trace coverage: inventory lookup failed",
					zap.String("provider", provider),
					zap.String("scope", scope),
					zap.Error(err))
			} else {
				invCount = n
			}
		}

		var sum traceindex.Summary
		if h.traceIndex != nil {
			s, err := h.traceIndex.Coverage(ctx, provider, scope, invCount)
			if err != nil {
				h.logger.Warn("trace coverage: index Coverage failed",
					zap.String("provider", provider),
					zap.String("scope", scope),
					zap.Error(err))
			} else {
				sum = s
			}
		}

		out.InventoryCount += invCount
		out.EmittingCount += sum.EmittingCount
		// Weight the strong / weak split by emitting count so a scope
		// with 1 emitter doesn't drown out a scope with 100 emitters.
		if sum.EmittingCount > 0 {
			w := float64(sum.EmittingCount)
			strongWeighted += sum.StrongMatchPct * w
			weakWeighted += sum.WeakMatchPct * w
			weightTotal += w
		}
		if !sum.LastIndexUpdateAt.IsZero() && sum.LastIndexUpdateAt.After(lastUpdate) {
			lastUpdate = sum.LastIndexUpdateAt
		}
	}

	out.CoveragePct = computeTraceCoveragePct(out.EmittingCount, out.InventoryCount)
	if weightTotal > 0 {
		out.StrongMatchPct = round1(strongWeighted / weightTotal)
		out.WeakMatchPct = round1(weakWeighted / weightTotal)
	}
	if !lastUpdate.IsZero() {
		t := lastUpdate.UTC()
		out.LastIndexUpdateAt = &t
	}
	return out
}

// computeTraceCoveragePct returns emitting / inventory * 100 rounded to
// one decimal. Zero-safe: returns 0 when inventory is 0. Mirrors
// computeCoveragePct from discovery_summary.go so the dashboard's two
// coverage values use identical math.
func computeTraceCoveragePct(emitting, inventory int) float64 {
	if inventory <= 0 {
		return 0
	}
	pct := float64(emitting) / float64(inventory) * 100.0
	return round1(pct)
}

// round1 rounds to one decimal. Factored out so coverage_pct and the
// strong/weak match split share one rounding rule.
func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
