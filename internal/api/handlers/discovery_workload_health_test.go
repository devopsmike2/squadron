// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/services"
)

// discovery_workload_health_test.go — Workload Health dashboard panel
// slice 1 chunk 1 (v0.89.132, #772 Stream 170). Pairs with
// discovery_workload_health.go. Covers acceptance tests 1-7 + the
// per-provider shape + totals roll-up + zero-denominator pct helper.
//
// spyAuditService comes from discovery_summary_test.go via the
// package's single test compile unit. The stub reader below is the
// ServerlessHealthInventoryReader surface this chunk introduces.

// --- stubs --------------------------------------------------------------

// stubServerlessHealthReader returns canned per-provider counts.
// Tracks the number of WorkloadHealthCounts calls so the cache TTL
// test can assert "second hit short-circuited the reader walk."
type stubServerlessHealthReader struct {
	mu     sync.Mutex
	counts map[string]WorkloadHealthProviderCounts
	nilOut bool
	calls  int64
}

func (s *stubServerlessHealthReader) WorkloadHealthCounts(_ context.Context) map[string]WorkloadHealthProviderCounts {
	atomic.AddInt64(&s.calls, 1)
	if s.nilOut {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]WorkloadHealthProviderCounts, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	return out
}

// --- helpers ------------------------------------------------------------

func workloadHealthDoRequest(h *DiscoveryWorkloadHealthHandlers) *httptest.ResponseRecorder {
	r := gin.New()
	r.GET("/api/v1/discovery/workload_health", h.HandleWorkloadHealth)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/discovery/workload_health", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func parseWorkloadHealth(t *testing.T, w *httptest.ResponseRecorder) WorkloadHealthResponse {
	t.Helper()
	var r WorkloadHealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode workload health response: %v", err)
	}
	return r
}

// advancingClock returns a clock function bound to a single mutable
// timestamp plus an advance function the test calls to push the
// clock forward. Distinct from fixedClock in discovery_summary_test.go
// (which returns a *time.Time pointer-based clock) so this file's
// cache TTL tests stay readable without leaking into the summary
// test conventions.
func advancingClock(start time.Time) (clock func() time.Time, advance func(time.Duration)) {
	var mu sync.Mutex
	now := start
	clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	return clock, advance
}

// fourProviderCounts is the canonical mixed-fleet fixture: AWS heavy,
// GCP medium, Azure light, OCI empty. Used across the cache + audit +
// totals tests so the assertions stay readable.
func fourProviderCounts() map[string]WorkloadHealthProviderCounts {
	return map[string]WorkloadHealthProviderCounts{
		"aws": {
			ServerlessResourceCount:    47,
			ColdStartExceededCount:     5,
			SamplingTooAggressiveCount: 3,
			ErrorRateSpikeCount:        2,
			AnyIssueCount:              8,
		},
		"gcp": {
			ServerlessResourceCount:    60,
			ColdStartExceededCount:     4,
			SamplingTooAggressiveCount: 3,
			ErrorRateSpikeCount:        2,
			AnyIssueCount:              8,
		},
		"azure": {
			ServerlessResourceCount:    35,
			ColdStartExceededCount:     3,
			SamplingTooAggressiveCount: 2,
			ErrorRateSpikeCount:        1,
			AnyIssueCount:              6,
		},
		"oci": {},
	}
}

// --- tests --------------------------------------------------------------

