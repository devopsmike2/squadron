// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// -- v0.89.118 slice 2 chunk 3 OCI MetricQuerier tests ----------------

// monitoringFake is the chunk-3 in-memory MonitoringClient. Implements
// MonitoringClient with deterministic per-call hooks so each test can
// return a canned response, count calls for the rate-limiter pin, or
// inject errors.
type monitoringFake struct {
	mu             sync.Mutex
	calls          int
	respondWith    []ociMetricDataPoint
	respondErr     error
	receivedQuery  []string
	receivedNS     []string
	respondPerCall func(callN int, query string) ([]ociMetricDataPoint, error)
}

func (f *monitoringFake) SummarizeMetricsData(
	ctx context.Context,
	compartmentID, namespace, query string,
	startTime, endTime time.Time,
) ([]ociMetricDataPoint, error) {
	f.mu.Lock()
	f.calls++
	callN := f.calls
	f.receivedQuery = append(f.receivedQuery, query)
	f.receivedNS = append(f.receivedNS, namespace)
	perCall := f.respondPerCall
	canned := f.respondWith
	cannedErr := f.respondErr
	f.mu.Unlock()
	if perCall != nil {
		return perCall(callN, query)
	}
	return canned, cannedErr
}

// newMetricsTestScanner wires the monitoringFake plus the default
// rate limiter onto a minimal Scanner.
func newMetricsTestScanner(t *testing.T, mf *monitoringFake) *Scanner {
	t.Helper()
	s := &Scanner{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
		Region:      "us-phoenix-1",
	}
	s.WithMonitoringClient(mf)
	// burst=1 forces each call to acquire a token (deterministic
	// 10-TPS pin).
	s.WithMonitoringRateLimiter(rate.NewLimiter(rate.Limit(OCIMonitoringRateLimitTPS), 1))
	return s
}

// TestOCIMonitoringRateLimitTPS_Constant pins the rate limit constant
// to 10. Slice 2 §12 names the value; changing it requires updating
// the runbook (chunk 5).
func TestOCIMonitoringRateLimitTPS_Constant(t *testing.T) {
	if OCIMonitoringRateLimitTPS != 10 {
		t.Errorf("OCIMonitoringRateLimitTPS = %d, want 10", OCIMonitoringRateLimitTPS)
	}
}

// TestOCIFunctionsConstants_Stable pins the metric namespace and
// metric names. The chunk-4 proposer + chunk-5 runbook reference
// these strings.
func TestOCIFunctionsConstants_Stable(t *testing.T) {
	if OCIFunctionsMetricNamespace != "oci_faas" {
		t.Errorf("OCIFunctionsMetricNamespace = %q, want %q",
			OCIFunctionsMetricNamespace, "oci_faas")
	}
	if OCIFunctionsFunctionDurationMetric != "FunctionExecutionDuration" {
		t.Errorf("OCIFunctionsFunctionDurationMetric = %q, want %q",
			OCIFunctionsFunctionDurationMetric, "FunctionExecutionDuration")
	}
	if OCIFunctionsColdStartCountMetric != "cold_start_count" {
		t.Errorf("OCIFunctionsColdStartCountMetric = %q, want %q",
			OCIFunctionsColdStartCountMetric, "cold_start_count")
	}
}

// TestScannerSatisfiesMetricQuerier — compile-time pin that the OCI
// Scanner type implements scanner.MetricQuerier.
func TestScannerSatisfiesMetricQuerier(t *testing.T) {
	var _ scanner.MetricQuerier = (*Scanner)(nil)
}

// TestOCIQueryAggregate_NoMonitoringClient_ReturnsNotImplemented —
// when the Scanner has not been wired with a MonitoringClient,
// QueryAggregate returns scanner.ErrMetricNotImplemented mirroring
// the chunk-1 skeleton's surface.
func TestOCIQueryAggregate_NoMonitoringClient_ReturnsNotImplemented(t *testing.T) {
	s := &Scanner{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		Region:      "us-phoenix-1",
	}
	_, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		OCIFunctionsFunctionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err == nil {
		t.Fatal("QueryAggregate must return an error when no monitoring client wired")
	}
	if !errors.Is(err, scanner.ErrMetricNotImplemented) {
		t.Errorf("error %q must resolve to scanner.ErrMetricNotImplemented via errors.Is", err)
	}
}

