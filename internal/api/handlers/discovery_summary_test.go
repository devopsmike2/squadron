// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/services"
)

// --- test stubs ---------------------------------------------------------

// stubAWSStore counts ListAWSAccountIDs calls so the cache-expiry test
// can assert the second hit re-queries.
type stubAWSStore struct {
	ids   []string
	err   error
	calls int64
}

func (s *stubAWSStore) ListAWSAccountIDs(_ context.Context) ([]string, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}

type stubGCPStore struct {
	ids   []string
	err   error
	calls int64
}

func (s *stubGCPStore) ListGCPConnectionIDs(_ context.Context) ([]string, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}

type stubAzureStore struct {
	ids   []string
	err   error
	calls int64
}

func (s *stubAzureStore) ListAzureConnectionIDs(_ context.Context) ([]string, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}

type stubOCIStore struct {
	ids   []string
	err   error
	calls int64
}

func (s *stubOCIStore) ListOCIConnectionIDs(_ context.Context) ([]string, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.err != nil {
		return nil, s.err
	}
	return s.ids, nil
}

// stubAuditQuery returns canned scan + proposal results per provider.
type stubAuditQuery struct {
	mu        sync.Mutex
	scans     map[string]map[string]ScanSummary // provider → scope → summary
	proposals []ProposalEvent
	scanErr   error
	propErr   error
	scanCalls int64
	propCalls int64
}

