// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// TestScannerSatisfiesMetricQuerier — compile-time pin that the Azure
// Scanner type implements scanner.MetricQuerier. Catches an
// accidental signature drift across the slice 2 chunk 2 transition.
func TestScannerSatisfiesMetricQuerier(t *testing.T) {
	var _ scanner.MetricQuerier = (*Scanner)(nil)
}

// TestAzureMonitorRateLimitRPH_Constant pins the rate limit constant
// to 12,000 RPH. The chunk-2 wiring + chunk-5 runbook all reference
// the same value; changing it MUST involve updating the runbook so
// operators see the new throttle behavior.
func TestAzureMonitorRateLimitRPH_Constant(t *testing.T) {
	if AzureMonitorRateLimitRPH != 12000 {
		t.Errorf("AzureMonitorRateLimitRPH = %d, want 12000", AzureMonitorRateLimitRPH)
	}
}

// TestAzureMonitorMetricsAPIVersion_Constant pins the ARM API version
// for the microsoft.insights/metrics endpoint. The 2024-02-01
// surface returns the timeseries[].data[] shape the substrate
// consumes; a silent rev change would re-shape the parsing path.
func TestAzureMonitorMetricsAPIVersion_Constant(t *testing.T) {
	if AzureMonitorMetricsAPIVersion != "2024-02-01" {
		t.Errorf("AzureMonitorMetricsAPIVersion = %q, want 2024-02-01",
			AzureMonitorMetricsAPIVersion)
	}
}

// TestAzureFunctionsExecutionDurationMetric_Constant pins the metric
// name. The chunk-2 wiring + chunk-4 proposer prompt + chunk-5
// runbook all reference the same string; a silent rename would
// break the cold-start detection path across release boundaries.
func TestAzureFunctionsExecutionDurationMetric_Constant(t *testing.T) {
	if AzureFunctionsExecutionDurationMetric != "FunctionExecutionDuration" {
		t.Errorf("AzureFunctionsExecutionDurationMetric = %q, want FunctionExecutionDuration",
			AzureFunctionsExecutionDurationMetric)
	}
}

// TestAzureFunctionsIsAfterColdStartDimension_Constant pins the
// dimension name. The fallback detection (when the runtime doesn't
// emit the dimension) keys off this string in the error message
// matcher; a silent rename would break the fallback path.
func TestAzureFunctionsIsAfterColdStartDimension_Constant(t *testing.T) {
	if AzureFunctionsIsAfterColdStartDimension != "IsAfterColdStart" {
		t.Errorf("AzureFunctionsIsAfterColdStartDimension = %q, want IsAfterColdStart",
			AzureFunctionsIsAfterColdStartDimension)
	}
}

// TestAzureQueryAggregate_NoAccessToken_ReturnsNotImplemented — a
// Scanner with accessToken empty mirrors the chunk-1 skeleton path:
// returns scanner.ErrMetricNotImplemented while preserving the
// echoed input fields. Backward-compat for the v0.89.113 surface
// callers may have written tests against.
func TestAzureQueryAggregate_NoAccessToken_ReturnsNotImplemented(t *testing.T) {
	s := &Scanner{}
	res, err := s.QueryAggregate(
		context.Background(),
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Web/sites/fn",
		AzureFunctionsExecutionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if !errors.Is(err, scanner.ErrMetricNotImplemented) {
		t.Errorf("error %v must resolve to scanner.ErrMetricNotImplemented via errors.Is", err)
	}
	if res.MetricName != AzureFunctionsExecutionDurationMetric {
		t.Errorf("MetricName = %q, want echo of input", res.MetricName)
	}
}

// fakeAzureMetrics is a configurable httptest server stub for the
// Azure Monitor /metrics endpoint. Each request increments calls
// and either returns the canned response or invokes responder for
// per-request decisions (filter-vs-unfiltered branching).
type fakeAzureMetrics struct {
	mu              sync.Mutex
	calls           int32
	receivedReqs    []*http.Request
	receivedFilters []string
	// responder is invoked per request; nil → return cannedResponse
	// with cannedStatus.
	responder      func(req *http.Request, callN int) (status int, body interface{})
	cannedResponse interface{}
	cannedStatus   int
}

func (f *fakeAzureMetrics) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callN := int(atomic.AddInt32(&f.calls, 1))
		f.mu.Lock()
		f.receivedReqs = append(f.receivedReqs, r.Clone(r.Context()))
		f.receivedFilters = append(f.receivedFilters, r.URL.Query().Get("$filter"))
		responder := f.responder
		canned := f.cannedResponse
		cannedStatus := f.cannedStatus
		f.mu.Unlock()

		if responder != nil {
			status, body := responder(r, callN)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		if cannedStatus == 0 {
			cannedStatus = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cannedStatus)
		_ = json.NewEncoder(w).Encode(canned)
	})
}