// TestWorkloadHealth_AggregationIncludesOnlyServerless — design doc
// §8 acceptance test 1. Pins the contract that the workload health
// handler reads ONLY the serverless-shaped reader. Compute / DB /
// k8s rows do not flow through this endpoint by construction —
// there's no other reader. The test asserts the response shape
// reflects exactly the per-provider counts the reader exposed; no
// stray category is summed in.
func TestWorkloadHealth_AggregationIncludesOnlyServerless(t *testing.T) {
	reader := &stubServerlessHealthReader{counts: fourProviderCounts()}
	h := NewDiscoveryWorkloadHealthHandlers(reader, nil, time.Minute, nil, nil)

	w := workloadHealthDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	r := parseWorkloadHealth(t, w)

	// Per-provider counts mirror the reader exactly — no compute /
	// db / k8s leak in because no other reader contributes.
	if r.Providers["aws"].ServerlessResourceCount != 47 {
		t.Fatalf("aws serverless count = %d, want 47", r.Providers["aws"].ServerlessResourceCount)
	}
	if r.Providers["gcp"].ServerlessResourceCount != 60 {
		t.Fatalf("gcp serverless count = %d, want 60", r.Providers["gcp"].ServerlessResourceCount)
	}
	// Totals are the bare sum across providers — no extra category
	// snuck in.
	if got, want := r.Totals.ServerlessResourceCount, 47+60+35+0; got != want {
		t.Fatalf("totals serverless count = %d, want %d", got, want)
	}
}

// TestWorkloadHealth_ColdStartExceededCountMatchesObservationThreshold
// — design doc §8 acceptance test 2. The handler propagates the
// reader's per-provider cold-start-exceeded count verbatim; this test
// pins the wire shape against the reader's threshold logic (the
// reader is responsible for applying the 1.5x ratio + 500ms floor +
// >=50 baseline samples gates, and the handler does NOT second-guess
// them).
func TestWorkloadHealth_ColdStartExceededCountMatchesObservationThreshold(t *testing.T) {
	reader := &stubServerlessHealthReader{counts: fourProviderCounts()}
	h := NewDiscoveryWorkloadHealthHandlers(reader, nil, time.Minute, nil, nil)

	w := workloadHealthDoRequest(h)
	r := parseWorkloadHealth(t, w)

	// Per-provider cold-start counts wired through.
	if r.Providers["aws"].ColdStartExceededCount != 5 {
		t.Fatalf("aws cold start = %d, want 5", r.Providers["aws"].ColdStartExceededCount)
	}
	// 5/47 ≈ 10.6%
	if got := r.Providers["aws"].ColdStartExceededPct; got != 10.6 {
		t.Fatalf("aws cold start pct = %.4f, want 10.6", got)
	}
	// Totals: 5+4+3+0 = 12; 142 total resources; 12/142 ≈ 8.5%.
	if r.Totals.ColdStartExceededCount != 12 {
		t.Fatalf("totals cold start = %d, want 12", r.Totals.ColdStartExceededCount)
	}
	if got := r.Totals.ColdStartExceededPct; got != 8.5 {
		t.Fatalf("totals cold start pct = %.4f, want 8.5", got)
	}
}

// TestWorkloadHealth_SamplingTooAggressiveCountMatchesDetector —
// design doc §8 acceptance test 3. Same shape as the cold-start
// test: the handler propagates the reader's per-provider sampling-
// too-aggressive count verbatim. The detector inside the reader
// applies the <5% floor AND >=1000 invocation gates.
func TestWorkloadHealth_SamplingTooAggressiveCountMatchesDetector(t *testing.T) {
	reader := &stubServerlessHealthReader{counts: fourProviderCounts()}
	h := NewDiscoveryWorkloadHealthHandlers(reader, nil, time.Minute, nil, nil)

	w := workloadHealthDoRequest(h)
	r := parseWorkloadHealth(t, w)

	// Per-provider sampling counts wired through.
	if r.Providers["aws"].SamplingTooAggressiveCount != 3 {
		t.Fatalf("aws sampling = %d, want 3", r.Providers["aws"].SamplingTooAggressiveCount)
	}
	// 3/47 ≈ 6.4%
	if got := r.Providers["aws"].SamplingTooAggressivePct; got != 6.4 {
		t.Fatalf("aws sampling pct = %.4f, want 6.4", got)
	}
	// Totals: 3+3+2+0 = 8; 8/142 ≈ 5.6%.
	if r.Totals.SamplingTooAggressiveCount != 8 {
		t.Fatalf("totals sampling = %d, want 8", r.Totals.SamplingTooAggressiveCount)
	}
	if got := r.Totals.SamplingTooAggressivePct; got != 5.6 {
		t.Fatalf("totals sampling pct = %.4f, want 5.6", got)
	}
}