// TestOCIQueryAggregate_FunctionDuration_ReturnsP95 — slice 2 chunk 3
// acceptance test 8: function_duration with cold_start_count > 0.
// The fake returns multi-datapoint P95 values; QueryAggregate rolls
// them up via MAX (worst-case 5-minute P95 across the window).
func TestOCIQueryAggregate_FunctionDuration_ReturnsP95(t *testing.T) {
	mf := &monitoringFake{
		respondWith: []ociMetricDataPoint{
			{Timestamp: time.Now().Add(-20 * time.Minute), Value: 1200.0, SampleCount: 1},
			{Timestamp: time.Now().Add(-15 * time.Minute), Value: 4230.0, SampleCount: 1},
			{Timestamp: time.Now().Add(-10 * time.Minute), Value: 800.0, SampleCount: 1},
		},
	}
	s := newMetricsTestScanner(t, mf)
	res, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		OCIFunctionsFunctionDurationMetric,
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
		t.Errorf("SampleCount = %d, want 3", res.SampleCount)
	}
	if res.Unit != ociMetricUnitMs {
		t.Errorf("Unit = %q, want %q", res.Unit, ociMetricUnitMs)
	}
	if res.MetricName != OCIFunctionsFunctionDurationMetric {
		t.Errorf("MetricName = %q, want %q", res.MetricName, OCIFunctionsFunctionDurationMetric)
	}
	if res.Statistic != scanner.StatisticP95 {
		t.Errorf("Statistic = %q, want %q", res.Statistic, scanner.StatisticP95)
	}
	// The MQL query should embed the P95 percentile aggregation.
	if len(mf.receivedQuery) != 1 {
		t.Fatalf("expected 1 query, got %d", len(mf.receivedQuery))
	}
	q := mf.receivedQuery[0]
	if !strings.Contains(q, OCIFunctionsFunctionDurationMetric) {
		t.Errorf("query %q missing metric name", q)
	}
	if !strings.Contains(q, ".percentile(0.95)") {
		t.Errorf("query %q missing percentile(0.95) aggregation", q)
	}
	if !strings.Contains(q, "resourceId = \"ocid1.fnfunc.oc1.phx.xxx\"") {
		t.Errorf("query %q missing resourceId filter", q)
	}
}

// TestOCIQueryAggregate_ColdStartCount_ReturnsCount — slice 2 chunk 3
// acceptance test 9 helper: cold_start_count with non-zero datapoints
// returns the SUM (total cold starts in the window).
func TestOCIQueryAggregate_ColdStartCount_ReturnsCount(t *testing.T) {
	mf := &monitoringFake{
		respondWith: []ociMetricDataPoint{
			{Timestamp: time.Now().Add(-20 * time.Minute), Value: 3.0, SampleCount: 1},
			{Timestamp: time.Now().Add(-15 * time.Minute), Value: 7.0, SampleCount: 1},
			{Timestamp: time.Now().Add(-10 * time.Minute), Value: 2.0, SampleCount: 1},
		},
	}
	s := newMetricsTestScanner(t, mf)
	res, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		OCIFunctionsColdStartCountMetric,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 12.0 {
		t.Errorf("Value = %v, want SUM across datapoints (12.0)", res.Value)
	}
	if res.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", res.SampleCount)
	}
	// The MQL query should embed the SUM aggregation.
	q := mf.receivedQuery[0]
	if !strings.Contains(q, "cold_start_count") {
		t.Errorf("query %q missing metric name", q)
	}
	if !strings.Contains(q, ".sum()") {
		t.Errorf("query %q missing sum() aggregation", q)
	}
}

// TestOCIQueryAggregate_EmptyResponse_ReturnsZero — slice 2 chunk 3
// empty-response semantics: zero datapoints return Value=0,
// SampleCount=0, no error.
func TestOCIQueryAggregate_EmptyResponse_ReturnsZero(t *testing.T) {
	mf := &monitoringFake{
		respondWith: []ociMetricDataPoint{},
	}
	s := newMetricsTestScanner(t, mf)
	res, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		OCIFunctionsFunctionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("Value = %v, want 0 on empty response", res.Value)
	}
	if res.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0 on empty response", res.SampleCount)
	}
}