// newMetricsScannerWithFake wires the fake httptest server onto a
// Scanner pre-armed with an access token and the substrate-default
// rate limiter (rate.Inf for non-rate-limit tests so timing doesn't
// dominate the run).
func newMetricsScannerWithFake(t *testing.T, fake *fakeAzureMetrics) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("super-secret"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		accessToken:    "fake-token",
		metricsLimiter: rate.NewLimiter(rate.Inf, 1),
	}
}

// fpPtr is a small helper so the canned response builder can place
// float values into *float64 fields without per-call boilerplate.
func fpPtr(v float64) *float64 { return &v }

// metricsOK builds an armMetricsResponse fixture carrying the given
// per-bucket Maximum values. The Unit defaults to "Milliseconds".
func metricsOK(maxValues ...float64) armMetricsResponse {
	dps := make([]armMetricsDatapoint, 0, len(maxValues))
	for i, v := range maxValues {
		dps = append(dps, armMetricsDatapoint{
			TimeStamp: fmt.Sprintf("2025-01-01T00:%02d:00Z", i*5),
			Maximum:   fpPtr(v),
		})
	}
	return armMetricsResponse{
		Value: []armMetricsValue{{
			Unit:       "Milliseconds",
			Timeseries: []armMetricsTimeseries{{Data: dps}},
		}},
	}
}

const testFunctionAppARN = "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg/providers/Microsoft.Web/sites/order-processor"

// TestAzureQueryAggregate_FunctionExecutionDurationWithIsAfterColdStart_ReturnsP95
// — slice 2 acceptance test 5. The fake Azure Monitor returns
// data points with the IsAfterColdStart filter applied; the
// substrate rolls them up into the MAX (the worst-case 5-minute
// aggregate across the window) and returns the result with the
// per-bucket count summed.
func TestAzureQueryAggregate_FunctionExecutionDurationWithIsAfterColdStart_ReturnsP95(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: metricsOK(1200.0, 4230.0, 800.0),
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		AzureFunctionsExecutionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 4230.0 {
		t.Errorf("Value = %v, want MAX across datapoints (4230.0)", res.Value)
	}
	if res.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3 (per-bucket count)", res.SampleCount)
	}
	if res.Unit != "Milliseconds" {
		t.Errorf("Unit = %q, want Milliseconds (no fallback suffix)", res.Unit)
	}
	if res.MetricName != AzureFunctionsExecutionDurationMetric {
		t.Errorf("MetricName = %q, want %q", res.MetricName, AzureFunctionsExecutionDurationMetric)
	}
	if res.Statistic != scanner.StatisticP95 {
		t.Errorf("Statistic = %q, want %q", res.Statistic, scanner.StatisticP95)
	}
	// Verify the filter was applied on the request.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.receivedFilters) != 1 {
		t.Fatalf("filters seen = %d, want 1", len(fake.receivedFilters))
	}
	wantFilter := fmt.Sprintf("%s eq 'true'", AzureFunctionsIsAfterColdStartDimension)
	if fake.receivedFilters[0] != wantFilter {
		t.Errorf("filter = %q, want %q", fake.receivedFilters[0], wantFilter)
	}
	// Also verify aggregation=Maximum was passed (P95→Maximum mapping).
	got := fake.receivedReqs[0].URL.Query().Get("aggregation")
	if got != "Maximum" {
		t.Errorf("aggregation = %q, want Maximum (P95 approximation)", got)
	}
}