func (s *stubAuditQuery) ListRecentScanCompletedByProvider(_ context.Context, provider string) (map[string]ScanSummary, error) {
	atomic.AddInt64(&s.scanCalls, 1)
	if s.scanErr != nil {
		return nil, s.scanErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.scans[provider]; ok {
		return v, nil
	}
	return map[string]ScanSummary{}, nil
}

func (s *stubAuditQuery) ListRecentDiscoveryProposals(_ context.Context, limit int) ([]ProposalEvent, error) {
	atomic.AddInt64(&s.propCalls, 1)
	if s.propErr != nil {
		return nil, s.propErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit > 0 && len(s.proposals) > limit {
		return append([]ProposalEvent{}, s.proposals[:limit]...), nil
	}
	return append([]ProposalEvent{}, s.proposals...), nil
}

// spyAuditService counts Record calls so the cache tests can assert
// the cache-miss emit fires only once.
type spyAuditService struct {
	mu      sync.Mutex
	entries []services.AuditEntry
}

func (s *spyAuditService) Record(_ context.Context, e services.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}
func (s *spyAuditService) List(_ context.Context, _ services.AuditEventFilter) ([]*services.AuditEvent, error) {
	return nil, nil
}
func (s *spyAuditService) Get(_ context.Context, _ string) (*services.AuditEvent, error) {
	return nil, nil
}
func (s *spyAuditService) SetExplanation(_ context.Context, _, _, _ string, _ time.Time) error {
	return nil
}
func (s *spyAuditService) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// --- helpers ------------------------------------------------------------

// summaryDoRequest exercises the handler end-to-end through a Gin
// router (no middleware) so the test sees the JSON the wire would
// see. The handler is mounted at the unified-dashboard route shape.
func summaryDoRequest(h *DiscoverySummaryHandlers) *httptest.ResponseRecorder {
	r := gin.New()
	r.GET("/api/v1/discovery/summary", h.HandleSummary)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/discovery/summary", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// parseSummary decodes the JSON wire shape so individual tests stay
// focused on the assertion rather than the boilerplate.
func parseSummary(t *testing.T, w *httptest.ResponseRecorder) SummaryResponse {
	t.Helper()
	var r SummaryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, w.Body.String())
	}
	return r
}

// fixedClock returns a clock function bound to *time.Time so tests
// can advance the clock by reassigning the underlying pointer.
func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

// --- 1. AggregatesAllFourProviders --------------------------------------

func TestDiscoverySummary_AggregatesAllFourProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	gcp := &stubGCPStore{ids: []string{"gcp-conn-1"}}
	az := &stubAzureStore{ids: []string{"az-conn-1"}}
	oci := &stubOCIStore{ids: []string{"oci-conn-1"}}

	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	audit := &stubAuditQuery{
		scans: map[string]map[string]ScanSummary{
			"aws": {
				"111122223333": {ScopeID: "111122223333", CompletedAt: now,
					InstanceCount: 10, InstrumentedCount: 6, UninstrumentedCount: 4},
			},
			"gcp": {
				"gcp-conn-1": {ScopeID: "gcp-conn-1", CompletedAt: now,
					InstanceCount: 20, InstrumentedCount: 12, UninstrumentedCount: 8},
			},
			"azure": {
				"az-conn-1": {ScopeID: "az-conn-1", CompletedAt: now,
					InstanceCount: 30, InstrumentedCount: 18, UninstrumentedCount: 12},
			},
			"oci": {
				"oci-conn-1": {ScopeID: "oci-conn-1", CompletedAt: now,
					InstanceCount: 40, InstrumentedCount: 24, UninstrumentedCount: 16},
			},
		},
	}
	h := NewDiscoverySummaryHandlers(aws, gcp, az, oci, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	r := parseSummary(t, w)

	if got, want := r.Totals.InstanceCount, 100; got != want {
		t.Errorf("totals.instance_count = %d, want %d", got, want)
	}
	if got := r.Providers["aws"].InstanceCount; got != 10 {
		t.Errorf("aws.instance_count = %d, want 10", got)
	}
	if got := r.Providers["gcp"].InstanceCount; got != 20 {
		t.Errorf("gcp.instance_count = %d, want 20", got)
	}
	if got := r.Providers["azure"].InstanceCount; got != 30 {
		t.Errorf("azure.instance_count = %d, want 30", got)
	}
	if got := r.Providers["oci"].InstanceCount; got != 40 {
		t.Errorf("oci.instance_count = %d, want 40", got)
	}
	if got := r.Totals.ConnectionCount; got != 4 {
		t.Errorf("totals.connection_count = %d, want 4", got)
	}
	if !r.Providers["aws"].Enabled || !r.Providers["gcp"].Enabled ||
		!r.Providers["azure"].Enabled || !r.Providers["oci"].Enabled {
		t.Errorf("all four providers should be enabled; got %+v", r.Providers)
	}
}

// --- 2. DisabledProvidersShowZero ---------------------------------------

func TestDiscoverySummary_DisabledProvidersShowZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	gcp := &stubGCPStore{ids: []string{"gcp-conn-1"}}
	az := &stubAzureStore{ids: []string{"az-conn-1"}}
	// oci is nil — provider isn't wired.

	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	audit := &stubAuditQuery{
		scans: map[string]map[string]ScanSummary{
			"aws": {
				"111122223333": {ScopeID: "111122223333", CompletedAt: now,
					InstanceCount: 10, InstrumentedCount: 6, UninstrumentedCount: 4},
			},
			"gcp": {
				"gcp-conn-1": {ScopeID: "gcp-conn-1", CompletedAt: now,
					InstanceCount: 20, InstrumentedCount: 12, UninstrumentedCount: 8},
			},
			"azure": {
				"az-conn-1": {ScopeID: "az-conn-1", CompletedAt: now,
					InstanceCount: 30, InstrumentedCount: 18, UninstrumentedCount: 12},
			},
		},
	}

	h := NewDiscoverySummaryHandlers(aws, gcp, az, nil, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	r := parseSummary(t, w)

	if r.Providers["oci"].Enabled {
		t.Errorf("oci should be enabled=false")
	}
	if r.Providers["oci"].InstanceCount != 0 {
		t.Errorf("oci.instance_count = %d, want 0", r.Providers["oci"].InstanceCount)
	}
	if r.Providers["oci"].ConnectionCount != 0 {
		t.Errorf("oci.connection_count = %d, want 0", r.Providers["oci"].ConnectionCount)
	}
	// Other three providers stay as before — totals only count enabled.
	if got, want := r.Totals.InstanceCount, 60; got != want {
		t.Errorf("totals.instance_count = %d, want %d", got, want)
	}
	if got := r.Totals.ConnectionCount; got != 3 {
		t.Errorf("totals.connection_count = %d, want 3", got)
	}
}

// --- 3. CacheTTLBehavior ------------------------------------------------

func TestDiscoverySummary_CacheTTLBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	audit := &stubAuditQuery{}
	spy := &spyAuditService{}

	// Pin the clock so the second call is comfortably within the TTL.
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	h := NewDiscoverySummaryHandlers(aws, nil, nil, nil, spy, audit, 30*time.Second, clock, nil)

	w1 := summaryDoRequest(h)
	if w1.Code != http.StatusOK {
		t.Fatalf("first call status = %d, want 200", w1.Code)
	}
	w2 := summaryDoRequest(h)
	if w2.Code != http.StatusOK {
		t.Fatalf("second call status = %d, want 200", w2.Code)
	}
	if w1.Body.String() != w2.Body.String() {
		t.Errorf("cache hit should return the same body; w1=%s w2=%s", w1.Body.String(), w2.Body.String())
	}
	// Exactly one cache-miss audit emission.
	if got := spy.count(); got != 1 {
		t.Errorf("audit emissions = %d, want 1 (cache hits must not emit)", got)
	}
	// AWS list called exactly once — second hit short-circuited.
	if got := atomic.LoadInt64(&aws.calls); got != 1 {
		t.Errorf("aws list calls = %d, want 1", got)
	}
}

