// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// --- stubs --------------------------------------------------------------
//
// The AWS / GCP / Azure / OCI store stubs reuse the type names from
// discovery_summary_test.go via the package's single test compile unit
// — the stubs already in that file (stubAWSStore / stubGCPStore /
// stubAzureStore / stubOCIStore / spyAuditService) satisfy this file's
// needs too. The two trace-coverage-specific stubs below are the
// TraceIndex + InventoryCountQuery surfaces this chunk introduces.

// stubTraceIndex returns canned Summary values per (provider, scope)
// pair. Tracks calls so cache-TTL tests can assert "second hit
// short-circuited the index walk."
type stubTraceIndex struct {
	mu        sync.Mutex
	summaries map[string]traceindex.Summary // key: provider + "|" + scope
	err       error
	calls     int64
}

func (s *stubTraceIndex) Coverage(_ context.Context, provider, scopeID string, inventoryCount int) (traceindex.Summary, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return traceindex.Summary{}, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.summaries[provider+"|"+scopeID]; ok {
		v.Provider = provider
		v.ScopeID = scopeID
		v.InventoryCount = inventoryCount
		return v, nil
	}
	return traceindex.Summary{
		Provider:       provider,
		ScopeID:        scopeID,
		InventoryCount: inventoryCount,
	}, nil
}

// stubInventoryQuery returns canned inventory counts per (provider,
// scope) pair. Tracks calls so cache-TTL tests can assert "second hit
// short-circuited."
type stubInventoryQuery struct {
	mu     sync.Mutex
	counts map[string]int // key: provider + "|" + scope
	err    error
	calls  int64
}

// stubPendingQuery — v0.89.82 (#713 Stream 111) — returns canned
// "primitive_enabled but no recent emission" counts per (provider,
// scope) pair. Mirrors stubInventoryQuery's shape so the slice-2
// chunk-3 tests stay structurally adjacent to the slice-1 tests.
//
// IMPORTANT: the production InventoryStore performs the actual
// primitive_enabled + last_seen_at axis logic; this stub just returns
// pre-decided counts. The handler-level tests below verify the
// summing + wiring path, not the axis logic itself (which lives in
// the inventory store and is covered by its own tests in a later
// chunk).
type stubPendingQuery struct {
	mu     sync.Mutex
	counts map[string]int // key: provider + "|" + scope
	err    error
	calls  int64
}

func (s *stubPendingQuery) PendingTraceEmissionCount(_ context.Context, provider, scopeID string, _ time.Duration) (int, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return 0, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.counts[provider+"|"+scopeID]; ok {
		return v, nil
	}
	return 0, nil
}

func (s *stubInventoryQuery) InventoryCountForScope(_ context.Context, provider, scopeID string) (int, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return 0, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.counts[provider+"|"+scopeID]; ok {
		return v, nil
	}
	return 0, nil
}

// --- helpers ------------------------------------------------------------

func traceCoverageDoRequest(h *DiscoveryTraceCoverageHandlers) *httptest.ResponseRecorder {
	r := gin.New()
	r.GET("/api/v1/discovery/trace_coverage", h.HandleTraceCoverage)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/discovery/trace_coverage", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func parseTraceCoverage(t *testing.T, w *httptest.ResponseRecorder) TraceCoverageResponse {
	t.Helper()
	var r TraceCoverageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, w.Body.String())
	}
	return r
}

// --- 1. AggregatesAllFourProviders --------------------------------------