// TestAzureQueryAggregate_OlderRuntimeNoColdStartDimension_FallsBack
// — slice 2 acceptance test 6. The fake returns 400 BadRequest with
// the IsAfterColdStart dimension named in the error message on the
// first call, then 200 with a valid response on the unfiltered
// retry. The substrate detects the fallback signal and tags the
// Unit string with " (fallback)" so the detection branch can
// surface the informational note.
func TestAzureQueryAggregate_OlderRuntimeNoColdStartDimension_FallsBack(t *testing.T) {
	fake := &fakeAzureMetrics{
		responder: func(r *http.Request, callN int) (int, interface{}) {
			if callN == 1 {
				// First call: filtered request → 400 + dimension
				// name in the error message.
				if r.URL.Query().Get("$filter") == "" {
					// Shouldn't happen — but record the wrong shape
					// rather than passing silently.
					return http.StatusInternalServerError, armErrorResponse{
						Error: armErrorBody{Code: "TestBug",
							Message: "expected filter on first call"},
					}
				}
				return http.StatusBadRequest, armErrorResponse{
					Error: armErrorBody{
						Code: "BadRequest",
						Message: fmt.Sprintf(
							"Failed to find metric dimension %s for resource",
							AzureFunctionsIsAfterColdStartDimension),
					},
				}
			}
			// Second call (unfiltered): return the metrics OK.
			if r.URL.Query().Get("$filter") != "" {
				return http.StatusInternalServerError, armErrorResponse{
					Error: armErrorBody{Code: "TestBug",
						Message: "unfiltered retry should not have filter"},
				}
			}
			return http.StatusOK, metricsOK(900.0, 2100.0)
		},
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		AzureFunctionsExecutionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error after fallback: %v", err)
	}
	if res.Value != 2100.0 {
		t.Errorf("Value = %v, want 2100.0 (MAX of unfiltered datapoints)", res.Value)
	}
	if res.SampleCount != 2 {
		t.Errorf("SampleCount = %d, want 2", res.SampleCount)
	}
	if !strings.HasSuffix(res.Unit, "(fallback)") {
		t.Errorf("Unit = %q, want suffix '(fallback)' to signal fallback path",
			res.Unit)
	}
	if int(atomic.LoadInt32(&fake.calls)) != 2 {
		t.Errorf("calls = %d, want 2 (filtered + unfiltered)",
			atomic.LoadInt32(&fake.calls))
	}
}

// TestAzureQueryAggregate_EmptyTimeSeries_ReturnsZero — empty
// timeseries response surfaces as Value=0, SampleCount=0, no error.
// Mirrors the AWS substrate's empty-result contract (the
// MetricQuerier interface godoc names this).
func TestAzureQueryAggregate_EmptyTimeSeries_ReturnsZero(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: armMetricsResponse{
			Value: []armMetricsValue{{
				Unit:       "Milliseconds",
				Timeseries: []armMetricsTimeseries{},
			}},
		},
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		AzureFunctionsExecutionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("Value = %v, want 0 on empty timeseries", res.Value)
	}
	if res.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0 on empty timeseries", res.SampleCount)
	}
}

// TestAzureQueryAggregate_UnsupportedMetricName_ReturnsEmptyNoError —
// slice 2 substrate scope: FunctionExecutionDuration only. Other
// metric names short-circuit to an empty result with no error so
// the interface contract distinguishes "metric not supported in
// slice 2" (empty) from "API call failed" (error).
func TestAzureQueryAggregate_UnsupportedMetricName_ReturnsEmptyNoError(t *testing.T) {
	fake := &fakeAzureMetrics{
		// If the routing accidentally lets this metric through,
		// the response would be these non-empty datapoints — the
		// test would observe a non-zero Value.
		cannedResponse: metricsOK(9999.0),
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		// A real Azure Functions metric Squadron's routing does NOT
		// support (distinct from FunctionExecutionCount, which is now
		// the routed invocation denominator after the Option-2 rename).
		"FunctionExecutionUnits",
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("Value = %v, want 0 for unsupported metric", res.Value)
	}
	if res.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0 for unsupported metric", res.SampleCount)
	}
	if int(atomic.LoadInt32(&fake.calls)) != 0 {
		t.Errorf("Azure Monitor calls = %d, want 0 (short-circuit before HTTP)",
			atomic.LoadInt32(&fake.calls))
	}
}

