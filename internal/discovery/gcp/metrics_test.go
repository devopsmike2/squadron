// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

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

// metricsFake is the slice 2 chunk 1 in-memory Cloud Monitoring
// client. Implements metricsClient with deterministic per-call hooks
// so each test can return a canned response, simulate an empty
// response, or count calls for the rate-limiter pin. Mirrors the AWS
// slice 1 chunk 2 cwFake shape so the test scaffolding stays
// recognisable across providers.
type metricsFake struct {
	mu             sync.Mutex
	calls          int
	respondWith    []TimeSeriesPoint
	respondErr     error
	respondPerCall func(int, string, string, string) ([]TimeSeriesPoint, error)
	receivedFilter []string
	receivedStat   []string
	receivedProj   []string
}

func (f *metricsFake) QueryTimeSeries(
	_ context.Context,
	projectName string,
	filter string,
	_, _ time.Time,
	stat string,
) ([]TimeSeriesPoint, error) {
	f.mu.Lock()
	f.calls++
	callN := f.calls
	f.receivedFilter = append(f.receivedFilter, filter)
	f.receivedStat = append(f.receivedStat, stat)
	f.receivedProj = append(f.receivedProj, projectName)
	perCall := f.respondPerCall
	canned := f.respondWith
	cannedErr := f.respondErr
	f.mu.Unlock()
	if perCall != nil {
		return perCall(callN, projectName, filter, stat)
	}
	return canned, cannedErr
}

// newMetricsTestScanner builds a GCP Scanner suitable for the
// MetricQuerier skeleton-path tests. The Cloud Monitoring wiring
// stays nil so QueryAggregate's skeleton path returns
// scanner.ErrMetricNotImplemented.
func newMetricsTestScanner() *Scanner {
	return &Scanner{ProjectID: "test-project"}
}

// newMetricsTestScannerWithFake wires the metricsFake plus the
// default 60-RPM rate limiter onto a Scanner. The returned Scanner
// satisfies scanner.MetricQuerier with real chunk-1 behaviour —
// empty fake responses surface as the substrate's empty-result
// contract, etc.
func newMetricsTestScannerWithFake(t *testing.T, f *metricsFake) *Scanner {
	t.Helper()
	s := newMetricsTestScanner().WithMetricsClient(f)
	// burst=1 makes each call acquire a token rather than coalescing
	// into a free-running burst — deterministic for the 60-RPM pin.
	s.WithMetricsRateLimiter(rate.NewLimiter(rate.Every(time.Second), 1))
	return s
}