func TestTraceCoverage_AggregatesAllFourProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	gcp := &stubGCPStore{ids: []string{"gcp-conn-1"}}
	az := &stubAzureStore{ids: []string{"az-conn-1"}}
	oci := &stubOCIStore{ids: []string{"oci-conn-1"}}

	// Each scope: inventory=10, emitting=5, coverage=50%.
	inv := &stubInventoryQuery{counts: map[string]int{
		"aws|111122223333": 10,
		"gcp|gcp-conn-1":   10,
		"azure|az-conn-1":  10,
		"oci|oci-conn-1":   10,
	}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|111122223333": {EmittingCount: 5, CoveragePct: 50, StrongMatchPct: 100},
		"gcp|gcp-conn-1":   {EmittingCount: 5, CoveragePct: 50, StrongMatchPct: 100},
		"azure|az-conn-1":  {EmittingCount: 5, CoveragePct: 50, StrongMatchPct: 100},
		"oci|oci-conn-1":   {EmittingCount: 5, CoveragePct: 50, StrongMatchPct: 100},
	}}

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, az, oci, idx, inv, nil /*pending*/, nil /*audit*/, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	r := parseTraceCoverage(t, w)

	if got, want := r.Totals.InventoryCount, 40; got != want {
		t.Errorf("totals.inventory_count = %d, want %d", got, want)
	}
	if got, want := r.Totals.EmittingCount, 20; got != want {
		t.Errorf("totals.emitting_count = %d, want %d", got, want)
	}
	if got, want := r.Totals.CoveragePct, 50.0; got != want {
		t.Errorf("totals.coverage_pct = %v, want %v", got, want)
	}
	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		if r.Providers[p].InventoryCount != 10 {
			t.Errorf("%s.inventory_count = %d, want 10", p, r.Providers[p].InventoryCount)
		}
		if r.Providers[p].EmittingCount != 5 {
			t.Errorf("%s.emitting_count = %d, want 5", p, r.Providers[p].EmittingCount)
		}
		if r.Providers[p].CoveragePct != 50 {
			t.Errorf("%s.coverage_pct = %v, want 50", p, r.Providers[p].CoveragePct)
		}
	}
}

// --- 2. DisabledProvider_ReturnsZero ------------------------------------

func TestTraceCoverage_DisabledProvider_ReturnsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	gcp := &stubGCPStore{ids: []string{"gcp-1"}}
	az := &stubAzureStore{ids: []string{"az-1"}}
	// oci nil — not wired.

	inv := &stubInventoryQuery{counts: map[string]int{
		"aws|acct-1": 10, "gcp|gcp-1": 10, "azure|az-1": 10,
	}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
		"gcp|gcp-1":  {EmittingCount: 5},
		"azure|az-1": {EmittingCount: 5},
	}}

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, az, nil, idx, inv, nil /*pending*/, nil /*audit*/, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	r := parseTraceCoverage(t, w)

	if r.Providers["oci"].InventoryCount != 0 || r.Providers["oci"].EmittingCount != 0 {
		t.Errorf("oci should be zero counts; got %+v", r.Providers["oci"])
	}
	if r.Providers["oci"].CoveragePct != 0 {
		t.Errorf("oci.coverage_pct = %v, want 0", r.Providers["oci"].CoveragePct)
	}
	if got, want := r.Totals.InventoryCount, 30; got != want {
		t.Errorf("totals.inventory_count = %d, want %d (oci excluded)", got, want)
	}
}

// --- 3. CacheTTLBehavior ------------------------------------------------

func TestTraceCoverage_CacheTTLBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 4}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 2},
	}}
	spy := &spyAuditService{}
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil /*pending*/, spy, 30*time.Second, clock, nil)

	w1 := traceCoverageDoRequest(h)
	w2 := traceCoverageDoRequest(h)
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("status w1=%d w2=%d", w1.Code, w2.Code)
	}
	if w1.Body.String() != w2.Body.String() {
		t.Errorf("cache hit should return identical body; w1=%s w2=%s", w1.Body.String(), w2.Body.String())
	}
	if got := spy.count(); got != 1 {
		t.Errorf("audit emissions = %d, want 1 (cache hit must not emit)", got)
	}
	if got := atomic.LoadInt64(&idx.calls); got != 1 {
		t.Errorf("trace index calls = %d, want 1 (second hit cached)", got)
	}
}

// --- 4. CacheExpires_AfterTTL -------------------------------------------