// TestOCIQueryAggregate_UnsupportedMetricName_ReturnsEmptyNoError —
// slice 2 substrate scope: function_duration + cold_start_count
// only. Other metric names short-circuit to an empty result with no
// error so the interface contract distinguishes "metric not
// supported" (empty) from "API call failed" (error).
func TestOCIQueryAggregate_UnsupportedMetricName_ReturnsEmptyNoError(t *testing.T) {
	mf := &monitoringFake{
		respondWith: []ociMetricDataPoint{
			{Value: 9999.0, SampleCount: 1},
		},
	}
	s := newMetricsTestScanner(t, mf)
	res, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		"some_other_metric", // unsupported
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("Value = %v, want 0 on unsupported metric", res.Value)
	}
	if res.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0 on unsupported metric", res.SampleCount)
	}
	if mf.calls != 0 {
		t.Errorf("calls = %d, want 0 (unsupported metric must short-circuit)", mf.calls)
	}
}

// TestOCIQueryAggregate_InvalidARN_ReturnsError — empty / malformed
// resource ARN returns an error from the parse step. Pinned so the
// chunk-4 detection branch's log surface points at the specific row
// that misfired.
func TestOCIQueryAggregate_InvalidARN_ReturnsError(t *testing.T) {
	mf := &monitoringFake{}
	s := newMetricsTestScanner(t, mf)
	_, err := s.QueryAggregate(
		context.Background(),
		"", // empty
		OCIFunctionsFunctionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err == nil {
		t.Fatal("expected error on empty ARN")
	}
	if mf.calls != 0 {
		t.Errorf("calls = %d, want 0 (parse must fail before dispatch)", mf.calls)
	}
}

// TestOCIWindowQuery formats hour-window MQL suffixes consistently.
func TestOCIWindowQuery(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{24 * time.Hour, "24h"},
		{168 * time.Hour, "168h"},
		{30 * time.Minute, "1h"},
		{0, "1h"},
		{2 * time.Hour, "2h"},
	}
	for _, c := range cases {
		got := ociWindowQuery(c.in)
		if got != c.want {
			t.Errorf("ociWindowQuery(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestOCIParseFunctionARN exercises both the bare-OCID + pipe-encoded
// paths.
func TestOCIParseFunctionARN(t *testing.T) {
	t.Run("bare OCID uses fallback compartment", func(t *testing.T) {
		comp, fn, err := parseOCIFunctionARN("ocid1.fnfunc.oc1.phx.xxx", "ocid1.tenancy.oc1..yyy")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if comp != "ocid1.tenancy.oc1..yyy" {
			t.Errorf("compartment = %q, want fallback %q", comp, "ocid1.tenancy.oc1..yyy")
		}
		if fn != "ocid1.fnfunc.oc1.phx.xxx" {
			t.Errorf("function = %q, want %q", fn, "ocid1.fnfunc.oc1.phx.xxx")
		}
	})
	t.Run("pipe encoded carries both", func(t *testing.T) {
		comp, fn, err := parseOCIFunctionARN("ocid1.compartment.oc1..ccc|ocid1.fnfunc.oc1.phx.xxx", "ignored")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if comp != "ocid1.compartment.oc1..ccc" {
			t.Errorf("compartment = %q", comp)
		}
		if fn != "ocid1.fnfunc.oc1.phx.xxx" {
			t.Errorf("function = %q", fn)
		}
	})
	t.Run("empty rejected", func(t *testing.T) {
		_, _, err := parseOCIFunctionARN("", "fallback")
		if err == nil {
			t.Error("expected error on empty ARN")
		}
	})
	t.Run("non OCID rejected", func(t *testing.T) {
		_, _, err := parseOCIFunctionARN("arn:aws:lambda:us-east-1:123:function:fn", "fallback")
		if err == nil {
			t.Error("expected error on non-OCID ARN")
		}
	})
}

// TestOCIRateLimiterCapsAt10TPS — slice 2 chunk 3 acceptance test 10.
// The substrate contract pins the per-tenancy limit at 10 TPS; 20
// requests should take at least ~1.5s. burst=1 means the first
// request goes through immediately and the remaining 19 each wait
// ~100ms, totalling ~1.9s. 1.5s is a comfortable lower bound that
// tolerates CI scheduler jitter.
func TestOCIRateLimiterCapsAt10TPS(t *testing.T) {
	if testing.Short() {
		t.Skip("rate limiter timing test; runs only without -short")
	}
	mf := &monitoringFake{
		respondWith: []ociMetricDataPoint{},
	}
	s := newMetricsTestScanner(t, mf)
	ctx := context.Background()
	const total = 20
	start := time.Now()
	for i := 0; i < total; i++ {
		_, err := s.QueryAggregate(ctx,
			"ocid1.fnfunc.oc1.phx.xxx",
			OCIFunctionsFunctionDurationMetric, 24*time.Hour, scanner.StatisticP95)
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond {
		t.Errorf("20 requests elapsed = %v, want >= 1.5s (rate limiter at 10 TPS)", elapsed)
	}
	if mf.calls != total {
		t.Errorf("monitoring calls = %d, want %d", mf.calls, total)
	}
}

// TestOCIQueryAggregate_ErrorPropagation surfaces the monitoring
// client's error wrapped under "summarize metrics: ...".
func TestOCIQueryAggregate_ErrorPropagation(t *testing.T) {
	mf := &monitoringFake{
		respondErr: errors.New("HTTP 500"),
	}
	s := newMetricsTestScanner(t, mf)
	_, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		OCIFunctionsFunctionDurationMetric,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "summarize metrics") {
		t.Errorf("error %q must wrap with 'summarize metrics'", err)
	}
}

// -- v0.89.122 sampling rate slice 1 chunk 1 additions ---------------------

// TestOCIFunctionsInvocationCountMetric_Constant pins the OCI
// Monitoring counter name for function_invocation_count — the
// sampling-rate-slice-1 denominator (§4.5).
func TestOCIFunctionsInvocationCountMetric_Constant(t *testing.T) {
	if OCIFunctionsInvocationCountMetric != "FunctionInvocationCount" {
		t.Fatalf("OCIFunctionsInvocationCountMetric = %q, want FunctionInvocationCount",
			OCIFunctionsInvocationCountMetric)
	}
}

// TestOCIQueryAggregate_InvocationCount_ReturnsSumOverWindow —
// acceptance test 7 (sampling rate slice 1 §11). Multi-datapoint
// response with per-resolution counts; QueryAggregate sums across
// datapoints and returns the total invocations. Mirrors the
// cold_start_count path and pins the MQL query carries .sum() and
// the right metric name.
func TestOCIQueryAggregate_InvocationCount_ReturnsSumOverWindow(t *testing.T) {
	mf := &monitoringFake{
		respondWith: []ociMetricDataPoint{
			{Timestamp: time.Now().Add(-20 * time.Minute), Value: 1200.0, SampleCount: 1},
			{Timestamp: time.Now().Add(-15 * time.Minute), Value: 800.0, SampleCount: 1},
			{Timestamp: time.Now().Add(-10 * time.Minute), Value: 3000.0, SampleCount: 1},
		},
	}
	s := newMetricsTestScanner(t, mf)
	res, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		OCIFunctionsInvocationCountMetric,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 5000.0 {
		t.Errorf("Value = %v, want SUM across datapoints (5000.0)", res.Value)
	}
	if res.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", res.SampleCount)
	}
	if res.MetricName != OCIFunctionsInvocationCountMetric {
		t.Errorf("MetricName = %q, want %q", res.MetricName, OCIFunctionsInvocationCountMetric)
	}
	// MQL query embeds the metric name + .sum() reduction.
	if len(mf.receivedQuery) != 1 {
		t.Fatalf("calls = %d, want 1", len(mf.receivedQuery))
	}
	q := mf.receivedQuery[0]
	if !strings.Contains(q, OCIFunctionsInvocationCountMetric) {
		t.Errorf("query %q missing metric name", q)
	}
	if !strings.Contains(q, ".sum()") {
		t.Errorf("query %q missing .sum() reduction", q)
	}
	// Resource id must be quoted in the MQL filter.
	if !strings.Contains(q, `resourceId = "ocid1.fnfunc.oc1.phx.xxx"`) {
		t.Errorf("query %q missing quoted resourceId filter", q)
	}
}

// -- v0.89.127 error rate slice 1 chunk 1 additions ----------------------

// TestOCIFunctionsErrorResponseCountMetric_Constant pins the OCI
// Monitoring error metric. FunctionResponseCount (oci_faas) counts
// requests that returned an error response (error codes + 429
// throttles), so it is the error-rate numerator directly. A rename
// would silently break the error-rate detection branch.
func TestOCIFunctionsErrorResponseCountMetric_Constant(t *testing.T) {
	if OCIFunctionsErrorResponseCountMetric != "FunctionResponseCount" {
		t.Fatalf("OCIFunctionsErrorResponseCountMetric = %q, want %q",
			OCIFunctionsErrorResponseCountMetric, "FunctionResponseCount")
	}
}

// TestOCIQueryAggregate_InvocationCountError_ReturnsSumOverWindow —
// acceptance test 5 (error rate slice 1 §11). Multi-datapoint
// response with per-resolution counts; the #error variant uses the
// same SUM-across-datapoints rollup as the sampling-rate sibling.
// Returns the total error count across the window.
func TestOCIQueryAggregate_InvocationCountError_ReturnsSumOverWindow(t *testing.T) {
	mf := &monitoringFake{
		respondWith: []ociMetricDataPoint{
			{Timestamp: time.Now().Add(-20 * time.Minute), Value: 30.0, SampleCount: 1},
			{Timestamp: time.Now().Add(-15 * time.Minute), Value: 20.0, SampleCount: 1},
			{Timestamp: time.Now().Add(-10 * time.Minute), Value: 40.0, SampleCount: 1},
		},
	}
	s := newMetricsTestScanner(t, mf)
	res, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		OCIFunctionsErrorResponseCountMetric,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 90.0 {
		t.Errorf("Value = %v, want SUM across datapoints (90.0)", res.Value)
	}
	if res.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", res.SampleCount)
	}
	if res.MetricName != OCIFunctionsErrorResponseCountMetric {
		t.Errorf("MetricName = %q, want %q (suffix variant echoed verbatim)",
			res.MetricName, OCIFunctionsErrorResponseCountMetric)
	}
}

// TestOCIQueryAggregate_ErrorResponseCount_MQLUsesFunctionResponseCount
// pins the error-path MQL: FunctionResponseCount is a real oci_faas
// metric, so the query selects it directly with a .sum() rollup and a
// resourceId filter — no synthetic result="error" tag (which was never
// a valid oci_faas dimension) and no "#error" suffix on the wire.
func TestOCIQueryAggregate_InvocationCountError_MQLFiltersByResultError(t *testing.T) {
	mf := &monitoringFake{respondWith: []ociMetricDataPoint{}}
	s := newMetricsTestScanner(t, mf)
	_, err := s.QueryAggregate(
		context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx",
		OCIFunctionsErrorResponseCountMetric,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mf.receivedQuery) != 1 {
		t.Fatalf("calls = %d, want 1", len(mf.receivedQuery))
	}
	q := mf.receivedQuery[0]
	// The error path queries the real FunctionResponseCount metric directly.
	if !strings.HasPrefix(q, OCIFunctionsErrorResponseCountMetric+"[") {
		t.Errorf("query %q must start with the FunctionResponseCount metric", q)
	}
	if strings.Contains(q, "#error") {
		t.Errorf("query %q leaks a synthetic #error suffix onto the wire", q)
	}
	// FunctionResponseCount IS the error metric — no result="error" tag.
	if strings.Contains(q, `result = "error"`) {
		t.Errorf("query %q must not synthesise a result=\"error\" tag (not a valid oci_faas dimension)", q)
	}
	if !strings.Contains(q, `resourceId = "ocid1.fnfunc.oc1.phx.xxx"`) {
		t.Errorf("query %q missing quoted resourceId filter", q)
	}
	if !strings.Contains(q, ".sum()") {
		t.Errorf("query %q missing .sum() reduction", q)
	}
}

// TestSplitOCIMetricSuffix_Variants pins the table-driven decode
// of the synthetic suffix convention. Adding new suffix variants
// in future slices means adding entries to this test and the
// switch in splitOCIMetricSuffix; the helper's surface stays
// stable.
func TestSplitOCIMetricSuffix_Variants(t *testing.T) {
	cases := []struct {
		in          string
		wantBase    string
		wantFilter  string
		description string
	}{
		{
			in:          OCIFunctionsErrorResponseCountMetric,
			wantBase:    OCIFunctionsErrorResponseCountMetric,
			wantFilter:  "",
			description: "FunctionResponseCount passes through verbatim (no synthetic suffix)",
		},
		{
			in:          OCIFunctionsInvocationCountMetric,
			wantBase:    OCIFunctionsInvocationCountMetric,
			wantFilter:  "",
			description: "non-suffix metric passes through verbatim",
		},
		{
			in:          OCIFunctionsFunctionDurationMetric,
			wantBase:    OCIFunctionsFunctionDurationMetric,
			wantFilter:  "",
			description: "non-counter metric passes through verbatim",
		},
	}
	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			base, flt := splitOCIMetricSuffix(c.in)
			if base != c.wantBase {
				t.Errorf("base = %q, want %q", base, c.wantBase)
			}
			if flt != c.wantFilter {
				t.Errorf("filter = %q, want %q", flt, c.wantFilter)
			}
		})
	}
}