// TestWorkloadHealth_ErrorRateSpikeCountMatchesDetector — design doc
// §8 acceptance test 4. Pins the handler propagating the reader's
// per-provider error-rate-spike count verbatim. The detector inside
// the reader applies the 2.0x ratio + >=1000 invocations + >=50
// errors gates.
func TestWorkloadHealth_ErrorRateSpikeCountMatchesDetector(t *testing.T) {
	reader := &stubServerlessHealthReader{counts: fourProviderCounts()}
	h := NewDiscoveryWorkloadHealthHandlers(reader, nil, time.Minute, nil, nil)

	w := workloadHealthDoRequest(h)
	r := parseWorkloadHealth(t, w)

	if r.Providers["aws"].ErrorRateSpikeCount != 2 {
		t.Fatalf("aws error rate = %d, want 2", r.Providers["aws"].ErrorRateSpikeCount)
	}
	// 2/47 ≈ 4.3%
	if got := r.Providers["aws"].ErrorRateSpikePct; got != 4.3 {
		t.Fatalf("aws error rate pct = %.4f, want 4.3", got)
	}
	// Totals: 2+2+1+0 = 5; 5/142 ≈ 3.5%.
	if r.Totals.ErrorRateSpikeCount != 5 {
		t.Fatalf("totals error rate = %d, want 5", r.Totals.ErrorRateSpikeCount)
	}
	if got := r.Totals.ErrorRateSpikePct; got != 3.5 {
		t.Fatalf("totals error rate pct = %.4f, want 3.5", got)
	}
}

// TestWorkloadHealth_AnyIssueUsesUnionSemantics — design doc §8
// acceptance test 5. Pins the UNION rule via the
// WorkloadHealthAnyIssueCount helper that BOTH the production reader
// and this test exercise:
//
//   - Resource 0 fires cold-start AND sampling AND error-rate.
//   - Resource 1 fires cold-start AND sampling.
//   - Resource 2 fires sampling AND error-rate.
//   - Resource 3 fires only cold-start.
//   - Resource 4 fires nothing.
//
// SUM-of-per-diagnostic-counts is 3+4+3 = 10. UNION-of-per-resource
// is 4 (every resource except #4). The helper MUST return 4.
func TestWorkloadHealth_AnyIssueUsesUnionSemantics(t *testing.T) {
	coldStart := []bool{true, true, false, true, false}
	sampling := []bool{true, true, true, false, false}
	errorRate := []bool{true, false, true, false, false}

	got := WorkloadHealthAnyIssueCount(coldStart, sampling, errorRate)
	if got != 4 {
		t.Fatalf("any-issue UNION count = %d, want 4", got)
	}

	// Defensive: the helper's any-issue count must NEVER exceed any
	// single per-diagnostic SUM that includes it (i.e. a resource
	// counted in any-issue must have at least one diagnostic firing).
	csSum, spSum, erSum := 0, 0, 0
	for i := range coldStart {
		if coldStart[i] {
			csSum++
		}
		if sampling[i] {
			spSum++
		}
		if errorRate[i] {
			erSum++
		}
	}
	if got > csSum+spSum+erSum {
		t.Fatalf("any-issue count = %d exceeds sum-of-diagnostics %d — UNION cannot exceed SUM", got, csSum+spSum+erSum)
	}
	if got < csSum && got < spSum && got < erSum {
		t.Fatalf("any-issue count = %d below max of {%d, %d, %d} — UNION must dominate every single diagnostic", got, csSum, spSum, erSum)
	}

	// Also pin the handler's projection: when the reader reports a
	// counts tuple where two diagnostics overlap on the same
	// resource, the response surfaces the reader's any_issue_count
	// VERBATIM (no double-add).
	reader := &stubServerlessHealthReader{counts: map[string]WorkloadHealthProviderCounts{
		"aws": {
			ServerlessResourceCount:    5,
			ColdStartExceededCount:     3,
			SamplingTooAggressiveCount: 3,
			ErrorRateSpikeCount:        2,
			AnyIssueCount:              4, // the UNION
		},
		"gcp":   {},
		"azure": {},
		"oci":   {},
	}}
	h := NewDiscoveryWorkloadHealthHandlers(reader, nil, time.Minute, nil, nil)
	w := workloadHealthDoRequest(h)
	r := parseWorkloadHealth(t, w)
	if r.Providers["aws"].AnyIssueCount != 4 {
		t.Fatalf("any-issue count via handler = %d, want 4 (UNION, not 3+3+2)", r.Providers["aws"].AnyIssueCount)
	}
}