func TestTraceCoverage_CacheExpires_AfterTTL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 4}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 2},
	}}
	spy := &spyAuditService{}
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(&now)

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil /*pending*/, spy, 30*time.Second, clock, nil)

	if w := traceCoverageDoRequest(h); w.Code != http.StatusOK {
		t.Fatalf("first call status = %d", w.Code)
	}
	if got := atomic.LoadInt64(&idx.calls); got != 1 {
		t.Errorf("after first call: trace index calls = %d, want 1", got)
	}

	now = now.Add(31 * time.Second)
	if w := traceCoverageDoRequest(h); w.Code != http.StatusOK {
		t.Fatalf("second call status = %d", w.Code)
	}
	if got := atomic.LoadInt64(&idx.calls); got != 2 {
		t.Errorf("after TTL expire: trace index calls = %d, want 2", got)
	}
	if got := spy.count(); got != 2 {
		t.Errorf("audit emissions = %d, want 2 (two cache misses)", got)
	}
}

// --- 5. EmitsAuditOnCacheMiss -------------------------------------------

func TestTraceCoverage_EmitsAuditOnCacheMiss(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 4}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 2},
	}}
	spy := &spyAuditService{}
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil /*pending*/, spy, 30*time.Second, func() time.Time { return now }, nil)

	if w := traceCoverageDoRequest(h); w.Code != http.StatusOK {
		t.Fatalf("miss call status = %d", w.Code)
	}
	if spy.count() != 1 {
		t.Errorf("after miss: emissions = %d, want 1", spy.count())
	}
	e := spy.entries[0]
	if e.EventType != services.AuditEventTraceCoverageRequested {
		t.Errorf("event_type = %q, want %q", e.EventType, services.AuditEventTraceCoverageRequested)
	}
	if e.Actor != "system" {
		t.Errorf("actor = %q, want system", e.Actor)
	}
	if e.Action != "requested" {
		t.Errorf("action = %q, want requested", e.Action)
	}
	if e.Payload["cache_status"] != "miss" {
		t.Errorf("payload.cache_status = %v, want miss", e.Payload["cache_status"])
	}
	// Second call is a cache hit — no audit emit.
	if w := traceCoverageDoRequest(h); w.Code != http.StatusOK {
		t.Fatalf("hit call status = %d", w.Code)
	}
	if spy.count() != 1 {
		t.Errorf("after hit: emissions = %d, want 1 (cache hit must not emit)", spy.count())
	}
}

// --- 6. CoveragePctZeroSafe ---------------------------------------------

func TestTraceCoverage_CoveragePctZeroSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Connections exist but inventory_count=0 for every scope (no scan yet).
	aws := &stubAWSStore{ids: []string{"acct-1"}}
	gcp := &stubGCPStore{ids: []string{"gcp-1"}}
	az := &stubAzureStore{ids: []string{"az-1"}}
	oci := &stubOCIStore{ids: []string{"oci-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{}} // empty
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{}}

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, az, oci, idx, inv, nil /*pending*/, nil /*audit*/, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)

	if r.Totals.CoveragePct != 0 {
		t.Errorf("totals.coverage_pct = %v, want 0", r.Totals.CoveragePct)
	}
	if math.IsNaN(r.Totals.CoveragePct) || math.IsInf(r.Totals.CoveragePct, 0) {
		t.Errorf("totals.coverage_pct is NaN/Inf: %v", r.Totals.CoveragePct)
	}
	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		if r.Providers[p].CoveragePct != 0 {
			t.Errorf("%s.coverage_pct = %v, want 0", p, r.Providers[p].CoveragePct)
		}
		if math.IsNaN(r.Providers[p].CoveragePct) {
			t.Errorf("%s.coverage_pct NaN", p)
		}
	}
}

// --- 7. StrongAndWeakMatchPctsAggregate ---------------------------------

