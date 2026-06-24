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

// --- response types -----------------------------------------------------
//
// SummaryResponse / ProviderSummary / SummaryTotals /
// RecentRecommendation are the wire shape returned by
// GET /api/v1/discovery/summary. Defined per docs/proposals/unified-
// discovery-dashboard-slice1.md §4. The Enabled flag distinguishes
// "this provider has zero connections" from "this deployment never
// wired the provider's store" so the dashboard renders a connect-
// state card when Enabled=false instead of pretending zero instances
// exist. CoveragePct is computed server-side as instrumented /
// instance * 100 (zero-safe, rounded to one decimal).

// ProviderSummary is the per-provider aggregate.
//
// ServerlessCount — serverless tier slice 1 chunk 5 (v0.89.92,
// #725 Stream 123) — counts the serverless inventory rows the most
// recent scan_completed event surfaced for this provider. Mirrors the
// existing instance_count rollup pattern but at the per-tier
// granularity per docs/proposals/serverless-tier-slice1.md §6.3.
// Zero on cold start and on deployments that haven't yet observed a
// scan_completed audit row carrying the serverless_count field — the
// audit projection treats the field as optional so older scans don't
// regress.
type ProviderSummary struct {
	ConnectionCount     int        `json:"connection_count"`
	LastScanAt          *time.Time `json:"last_scan_at,omitempty"`
	InstanceCount       int        `json:"instance_count"`
	InstrumentedCount   int        `json:"instrumented_count"`
	UninstrumentedCount int        `json:"uninstrumented_count"`
	RecommendationCount int        `json:"recommendation_count"`
	ServerlessCount     int        `json:"serverless_count"`
	// OrchestrationCount — orchestration tier slice 1 chunk 4
	// (v0.89.97, #731 Stream 129) — counts the orchestration inventory
	// rows the most recent scan_completed event surfaced for this
	// provider. Mirrors the serverless_count pattern from v0.89.92. For
	// OCI it stays 0 in slice 1 — the OCI inventory never populates
	// orchestrations until slice 2.
	OrchestrationCount int `json:"orchestration_count"`
	// EventSourceCount — event source tier slice 1 chunk 5 (v0.89.102,
	// #738 Stream 136) — counts the event source inventory rows the
	// most recent scan_completed event surfaced for this provider.
	// Mirrors the orchestration_count pattern but populates for ALL
	// four providers (including OCI) since OCI Streaming ships in
	// slice 1 — see docs/proposals/event-source-tier-slice1.md §6.3.
	EventSourceCount int  `json:"event_source_count"`
	Enabled          bool `json:"enabled"`
}

// SummaryTotals is the cross-provider roll-up.
//
// ServerlessCount — serverless tier slice 1 chunk 5 (v0.89.92,
// #725 Stream 123) — cross-provider sum of ProviderSummary
// ServerlessCount. Zero on cold start.
type SummaryTotals struct {
	ConnectionCount     int `json:"connection_count"`
	InstanceCount       int `json:"instance_count"`
	InstrumentedCount   int `json:"instrumented_count"`
	UninstrumentedCount int `json:"uninstrumented_count"`
	RecommendationCount int `json:"recommendation_count"`
	ServerlessCount     int `json:"serverless_count"`
	// OrchestrationCount — orchestration tier slice 1 chunk 4
	// (v0.89.97, #731 Stream 129). Cross-provider sum of
	// ProviderSummary.OrchestrationCount. Zero on cold start.
	OrchestrationCount int `json:"orchestration_count"`
	// EventSourceCount — event source tier slice 1 chunk 5 (v0.89.102,
	// #738 Stream 136). Cross-provider sum of
	// ProviderSummary.EventSourceCount. Zero on cold start.
	EventSourceCount int     `json:"event_source_count"`
	CoveragePct      float64 `json:"coverage_pct"`
}

// RecentRecommendation is one row of the cross-provider recent table.
type RecentRecommendation struct {
	Provider    string    `json:"provider"`
	Kind        string    `json:"kind"`
	ResourceID  string    `json:"resource_id,omitempty"`
	ScopeID     string    `json:"scope_id"`
	Region      string    `json:"region"`
	GeneratedAt time.Time `json:"generated_at"`
}

// SummaryResponse is the JSON wire shape. Providers is always keyed by
// the four provider strings ("aws", "gcp", "azure", "oci") so the UI
// renders deterministically; RecentRecommendations is always a non-nil
// slice (the UI sees [] for no recommendations rather than null).
type SummaryResponse struct {
	Providers             map[string]ProviderSummary `json:"providers"`
	Totals                SummaryTotals              `json:"totals"`
	RecentRecommendations []RecentRecommendation     `json:"recent_recommendations"`
}