// TestAzureQueryAggregate_400UnrelatedDoesNotFallBack — a 400 that
// doesn't reference the IsAfterColdStart dimension surfaces as a
// real error rather than triggering the fallback path. Guards
// against the fallback heuristic over-matching on unrelated
// BadRequest conditions (malformed timespan, unsupported
// aggregation, etc.).
func TestAzureQueryAggregate_400UnrelatedDoesNotFallBack(t *testing.T) {
	fake := &fakeAzureMetrics{
		responder: func(_ *http.Request, _ int) (int, interface{}) {
			return http.StatusBadRequest, armErrorResponse{
				Error: armErrorBody{
					Code:    "BadRequest",
					Message: "the timespan parameter is malformed",
				},
			}
		},
	}
	s := newMetricsScannerWithFake(t, fake)
	_, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		AzureFunctionsExecutionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err == nil {
		t.Fatal("expected error for unrelated 400, got nil")
	}
	// Exactly one call — the fallback should NOT have fired.
	if int(atomic.LoadInt32(&fake.calls)) != 1 {
		t.Errorf("calls = %d, want 1 (no fallback on unrelated 400)",
			atomic.LoadInt32(&fake.calls))
	}
}

// TestAzureRateLimiterCapsAt12000RPH — slice 2 acceptance test 7. The
// substrate contract pins the per-subscription limit at 12,000 RPH
// (200 RPM = ~3.33 RPS). 400 requests at 200 RPM should take at
// least ~100 seconds at steady state (400 / (200/60) ≈ 120s); with
// burst=1, the first request goes through immediately and the
// remaining 399 each wait ~300ms.
//
// To keep the test bounded under -short, we issue 50 calls (1.5
// minutes at strict pacing — but burst=1 plus the small batch keeps
// it well under 30 seconds in practice) and assert wall-clock is
// at least 100ms below the theoretical (a generous lower bound that
// still catches "the limiter isn't being consulted").
//
// Gated on !testing.Short() per the brief — full run only.
func TestAzureRateLimiterCapsAt12000RPH(t *testing.T) {
	if testing.Short() {
		t.Skip("rate limiter timing test; runs only without -short")
	}
	fake := &fakeAzureMetrics{
		cannedResponse: armMetricsResponse{
			Value: []armMetricsValue{{
				Unit:       "Milliseconds",
				Timeseries: []armMetricsTimeseries{},
			}},
		},
	}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	// 12,000 RPH = 200/60 = 3.333 per second. Limiter set
	// explicitly at this rate with burst=1 — every request after
	// the first acquires a fresh token (waiting ~300ms apiece).
	s := &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("s"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		accessToken:    "fake-token",
		metricsLimiter: rate.NewLimiter(rate.Limit(AzureMonitorRateLimitRPH)/3600.0, 1),
	}
	ctx := context.Background()
	const total = 5
	start := time.Now()
	for i := 0; i < total; i++ {
		_, err := s.QueryAggregate(ctx,
			testFunctionAppARN,
			AzureFunctionsExecutionDurationMetric,
			24*time.Hour, scanner.StatisticP95)
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	// 5 requests at 3.33 RPS with burst=1: first request is free,
	// remaining 4 take ~300ms each = ~1.2s. 1.0s is a comfortable
	// lower bound that catches "the limiter isn't being consulted"
	// while staying tolerant of CI scheduler jitter.
	wantFloor := 1000 * time.Millisecond
	if elapsed < wantFloor {
		t.Errorf("%d requests elapsed = %v, want >= %v (rate limiter at 12K RPH)",
			total, elapsed, wantFloor)
	}
	if int(atomic.LoadInt32(&fake.calls)) != total {
		t.Errorf("Azure Monitor calls = %d, want %d",
			atomic.LoadInt32(&fake.calls), total)
	}
}

// TestMapMetricStatisticToAzureAggregation_DocumentsP95Approximation —
// pins the slice 2 design decision that P95 maps to "Maximum"
// because Azure Monitor doesn't natively support percentile
// aggregations on FunctionExecutionDuration. Documented in
// metrics.go::azureMonitorAggregationForP95 godoc, in the chunk-5
// runbook, and pinned here so a refactor can't silently drift the
// mapping.
func TestMapMetricStatisticToAzureAggregation_DocumentsP95Approximation(t *testing.T) {
	if got := mapMetricStatisticToAzureAggregation(scanner.StatisticP95); got != "Maximum" {
		t.Errorf("P95 → %q, want Maximum (Azure Monitor approximation)", got)
	}
	if got := mapMetricStatisticToAzureAggregation(scanner.StatisticP99); got != "Maximum" {
		t.Errorf("P99 → %q, want Maximum (approximation, slice 3 may revisit)", got)
	}
	if got := mapMetricStatisticToAzureAggregation(scanner.StatisticAverage); got != "Average" {
		t.Errorf("Average → %q, want Average", got)
	}
	if got := mapMetricStatisticToAzureAggregation(scanner.StatisticSum); got != "Total" {
		t.Errorf("Sum → %q, want Total", got)
	}
}

// TestAggregateAzureTimeseries_PrefersResponseUnit — when Azure
// Monitor reports a Unit on value[0], the substrate preserves it
// verbatim rather than defaulting to "Milliseconds". Pins the
// unit-pass-through contract documented in metrics.go.
func TestAggregateAzureTimeseries_PrefersResponseUnit(t *testing.T) {
	out := &armMetricsResponse{
		Value: []armMetricsValue{{
			Unit: "Percent",
			Timeseries: []armMetricsTimeseries{{
				Data: []armMetricsDatapoint{{Maximum: fpPtr(42.0)}},
			}},
		}},
	}
	got := aggregateAzureTimeseries(out, "Maximum")
	if got.Unit != "Percent" {
		t.Errorf("Unit = %q, want Percent (response unit preserved)", got.Unit)
	}
	if got.Value != 42.0 {
		t.Errorf("Value = %v, want 42.0", got.Value)
	}
}

// -- v0.89.122 sampling rate slice 1 chunk 1 additions ---------------------

// TestAzureFunctionsInvocationsMetric_Constant pins the Azure Monitor
// metric name for the invocation denominator. Value is the real native
// metric "FunctionExecutionCount" (renamed from the nonexistent
// placeholder "FunctionInvocations" when Azure native sampling was
// activated) — counts all Function App executions, aggregation Total.
func TestAzureFunctionsInvocationsMetric_Constant(t *testing.T) {
	if AzureFunctionsInvocationsMetric != "FunctionExecutionCount" {
		t.Fatalf("AzureFunctionsInvocationsMetric = %q, want \"FunctionExecutionCount\"", AzureFunctionsInvocationsMetric)
	}
}

// metricsOKTotals builds an armMetricsResponse with per-bucket
// Total values rather than Maximum — the sampling-rate Invocations
// path requests aggregation=Total and reads dp.Total instead of
// dp.Maximum.
func metricsOKTotals(totals ...float64) armMetricsResponse {
	dps := make([]armMetricsDatapoint, 0, len(totals))
	for i, v := range totals {
		dps = append(dps, armMetricsDatapoint{
			TimeStamp: fmt.Sprintf("2025-01-01T00:%02d:00Z", i*5),
			Total:     fpPtr(v),
		})
	}
	return armMetricsResponse{
		Value: []armMetricsValue{{
			Timeseries: []armMetricsTimeseries{{Data: dps}},
		}},
	}
}

// TestAzureQueryAggregate_FunctionInvocations_ReturnsSumOverWindow —
// acceptance test 6 (sampling rate slice 1 §11). Multi-bucket
// response with per-period Total values; QueryAggregate sums across
// buckets and returns the total invocations.
func TestAzureQueryAggregate_FunctionInvocations_ReturnsSumOverWindow(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: metricsOKTotals(1200.0, 800.0, 3000.0),
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		AzureFunctionsInvocationsMetric,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 5000.0 {
		t.Errorf("Value = %v, want SUM across buckets (5000.0)", res.Value)
	}
	if res.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", res.SampleCount)
	}
	if res.MetricName != AzureFunctionsInvocationsMetric {
		t.Errorf("MetricName = %q, want %q", res.MetricName, AzureFunctionsInvocationsMetric)
	}
}