func TestTraceCoverage_StrongAndWeakMatchPctsAggregate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// One connection per provider with distinct strong / weak splits.
	// Each provider's single-scope split should surface unchanged
	// (single-scope path: weighted average == the only sample).
	aws := &stubAWSStore{ids: []string{"acct-1"}}
	gcp := &stubGCPStore{ids: []string{"gcp-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10, "gcp|gcp-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5, StrongMatchPct: 88.0, WeakMatchPct: 12.0},
		"gcp|gcp-1":  {EmittingCount: 5, StrongMatchPct: 50.0, WeakMatchPct: 50.0},
	}}

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, nil, nil, idx, inv, nil /*pending*/, nil /*audit*/, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)

	if got, want := r.Providers["aws"].StrongMatchPct, 88.0; got != want {
		t.Errorf("aws.strong_match_pct = %v, want %v", got, want)
	}
	if got, want := r.Providers["aws"].WeakMatchPct, 12.0; got != want {
		t.Errorf("aws.weak_match_pct = %v, want %v", got, want)
	}
	if got, want := r.Providers["gcp"].StrongMatchPct, 50.0; got != want {
		t.Errorf("gcp.strong_match_pct = %v, want %v", got, want)
	}
	if got, want := r.Providers["gcp"].WeakMatchPct, 50.0; got != want {
		t.Errorf("gcp.weak_match_pct = %v, want %v", got, want)
	}
}

// --- 8. AllStoresNil_ReturnsEmpty ---------------------------------------

func TestTraceCoverage_AllStoresNil_ReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewDiscoveryTraceCoverageHandlers(nil, nil, nil, nil, nil, nil, nil /*pending*/, nil /*audit*/, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		if r.Providers[p].InventoryCount != 0 || r.Providers[p].EmittingCount != 0 {
			t.Errorf("%s should be all-zero; got %+v", p, r.Providers[p])
		}
	}
	if r.Totals.InventoryCount != 0 || r.Totals.EmittingCount != 0 {
		t.Errorf("totals should be zero; got %+v", r.Totals)
	}
}

// --- 9. LastIndexUpdateAt_SurfacedFromMaxScope --------------------------

func TestTraceCoverage_LastIndexUpdateAt_SurfacedFromMaxScope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1", "acct-2"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10, "aws|acct-2": 10}}
	earlier := time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC)
	later := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5, LastIndexUpdateAt: earlier},
		"aws|acct-2": {EmittingCount: 5, LastIndexUpdateAt: later},
	}}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil /*pending*/, nil /*audit*/, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)

	if r.Providers["aws"].LastIndexUpdateAt == nil {
		t.Fatalf("aws.last_index_update_at should be set")
	}
	if !r.Providers["aws"].LastIndexUpdateAt.Equal(later) {
		t.Errorf("aws.last_index_update_at = %v, want %v (max across scopes)",
			r.Providers["aws"].LastIndexUpdateAt, later)
	}
}

// --- 10. PendingTraceEmissionCount_IncludesUnemittingInstrumented ------
//
// v0.89.82 (#713 Stream 111, Trace integration slice 2 chunk 3). The
// stub returns the count the production InventoryStore would project
// for the slice-2 "primitive_enabled AND last_seen_at null-or-stale"
// rule; the handler test just verifies the wiring + per-provider
// sum. See docs/proposals/trace-integration-slice2.md §3.

func TestTraceCoverage_PendingTraceEmissionCount_IncludesUnemittingInstrumented(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}
	// Stub returns 3 — the production projection would have filtered
	// inventory rows to primitive_enabled=true AND last_seen_at older
	// than 24h (or null). The handler just sums.
	pending := &stubPendingQuery{counts: map[string]int{"aws|acct-1": 3}}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, pending, nil, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got, want := r.Providers["aws"].PendingTraceEmissionCount, 3; got != want {
		t.Errorf("aws.pending_trace_emission_count = %d, want %d", got, want)
	}
}