// TestGCPQueryAggregate_Skeleton_ReturnsNotImplemented — slice 2
// chunk 1 acceptance: the GCP MetricQuerier skeleton (no
// metricsClient wired) returns scanner.ErrMetricNotImplemented. The
// follow-up chunk that wires the real Cloud Monitoring SDK replaces
// the skeleton; until then callers MUST observe the sentinel.
func TestGCPQueryAggregate_Skeleton_ReturnsNotImplemented(t *testing.T) {
	s := newMetricsTestScanner()
	_, err := s.QueryAggregate(
		context.Background(),
		"projects/test-project/locations/us-central1/services/svc",
		CloudRunRequestLatenciesMetricType,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err == nil {
		t.Fatal("QueryAggregate skeleton must return an error")
	}
	if !errors.Is(err, scanner.ErrMetricNotImplemented) {
		t.Errorf("error %q must resolve to scanner.ErrMetricNotImplemented via errors.Is", err)
	}
}

// TestGCPCloudMonitoringRateLimitRPM_Constant pins the rate limit
// constant at 60 RPM. The runbook documents the choice; any change
// must update both this test and the runbook.
func TestGCPCloudMonitoringRateLimitRPM_Constant(t *testing.T) {
	if GCPCloudMonitoringRateLimitRPM != 60 {
		t.Errorf("GCPCloudMonitoringRateLimitRPM = %d, want 60", GCPCloudMonitoringRateLimitRPM)
	}
}

// TestCloudRunRequestLatenciesMetricType_Constant pins the Cloud
// Monitoring metric type identifier for Cloud Run latency.
func TestCloudRunRequestLatenciesMetricType_Constant(t *testing.T) {
	if CloudRunRequestLatenciesMetricType != "run.googleapis.com/request_latencies" {
		t.Errorf("CloudRunRequestLatenciesMetricType = %q, want run.googleapis.com/request_latencies", CloudRunRequestLatenciesMetricType)
	}
}

// TestCloudFunctionsExecutionTimesMetricType_Constant pins the Cloud
// Monitoring metric type identifier for Cloud Functions execution
// time.
func TestCloudFunctionsExecutionTimesMetricType_Constant(t *testing.T) {
	want := "cloudfunctions.googleapis.com/function/execution_times"
	if CloudFunctionsExecutionTimesMetricType != want {
		t.Errorf("CloudFunctionsExecutionTimesMetricType = %q, want %q", CloudFunctionsExecutionTimesMetricType, want)
	}
}

// TestGCPQueryAggregate_CloudRun_ReturnsP95FromCloudMonitoring —
// slice 2 §11 acceptance test 1. The fake returns a multi-point
// response carrying ALIGN_PERCENTILE_95 values; QueryAggregate rolls
// those up into the MAX (the worst-case 5-minute P95 across the
// window) and returns the result with SampleCount summed.
func TestGCPQueryAggregate_CloudRun_ReturnsP95FromCloudMonitoring(t *testing.T) {
	now := time.Now().UTC()
	f := &metricsFake{
		respondWith: []TimeSeriesPoint{
			{Value: 1200.0, SampleCount: 20, StartTime: now.Add(-15 * time.Minute), EndTime: now.Add(-10 * time.Minute)},
			{Value: 4230.0, SampleCount: 40, StartTime: now.Add(-10 * time.Minute), EndTime: now.Add(-5 * time.Minute)},
			{Value: 800.0, SampleCount: 10, StartTime: now.Add(-5 * time.Minute), EndTime: now},
		},
	}
	s := newMetricsTestScannerWithFake(t, f)
	const arn = "projects/test-project/locations/us-central1/services/order-processor"
	res, err := s.QueryAggregate(
		context.Background(),
		arn,
		CloudRunRequestLatenciesMetricType,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 4230.0 {
		t.Errorf("Value = %v, want MAX across points (4230.0)", res.Value)
	}
	if res.SampleCount != 70 {
		t.Errorf("SampleCount = %d, want 70 (20+40+10)", res.SampleCount)
	}
	if res.Unit != "ms" {
		t.Errorf("Unit = %q, want ms", res.Unit)
	}
	if res.MetricName != CloudRunRequestLatenciesMetricType {
		t.Errorf("MetricName = %q, want %q", res.MetricName, CloudRunRequestLatenciesMetricType)
	}
	if res.Statistic != scanner.StatisticP95 {
		t.Errorf("Statistic = %q, want %q", res.Statistic, scanner.StatisticP95)
	}
	if res.ResourceARN != arn {
		t.Errorf("ResourceARN = %q, want %q", res.ResourceARN, arn)
	}
	// Filter contains the service name + 2xx response code filter.
	if len(f.receivedFilter) != 1 {
		t.Fatalf("filter calls = %d, want 1", len(f.receivedFilter))
	}
	flt := f.receivedFilter[0]
	if !strings.Contains(flt, "service_name") || !strings.Contains(flt, `"order-processor"`) {
		t.Errorf("filter %q missing service_name=order-processor", flt)
	}
	if !strings.Contains(flt, `response_code_class = "2xx"`) {
		t.Errorf("filter %q missing 2xx response_code_class filter", flt)
	}
	if !strings.Contains(flt, CloudRunRequestLatenciesMetricType) {
		t.Errorf("filter %q missing metric type", flt)
	}
	// Project name is the projects/{p} form.
	if f.receivedProj[0] != "projects/test-project" {
		t.Errorf("project = %q, want projects/test-project", f.receivedProj[0])
	}
	// Statistic is mapped to ALIGN_PERCENTILE_95.
	if f.receivedStat[0] != "ALIGN_PERCENTILE_95" {
		t.Errorf("stat = %q, want ALIGN_PERCENTILE_95", f.receivedStat[0])
	}
}

// TestGCPQueryAggregate_CloudFunctions_ReturnsP95 — slice 2 §11
// acceptance test 3. Cloud Functions execution_times metric path.
// Mirrors the Cloud Run test but with the function-side filter and
// metric type.
func TestGCPQueryAggregate_CloudFunctions_ReturnsP95(t *testing.T) {
	f := &metricsFake{
		respondWith: []TimeSeriesPoint{
			{Value: 950.0, SampleCount: 30},
			{Value: 1850.0, SampleCount: 50},
		},
	}
	s := newMetricsTestScannerWithFake(t, f)
	const arn = "projects/test-project/locations/us-central1/functions/payment-webhook"
	res, err := s.QueryAggregate(
		context.Background(),
		arn,
		CloudFunctionsExecutionTimesMetricType,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 1850.0 {
		t.Errorf("Value = %v, want MAX across points (1850.0)", res.Value)
	}
	if res.SampleCount != 80 {
		t.Errorf("SampleCount = %d, want 80 (30+50)", res.SampleCount)
	}
	if res.Unit != "ms" {
		t.Errorf("Unit = %q, want ms", res.Unit)
	}
	flt := f.receivedFilter[0]
	if !strings.Contains(flt, "function_name") || !strings.Contains(flt, `"payment-webhook"`) {
		t.Errorf("filter %q missing function_name=payment-webhook", flt)
	}
	if !strings.Contains(flt, `status = "ok"`) {
		t.Errorf("filter %q missing status=ok filter", flt)
	}
}

// TestGCPQueryAggregate_EmptyTimeSeriesResponse_ReturnsZero —
// slice 2 §11 acceptance test 2. Empty points → Value=0,
// SampleCount=0, no error. Matches the MetricQuerier interface
// contract godoc.
func TestGCPQueryAggregate_EmptyTimeSeriesResponse_ReturnsZero(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{}}
	s := newMetricsTestScannerWithFake(t, f)
	res, err := s.QueryAggregate(
		context.Background(),
		"projects/test-project/locations/us-central1/services/quiet-svc",
		CloudRunRequestLatenciesMetricType,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("Value = %v, want 0 on empty points", res.Value)
	}
	if res.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0 on empty points", res.SampleCount)
	}
	if res.ObservedAt.IsZero() {
		t.Error("ObservedAt should be set even on empty response")
	}
}

// TestGCPQueryAggregate_UnsupportedMetricName_ReturnsEmptyNoError —
// slice 2 substrate scope is limited to two metrics; unsupported
// names return an empty result with no error so the interface
// contract distinguishes "metric not supported" from "API failed".
func TestGCPQueryAggregate_UnsupportedMetricName_ReturnsEmptyNoError(t *testing.T) {
	f := &metricsFake{}
	s := newMetricsTestScannerWithFake(t, f)
	res, err := s.QueryAggregate(
		context.Background(),
		"projects/test-project/locations/us-central1/services/svc",
		"compute.googleapis.com/instance/cpu/utilization",
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 || res.SampleCount != 0 {
		t.Errorf("expected empty result, got Value=%v SampleCount=%d", res.Value, res.SampleCount)
	}
	if f.calls != 0 {
		t.Errorf("Cloud Monitoring calls = %d, want 0 (short-circuit on unsupported metric)", f.calls)
	}
}

// TestGCPQueryAggregate_CloudRunMetricOnFunctionARN_EmptyNoError —
// when the resource kind doesn't match the metric type (e.g. Cloud
// Run latency metric on a Cloud Function ARN), the substrate
// returns empty without an error.
func TestGCPQueryAggregate_CloudRunMetricOnFunctionARN_EmptyNoError(t *testing.T) {
	f := &metricsFake{}
	s := newMetricsTestScannerWithFake(t, f)
	res, err := s.QueryAggregate(
		context.Background(),
		"projects/test-project/locations/us-central1/functions/some-fn",
		CloudRunRequestLatenciesMetricType,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 || res.SampleCount != 0 {
		t.Errorf("expected empty result, got Value=%v SampleCount=%d", res.Value, res.SampleCount)
	}
	if f.calls != 0 {
		t.Errorf("Cloud Monitoring calls = %d, want 0", f.calls)
	}
}

// TestGCPQueryAggregate_InvalidResourceName_ReturnsError — when the
// resource name doesn't match the GCP fully-qualified shape, the
// substrate surfaces an error so callers can log + skip the row.
func TestGCPQueryAggregate_InvalidResourceName_ReturnsError(t *testing.T) {
	f := &metricsFake{}
	s := newMetricsTestScannerWithFake(t, f)
	_, err := s.QueryAggregate(
		context.Background(),
		"arn:aws:lambda:us-east-1:123:function:not-gcp",
		CloudRunRequestLatenciesMetricType,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err == nil {
		t.Fatal("expected error for non-GCP ARN")
	}
	if !strings.Contains(err.Error(), "parse resource name") {
		t.Errorf("error %q must wrap parseGCPResourceName", err)
	}
}

// TestGCPRateLimiterCapsAt60RPM — slice 2 §11 acceptance test 4.
// The substrate contract pins the per-project limit at 60 RPM; 120
// requests should take at least ~60 seconds (120 / 60 = 2 minutes
// at steady state? — actually at 60 RPM = 1 RPS with burst=1, the
// first call is free, the remaining 119 each wait ~1s, totalling
// ~119s). To stay within reasonable test wall-clock the brief
// specifies asserting "duration >= 50s" as a comfortable lower
// bound that catches "the limiter isn't being consulted" while
// staying tolerant of CI scheduler jitter.
//
// Gated on !testing.Short(); the rate-limiter timing test is the
// only test in this suite that has a measurable wall-clock cost.
func TestGCPRateLimiterCapsAt60RPM(t *testing.T) {
	if testing.Short() {
		t.Skip("rate limiter timing test; runs only without -short")
	}
	f := &metricsFake{respondWith: []TimeSeriesPoint{}}
	s := newMetricsTestScannerWithFake(t, f)
	ctx := context.Background()
	const total = 120
	start := time.Now()
	for i := 0; i < total; i++ {
		_, err := s.QueryAggregate(ctx,
			"projects/test-project/locations/us-central1/services/rate-test",
			CloudRunRequestLatenciesMetricType, 24*time.Hour, scanner.StatisticP95)
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	// At 1 RPS with burst=1, 120 requests should take ~119s
	// (first request is free, remaining 119 take ~1s each). 50s
	// is the brief-specified lower bound — comfortable margin for
	// CI scheduler jitter and clock granularity.
	if elapsed < 50*time.Second {
		t.Errorf("120 requests elapsed = %v, want >= 50s (rate limiter at 60 RPM)", elapsed)
	}
	if f.calls != total {
		t.Errorf("Cloud Monitoring calls = %d, want %d", f.calls, total)
	}
}

// TestMapStatToGCP_Mapping — pin the scanner.MetricStatistic →
// Cloud Monitoring per-series-aligner mapping.
func TestMapStatToGCP_Mapping(t *testing.T) {
	cases := []struct {
		in   scanner.MetricStatistic
		want string
	}{
		{scanner.StatisticP95, "ALIGN_PERCENTILE_95"},
		{scanner.StatisticP99, "ALIGN_PERCENTILE_99"},
		{scanner.StatisticAverage, "ALIGN_PERCENTILE_95"},
		{scanner.StatisticSum, "ALIGN_PERCENTILE_95"},
	}
	for _, c := range cases {
		if got := mapStatToGCP(c.in); got != c.want {
			t.Errorf("mapStatToGCP(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseGCPResourceName_CloudRunFormat_Parses — happy path for
// Cloud Run resource names.
func TestParseGCPResourceName_CloudRunFormat_Parses(t *testing.T) {
	p, k, n, err := parseGCPResourceName("projects/my-proj/locations/us-central1/services/web-api")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "my-proj" {
		t.Errorf("project = %q, want my-proj", p)
	}
	if k != "services" {
		t.Errorf("kind = %q, want services", k)
	}
	if n != "web-api" {
		t.Errorf("name = %q, want web-api", n)
	}
}

// TestParseGCPResourceName_CloudFunctionsFormat_Parses — happy path
// for Cloud Functions resource names.
func TestParseGCPResourceName_CloudFunctionsFormat_Parses(t *testing.T) {
	p, k, n, err := parseGCPResourceName("projects/my-proj/locations/us-east1/functions/payment-webhook")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "my-proj" {
		t.Errorf("project = %q, want my-proj", p)
	}
	if k != "functions" {
		t.Errorf("kind = %q, want functions", k)
	}
	if n != "payment-webhook" {
		t.Errorf("name = %q, want payment-webhook", n)
	}
}

// TestParseGCPResourceName_InvalidFormat_Errors — table-driven
// coverage of the rejected shapes.
func TestParseGCPResourceName_InvalidFormat_Errors(t *testing.T) {
	cases := []string{
		"",
		"not-a-resource-path",
		"arn:aws:lambda:us-east-1:123:function:fn",
		"projects/my-proj",
		"projects/my-proj/locations/us-central1",
		"projects/my-proj/locations/us-central1/services",
		"projects//locations/us-central1/services/svc", // empty project
		"projects/my-proj//us-central1/services/svc",   // wrong second key
	}
	for _, in := range cases {
		_, _, _, err := parseGCPResourceName(in)
		if err == nil {
			t.Errorf("parseGCPResourceName(%q): expected error, got nil", in)
		}
	}
}

// -- v0.89.122 sampling rate slice 1 chunk 1 additions ---------------------

// TestCloudRunRequestCountMetricType_Constant pins the metric type
// for Cloud Run request count — the sampling-rate denominator
// (§4.2). Future SDK rename would silently break the
// observed/expected ratio detection without this pin.
func TestCloudRunRequestCountMetricType_Constant(t *testing.T) {
	if CloudRunRequestCountMetricType != "run.googleapis.com/request_count" {
		t.Fatalf("CloudRunRequestCountMetricType = %q, want run.googleapis.com/request_count",
			CloudRunRequestCountMetricType)
	}
}

// TestCloudFunctionsExecutionCountMetricType_Constant pins the metric
// type for Cloud Functions execution count (§4.3).
func TestCloudFunctionsExecutionCountMetricType_Constant(t *testing.T) {
	if CloudFunctionsExecutionCountMetricType != "cloudfunctions.googleapis.com/function/execution_count" {
		t.Fatalf("CloudFunctionsExecutionCountMetricType = %q, want cloudfunctions.googleapis.com/function/execution_count",
			CloudFunctionsExecutionCountMetricType)
	}
}

// TestGCPQueryAggregate_RequestCount_ReturnsSumOverWindow —
// acceptance test 5a (sampling rate slice 1 §11). Multi-point
// response; QueryAggregate sums across periods (not MAX like the
// latency path) and returns the total. Mirrors the AWS Invocations
// SUM-rollup contract for cross-cloud parity.
func TestGCPQueryAggregate_RequestCount_ReturnsSumOverWindow(t *testing.T) {
	f := &metricsFake{
		respondWith: []TimeSeriesPoint{
			{Value: 1200.0, SampleCount: 1},
			{Value: 800.0, SampleCount: 1},
			{Value: 3000.0, SampleCount: 1},
		},
	}
	s := newMetricsTestScannerWithFake(t, f)
	const arn = "projects/test-project/locations/us-central1/services/checkout"
	res, err := s.QueryAggregate(
		context.Background(),
		arn,
		CloudRunRequestCountMetricType,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 5000.0 {
		t.Errorf("Value = %v, want SUM across points (5000.0), NOT MAX (3000.0)", res.Value)
	}
	if res.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3", res.SampleCount)
	}
	if res.MetricName != CloudRunRequestCountMetricType {
		t.Errorf("MetricName = %q, want %q", res.MetricName, CloudRunRequestCountMetricType)
	}
	// Count metrics use ALIGN_DELTA so each per-period value is the
	// per-period delta — not the percentile aligner the latency path
	// uses.
	if f.receivedStat[0] != "ALIGN_DELTA" {
		t.Errorf("aligner = %q, want ALIGN_DELTA", f.receivedStat[0])
	}
}

// TestGCPQueryAggregate_ExecutionCount_ReturnsSumOverWindow —
// acceptance test 5b. Cloud Functions execution_count path. Same
// SUM rollup as the Cloud Run request_count case.
func TestGCPQueryAggregate_ExecutionCount_ReturnsSumOverWindow(t *testing.T) {
	f := &metricsFake{
		respondWith: []TimeSeriesPoint{
			{Value: 250.0, SampleCount: 1},
			{Value: 750.0, SampleCount: 1},
		},
	}
	s := newMetricsTestScannerWithFake(t, f)
	const arn = "projects/test-project/locations/us-central1/functions/report-builder"
	res, err := s.QueryAggregate(
		context.Background(),
		arn,
		CloudFunctionsExecutionCountMetricType,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 1000.0 {
		t.Errorf("Value = %v, want SUM (1000.0)", res.Value)
	}
	if res.SampleCount != 2 {
		t.Errorf("SampleCount = %d, want 2", res.SampleCount)
	}
	// Function-side filter applies status="ok" rather than the
	// response_code_class filter the Cloud Run path uses.
	flt := f.receivedFilter[0]
	if !strings.Contains(flt, `status = "ok"`) {
		t.Errorf("filter %q missing status=ok", flt)
	}
	if !strings.Contains(flt, "function_name") || !strings.Contains(flt, `"report-builder"`) {
		t.Errorf("filter %q missing function_name=report-builder", flt)
	}
	if !strings.Contains(flt, CloudFunctionsExecutionCountMetricType) {
		t.Errorf("filter %q missing metric type", flt)
	}
}

// TestGCPQueryAggregate_RequestCount_FiltersByResponseCodeClass pins
// the §4.2 filter choice: response_code_class != "5xx" (exclude
// only server errors; 4xx requests still reflect a real invocation
// the sampler observed).
func TestGCPQueryAggregate_RequestCount_FiltersByResponseCodeClass(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{}}
	s := newMetricsTestScannerWithFake(t, f)
	const arn = "projects/test-project/locations/us-central1/services/checkout"
	_, err := s.QueryAggregate(
		context.Background(),
		arn,
		CloudRunRequestCountMetricType,
		24*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.receivedFilter) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.receivedFilter))
	}
	flt := f.receivedFilter[0]
	if !strings.Contains(flt, `response_code_class != "5xx"`) {
		t.Errorf("filter %q missing response_code_class!=5xx", flt)
	}
	// Must NOT accidentally use the latency path's =2xx filter.
	if strings.Contains(flt, `response_code_class = "2xx"`) {
		t.Errorf("filter %q uses latency-path =2xx filter; sampling rate wants !=5xx", flt)
	}
}