// --- store interfaces ---------------------------------------------------
//
// The summary handler narrows each per-provider store to a list-only
// adapter so the test stubs stay small and the read-only invariant is
// enforced at the type level. Production wires concrete adapters in
// discovery_summary_wire.go.

// AWSSummaryStore lists AWS account IDs. Pairs with scan_completed
// events keyed by account_id.
type AWSSummaryStore interface {
	ListAWSAccountIDs(ctx context.Context) ([]string, error)
}

// GCPSummaryStore lists GCP connection IDs.
type GCPSummaryStore interface {
	ListGCPConnectionIDs(ctx context.Context) ([]string, error)
}

// AzureSummaryStore lists Azure connection IDs.
type AzureSummaryStore interface {
	ListAzureConnectionIDs(ctx context.Context) ([]string, error)
}

// OCISummaryStore lists OCI connection IDs.
type OCISummaryStore interface {
	ListOCIConnectionIDs(ctx context.Context) ([]string, error)
}

// --- audit query store --------------------------------------------------

// ScanSummary projects one scan_completed audit payload to the few
// fields the summary handler needs. Reconciles the AWS payload shape
// (instrumented+uninstrumented) with the non-AWS shape (total_resources
// + instrumented_count) — see totalInstancesFromPayload in the wire
// adapter.
type ScanSummary struct {
	ScopeID             string
	CompletedAt         time.Time
	InstanceCount       int
	InstrumentedCount   int
	UninstrumentedCount int
	// ServerlessCount — serverless tier slice 1 chunk 5 (v0.89.92,
	// #725 Stream 123) — projects the optional serverless_count
	// payload field from scan_completed audit rows. Zero on older
	// scans that pre-date the serverless tier; the summary handler
	// surfaces zero in that case without a separate cold-start branch.
	ServerlessCount int
	// OrchestrationCount — orchestration tier slice 1 chunk 4
	// (v0.89.97, #731 Stream 129) — projects the optional
	// orchestration_count payload field from scan_completed audit
	// rows. Zero on older scans that pre-date the orchestration tier.
	OrchestrationCount int
	// EventSourceCount — event source tier slice 1 chunk 5 (v0.89.102,
	// #738 Stream 136) — projects the optional event_source_count
	// payload field from scan_completed audit rows. Zero on older
	// scans that pre-date the event source tier. Populated for all
	// four providers including OCI.
	EventSourceCount int
}

// ProposalEvent is one row from discovery_proposal.created projected
// to the summary's recent-recommendations shape.
type ProposalEvent struct {
	Provider    string
	Kind        string
	ResourceID  string
	ScopeID     string
	Region      string
	GeneratedAt time.Time
}

// AuditQueryStore reads the audit log for the summary handler. The
// production implementation wraps applicationstore.ApplicationStore
// (see discovery_summary_wire.go); tests substitute a stub.
type AuditQueryStore interface {
	// ListRecentScanCompletedByProvider returns the most recent
	// scan_completed event per connection for the given provider,
	// keyed by scope_id (account_id / project_id / subscription_id /
	// tenancy_ocid). An empty map means no scan_completed events.
	ListRecentScanCompletedByProvider(ctx context.Context, provider string) (map[string]ScanSummary, error)

	// ListRecentDiscoveryProposals returns up to limit recent
	// discovery_proposal.created events across all providers,
	// newest-first.
	ListRecentDiscoveryProposals(ctx context.Context, limit int) ([]ProposalEvent, error)
}

// --- cache --------------------------------------------------------------

// summaryCache is a TTL cache around the SummaryResponse. The
// aggregation walks four provider stores + an audit-table sweep on
// every miss; the cache short-circuits subsequent calls within the
// TTL. Production TTL is 30s per design doc §5.1; Clock is injectable
// so cache-expiry tests can advance time without sleeping.
type summaryCache struct {
	mu       sync.Mutex
	cached   *SummaryResponse
	cachedAt time.Time
	ttl      time.Duration
	clock    func() time.Time
}

func newSummaryCache(ttl time.Duration, clock func() time.Time) *summaryCache {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &summaryCache{ttl: ttl, clock: clock}
}