// --- 11. PendingTraceEmissionCount_ExcludesPrimitiveDisabledRows -------
//
// Stub semantics: a row with primitive_enabled=false is never counted
// by the production projection, so the stub returns 0. This test pins
// that the handler doesn't somehow inject a non-zero pending count
// when the underlying query returns 0.

func TestTraceCoverage_PendingTraceEmissionCount_ExcludesPrimitiveDisabledRows(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 10},
	}}
	// Empty counts map → stub returns 0 for every (provider, scope).
	pending := &stubPendingQuery{counts: map[string]int{}}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, pending, nil, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got, want := r.Providers["aws"].PendingTraceEmissionCount, 0; got != want {
		t.Errorf("aws.pending_trace_emission_count = %d, want %d (no primitive_enabled rows)", got, want)
	}
}

// --- 12. PendingTraceEmissionCount_ExcludesRecentEmission --------------
//
// Mirrors the previous test: "recent emission" filtering is the
// production projection's responsibility (not the handler's). The
// stub abstracts the axis logic; here we just confirm that when the
// query reports 0 — because every primitive_enabled row has a recent
// last_seen_at — the handler surfaces 0.

func TestTraceCoverage_PendingTraceEmissionCount_ExcludesRecentEmission(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 10},
	}}
	// All primitive_enabled rows have last_seen_at within 24h → stub
	// returns 0. Recent-emission filtering is the production
	// InventoryStore's responsibility, not the handler's.
	pending := &stubPendingQuery{counts: map[string]int{"aws|acct-1": 0}}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, pending, nil, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got, want := r.Providers["aws"].PendingTraceEmissionCount, 0; got != want {
		t.Errorf("aws.pending_trace_emission_count = %d, want %d (every row recent)", got, want)
	}
}

// --- 13. PendingTraceEmissionCount_AcrossProvidersAggregates ----------
//
// All four providers wired with one scope each, distinct pending
// counts. Per-provider PendingTraceEmissionCount surfaces unchanged
// for the single-scope path; the cross-provider sum is the operator-
// visible number the dashboard sub-indicator renders.

func TestTraceCoverage_PendingTraceEmissionCount_AcrossProvidersAggregates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	gcp := &stubGCPStore{ids: []string{"gcp-1"}}
	az := &stubAzureStore{ids: []string{"az-1"}}
	oci := &stubOCIStore{ids: []string{"oci-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{
		"aws|acct-1": 10, "gcp|gcp-1": 10, "azure|az-1": 10, "oci|oci-1": 10,
	}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
		"gcp|gcp-1":  {EmittingCount: 5},
		"azure|az-1": {EmittingCount: 5},
		"oci|oci-1":  {EmittingCount: 5},
	}}
	pending := &stubPendingQuery{counts: map[string]int{
		"aws|acct-1": 2,
		"gcp|gcp-1":  4,
		"azure|az-1": 1,
		"oci|oci-1":  3,
	}}

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, az, oci, idx, inv, pending, nil, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got, want := r.Providers["aws"].PendingTraceEmissionCount, 2; got != want {
		t.Errorf("aws.pending = %d, want %d", got, want)
	}
	if got, want := r.Providers["gcp"].PendingTraceEmissionCount, 4; got != want {
		t.Errorf("gcp.pending = %d, want %d", got, want)
	}
	if got, want := r.Providers["azure"].PendingTraceEmissionCount, 1; got != want {
		t.Errorf("azure.pending = %d, want %d", got, want)
	}
	if got, want := r.Providers["oci"].PendingTraceEmissionCount, 3; got != want {
		t.Errorf("oci.pending = %d, want %d", got, want)
	}
	// Cross-provider sum — what the dashboard sub-indicator renders.
	sum := r.Providers["aws"].PendingTraceEmissionCount +
		r.Providers["gcp"].PendingTraceEmissionCount +
		r.Providers["azure"].PendingTraceEmissionCount +
		r.Providers["oci"].PendingTraceEmissionCount
	if got, want := sum, 10; got != want {
		t.Errorf("cross-provider pending sum = %d, want %d", got, want)
	}
}