// --- 4. CacheExpires ----------------------------------------------------

func TestDiscoverySummary_CacheExpires(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	audit := &stubAuditQuery{}
	spy := &spyAuditService{}

	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	clock := fixedClock(&now)

	h := NewDiscoverySummaryHandlers(aws, nil, nil, nil, spy, audit, 30*time.Second, clock, nil)

	if w := summaryDoRequest(h); w.Code != http.StatusOK {
		t.Fatalf("first call status = %d", w.Code)
	}
	if got := atomic.LoadInt64(&aws.calls); got != 1 {
		t.Errorf("after first call: aws list calls = %d, want 1", got)
	}

	// Advance clock past TTL.
	now = now.Add(31 * time.Second)
	if w := summaryDoRequest(h); w.Code != http.StatusOK {
		t.Fatalf("second call status = %d", w.Code)
	}
	if got := atomic.LoadInt64(&aws.calls); got != 2 {
		t.Errorf("after second call: aws list calls = %d, want 2", got)
	}
	if got := spy.count(); got != 2 {
		t.Errorf("audit emissions = %d, want 2 (two cache misses)", got)
	}
}

// --- 5. RecentRecommendationsLimitedTo10 --------------------------------

func TestDiscoverySummary_RecentRecommendationsLimitedTo10(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 15 recent recommendations — handler should cap at 10.
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	props := make([]ProposalEvent, 15)
	for i := range props {
		props[i] = ProposalEvent{
			Provider:    "aws",
			Kind:        "ec2-otel-tag",
			ScopeID:     fmt.Sprintf("acct-%02d", i),
			Region:      "us-east-1",
			GeneratedAt: now.Add(-time.Duration(i) * time.Minute),
		}
	}
	audit := &stubAuditQuery{proposals: props}

	h := NewDiscoverySummaryHandlers(nil, nil, nil, nil, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseSummary(t, w)
	if got := len(r.RecentRecommendations); got != 10 {
		t.Errorf("len(recent_recommendations) = %d, want 10", got)
	}
	// Newest-first: first element has the latest timestamp.
	if r.RecentRecommendations[0].GeneratedAt.Before(r.RecentRecommendations[1].GeneratedAt) {
		t.Errorf("recent_recommendations not sorted newest-first: %v", r.RecentRecommendations)
	}
}

// --- 6. EmitsAuditOnCacheMiss -------------------------------------------

func TestDiscoverySummary_EmitsAuditOnCacheMiss(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"acct-1"}}
	audit := &stubAuditQuery{}
	spy := &spyAuditService{}
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)

	h := NewDiscoverySummaryHandlers(aws, nil, nil, nil, spy, audit, 30*time.Second, func() time.Time { return now }, nil)

	if w := summaryDoRequest(h); w.Code != http.StatusOK {
		t.Fatalf("miss call status = %d", w.Code)
	}
	if spy.count() != 1 {
		t.Errorf("after miss: emissions = %d, want 1", spy.count())
	}
	e := spy.entries[0]
	if e.EventType != services.AuditEventDiscoverySummaryRequested {
		t.Errorf("event_type = %q, want %q", e.EventType, services.AuditEventDiscoverySummaryRequested)
	}
	if e.Actor != "system" {
		t.Errorf("actor = %q, want system", e.Actor)
	}
	if e.Action != "requested" {
		t.Errorf("action = %q, want requested", e.Action)
	}
	// Second call is a cache hit — no audit emit.
	if w := summaryDoRequest(h); w.Code != http.StatusOK {
		t.Fatalf("hit call status = %d", w.Code)
	}
	if spy.count() != 1 {
		t.Errorf("after hit: emissions = %d, want 1 (cache hits must not emit)", spy.count())
	}
}