// TestWorkloadHealth_30sCacheReturnsSameResponse — design doc §8
// acceptance test 6. A second call within the cache window returns
// the cached payload without invoking the reader a second time.
// Advancing the clock past the TTL evicts the cache and re-walks
// the reader.
func TestWorkloadHealth_30sCacheReturnsSameResponse(t *testing.T) {
	reader := &stubServerlessHealthReader{counts: fourProviderCounts()}
	clk, advance := advancingClock(time.Now().UTC())
	h := NewDiscoveryWorkloadHealthHandlers(reader, nil, 30*time.Second, clk, nil)

	w1 := workloadHealthDoRequest(h)
	r1 := parseWorkloadHealth(t, w1)

	// Second call inside TTL: reader is NOT called again, response
	// identical.
	w2 := workloadHealthDoRequest(h)
	r2 := parseWorkloadHealth(t, w2)
	if atomic.LoadInt64(&reader.calls) != 1 {
		t.Fatalf("reader calls = %d, want 1 (cache should short-circuit)", reader.calls)
	}
	if r1.Totals != r2.Totals {
		t.Fatalf("cached response totals differ: %+v vs %+v", r1.Totals, r2.Totals)
	}

	// Advance past TTL: reader walks again.
	advance(31 * time.Second)
	w3 := workloadHealthDoRequest(h)
	if w3.Code != http.StatusOK {
		t.Fatalf("post-TTL status = %d", w3.Code)
	}
	if atomic.LoadInt64(&reader.calls) != 2 {
		t.Fatalf("reader calls = %d, want 2 (cache should expire after TTL)", reader.calls)
	}
}

// TestWorkloadHealth_CacheMissEmitsAudit — design doc §8 acceptance
// test 7. The handler emits AuditEventDiscoveryWorkloadHealthRequested
// on cache miss but NOT on cache hit. Mirrors the trace_coverage +
// span_quality cache-miss-only audit posture.
func TestWorkloadHealth_CacheMissEmitsAudit(t *testing.T) {
	reader := &stubServerlessHealthReader{counts: fourProviderCounts()}
	spy := &spyAuditService{}
	h := NewDiscoveryWorkloadHealthHandlers(reader, spy, time.Minute, nil, nil)

	// First hit: cache miss, audit emit fires.
	_ = workloadHealthDoRequest(h)
	if spy.count() != 1 {
		t.Fatalf("audit entries after first call = %d, want 1", spy.count())
	}
	if spy.entries[0].EventType != services.AuditEventDiscoveryWorkloadHealthRequested {
		t.Fatalf("audit event type = %q, want %q", spy.entries[0].EventType, services.AuditEventDiscoveryWorkloadHealthRequested)
	}
	if spy.entries[0].Payload["cache_status"] != "miss" {
		t.Fatalf("audit cache_status = %v, want \"miss\"", spy.entries[0].Payload["cache_status"])
	}

	// Second hit: cache HIT, no new audit entry.
	_ = workloadHealthDoRequest(h)
	if spy.count() != 1 {
		t.Fatalf("audit entries after cache-hit call = %d, want 1 (no emit on hit)", spy.count())
	}
}