// --- Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) ----
//
// stubServerlessQuery returns canned (inventory, emitting) counts per
// (provider, scope) pair. Mirrors stubPendingQuery's shape; the
// production query performs the actual serverless_instance table walk
// and the per-row last_seen_at < 24h check.

type stubServerlessQuery struct {
	mu        sync.Mutex
	inventory map[string]int // key: provider + "|" + scope
	emitting  map[string]int // key: provider + "|" + scope
	err       error
	calls     int64
}

func (s *stubServerlessQuery) ServerlessCoverageForScope(_ context.Context, provider, scopeID string, _ time.Duration) (inv, emit int, err error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return 0, 0, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + "|" + scopeID
	return s.inventory[key], s.emitting[key], nil
}

// TestTraceCoverage_IncludesServerlessPct — §11 acceptance test 12.
// Seeding the serverless coverage query with 2 inventory + 1 emitting
// for one scope must surface ServerlessPct=50.0 on the matching
// ProviderTraceCoverage.
func TestTraceCoverage_IncludesServerlessPct(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}
	srv := &stubServerlessQuery{
		inventory: map[string]int{"aws|acct-1": 2},
		emitting:  map[string]int{"aws|acct-1": 1},
	}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil).
		WithServerlessQuery(srv)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got, want := r.Providers["aws"].ServerlessPct, 50.0; got != want {
		t.Errorf("aws.serverless_pct = %v, want %v", got, want)
	}
	// Other providers (with no serverless query data for those scopes,
	// AND those provider stores nil) stay at 0 — cold-start posture.
	if got := r.Providers["gcp"].ServerlessPct; got != 0 {
		t.Errorf("gcp.serverless_pct = %v, want 0", got)
	}
}

// --- Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129) -
//
// stubOrchestrationQuery mirrors stubServerlessQuery. The production
// query performs the actual orchestration_instance table walk and the
// per-row last_seen_at < 24h check.

type stubOrchestrationQuery struct {
	mu        sync.Mutex
	inventory map[string]int
	emitting  map[string]int
	err       error
	calls     int64
}

func (s *stubOrchestrationQuery) OrchestrationCoverageForScope(_ context.Context, provider, scopeID string, _ time.Duration) (inv, emit int, err error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return 0, 0, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + "|" + scopeID
	return s.inventory[key], s.emitting[key], nil
}

// TestTraceCoverage_IncludesOrchestrationPct — §11 acceptance test
// 12 for the orchestration tier: seeding the orchestration coverage
// query with 2 inventory + 1 emitting for one scope must surface
// OrchestrationPct=50.0 on the matching ProviderTraceCoverage.
func TestTraceCoverage_IncludesOrchestrationPct(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}
	orch := &stubOrchestrationQuery{
		inventory: map[string]int{"aws|acct-1": 2},
		emitting:  map[string]int{"aws|acct-1": 1},
	}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil).
		WithOrchestrationQuery(orch)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got, want := r.Providers["aws"].OrchestrationPct, 50.0; got != want {
		t.Errorf("aws.orchestration_pct = %v, want %v", got, want)
	}
	if got := r.Providers["gcp"].OrchestrationPct; got != 0 {
		t.Errorf("gcp.orchestration_pct = %v, want 0", got)
	}
}

// TestTraceCoverage_OCIOrchestrationPctIsZero — slice 1 contract:
// OCI orchestration coverage is deferred to slice 2 and the OCI
// substrate is expected to return (0, 0) from its
// OrchestrationCoverageForScope. The dashboard chip stays at 0 for
// OCI through slice 1.
func TestTraceCoverage_OCIOrchestrationPctIsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oci := &stubOCIStore{ids: []string{"oci-scope-1"}}
	orch := &stubOrchestrationQuery{
		inventory: map[string]int{}, // empty — OCI returns no rows
		emitting:  map[string]int{},
	}
	h := NewDiscoveryTraceCoverageHandlers(nil, nil, nil, oci, nil, nil, nil, nil, time.Second, nil, nil).
		WithOrchestrationQuery(orch)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got := r.Providers["oci"].OrchestrationPct; got != 0 {
		t.Errorf("oci.orchestration_pct = %v, want 0 (slice 1 contract — OCI deferred)", got)
	}
}