// --- 7. CoveragePctZeroSafe ---------------------------------------------

func TestDiscoverySummary_CoveragePctZeroSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Zero instances across the board.
	aws := &stubAWSStore{ids: []string{"acct-1"}}
	gcp := &stubGCPStore{ids: []string{"gcp-1"}}
	az := &stubAzureStore{ids: []string{"az-1"}}
	oci := &stubOCIStore{ids: []string{"oci-1"}}
	audit := &stubAuditQuery{} // no scan_completed events

	h := NewDiscoverySummaryHandlers(aws, gcp, az, oci, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseSummary(t, w)
	if r.Totals.CoveragePct != 0 {
		t.Errorf("coverage_pct = %v, want 0", r.Totals.CoveragePct)
	}
	if math.IsNaN(r.Totals.CoveragePct) || math.IsInf(r.Totals.CoveragePct, 0) {
		t.Errorf("coverage_pct is NaN/Inf: %v", r.Totals.CoveragePct)
	}
}

// --- 8. CoveragePctRoundsToOneDecimal -----------------------------------

func TestDiscoverySummary_CoveragePctRoundsToOneDecimal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name              string
		instrumented      int
		instanceCount     int
		wantPct           float64
	}{
		{"67_of_100_clean", 67, 100, 67.0},
		{"2_of_3_rounded", 2, 3, 66.7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aws := &stubAWSStore{ids: []string{"acct-1"}}
			now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
			audit := &stubAuditQuery{
				scans: map[string]map[string]ScanSummary{
					"aws": {
						"acct-1": {ScopeID: "acct-1", CompletedAt: now,
							InstanceCount:       tc.instanceCount,
							InstrumentedCount:   tc.instrumented,
							UninstrumentedCount: tc.instanceCount - tc.instrumented,
						},
					},
				},
			}
			h := NewDiscoverySummaryHandlers(aws, nil, nil, nil, nil, audit, time.Second, nil, nil)
			w := summaryDoRequest(h)
			r := parseSummary(t, w)
			if r.Totals.CoveragePct != tc.wantPct {
				t.Errorf("coverage_pct = %v, want %v", r.Totals.CoveragePct, tc.wantPct)
			}
		})
	}
}

// --- 9. AllStoresNil_ReturnsEmpty ---------------------------------------

func TestDiscoverySummary_AllStoresNil_ReturnsEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewDiscoverySummaryHandlers(nil, nil, nil, nil, nil, nil, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	r := parseSummary(t, w)
	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		if r.Providers[p].Enabled {
			t.Errorf("provider %q should be enabled=false", p)
		}
	}
	if r.Totals.InstanceCount != 0 || r.Totals.ConnectionCount != 0 {
		t.Errorf("totals should be zero; got %+v", r.Totals)
	}
	// recent_recommendations must be [] not null on the wire.
	if r.RecentRecommendations == nil {
		t.Errorf("recent_recommendations should be a non-nil slice")
	}
	// And the JSON encoding should not include the key as null —
	// assert by checking the raw body for "null".
	if got := w.Body.String(); !contains(got, `"recent_recommendations":[]`) {
		t.Errorf("body should carry recent_recommendations:[], got %s", got)
	}
}