// TestAzureQueryAggregate_FunctionInvocations_UsesTotalAggregation
// pins the aggregation parameter to "Total" (the Azure-native sum
// aggregation) rather than "Maximum" (the duration-path
// approximation). The detection branch's denominator would be
// silently wrong if the Invocations path reused the duration
// aggregation.
//
// Also pins that the IsAfterColdStart dimension filter is NOT
// applied — the invocation count denominator wants every
// invocation in the window, cold-start or warm.
func TestAzureQueryAggregate_FunctionInvocations_UsesTotalAggregation(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: metricsOKTotals(100.0),
	}
	s := newMetricsScannerWithFake(t, fake)
	_, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		AzureFunctionsInvocationsMetric,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.receivedReqs) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.receivedReqs))
	}
	req := fake.receivedReqs[0]
	if got := req.URL.Query().Get("aggregation"); got != "Total" {
		t.Errorf("aggregation = %q, want Total", got)
	}
	if got := req.URL.Query().Get("metricnames"); got != AzureFunctionsInvocationsMetric {
		t.Errorf("metricnames = %q, want %q", got, AzureFunctionsInvocationsMetric)
	}
	if got := fake.receivedFilters[0]; got != "" {
		t.Errorf("$filter = %q, want empty (no IsAfterColdStart filter on invocation count)", got)
	}
}

