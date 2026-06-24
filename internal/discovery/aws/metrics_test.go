// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	smithy "github.com/aws/smithy-go"
	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// newMetricsTestScanner builds an AWS Scanner suitable for skeleton-
// path tests. The CloudWatch wiring is not exercised in chunk 1 so
// the factory + factoryBuilder fields stay nil; QueryAggregate's
// skeleton path never touches them.
func newMetricsTestScanner() *Scanner {
	return NewScannerForValidation(credstore.AWSCredentials{}, "123456789012")
}

// TestQueryAggregate_Skeleton_ReturnsNotImplemented — slice 1 chunk 1
// acceptance test 1 (modified for chunk 1): the AWS MetricQuerier
// skeleton returns scanner.ErrMetricNotImplemented. Chunk 2 replaces
// the skeleton with the real CloudWatch GetMetricStatistics
// implementation; until then, callers MUST observe the sentinel error
// so the chunk-2 transition cleanly takes over.
//
// The errors.Is path (NOT a string comparison) is what the substrate
// contract is pinned to; see scanner/metrics_test.go::
// TestErrMetricNotImplemented_SentinelComparable for the sibling pin.
func TestQueryAggregate_Skeleton_ReturnsNotImplemented(t *testing.T) {
	s := newMetricsTestScanner()
	_, err := s.QueryAggregate(
		context.Background(),
		"arn:aws:lambda:us-east-1:123456789012:function:order-processor",
		LambdaInitDurationMetricName,
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

// TestQueryAggregate_Skeleton_PreservesInputFieldsInResult — even on
// the skeleton path the returned AggregateMetricResult echoes the
// caller's request fields (ResourceARN, MetricName, Window, Statistic).
// Callers that ignore the error path during testing still observe the
// request shape; the chunk-2 implementation will populate the Value,
// Unit, SampleCount, ObservedAt fields in addition.
func TestQueryAggregate_Skeleton_PreservesInputFieldsInResult(t *testing.T) {
	s := newMetricsTestScanner()
	const arn = "arn:aws:lambda:us-east-1:123456789012:function:fn"
	res, _ := s.QueryAggregate(
		context.Background(),
		arn,
		LambdaInitDurationMetricName,
		7*24*time.Hour,
		scanner.StatisticP95,
	)
	if res.ResourceARN != arn {
		t.Errorf("ResourceARN = %q, want %q", res.ResourceARN, arn)
	}
	if res.MetricName != LambdaInitDurationMetricName {
		t.Errorf("MetricName = %q, want %q", res.MetricName, LambdaInitDurationMetricName)
	}
	if res.Window != 7*24*time.Hour {
		t.Errorf("Window = %v, want %v", res.Window, 7*24*time.Hour)
	}
	if res.Statistic != scanner.StatisticP95 {
		t.Errorf("Statistic = %q, want %q", res.Statistic, scanner.StatisticP95)
	}
	// Value / Unit / SampleCount / ObservedAt stay zero on the
	// skeleton path — chunk 2 populates them.
	if res.Value != 0 {
		t.Errorf("Value = %v, want 0 on the skeleton path", res.Value)
	}
	if res.SampleCount != 0 {
		t.Errorf("SampleCount = %v, want 0 on the skeleton path", res.SampleCount)
	}
	if res.Unit != "" {
		t.Errorf("Unit = %q, want empty on the skeleton path", res.Unit)
	}
	if !res.ObservedAt.IsZero() {
		t.Errorf("ObservedAt = %v, want zero on the skeleton path", res.ObservedAt)
	}
}

// TestAWSCloudWatchRateLimitRPS_Constant pins the rate limit constant
// to 10. Chunk 2's CloudWatch wiring consults this constant; the
// chunk-4 runbook documents the choice. Changing the value here MUST
// involve updating the runbook so operators see the new throttle
// behavior.
//
// See docs/proposals/cold-start-latency-slice1.md §5 + §12.
func TestAWSCloudWatchRateLimitRPS_Constant(t *testing.T) {
	if AWSCloudWatchRateLimitRPS != 10 {
		t.Errorf("AWSCloudWatchRateLimitRPS = %d, want 10", AWSCloudWatchRateLimitRPS)
	}
}

// TestLambdaInitDurationMetricName_Constant pins the CloudWatch metric
// name to "InitDuration". The chunk-2 wiring + chunk-3 proposer prompt
// + chunk-4 runbook all reference the same string; a silent rename
// would break the cold-start detection path across release boundaries.
func TestLambdaInitDurationMetricName_Constant(t *testing.T) {
	if LambdaInitDurationMetricName != "InitDuration" {
		t.Errorf("LambdaInitDurationMetricName = %q, want %q",
			LambdaInitDurationMetricName, "InitDuration")
	}
}

// TestLambdaMetricNamespace_Constant pins the CloudWatch namespace to
// "AWS/Lambda". GetMetricStatistics requires (namespace, metricName)
// as a tuple; a silent rename here would silently produce empty
// result sets in chunk 2.
func TestLambdaMetricNamespace_Constant(t *testing.T) {
	if LambdaMetricNamespace != "AWS/Lambda" {
		t.Errorf("LambdaMetricNamespace = %q, want %q",
			LambdaMetricNamespace, "AWS/Lambda")
	}
}

// TestScannerSatisfiesMetricQuerier — compile-time pin that the AWS
// Scanner type implements scanner.MetricQuerier. Chunk 2 keeps the
// interface, replaces the body; this assertion catches an accidental
// signature drift across the transition.
func TestScannerSatisfiesMetricQuerier(t *testing.T) {
	var _ scanner.MetricQuerier = (*Scanner)(nil)
}

// -- v0.89.114 chunk 2 additions ------------------------------------------

// cwFake is the chunk-2 in-memory CloudWatch client. Implements
// CloudWatchClient with deterministic per-call hooks so each test
// can return a canned response, simulate a throttle storm, or count
// calls for the rate-limiter pin.
type cwFake struct {
	mu              sync.Mutex
	calls           int
	throttleFirstN  int
	respondWith     *cloudwatch.GetMetricStatisticsOutput
	respondErr      error
	respondPerCall  func(int, *cloudwatch.GetMetricStatisticsInput) (*cloudwatch.GetMetricStatisticsOutput, error)
	receivedInputs  []*cloudwatch.GetMetricStatisticsInput
}

func (f *cwFake) GetMetricStatistics(
	ctx context.Context,
	in *cloudwatch.GetMetricStatisticsInput,
	_ ...func(*cloudwatch.Options),
) (*cloudwatch.GetMetricStatisticsOutput, error) {
	f.mu.Lock()
	f.calls++
	callN := f.calls
	f.receivedInputs = append(f.receivedInputs, in)
	throttleFirstN := f.throttleFirstN
	perCall := f.respondPerCall
	canned := f.respondWith
	cannedErr := f.respondErr
	f.mu.Unlock()
	if perCall != nil {
		return perCall(callN, in)
	}
	if throttleFirstN >= callN {
		return nil, &cwThrottleErr{}
	}
	return canned, cannedErr
}

// cwThrottleErr satisfies smithy.APIError with the ThrottlingException
// error code that isCloudWatchThrottleError flips the retry path on.
type cwThrottleErr struct{}

func (e *cwThrottleErr) Error() string                            { return "ThrottlingException: rate exceeded" }
func (e *cwThrottleErr) ErrorCode() string                        { return "ThrottlingException" }
func (e *cwThrottleErr) ErrorMessage() string                     { return "rate exceeded" }
func (e *cwThrottleErr) ErrorFault() smithy.ErrorFault            { return smithy.FaultClient }

// newMetricsTestScannerWithCW wires the cwFake plus the default
// 10-RPS rate limiter onto a validation-shaped Scanner. The
// returned Scanner satisfies scanner.MetricQuerier with real
// chunk-2 behaviour — empty cwFake responses surface as the
// substrate's empty-result contract, throttle-first-N flips the
// retry path, etc.
func newMetricsTestScannerWithCW(t *testing.T, cw *cwFake) *Scanner {
	t.Helper()
	s := newMetricsTestScanner().WithCloudWatchClient(cw)
	// burst=1 makes each call acquire a token rather than coalescing
	// into a free-running burst — deterministic for the 10-RPS pin.
	s.WithCloudWatchRateLimiter(rate.NewLimiter(rate.Limit(AWSCloudWatchRateLimitRPS), 1))
	return s
}

// TestQueryAggregate_InitDuration_ReturnsP95FromCloudWatch — slice 1
// chunk 2 acceptance test 1. The fake returns a multi-datapoint
// response carrying an ExtendedStatistics p95 value; QueryAggregate
// rolls those up into the MAX (the worst-case 5-minute P95 across
// the window) and returns the result with SampleCount summed from
// the per-datapoint counts.
func TestQueryAggregate_InitDuration_ReturnsP95FromCloudWatch(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{
				{
					ExtendedStatistics: map[string]float64{"p95": 1200.0},
					SampleCount:        awssdk.Float64(20),
					Unit:               cwtypes.StandardUnitMilliseconds,
				},
				{
					ExtendedStatistics: map[string]float64{"p95": 4230.0},
					SampleCount:        awssdk.Float64(40),
					Unit:               cwtypes.StandardUnitMilliseconds,
				},
				{
					ExtendedStatistics: map[string]float64{"p95": 800.0},
					SampleCount:        awssdk.Float64(10),
					Unit:               cwtypes.StandardUnitMilliseconds,
				},
			},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	res, err := s.QueryAggregate(
		context.Background(),
		"arn:aws:lambda:us-east-1:123456789012:function:order-processor",
		LambdaInitDurationMetricName,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 4230.0 {
		t.Errorf("Value = %v, want MAX across datapoints (4230.0)", res.Value)
	}
	if res.SampleCount != 70 {
		t.Errorf("SampleCount = %d, want 70 (20+40+10)", res.SampleCount)
	}
	if res.Unit != "Milliseconds" {
		t.Errorf("Unit = %q, want Milliseconds", res.Unit)
	}
	if res.MetricName != LambdaInitDurationMetricName {
		t.Errorf("MetricName = %q, want %q", res.MetricName, LambdaInitDurationMetricName)
	}
	if res.Statistic != scanner.StatisticP95 {
		t.Errorf("Statistic = %q, want %q", res.Statistic, scanner.StatisticP95)
	}
}

// TestQueryAggregate_InitDuration_EmptyResponseReturnsZero — slice 1
// chunk 2 acceptance test 2. Empty Datapoints → Value=0,
// SampleCount=0, no error. Matches the MetricQuerier interface
// contract godoc: callers MUST distinguish "value is genuinely 0"
// from "no datapoints exist" by checking SampleCount.
func TestQueryAggregate_InitDuration_EmptyResponseReturnsZero(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	res, err := s.QueryAggregate(
		context.Background(),
		"arn:aws:lambda:us-east-1:123456789012:function:cold-start-fn",
		LambdaInitDurationMetricName,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("Value = %v, want 0 on empty datapoints", res.Value)
	}
	if res.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0 on empty datapoints", res.SampleCount)
	}
}

// TestQueryAggregate_InitDuration_ThrottleRetryEventuallySucceeds —
// slice 1 chunk 2 acceptance test 3. CloudWatch returns
// ThrottlingException on the first call, then succeeds. The handler's
// retry loop sleeps cloudWatchThrottleInitialBackoff between attempts
// and surfaces the eventual success.
func TestQueryAggregate_InitDuration_ThrottleRetryEventuallySucceeds(t *testing.T) {
	// Speed up the throttle backoff so the test is fast.
	originalBackoff := cloudWatchThrottleInitialBackoff
	cloudWatchThrottleInitialBackoff = 1 * time.Millisecond
	t.Cleanup(func() { cloudWatchThrottleInitialBackoff = originalBackoff })

	cw := &cwFake{
		throttleFirstN: 1,
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{{
				ExtendedStatistics: map[string]float64{"p95": 750.0},
				SampleCount:        awssdk.Float64(15),
			}},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	res, err := s.QueryAggregate(
		context.Background(),
		"arn:aws:lambda:us-east-1:123456789012:function:retry-fn",
		LambdaInitDurationMetricName,
		24*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		t.Fatalf("unexpected error after throttle retry: %v", err)
	}
	if res.Value != 750.0 {
		t.Errorf("Value = %v, want 750.0", res.Value)
	}
	if cw.calls != 2 {
		t.Errorf("calls = %d, want 2 (one throttled + one success)", cw.calls)
	}
}

// TestQueryAggregate_RateLimiterCapsAt10RPS — slice 1 chunk 2
// acceptance test 4. The substrate contract pins the per-account
// limit at 10 RPS; 20 requests should take at least ~1 second
// (20 requests / 10 RPS = 2 seconds at steady state; allowing for
// the burst-of-1 the first request goes through immediately, the
// remaining 19 each wait ~100ms, totalling ~1.9s).
//
// To keep the test under a few seconds, we issue 20 calls and assert
// the wall-clock elapsed is at least 900ms — slightly under the
// theoretical 1.9s to be tolerant of clock granularity and
// scheduler jitter on CI runners.
func TestQueryAggregate_RateLimiterCapsAt10RPS(t *testing.T) {
	if testing.Short() {
		t.Skip("rate limiter timing test; runs only without -short")
	}
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	ctx := context.Background()
	const total = 20
	start := time.Now()
	for i := 0; i < total; i++ {
		_, err := s.QueryAggregate(ctx,
			"arn:aws:lambda:us-east-1:123456789012:function:rate-test",
			LambdaInitDurationMetricName, 24*time.Hour, scanner.StatisticP95)
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	// At 10 RPS with burst=1, 20 requests should take ~1.9s
	// (first request is free, remaining 19 take ~100ms each).
	// 900ms is a comfortable lower bound that catches "the
	// limiter isn't being consulted" while staying tolerant of
	// CI scheduler jitter.
	if elapsed < 900*time.Millisecond {
		t.Errorf("20 requests elapsed = %v, want >= 900ms (rate limiter at 10 RPS)", elapsed)
	}
	if cw.calls != total {
		t.Errorf("CloudWatch calls = %d, want %d", cw.calls, total)
	}
}

// TestQueryAggregate_NonInitDurationMetric_ReturnsEmptyNoError —
// slice 1 substrate scope: InitDuration only. Other metric names
// short-circuit to an empty result with no error so the interface
// contract distinguishes "metric not supported in slice 1" (empty)
// from "API call failed" (error).
func TestQueryAggregate_NonInitDurationMetric_ReturnsEmptyNoError(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			// If the routing accidentally lets this metric through,
			// the response would be these non-empty datapoints —
			// the test would observe a non-zero Value.
			Datapoints: []cwtypes.Datapoint{{
				ExtendedStatistics: map[string]float64{"p95": 9999.0},
			}},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	res, err := s.QueryAggregate(context.Background(),
		"arn:aws:lambda:us-east-1:123456789012:function:fn",
		"Duration", // not InitDuration
		24*time.Hour, scanner.StatisticP95)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Value != 0 {
		t.Errorf("Value = %v, want 0 for unsupported metric", res.Value)
	}
	if res.SampleCount != 0 {
		t.Errorf("SampleCount = %d, want 0 for unsupported metric", res.SampleCount)
	}
	if cw.calls != 0 {
		t.Errorf("CloudWatch calls = %d, want 0 (short-circuit before SDK call)", cw.calls)
	}
}

// TestQueryAggregate_InvalidLambdaARN_ReturnsError — non-Lambda ARN
// returns an error before the CloudWatch call lands. The fake
// confirms zero SDK calls were issued.
func TestQueryAggregate_InvalidLambdaARN_ReturnsError(t *testing.T) {
	cw := &cwFake{}
	s := newMetricsTestScannerWithCW(t, cw)
	_, err := s.QueryAggregate(context.Background(),
		"arn:aws:ec2:us-east-1:123456789012:instance/i-abc",
		LambdaInitDurationMetricName, 24*time.Hour, scanner.StatisticP95)
	if err == nil {
		t.Fatal("expected error for non-Lambda ARN, got nil")
	}
	if cw.calls != 0 {
		t.Errorf("CloudWatch calls = %d, want 0 (error before SDK call)", cw.calls)
	}
}

// TestQueryAggregate_PassesP95ExtendedStatistic — verify the request
// body the SDK fake observes carries the namespace, metric name,
// dimension, and ExtendedStatistic="p95" expected per the slice 1
// design doc §3 step 1.
func TestQueryAggregate_PassesP95ExtendedStatistic(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	_, err := s.QueryAggregate(context.Background(),
		"arn:aws:lambda:us-west-2:111111111111:function:checkout",
		LambdaInitDurationMetricName, 24*time.Hour, scanner.StatisticP95)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cw.calls != 1 {
		t.Fatalf("calls = %d, want 1", cw.calls)
	}
	in := cw.receivedInputs[0]
	if in.Namespace == nil || *in.Namespace != LambdaMetricNamespace {
		t.Errorf("Namespace = %v, want %q", in.Namespace, LambdaMetricNamespace)
	}
	if in.MetricName == nil || *in.MetricName != LambdaInitDurationMetricName {
		t.Errorf("MetricName = %v, want %q", in.MetricName, LambdaInitDurationMetricName)
	}
	if len(in.ExtendedStatistics) != 1 || in.ExtendedStatistics[0] != "p95" {
		t.Errorf("ExtendedStatistics = %v, want [\"p95\"]", in.ExtendedStatistics)
	}
	if len(in.Dimensions) != 1 || in.Dimensions[0].Name == nil ||
		*in.Dimensions[0].Name != "FunctionName" ||
		in.Dimensions[0].Value == nil || *in.Dimensions[0].Value != "checkout" {
		t.Errorf("Dimensions = %v, want [{FunctionName checkout}]", in.Dimensions)
	}
	if in.Period == nil || *in.Period != int32(cloudWatchMetricPeriodSeconds) {
		t.Errorf("Period = %v, want %d", in.Period, cloudWatchMetricPeriodSeconds)
	}
}

// TestQueryAggregate_NilCloudWatchClient_ReturnsNotImplemented —
// a Scanner with cwClient=nil mirrors the chunk-1 skeleton path:
// returns scanner.ErrMetricNotImplemented while preserving the
// echoed input fields. Backward-compat for the v0.89.113 surface
// callers may have written tests against.
func TestQueryAggregate_NilCloudWatchClient_ReturnsNotImplemented(t *testing.T) {
	// newMetricsTestScanner builds a Scanner WITHOUT
	// WithCloudWatchClient, so cwClient stays nil.
	s := newMetricsTestScanner()
	_, err := s.QueryAggregate(context.Background(),
		"arn:aws:lambda:us-east-1:123456789012:function:fn",
		LambdaInitDurationMetricName, 24*time.Hour, scanner.StatisticP95)
	if err == nil {
		t.Fatal("expected ErrMetricNotImplemented, got nil")
	}
	if !errors.Is(err, scanner.ErrMetricNotImplemented) {
		t.Errorf("error %q must resolve to scanner.ErrMetricNotImplemented", err)
	}
}
