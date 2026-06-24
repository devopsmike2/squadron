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
//
// PendingTraceEmissionCount — v0.89.82 (#713 Stream 111, Trace
// integration slice 2 chunk 3) — counts inventory rows for this
// provider that have primitive_enabled=true AND
// (last_seen_at IS NULL OR last_seen_at is older than 24h). It surfaces
// the slice-2 "instrumented but not emitting" gap — resources the
// scanner believes are wired but whose traces aren't actually flowing.
// Zero on cold start; the dashboard hides the sub-indicator when the
// fleet-wide sum is 0 (design doc §10 acceptance test 10).
type ProviderTraceCoverage struct {
	InventoryCount            int        `json:"inventory_count"`
	EmittingCount             int        `json:"emitting_count"`
	CoveragePct               float64    `json:"coverage_pct"`
	StrongMatchPct            float64    `json:"strong_match_pct"`
	WeakMatchPct              float64    `json:"weak_match_pct"`
	LastIndexUpdateAt         *time.Time `json:"last_index_update_at,omitempty"`
	PendingTraceEmissionCount int        `json:"pending_trace_emission_count"`
	// ServerlessPct — serverless tier slice 1 chunk 5 (v0.89.92,
	// #725 Stream 123) — per-provider serverless coverage rendered
	// as emitting / inventory * 100 over the serverless-only
	// sub-counts. A serverless function counts as "emitting" when
	// last_seen_at is within 24h, same rule as the other tiers
	// (docs/proposals/serverless-tier-slice1.md §6.4). Zero on cold
	// start; the dashboard hides the chip line when fleet-wide
	// inventory is 0 (design doc §7 + §11 acceptance test 13).
	ServerlessPct float64 `json:"serverless_pct"`
	// OrchestrationPct — orchestration tier slice 1 chunk 4
	// (v0.89.97, #731 Stream 129) — per-provider orchestration
	// coverage rendered as emitting / inventory * 100 over the
	// orchestration-only sub-counts. A workflow / state machine
	// counts as "emitting" when last_seen_at is within 24h, same
	// rule as the other tiers (docs/proposals/
	// orchestration-tier-slice1.md §6.4). Zero on cold start and
	// always zero for OCI in slice 1 (OCI orchestration is deferred
	// to slice 2 per the design doc §2). The dashboard hides the
	// ORCH chip line when fleet-wide inventory is 0.
	OrchestrationPct float64 `json:"orchestration_pct"`
	// EventSourcePct — event source tier slice 1 chunk 5 (v0.89.102,
	// #738 Stream 136) — per-provider event source coverage rendered
	// as emitting / inventory * 100 over the event-source-only
	// sub-counts. An event source counts as "emitting" when
	// last_seen_at is within 24h (docs/proposals/
	// event-source-tier-slice1.md §6.4). Zero on cold start. Unlike
	// OrchestrationPct, all four providers populate (including OCI)
	// since OCI Streaming ships in slice 1. The dashboard hides the
	// EVT chip line when fleet-wide inventory is 0.
	EventSourcePct float64 `json:"event_source_pct"`
	// PropagationPct — event source tier slice 2 chunk 1 (v0.89.105,
	// #741 Stream 139) — % of inventoried event sources whose
	// has_propagation_config bool is true. Numerator is the count of
	// per-provider event_source_instance rows where
	// has_propagation_config = true; denominator is the per-provider
	// event source inventory total. 0 when there are no event
	// sources in this provider's inventory. Computed alongside the
	// existing EventSourcePct so the dashboard's chip can render
	// "EVT N% (prop M%)" in one round-trip (chunk 5 wires the chip
	// suffix; this chunk just ships the numeric value).
	//
	// All four providers populate (including OCI for slice 2 chunks
	// 4) when the per-cloud scanners set has_propagation_config. In
	// slice 2 chunk 1, only AWS EventBridge populates the bool;
	// chunks 2-4 follow. Until then, GCP / Azure / OCI snapshots
	// default has_propagation_config to false, so their
	// PropagationPct stays at 0 even when event sources are present.
	//
	// See docs/proposals/event-source-tier-slice2.md §6 for the API
	// surface and §11 acceptance test 15 (TestTraceCoverage_
	// IncludesPropagationPct).
	PropagationPct float64 `json:"propagation_pct"`
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

// PendingTraceEmissionCountQuery answers "how many inventory rows for
// this (provider, scopeID) have primitive_enabled=true AND last_seen_at
// null-or-stale." Returns 0 on cold start. Nil implementation short-
// circuits to 0 the same way InventoryCountQuery does.
//
// staleAfter is the cutoff age above which a last_seen_at counts as
// "no recent emission" — production passes pendingTraceEmissionStaleAfter
// (24h per docs/proposals/trace-integration-slice2.md §3); tests are
// free to inject any duration.
type PendingTraceEmissionCountQuery interface {
	PendingTraceEmissionCount(ctx context.Context, provider, scopeID string, staleAfter time.Duration) (int, error)
}

// ServerlessCoverageQuery — serverless tier slice 1 chunk 5 (v0.89.92,
// #725 Stream 123). Returns per-scope serverless inventory + emitting
// counts so the per-provider ServerlessPct chip can be computed
// alongside the existing coverage_pct. A row counts as "emitting"
// when its last_seen_at is within staleAfter (24h per design doc §6.4).
// Zero counts on cold start; nil implementation short-circuits to
// (0, 0) the same way InventoryCountQuery does.
type ServerlessCoverageQuery interface {
	ServerlessCoverageForScope(ctx context.Context, provider, scopeID string, staleAfter time.Duration) (inventory, emitting int, err error)
}

// OrchestrationCoverageQuery — orchestration tier slice 1 chunk 4
// (v0.89.97, #731 Stream 129). Returns per-scope orchestration
// inventory + emitting counts so the per-provider OrchestrationPct
// chip can be computed alongside the existing coverage_pct. A row
// counts as "emitting" when its last_seen_at is within staleAfter
// (24h per design doc §6.4). Zero counts on cold start; nil
// implementation short-circuits to (0, 0) the same way
// ServerlessCoverageQuery does. OCI always reports (0, 0) in slice 1
// because the OCI scanner returns no orchestrations.
type OrchestrationCoverageQuery interface {
	OrchestrationCoverageForScope(ctx context.Context, provider, scopeID string, staleAfter time.Duration) (inventory, emitting int, err error)
}

// EventSourceCoverageQuery — event source tier slice 1 chunk 5
// (v0.89.102, #738 Stream 136). Returns per-scope event source
// inventory + emitting counts so the per-provider EventSourcePct
// chip can be computed alongside the existing coverage_pct. A row
// counts as "emitting" when its last_seen_at is within staleAfter
// (24h per design doc §6.4). Zero counts on cold start; nil
// implementation short-circuits to (0, 0) the same way
// OrchestrationCoverageQuery does. Unlike orchestration, OCI is
// expected to return real counts in slice 1 — OCI Streaming ships
// as a real surface alongside the other three clouds.
type EventSourceCoverageQuery interface {
	EventSourceCoverageForScope(ctx context.Context, provider, scopeID string, staleAfter time.Duration) (inventory, emitting int, err error)
}

// EventSourcePropagationQuery — event source tier slice 2 chunk 1
// (v0.89.105, #741 Stream 139). Returns per-scope (inventory,
// propagating) counts for the PropagationPct rollup. The numerator
// (propagating) counts event_source_instance rows whose
// has_propagation_config bool inside snapshot_json is true; the
// denominator (inventory) is the full event source row count. The
// production InventoryStore performs the per-row JSON projection;
// tests substitute a stub returning canned counts the same way
// EventSourceCoverageQuery does.
//
// Zero counts on cold start; nil implementation short-circuits to
// (0, 0). All four providers (including OCI) are expected to return
// real counts once the per-cloud slice-2 chunks (2-4) land — the
// chunk-1 query interface is provider-agnostic so the per-scope walk
// composes against any provider's event_source_instance rows
// uniformly.
type EventSourcePropagationQuery interface {
	EventSourcePropagationForScope(ctx context.Context, provider, scopeID string) (inventory, propagating int, err error)
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

// pendingTraceEmissionStaleAfter is the cutoff for the slice-2
// "primitive_enabled but no recent emission" detection rule per
// docs/proposals/trace-integration-slice2.md §3: a row counts as
// pending when last_seen_at is null or older than 24h.
const pendingTraceEmissionStaleAfter = 24 * time.Hour

// DiscoveryTraceCoverageHandlers serves
// GET /api/v1/discovery/trace_coverage. Each per-provider store is
// OPTIONAL — a nil store yields a zero-count ProviderTraceCoverage so
// a deployment that wired only two clouds renders the other two
// providers as empty rather than failing. traceIndex nil short-circuits
// every provider to all-zero counts (the same posture as a deployment
// that hasn't observed any spans yet); inventoryQuery nil short-circuits
// the inventory_count lookup to 0 for every scope.
type DiscoveryTraceCoverageHandlers struct {
	awsStore        AWSSummaryStore
	gcpStore        GCPSummaryStore
	azureStore      AzureSummaryStore
	ociStore        OCISummaryStore
	traceIndex      TraceIndex
	inventoryQuery  InventoryCountQuery
	pendingQuery       PendingTraceEmissionCountQuery
	serverlessQuery    ServerlessCoverageQuery
	orchestrationQuery OrchestrationCoverageQuery
	eventSourceQuery   EventSourceCoverageQuery
	// propagationQuery — event source tier slice 2 chunk 1 (v0.89.105,
	// #741 Stream 139). Optional per-scope query feeding
	// PropagationPct. nil short-circuits the column to 0 for every
	// provider — same nil-tolerant posture as eventSourceQuery.
	propagationQuery EventSourcePropagationQuery
	auditService     services.AuditService
	cache           *traceCoverageCache
	logger          *zap.Logger
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
	pendingQuery PendingTraceEmissionCountQuery,
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
		pendingQuery:   pendingQuery,
		auditService:   auditService,
		cache:          newTraceCoverageCache(ttl, clock),
		logger:         logger,
	}
}

// WithServerlessQuery — serverless tier slice 1 chunk 5 (v0.89.92,
// #725 Stream 123). Optional setter for the per-scope serverless
// coverage query. nil short-circuits the ServerlessPct column to 0 for
// every provider (deployments that haven't wired the new query stay on
// cold-start posture). Kept as a setter rather than a positional ctor
// arg to preserve the v0.89.82 NewDiscoveryTraceCoverageHandlers
// signature — existing call sites and tests don't churn just because
// the serverless tier landed.
func (h *DiscoveryTraceCoverageHandlers) WithServerlessQuery(q ServerlessCoverageQuery) *DiscoveryTraceCoverageHandlers {
	h.serverlessQuery = q
	return h
}

// WithOrchestrationQuery — orchestration tier slice 1 chunk 4
// (v0.89.97, #731 Stream 129). Optional setter for the per-scope
// orchestration coverage query. nil short-circuits the
// OrchestrationPct column to 0 for every provider. Same setter-only
// extension pattern as WithServerlessQuery — deployments that haven't
// wired the new query stay on cold-start posture, no constructor
// signature churn.
func (h *DiscoveryTraceCoverageHandlers) WithOrchestrationQuery(q OrchestrationCoverageQuery) *DiscoveryTraceCoverageHandlers {
	h.orchestrationQuery = q
	return h
}

// WithEventSourceQuery — event source tier slice 1 chunk 5
// (v0.89.102, #738 Stream 136). Optional setter for the per-scope
// event source coverage query. nil short-circuits the EventSourcePct
// column to 0 for every provider. Same setter-only extension pattern
// as WithOrchestrationQuery.
func (h *DiscoveryTraceCoverageHandlers) WithEventSourceQuery(q EventSourceCoverageQuery) *DiscoveryTraceCoverageHandlers {
	h.eventSourceQuery = q
	return h
}

// WithEventSourcePropagationQuery — event source tier slice 2 chunk 1
// (v0.89.105, #741 Stream 139). Optional setter for the per-scope
// event source propagation query. nil short-circuits the
// PropagationPct column to 0 for every provider — the slice-2 chunks
// 2-4 add per-cloud propagation detection scanners; this query reads
// the persisted has_propagation_config bool from snapshot_json. Same
// setter-only extension pattern as WithEventSourceQuery so existing
// call sites don't churn.
func (h *DiscoveryTraceCoverageHandlers) WithEventSourcePropagationQuery(q EventSourcePropagationQuery) *DiscoveryTraceCoverageHandlers {
	h.propagationQuery = q
	return h
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
		// v0.89.82 (#713 Stream 111) — slice-2 pending-emission roll
		// up. Same nil-tolerant posture as inventoryQuery: a nil
		// pendingQuery short-circuits to 0 so a deployment that hasn't
		// wired the projection sees the sub-indicator hidden.
		if h.pendingQuery != nil {
			n, err := h.pendingQuery.PendingTraceEmissionCount(ctx, provider, scope, pendingTraceEmissionStaleAfter)
			if err != nil {
				h.logger.Warn("trace coverage: pending lookup failed",
					zap.String("provider", provider),
					zap.String("scope", scope),
					zap.Error(err))
			} else {
				out.PendingTraceEmissionCount += n
			}
		}
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

	// Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) —
	// roll up per-scope serverless coverage when the optional
	// ServerlessCoverageQuery is wired. Sums the per-scope counts and
	// computes ServerlessPct = emitting / inventory * 100. nil query
	// short-circuits to 0; a failed per-scope query logs a warning and
	// stays at 0 for that scope. Mirrors the existing
	// inventoryQuery / pendingQuery nil-tolerant posture so a
	// deployment that hasn't wired the new query renders the chip line
	// hidden (UI hides the line when the fleet-wide inventory is 0,
	// per design doc §11 acceptance test 13).
	if h.serverlessQuery != nil {
		var slInv, slEmit int
		for _, scope := range scopes {
			if scope == "" {
				continue
			}
			inv, emit, err := h.serverlessQuery.ServerlessCoverageForScope(ctx, provider, scope, pendingTraceEmissionStaleAfter)
			if err != nil {
				h.logger.Warn("trace coverage: serverless lookup failed",
					zap.String("provider", provider),
					zap.String("scope", scope),
					zap.Error(err))
				continue
			}
			slInv += inv
			slEmit += emit
		}
		out.ServerlessPct = computeTraceCoveragePct(slEmit, slInv)
	}

	// Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129) —
	// roll up per-scope orchestration coverage when the optional
	// OrchestrationCoverageQuery is wired. Sums per-scope counts and
	// computes OrchestrationPct = emitting / inventory * 100. nil query
	// short-circuits to 0; a failed per-scope query logs a warning and
	// stays at 0 for that scope. Mirrors the serverlessQuery
	// nil-tolerant posture so a deployment that hasn't wired the new
	// query renders the chip line hidden. OCI scopes still get
	// queried — the OCI substrate returns (0, 0) in slice 1 so the
	// rollup naturally stays at zero.
	if h.orchestrationQuery != nil {
		var ocInv, ocEmit int
		for _, scope := range scopes {
			if scope == "" {
				continue
			}
			inv, emit, err := h.orchestrationQuery.OrchestrationCoverageForScope(ctx, provider, scope, pendingTraceEmissionStaleAfter)
			if err != nil {
				h.logger.Warn("trace coverage: orchestration lookup failed",
					zap.String("provider", provider),
					zap.String("scope", scope),
					zap.Error(err))
				continue
			}
			ocInv += inv
			ocEmit += emit
		}
		out.OrchestrationPct = computeTraceCoveragePct(ocEmit, ocInv)
	}

	// Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136) —
	// roll up per-scope event source coverage when the optional
	// EventSourceCoverageQuery is wired. Mirrors the orchestrationQuery
	// nil-tolerant posture. All four providers (including OCI) are
	// expected to surface real counts in slice 1 since OCI Streaming
	// ships as a real surface alongside the other three clouds.
	if h.eventSourceQuery != nil {
		var esInv, esEmit int
		for _, scope := range scopes {
			if scope == "" {
				continue
			}
			inv, emit, err := h.eventSourceQuery.EventSourceCoverageForScope(ctx, provider, scope, pendingTraceEmissionStaleAfter)
			if err != nil {
				h.logger.Warn("trace coverage: event source lookup failed",
					zap.String("provider", provider),
					zap.String("scope", scope),
					zap.Error(err))
				continue
			}
			esInv += inv
			esEmit += emit
		}
		out.EventSourcePct = computeTraceCoveragePct(esEmit, esInv)
	}

	// Event source tier slice 2 chunk 1 (v0.89.105, #741 Stream 139) —
	// roll up per-scope event source propagation counts when the
	// optional EventSourcePropagationQuery is wired. Numerator is the
	// per-provider sum of rows whose has_propagation_config is true;
	// denominator is the per-provider event source inventory. nil
	// query short-circuits to 0; a failed per-scope query logs a
	// warning and stays at 0 for that scope. Mirrors the
	// eventSourceQuery nil-tolerant posture so a deployment that
	// hasn't wired the new query renders the dashboard chip suffix
	// hidden (chunk 5 wires the suffix; the chip line is hidden when
	// PropagationPct is 0 and inventory is 0).
	if h.propagationQuery != nil {
		var pInv, pProp int
		for _, scope := range scopes {
			if scope == "" {
				continue
			}
			inv, prop, err := h.propagationQuery.EventSourcePropagationForScope(ctx, provider, scope)
			if err != nil {
				h.logger.Warn("trace coverage: event source propagation lookup failed",
					zap.String("provider", provider),
					zap.String("scope", scope),
					zap.Error(err))
				continue
			}
			pInv += inv
			pProp += prop
		}
		out.PropagationPct = computeTraceCoveragePct(pProp, pInv)
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