func contains(haystack, needle string) bool {
	// Tiny helper so the test file doesn't pull in strings just for one use.
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// --- 10. AuthRequired ---------------------------------------------------

// TestDiscoverySummary_AuthRequired wires the RequireBearer middleware
// in front of the summary handler and asserts an un-bearered request
// is rejected with 401. This mirrors the production server-side
// posture: GET /api/v1/discovery/summary sits under the same auth
// middleware as the rest of /api/v1/*.
func TestDiscoverySummary_AuthRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewDiscoverySummaryHandlers(nil, nil, nil, nil, nil, nil, time.Second, nil, nil)
	auth := stubAuthService{}
	r := gin.New()
	r.Use(middleware.RequireBearer(auth, zap.NewNop()))
	r.GET("/api/v1/discovery/summary", h.HandleSummary)

	// No Authorization header — must 401.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/discovery/summary", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

// stubAuthService produces a deterministic 401 path. Validate is the
// only method RequireBearer calls; it returns nil,nil to model "token
// unknown / revoked / expired" so the middleware aborts with 401.
type stubAuthService struct{}

func (stubAuthService) Issue(_ context.Context, _ string, _ []string, _ *time.Time) (*services.APIToken, string, error) {
	return nil, "", errors.New("not implemented")
}
func (stubAuthService) List(_ context.Context) ([]*services.APIToken, error) { return nil, nil }
func (stubAuthService) Revoke(_ context.Context, _ string) error             { return nil }
func (stubAuthService) Validate(_ context.Context, _ string) (*services.APIToken, error) {
	// Returning nil,nil matches the "token unknown" path the middleware
	// translates into 401. Returning an error would 500.
	return nil, nil
}

// --- Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) ----
//
// TestDiscoverySummary_IncludesServerlessCount — §11 acceptance test
// 11: seeding the audit projection with a serverless_count per
// provider must surface on each ProviderSummary.ServerlessCount field
// and roll up to Totals.ServerlessCount. Mirrors the existing
// AggregatesAllFourProviders test posture.
func TestDiscoverySummary_IncludesServerlessCount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	gcp := &stubGCPStore{ids: []string{"gcp-conn-1"}}
	az := &stubAzureStore{ids: []string{"az-conn-1"}}
	oci := &stubOCIStore{ids: []string{"oci-conn-1"}}

	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	audit := &stubAuditQuery{
		scans: map[string]map[string]ScanSummary{
			"aws": {
				"111122223333": {ScopeID: "111122223333", CompletedAt: now,
					InstanceCount: 10, InstrumentedCount: 6, UninstrumentedCount: 4,
					ServerlessCount: 3},
			},
			"gcp": {
				"gcp-conn-1": {ScopeID: "gcp-conn-1", CompletedAt: now,
					InstanceCount: 20, InstrumentedCount: 12, UninstrumentedCount: 8,
					ServerlessCount: 5},
			},
			"azure": {
				"az-conn-1": {ScopeID: "az-conn-1", CompletedAt: now,
					InstanceCount: 30, InstrumentedCount: 18, UninstrumentedCount: 12,
					ServerlessCount: 2},
			},
			"oci": {
				"oci-conn-1": {ScopeID: "oci-conn-1", CompletedAt: now,
					InstanceCount: 40, InstrumentedCount: 24, UninstrumentedCount: 16,
					ServerlessCount: 1},
			},
		},
	}
	h := NewDiscoverySummaryHandlers(aws, gcp, az, oci, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	r := parseSummary(t, w)

	cases := []struct {
		name string
		want int
	}{
		{"aws", 3}, {"gcp", 5}, {"azure", 2}, {"oci", 1},
	}
	for _, tc := range cases {
		if got := r.Providers[tc.name].ServerlessCount; got != tc.want {
			t.Errorf("%s.serverless_count = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got, want := r.Totals.ServerlessCount, 11; got != want {
		t.Errorf("totals.serverless_count = %d, want %d", got, want)
	}
}

// --- Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129) -
//
// TestDiscoverySummary_IncludesOrchestrationCount — §11 acceptance
// test 11 for the orchestration tier: seeding the audit projection
// with an orchestration_count per provider must surface on each
// ProviderSummary.OrchestrationCount field and roll up to
// Totals.OrchestrationCount. For OCI it stays 0 in slice 1 — OCI
// orchestration is deferred to slice 2 — but the test still drives a
// nonzero OCI value to assert the substrate handles the field
// uniformly. The separate OCI-zero-on-empty-payload contract is
// pinned in TestDiscoverySummary_OCIOrchestrationCountIsZero.
func TestDiscoverySummary_IncludesOrchestrationCount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	gcp := &stubGCPStore{ids: []string{"gcp-conn-1"}}
	az := &stubAzureStore{ids: []string{"az-conn-1"}}
	oci := &stubOCIStore{ids: []string{"oci-conn-1"}}

	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	audit := &stubAuditQuery{
		scans: map[string]map[string]ScanSummary{
			"aws": {
				"111122223333": {ScopeID: "111122223333", CompletedAt: now,
					InstanceCount: 10, InstrumentedCount: 6, UninstrumentedCount: 4,
					OrchestrationCount: 3},
			},
			"gcp": {
				"gcp-conn-1": {ScopeID: "gcp-conn-1", CompletedAt: now,
					InstanceCount: 20, InstrumentedCount: 12, UninstrumentedCount: 8,
					OrchestrationCount: 2},
			},
			"azure": {
				"az-conn-1": {ScopeID: "az-conn-1", CompletedAt: now,
					InstanceCount: 30, InstrumentedCount: 18, UninstrumentedCount: 12,
					OrchestrationCount: 1},
			},
			"oci": {
				"oci-conn-1": {ScopeID: "oci-conn-1", CompletedAt: now,
					InstanceCount: 40, InstrumentedCount: 24, UninstrumentedCount: 16,
					OrchestrationCount: 0},
			},
		},
	}
	h := NewDiscoverySummaryHandlers(aws, gcp, az, oci, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	r := parseSummary(t, w)

	cases := []struct {
		name string
		want int
	}{
		{"aws", 3}, {"gcp", 2}, {"azure", 1}, {"oci", 0},
	}
	for _, tc := range cases {
		if got := r.Providers[tc.name].OrchestrationCount; got != tc.want {
			t.Errorf("%s.orchestration_count = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got, want := r.Totals.OrchestrationCount, 6; got != want {
		t.Errorf("totals.orchestration_count = %d, want %d", got, want)
	}
}

// TestDiscoverySummary_OCIOrchestrationCountIsZero — slice 1 contract
// per docs/proposals/orchestration-tier-slice1.md §6.3: OCI is
// deferred to slice 2, so OCI scan_completed events never populate
// orchestration_count and the dashboard shows 0 for OCI. The
// projection treats the missing field as 0 (intFromPayload's zero-safe
// posture).
func TestDiscoverySummary_OCIOrchestrationCountIsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oci := &stubOCIStore{ids: []string{"oci-conn-1"}}
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	audit := &stubAuditQuery{
		scans: map[string]map[string]ScanSummary{
			"oci": {
				"oci-conn-1": {ScopeID: "oci-conn-1", CompletedAt: now,
					InstanceCount: 5, InstrumentedCount: 3, UninstrumentedCount: 2},
			},
		},
	}
	h := NewDiscoverySummaryHandlers(nil, nil, nil, oci, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	r := parseSummary(t, w)
	if got := r.Providers["oci"].OrchestrationCount; got != 0 {
		t.Errorf("oci.orchestration_count = %d, want 0 (slice 1 contract — OCI deferred to slice 2)", got)
	}
	if got := r.Totals.OrchestrationCount; got != 0 {
		t.Errorf("totals.orchestration_count = %d, want 0 (OCI-only deployment)", got)
	}
}

// TestDiscoverySummary_ServerlessCountZeroOnColdStart — cold-start
// posture: when the audit projection carries no serverless_count
// field (older scans pre-date the field), every provider's
// ServerlessCount stays 0 and Totals.ServerlessCount stays 0. Pins the
// backward-compat invariant the wire adapter's intFromPayload helper
// already enforces — surfacing it as a regression test so a future
// refactor of the projection doesn't break older-scan deployments.
func TestDiscoverySummary_ServerlessCountZeroOnColdStart(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	audit := &stubAuditQuery{
		scans: map[string]map[string]ScanSummary{
			"aws": {
				"111122223333": {ScopeID: "111122223333", CompletedAt: now,
					InstanceCount: 10, InstrumentedCount: 6, UninstrumentedCount: 4},
			},
		},
	}
	h := NewDiscoverySummaryHandlers(aws, nil, nil, nil, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	r := parseSummary(t, w)

	if got := r.Providers["aws"].ServerlessCount; got != 0 {
		t.Errorf("aws.serverless_count = %d, want 0 (cold start)", got)
	}
	if got := r.Totals.ServerlessCount; got != 0 {
		t.Errorf("totals.serverless_count = %d, want 0 (cold start)", got)
	}
}

// --- Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136) -
//
// TestDiscoverySummary_IncludesEventSourceCount — §11 acceptance test
// 13 for the event source tier: seeding the audit projection with an
// event_source_count per provider must surface on each
// ProviderSummary.EventSourceCount field and roll up to
// Totals.EventSourceCount. Unlike orchestration where OCI was always
// zero, the event source tier ships all four providers in slice 1 —
// OCI Streaming is a real surface.
func TestDiscoverySummary_IncludesEventSourceCount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	gcp := &stubGCPStore{ids: []string{"gcp-conn-1"}}
	az := &stubAzureStore{ids: []string{"az-conn-1"}}
	oci := &stubOCIStore{ids: []string{"oci-conn-1"}}

	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	audit := &stubAuditQuery{
		scans: map[string]map[string]ScanSummary{
			"aws": {
				"111122223333": {ScopeID: "111122223333", CompletedAt: now,
					InstanceCount: 10, InstrumentedCount: 6, UninstrumentedCount: 4,
					EventSourceCount: 4},
			},
			"gcp": {
				"gcp-conn-1": {ScopeID: "gcp-conn-1", CompletedAt: now,
					InstanceCount: 20, InstrumentedCount: 12, UninstrumentedCount: 8,
					EventSourceCount: 3},
			},
			"azure": {
				"az-conn-1": {ScopeID: "az-conn-1", CompletedAt: now,
					InstanceCount: 30, InstrumentedCount: 18, UninstrumentedCount: 12,
					EventSourceCount: 2},
			},
			"oci": {
				"oci-conn-1": {ScopeID: "oci-conn-1", CompletedAt: now,
					InstanceCount: 40, InstrumentedCount: 24, UninstrumentedCount: 16,
					EventSourceCount: 1},
			},
		},
	}
	h := NewDiscoverySummaryHandlers(aws, gcp, az, oci, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	r := parseSummary(t, w)

	cases := []struct {
		name string
		want int
	}{
		{"aws", 4}, {"gcp", 3}, {"azure", 2}, {"oci", 1},
	}
	for _, tc := range cases {
		if got := r.Providers[tc.name].EventSourceCount; got != tc.want {
			t.Errorf("%s.event_source_count = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got, want := r.Totals.EventSourceCount, 10; got != want {
		t.Errorf("totals.event_source_count = %d, want %d", got, want)
	}
}

// TestDiscoverySummary_TotalsAggregateEventSourceAcrossProviders —
// confirms the cross-provider rollup sums correctly across enabled
// providers and skips disabled ones. Seeds nonzero counts in two of
// four providers; asserts the third + fourth contribute zero.
func TestDiscoverySummary_TotalsAggregateEventSourceAcrossProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	aws := &stubAWSStore{ids: []string{"111122223333"}}
	gcp := &stubGCPStore{ids: []string{"gcp-conn-1"}}
	az := &stubAzureStore{ids: []string{"az-conn-1"}}
	oci := &stubOCIStore{ids: []string{"oci-conn-1"}}

	now := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	audit := &stubAuditQuery{
		scans: map[string]map[string]ScanSummary{
			"aws": {
				"111122223333": {ScopeID: "111122223333", CompletedAt: now,
					InstanceCount: 10, EventSourceCount: 5},
			},
			"gcp": {
				"gcp-conn-1": {ScopeID: "gcp-conn-1", CompletedAt: now,
					InstanceCount: 20, EventSourceCount: 7},
			},
			// azure + oci scans have no event_source_count field set —
			// cold-start posture: contribute 0 to the totals.
			"azure": {
				"az-conn-1": {ScopeID: "az-conn-1", CompletedAt: now, InstanceCount: 30},
			},
			"oci": {
				"oci-conn-1": {ScopeID: "oci-conn-1", CompletedAt: now, InstanceCount: 40},
			},
		},
	}
	h := NewDiscoverySummaryHandlers(aws, gcp, az, oci, nil, audit, time.Second, nil, nil)
	w := summaryDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	r := parseSummary(t, w)
	if got, want := r.Totals.EventSourceCount, 12; got != want {
		t.Errorf("totals.event_source_count = %d, want %d", got, want)
	}
	if got := r.Providers["azure"].EventSourceCount; got != 0 {
		t.Errorf("azure.event_source_count = %d, want 0 (cold-start projection)", got)
	}
	if got := r.Providers["oci"].EventSourceCount; got != 0 {
		t.Errorf("oci.event_source_count = %d, want 0 (cold-start projection)", got)
	}
}