// Get returns the cached response and true if within the TTL window;
// otherwise (nil, false). A nil cached value always returns (nil, false).
func (c *summaryCache) Get() (*SummaryResponse, bool) {
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
func (c *summaryCache) Set(r *SummaryResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = r
	c.cachedAt = c.clock()
}

// --- handler ------------------------------------------------------------

// DefaultSummaryCacheTTL is the production cache TTL per design doc §5.1.
const DefaultSummaryCacheTTL = 30 * time.Second

// recentRecommendationsLimit caps the recent_recommendations slice
// returned to the dashboard (design doc §4).
const recentRecommendationsLimit = 10

// summaryAggregationTimeout caps the per-request aggregation walk so a
// slow store doesn't block the dashboard forever.
const summaryAggregationTimeout = 10 * time.Second

// DiscoverySummaryHandlers serves the unified Discovery dashboard
// aggregation endpoint. Each store reference is OPTIONAL — a nil store
// maps to providers.<provider>.enabled=false so a deployment that
// wired only two clouds renders the other two as connect-state cards.
// auditQuery nil disables scan-completed projection + the recent
// recommendations table; auditService nil disables the cache-miss
// emit. The handler is read-only — nothing it does mutates any store.
type DiscoverySummaryHandlers struct {
	awsStore     AWSSummaryStore
	gcpStore     GCPSummaryStore
	azureStore   AzureSummaryStore
	ociStore     OCISummaryStore
	auditService services.AuditService
	auditQuery   AuditQueryStore
	cache        *summaryCache
	logger       *zap.Logger
}

// NewDiscoverySummaryHandlers builds the handler. ttl <= 0 falls
// through to DefaultSummaryCacheTTL; a nil clock falls through to
// time.Now (production posture). Any store may be nil to express
// "this provider isn't wired in this deployment" — the handler still
// returns 200 with the corresponding card in the enabled=false state.
func NewDiscoverySummaryHandlers(
	aws AWSSummaryStore,
	gcp GCPSummaryStore,
	azure AzureSummaryStore,
	oci OCISummaryStore,
	auditService services.AuditService,
	auditQuery AuditQueryStore,
	ttl time.Duration,
	clock func() time.Time,
	logger *zap.Logger,
) *DiscoverySummaryHandlers {
	if ttl <= 0 {
		ttl = DefaultSummaryCacheTTL
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DiscoverySummaryHandlers{
		awsStore:     aws,
		gcpStore:     gcp,
		azureStore:   azure,
		ociStore:     oci,
		auditService: auditService,
		auditQuery:   auditQuery,
		cache:        newSummaryCache(ttl, clock),
		logger:       logger,
	}
}

// HandleSummary serves GET /api/v1/discovery/summary.
//
// Cache hit: return the cached response immediately (no audit emit).
// Cache miss: walk each enabled provider in parallel, project audit
// rows, compose the response, cache it, and emit one
// discovery.summary.requested audit row. Per design doc §7 contract
// item 5 the route is mounted under the existing auth middleware.
func (h *DiscoverySummaryHandlers) HandleSummary(c *gin.Context) {
	if cached, ok := h.cache.Get(); ok {
		c.JSON(http.StatusOK, cached)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), summaryAggregationTimeout)
	defer cancel()

	resp := h.aggregate(ctx)

	if h.auditService != nil {
		_ = h.auditService.Record(ctx, services.AuditEntry{
			Actor:     "system",
			EventType: services.AuditEventDiscoverySummaryRequested,
			Action:    "requested",
			Payload: map[string]any{
				"providers_enabled":      enabledProviderList(resp),
				"total_connection_count": resp.Totals.ConnectionCount,
				"total_instance_count":   resp.Totals.InstanceCount,
				"coverage_pct":           resp.Totals.CoveragePct,
				"recorded_at":            time.Now().UTC(),
			},
		})
	}

	h.cache.Set(resp)
	c.JSON(http.StatusOK, resp)
}

// aggregate walks each provider's store + the audit query store in
// parallel. One provider's failure does not sink the others — it
// surfaces as a card with zero counts plus a logged warning. The map
// is pre-populated with all four provider keys so the response shape
// is stable across deployments.
func (h *DiscoverySummaryHandlers) aggregate(ctx context.Context) *SummaryResponse {
	resp := &SummaryResponse{
		Providers: map[string]ProviderSummary{
			"aws":   {Enabled: h.awsStore != nil},
			"gcp":   {Enabled: h.gcpStore != nil},
			"azure": {Enabled: h.azureStore != nil},
			"oci":   {Enabled: h.ociStore != nil},
		},
		RecentRecommendations: []RecentRecommendation{},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	assign := func(name string, s ProviderSummary) {
		mu.Lock()
		s.Enabled = true
		resp.Providers[name] = s
		mu.Unlock()
	}

	if h.awsStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assign("aws", h.aggregateProvider(ctx, "aws", func(c context.Context) ([]string, error) {
				return h.awsStore.ListAWSAccountIDs(c)
			}))
		}()
	}
	if h.gcpStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assign("gcp", h.aggregateProvider(ctx, "gcp", func(c context.Context) ([]string, error) {
				return h.gcpStore.ListGCPConnectionIDs(c)
			}))
		}()
	}
	if h.azureStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assign("azure", h.aggregateProvider(ctx, "azure", func(c context.Context) ([]string, error) {
				return h.azureStore.ListAzureConnectionIDs(c)
			}))
		}()
	}
	if h.ociStore != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assign("oci", h.aggregateProvider(ctx, "oci", func(c context.Context) ([]string, error) {
				return h.ociStore.ListOCIConnectionIDs(c)
			}))
		}()
	}
	wg.Wait()

	for _, p := range resp.Providers {
		resp.Totals.ConnectionCount += p.ConnectionCount
		resp.Totals.InstanceCount += p.InstanceCount
		resp.Totals.InstrumentedCount += p.InstrumentedCount
		resp.Totals.UninstrumentedCount += p.UninstrumentedCount
		resp.Totals.RecommendationCount += p.RecommendationCount
		// Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123).
		resp.Totals.ServerlessCount += p.ServerlessCount
		// Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129).
		resp.Totals.OrchestrationCount += p.OrchestrationCount
		// Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136).
		resp.Totals.EventSourceCount += p.EventSourceCount
	}
	resp.Totals.CoveragePct = computeCoveragePct(resp.Totals.InstrumentedCount, resp.Totals.InstanceCount)

	if h.auditQuery != nil {
		events, err := h.auditQuery.ListRecentDiscoveryProposals(ctx, recentRecommendationsLimit)
		if err != nil {
			h.logger.Warn("discovery summary: list recent proposals failed", zap.Error(err))
		} else {
			recs := make([]RecentRecommendation, 0, len(events))
			for _, e := range events {
				recs = append(recs, RecentRecommendation{
					Provider:    e.Provider,
					Kind:        e.Kind,
					ResourceID:  e.ResourceID,
					ScopeID:     e.ScopeID,
					Region:      e.Region,
					GeneratedAt: e.GeneratedAt,
				})
			}
			sort.SliceStable(recs, func(i, j int) bool {
				return recs[i].GeneratedAt.After(recs[j].GeneratedAt)
			})
			if len(recs) > recentRecommendationsLimit {
				recs = recs[:recentRecommendationsLimit]
			}
			resp.RecentRecommendations = recs
		}
	}

	return resp
}