// TestTraceCoverage_ServerlessPct_ZeroOnColdStart — when the
// ServerlessCoverageQuery is nil (deployments that haven't wired the
// new substrate), every provider's ServerlessPct stays 0 and the
// response shape carries the field unconditionally. Pins the
// nil-tolerant posture documented on the WithServerlessQuery setter.
func TestTraceCoverage_ServerlessPct_ZeroOnColdStart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}

	// No WithServerlessQuery call — nil serverlessQuery on the handler.
	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got := r.Providers["aws"].ServerlessPct; got != 0 {
		t.Errorf("aws.serverless_pct = %v, want 0 (no serverless query wired)", got)
	}
}

// --- Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136) -
//
// stubEventSourceQuery mirrors stubOrchestrationQuery. The production
// query performs the actual event_source_instance table walk and the
// per-row last_seen_at < 24h check.

type stubEventSourceQuery struct {
	mu        sync.Mutex
	inventory map[string]int
	emitting  map[string]int
	err       error
	calls     int64
}

func (s *stubEventSourceQuery) EventSourceCoverageForScope(_ context.Context, provider, scopeID string, _ time.Duration) (inv, emit int, err error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return 0, 0, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + "|" + scopeID
	return s.inventory[key], s.emitting[key], nil
}

// TestTraceCoverage_IncludesEventSourcePct — §11 acceptance test 14
// for the event source tier: seeding the event source coverage query
// with 2 inventory + 1 emitting for one scope must surface
// EventSourcePct=50.0 on the matching ProviderTraceCoverage. All four
// providers populate (including OCI) since OCI Streaming ships in
// slice 1.
func TestTraceCoverage_IncludesEventSourcePct(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}
	evt := &stubEventSourceQuery{
		inventory: map[string]int{"aws|acct-1": 2},
		emitting:  map[string]int{"aws|acct-1": 1},
	}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil).
		WithEventSourceQuery(evt)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got, want := r.Providers["aws"].EventSourcePct, 50.0; got != want {
		t.Errorf("aws.event_source_pct = %v, want %v", got, want)
	}
	// Other providers contribute zero — no scopes seeded.
	if got := r.Providers["gcp"].EventSourcePct; got != 0 {
		t.Errorf("gcp.event_source_pct = %v, want 0", got)
	}
}

// TestTraceCoverage_EventSourcePct_ZeroOnColdStart — when the
// EventSourceCoverageQuery is nil, every provider's EventSourcePct
// stays 0. Pins the nil-tolerant posture documented on
// WithEventSourceQuery.
func TestTraceCoverage_EventSourcePct_ZeroOnColdStart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}

	// No WithEventSourceQuery call — nil eventSourceQuery on the handler.
	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got := r.Providers["aws"].EventSourcePct; got != 0 {
		t.Errorf("aws.event_source_pct = %v, want 0 (no event source query wired)", got)
	}
}

// --- Event source tier slice 2 chunk 1 (v0.89.105, #741 Stream 139) -
//
// stubPropagationQuery mirrors stubEventSourceQuery. The production
// query walks the event_source_instance table and projects the
// has_propagation_config bool out of each row's snapshot_json blob.

type stubPropagationQuery struct {
	mu          sync.Mutex
	inventory   map[string]int
	propagating map[string]int
	err         error
	calls       int64
}

func (s *stubPropagationQuery) EventSourcePropagationForScope(_ context.Context, provider, scopeID string) (inv, prop int, err error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return 0, 0, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := provider + "|" + scopeID
	return s.inventory[key], s.propagating[key], nil
}

