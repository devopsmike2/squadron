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

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, az, oci, idx, inv, nil, time.Second, nil, nil)
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

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, az, nil, idx, inv, nil, time.Second, nil, nil)
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

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, spy, 30*time.Second, clock, nil)

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

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, spy, 30*time.Second, clock, nil)

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

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, spy, 30*time.Second, func() time.Time { return now }, nil)

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

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, az, oci, idx, inv, nil, time.Second, nil, nil)
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

	h := NewDiscoveryTraceCoverageHandlers(aws, gcp, nil, nil, idx, inv, nil, time.Second, nil, nil)
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

	h := NewDiscoveryTraceCoverageHandlers(nil, nil, nil, nil, nil, nil, nil, time.Second, nil, nil)
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

	h := NewDiscoveryTraceCoverageHandlers(aws, nil, nil, nil, idx, inv, nil, time.Second, nil, nil)
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