// aggregateProvider pairs the store's connection-ID list with the
// audit query store's scan_completed projection. Slice 1 uses
// uninstrumented_count as the recommendation_count proxy per design
// doc §5 (no recommendation rows persisted in slice 1).
func (h *DiscoverySummaryHandlers) aggregateProvider(
	ctx context.Context,
	provider string,
	listConns func(context.Context) ([]string, error),
) ProviderSummary {
	ids, err := listConns(ctx)
	if err != nil {
		h.logger.Warn("discovery summary: provider list failed",
			zap.String("provider", provider), zap.Error(err))
		return ProviderSummary{Enabled: true}
	}
	ps := ProviderSummary{ConnectionCount: len(ids), Enabled: true}

	if h.auditQuery == nil {
		return ps
	}

	scans, err := h.auditQuery.ListRecentScanCompletedByProvider(ctx, provider)
	if err != nil {
		h.logger.Warn("discovery summary: list scan_completed failed",
			zap.String("provider", provider), zap.Error(err))
		return ps
	}

	var latest *time.Time
	for _, id := range ids {
		s, ok := scans[id]
		if !ok {
			continue
		}
		ps.InstanceCount += s.InstanceCount
		ps.InstrumentedCount += s.InstrumentedCount
		ps.UninstrumentedCount += s.UninstrumentedCount
		// Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) —
		// roll up the optional per-scan serverless_count projection.
		ps.ServerlessCount += s.ServerlessCount
		// Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129)
		// — roll up the optional per-scan orchestration_count
		// projection. OCI rows always carry zero in slice 1 because the
		// OCI scanner returns no orchestrations.
		ps.OrchestrationCount += s.OrchestrationCount
		// Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136)
		// — roll up the optional per-scan event_source_count projection.
		// All four providers populate (including OCI) since OCI Streaming
		// ships in slice 1.
		ps.EventSourceCount += s.EventSourceCount
		if latest == nil || s.CompletedAt.After(*latest) {
			t := s.CompletedAt
			latest = &t
		}
	}
	ps.RecommendationCount = ps.UninstrumentedCount
	ps.LastScanAt = latest
	return ps
}

// computeCoveragePct returns instrumented / instance * 100 rounded to
// one decimal. Zero-safe: returns 0 when instance is 0.
func computeCoveragePct(instrumented, instance int) float64 {
	if instance <= 0 {
		return 0
	}
	pct := float64(instrumented) / float64(instance) * 100.0
	return math.Round(pct*10) / 10
}

// enabledProviderList returns the four-provider key set whose Enabled
// flag is true, sorted for stable audit payloads.
func enabledProviderList(r *SummaryResponse) []string {
	keys := make([]string, 0, len(r.Providers))
	for k, v := range r.Providers {
		if v.Enabled {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