// TestTraceCoverage_IncludesPropagationPct — §11 acceptance test 15.
// Seeding the propagation coverage query with 4 inventory + 1
// propagating for one scope must surface PropagationPct=25.0 on the
// matching ProviderTraceCoverage.
func TestTraceCoverage_IncludesPropagationPct(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}
	prop := &stubPropagationQuery{
		inventory:   map[string]int{"aws|acct-1": 4},
		propagating: map[string]int{"aws|acct-1": 1},
	}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil).
		WithEventSourcePropagationQuery(prop)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got, want := r.Providers["aws"].PropagationPct, 25.0; got != want {
		t.Errorf("aws.propagation_pct = %v, want %v", got, want)
	}
	// Other providers contribute zero — no scopes seeded.
	if got := r.Providers["gcp"].PropagationPct; got != 0 {
		t.Errorf("gcp.propagation_pct = %v, want 0", got)
	}
}

// TestTraceCoverage_PropagationPctIsZeroWhenNoEventSources — when the
// query returns zero inventory, PropagationPct stays at 0 (not NaN).
// Pins the zero-safe shape mirroring CoveragePctZeroSafe so the
// dashboard never renders NaN% for a fresh deployment.
func TestTraceCoverage_PropagationPctIsZeroWhenNoEventSources(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}
	// Inventory=0, propagating=0 — cold-start posture.
	prop := &stubPropagationQuery{
		inventory:   map[string]int{},
		propagating: map[string]int{},
	}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil).
		WithEventSourcePropagationQuery(prop)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	if got := r.Providers["aws"].PropagationPct; got != 0 {
		t.Errorf("aws.propagation_pct = %v, want 0 on cold start", got)
	}
	if math.IsNaN(r.Providers["aws"].PropagationPct) {
		t.Errorf("aws.propagation_pct is NaN — zero-safe contract violated")
	}
}

// TestTraceCoverage_PropagationPctIsZeroWhenQueryNil — same posture as
// TestTraceCoverage_EventSourcePct_ZeroOnColdStart but for the
// propagation query: a deployment that hasn't wired the new query
// renders PropagationPct = 0 across every provider.
func TestTraceCoverage_PropagationPctIsZeroWhenQueryNil(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	inv := &stubInventoryQuery{counts: map[string]int{"aws|acct-1": 10}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 5},
	}}

	// No WithEventSourcePropagationQuery call — nil propagationQuery.
	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		if got := r.Providers[p].PropagationPct; got != 0 {
			t.Errorf("%s.propagation_pct = %v, want 0 (no propagation query wired)", p, got)
		}
	}
}

// TestTraceCoverage_PropagationPctAggregatesPerProvider — multiple
// scopes inside one provider sum their per-scope counts. Two scopes
// each with 5 inventory / 3 propagating roll up to 10 / 6 = 60%.
func TestTraceCoverage_PropagationPctAggregatesPerProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1", "acct-2"}}
	inv := &stubInventoryQuery{counts: map[string]int{
		"aws|acct-1": 20, "aws|acct-2": 20,
	}}
	idx := &stubTraceIndex{summaries: map[string]traceindex.Summary{
		"aws|acct-1": {EmittingCount: 10},
		"aws|acct-2": {EmittingCount: 10},
	}}
	prop := &stubPropagationQuery{
		inventory: map[string]int{
			"aws|acct-1": 5, "aws|acct-2": 5,
		},
		propagating: map[string]int{
			"aws|acct-1": 3, "aws|acct-2": 3,
		},
	}

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, nil, time.Second, nil, nil).
		WithEventSourcePropagationQuery(prop)
	w := traceCoverageDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseTraceCoverage(t, w)
	// Per-provider rollup: (3+3)/(5+5) = 60%.
	if got, want := r.Providers["aws"].PropagationPct, 60.0; got != want {
		t.Errorf("aws.propagation_pct = %v, want %v", got, want)
	}
}