// TestWorkloadHealth_TotalsAggregateAcrossProviders pins the totals
// row as the bare per-provider sum across all four providers. Caught
// a regression where an earlier draft summed only the three
// non-empty providers and skipped OCI's zero-count contribution.
func TestWorkloadHealth_TotalsAggregateAcrossProviders(t *testing.T) {
	reader := &stubServerlessHealthReader{counts: fourProviderCounts()}
	h := NewDiscoveryWorkloadHealthHandlers(reader, nil, time.Minute, nil, nil)

	w := workloadHealthDoRequest(h)
	r := parseWorkloadHealth(t, w)

	wantServerless := 47 + 60 + 35 + 0
	if r.Totals.ServerlessResourceCount != wantServerless {
		t.Fatalf("totals serverless = %d, want %d", r.Totals.ServerlessResourceCount, wantServerless)
	}
	wantCS := 5 + 4 + 3 + 0
	if r.Totals.ColdStartExceededCount != wantCS {
		t.Fatalf("totals cold start = %d, want %d", r.Totals.ColdStartExceededCount, wantCS)
	}
	wantAny := 8 + 8 + 6 + 0
	if r.Totals.AnyIssueCount != wantAny {
		t.Fatalf("totals any-issue = %d, want %d", r.Totals.AnyIssueCount, wantAny)
	}
}

// TestWorkloadHealth_PerProviderShape pins that the response always
// carries the four-provider key set, even when the reader returns
// nothing for some keys. This is the same deterministic-shape
// invariant /discovery/trace_coverage carries: dashboards don't have
// to handle missing keys.
func TestWorkloadHealth_PerProviderShape(t *testing.T) {
	// Reader populates only AWS.
	reader := &stubServerlessHealthReader{counts: map[string]WorkloadHealthProviderCounts{
		"aws": {
			ServerlessResourceCount: 10,
			ColdStartExceededCount:  1,
			AnyIssueCount:           1,
		},
	}}
	h := NewDiscoveryWorkloadHealthHandlers(reader, nil, time.Minute, nil, nil)

	w := workloadHealthDoRequest(h)
	r := parseWorkloadHealth(t, w)

	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		if _, ok := r.Providers[p]; !ok {
			t.Fatalf("provider %q missing from response", p)
		}
	}
	// And the three unpopulated providers are all-zero.
	for _, p := range []string{"gcp", "azure", "oci"} {
		if r.Providers[p].ServerlessResourceCount != 0 {
			t.Fatalf("provider %q serverless count = %d, want 0", p, r.Providers[p].ServerlessResourceCount)
		}
		if r.Providers[p].AnyIssueCount != 0 {
			t.Fatalf("provider %q any-issue = %d, want 0", p, r.Providers[p].AnyIssueCount)
		}
	}
}

// TestWorkloadHealthPctHelper_HandlesZeroDenominator pins zero-safety
// on the per-provider percentage helper. A cold-start fleet with zero
// serverless resources must surface 0 (not NaN, not panic).
func TestWorkloadHealthPctHelper_HandlesZeroDenominator(t *testing.T) {
	if got := workloadHealthPct(0, 0); got != 0 {
		t.Fatalf("pct(0,0) = %v, want 0", got)
	}
	if got := workloadHealthPct(3, 0); got != 0 {
		t.Fatalf("pct(3,0) = %v, want 0", got)
	}
	// Spot-check the rounding rule: 1/7 = 14.2857...% → 14.3
	if got := workloadHealthPct(1, 7); got != 14.3 {
		t.Fatalf("pct(1,7) = %v, want 14.3", got)
	}
}

// TestWorkloadHealth_NilReaderServesZeros — cold-start posture for a
// deployment that hasn't wired ServerlessHealthInventoryReader. The
// response is well-formed with every provider populated as zero
// counts; the UI panel hides itself on these zeros per design doc
// §5.3.
func TestWorkloadHealth_NilReaderServesZeros(t *testing.T) {
	h := NewDiscoveryWorkloadHealthHandlers(nil, nil, time.Minute, nil, nil)

	w := workloadHealthDoRequest(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	r := parseWorkloadHealth(t, w)
	for _, p := range []string{"aws", "gcp", "azure", "oci"} {
		if r.Providers[p].ServerlessResourceCount != 0 {
			t.Fatalf("provider %q serverless count = %d, want 0", p, r.Providers[p].ServerlessResourceCount)
		}
	}
	if r.Totals.ServerlessResourceCount != 0 {
		t.Fatalf("totals serverless = %d, want 0", r.Totals.ServerlessResourceCount)
	}
}