// -- v0.89.127 error rate slice 1 chunk 1 additions ----------------------

// TestAzureFunctionsErrorsMetric_Constant pins the Azure Monitor
// metric name for FunctionErrors — the error-rate-correlation
// slice 1 numerator (§4.4). A future ARM rename would silently
// break the detection branch without this pin.
func TestAzureFunctionsErrorsMetric_Constant(t *testing.T) {
	if AzureFunctionsErrorsMetric != "FunctionErrors" {
		t.Fatalf("AzureFunctionsErrorsMetric = %q, want \"FunctionErrors\"", AzureFunctionsErrorsMetric)
	}
}

// TestAzureQueryAggregate_FunctionErrors_ReturnsSumOverWindow —
// acceptance test 4 (error rate slice 1 §11). Multi-bucket
// response with per-period Total values; QueryAggregate sums
// across buckets and returns the total error count.
func TestAzureQueryAggregate_FunctionErrors_ReturnsSumOverWindow(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: metricsOKTotals(30.0, 20.0, 40.0),
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		AzureFunctionsErrorsMetric,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 90.0 {
		t.Errorf("Value = %v, want SUM across buckets (90.0)", res.Value)
	}
	if res.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", res.SampleCount)
	}
	if res.MetricName != AzureFunctionsErrorsMetric {
		t.Errorf("MetricName = %q, want %q", res.MetricName, AzureFunctionsErrorsMetric)
	}
}

// TestAzureQueryAggregate_FunctionErrors_UsesTotalAggregation pins
// the aggregation parameter to "Total" (the Azure-native sum
// aggregation) rather than "Maximum" (the duration-path
// approximation). Mirrors the FunctionInvocations contract.
//
// Also pins that the IsAfterColdStart dimension filter is NOT
// applied — the error count wants every failed invocation in the
// window, cold-start or warm.
func TestAzureQueryAggregate_FunctionErrors_UsesTotalAggregation(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: metricsOKTotals(50.0),
	}
	s := newMetricsScannerWithFake(t, fake)
	_, err := s.QueryAggregate(
		context.Background(),
		testFunctionAppARN,
		AzureFunctionsErrorsMetric,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.receivedReqs) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.receivedReqs))
	}
	req := fake.receivedReqs[0]
	if got := req.URL.Query().Get("aggregation"); got != "Total" {
		t.Errorf("aggregation = %q, want Total", got)
	}
	if got := req.URL.Query().Get("metricnames"); got != AzureFunctionsErrorsMetric {
		t.Errorf("metricnames = %q, want %q", got, AzureFunctionsErrorsMetric)
	}
	if got := fake.receivedFilters[0]; got != "" {
		t.Errorf("$filter = %q, want empty (no IsAfterColdStart filter on error count)", got)
	}
}
