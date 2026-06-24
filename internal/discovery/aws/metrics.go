// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	smithy "github.com/aws/smithy-go"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// AWSCloudWatchRateLimitRPS pins the per-account rate limit the slice 1
// substrate enforces for CloudWatch GetMetricStatistics requests.
// CloudWatch GetMetricStatistics is rate-limited per AWS account at
// ~50 RPS (varies by account size); the slice 1 substrate sits well
// under that ceiling at 10 RPS so multi-instance Squadron deployments
// scanning the same account stay below the throttle limit.
//
// Pinned to 10 by the metrics_test.go::TestAWSCloudWatchRateLimitRPS_Constant
// test. The chunk-4 runbook documents the choice — changes have to
// update the test (and the runbook).
//
// See docs/proposals/cold-start-latency-slice1.md §5 + §12 (threat
// model: CloudWatch GetMetricStatistics rate limits).
const AWSCloudWatchRateLimitRPS = 10

// LambdaInitDurationMetricName is the CloudWatch metric name for AWS
// Lambda cold-start initialization duration. Slice 1's cold-start
// detection queries this metric exclusively per design doc §3 — the
// substrate is deliberately narrow, and the InitDuration metric is the
// one signal that uniquely identifies cold-start latency (as opposed
// to overall request latency, which the Duration metric covers).
//
// Pinned to "InitDuration" by
// metrics_test.go::TestLambdaInitDurationMetricName_Constant.
const LambdaInitDurationMetricName = "InitDuration"

// LambdaMetricNamespace is the CloudWatch namespace for AWS Lambda
// metrics. CloudWatch GetMetricStatistics requires (namespace,
// metricName) as a tuple to disambiguate metrics from different
// services. Pinned to "AWS/Lambda" by
// metrics_test.go::TestLambdaMetricNamespace_Constant.
const LambdaMetricNamespace = "AWS/Lambda"

// LambdaInvocationsMetricName is the CloudWatch metric name for AWS
// Lambda invocation count. Sampling rate analysis slice 1 chunk 1
// (v0.89.122) uses this as the denominator for the
// observed_span_count / expected_invocation_count ratio per
// docs/proposals/sampling-rate-analysis-slice1.md §4.1.
//
// CloudWatch reports this metric as a counter (Statistics=["Sum"]
// rather than ExtendedStatistics=["p95"] like InitDuration) — the
// QueryAggregate routing accordingly branches into queryLambdaInvocations
// rather than the existing init-duration path.
//
// IAM stays unchanged from cold-start slice 1: the same
// cloudwatch:GetMetricStatistics permission covers both metric
// names.
//
// Pinned to "Invocations" by
// metrics_test.go::TestLambdaInvocationsMetricName_Constant.
const LambdaInvocationsMetricName = "Invocations"

// cloudWatchMetricPeriodSeconds is the 5-minute aggregation period
// the slice 1 substrate uses for every CloudWatch GetMetricStatistics
// call. The design doc §3 step 1 names this number: "5-minute period
// (gives ~288 data points)" across a 24h window. Stays a single
// package constant so the cross-window math (24h current vs 168h
// baseline) lines up at the same granularity.
const cloudWatchMetricPeriodSeconds = 300

// CloudWatchClient is the minimal surface the AWS MetricQuerier needs
// from the CloudWatch SDK. Slice 1 chunk 2 (v0.89.114) consumes only
// GetMetricStatistics — the rest of the SDK is intentionally outside
// the interface so the test fake stays a single-method shape.
//
// The interface lives at package scope so the production
// sdkClientFactory + the metrics_test.go fake (cwFake) both implement
// against it. Future slices (sampling-rate analysis, error-rate
// correlation) extend by adding methods to this interface and the
// fake; the QueryAggregate path won't grow as new metrics land.
type CloudWatchClient interface {
	GetMetricStatistics(
		ctx context.Context,
		in *cloudwatch.GetMetricStatisticsInput,
		optFns ...func(*cloudwatch.Options),
	) (*cloudwatch.GetMetricStatisticsOutput, error)
}

// cloudWatchThrottleMaxRetries caps the in-handler retry budget for
// CloudWatch ThrottlingException responses. The rate limiter keeps
// Squadron well under the per-account RPS ceiling, but neighbouring
// tenants sharing the same account can still trip the shared
// throttle — so QueryAggregate retries with exponential backoff a
// small number of times before surfacing the error. The cap stays
// small (3) so a genuinely sustained throttle storm fails fast
// rather than blocking the whole scan; chunk 4's runbook documents
// the operator-visible behaviour.
//
// See docs/proposals/cold-start-latency-slice1.md §11 acceptance
// test 3 (TestQueryAggregate_InitDuration_ThrottleRetryEventuallySucceeds).
const cloudWatchThrottleMaxRetries = 3

// cloudWatchThrottleInitialBackoff is the first sleep interval the
// retry loop uses after a ThrottlingException response. Each
// subsequent retry doubles the interval up to
// cloudWatchThrottleMaxRetries iterations. Kept short (50ms) so the
// test-suite path is fast; production behaviour relies on the rate
// limiter rather than the backoff to pace requests. Exposed as a
// `var` (not a const) so tests can replace it via the t.Cleanup
// pattern when exercising the throttle-retry path without sleeping
// the wall clock.
var cloudWatchThrottleInitialBackoff = 50 * time.Millisecond

// QueryAggregate implements scanner.MetricQuerier for AWS via
// CloudWatch GetMetricStatistics. Slice 1 chunk 2 (v0.89.114) wires
// the real CloudWatch call, the per-account rate limiter, the
// throttle-retry path, and the empty-result-set semantics the
// MetricQuerier interface contract specifies.
//
// Routing per design doc §3 + §5:
//
//   - metricName == LambdaInitDurationMetricName → real CloudWatch
//     GetMetricStatistics call against the AWS/Lambda namespace with
//     FunctionName extracted from the ARN as the single dimension.
//   - Any other metricName → slice 1 supports InitDuration only;
//     returns an empty AggregateMetricResult with SampleCount=0 and
//     no error. The chunk-2 detection branch is the only caller in
//     slice 1, and it always asks for InitDuration; the empty-result
//     branch keeps the interface contract honest for future slices
//     that may probe additional metric names speculatively.
//   - cwClient nil (the Scanner was constructed without the chunk-2
//     wiring — historical NewScannerForValidation paths in tests
//     that build Scanners directly) → returns
//     scanner.ErrMetricNotImplemented, mirroring the chunk-1
//     skeleton's surface so callers can errors.Is-detect the
//     unwired path.
//
// Empty datapoint handling (acceptance test 2): when CloudWatch
// returns Datapoints=[], the function returns Value=0,
// SampleCount=0, no error. Callers MUST check SampleCount before
// reading Value when distinguishing "value is genuinely 0" from "no
// datapoints existed".
//
// Throttle handling (acceptance test 3): when the SDK returns a
// ThrottlingException smithy.APIError, the function sleeps for
// cloudWatchThrottleInitialBackoff and retries with exponential
// backoff up to cloudWatchThrottleMaxRetries times before surfacing
// the error. The rate limiter is the primary defence; the retry
// loop catches the residual case where a neighbouring tenant on the
// same account briefly crosses the shared throttle.
//
// Rate limiter (acceptance test 4): a Wait call against the
// per-Scanner cwRateLimiter precedes every CloudWatch call,
// capping the per-account RPS at AWSCloudWatchRateLimitRPS.
//
// See docs/proposals/cold-start-latency-slice1.md §5, §11.
func (s *Scanner) QueryAggregate(
	ctx context.Context,
	resourceARN string,
	metricName string,
	window time.Duration,
	stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	if s.cwClient == nil {
		// Surfaces the chunk-1 skeleton sentinel so callers that
		// haven't wired the CloudWatch client (validation-only
		// Scanners, partially-constructed test fixtures) observe
		// the same shape as v0.89.113.
		return scanner.AggregateMetricResult{
			ResourceARN: resourceARN,
			MetricName:  metricName,
			Window:      window,
			Statistic:   stat,
		}, scanner.ErrMetricNotImplemented
	}

	// Sampling rate slice 1 chunk 1 (v0.89.122): Invocations is the
	// second supported AWS/Lambda metric. Routes into a sibling
	// helper that uses Statistics=["Sum"] rather than the
	// percentile-based ExtendedStatistics the init-duration path
	// uses. The two CloudWatch surfaces share the function-name
	// extraction + the rate limiter + the throttle-retry helper.
	if metricName == LambdaInvocationsMetricName {
		return s.queryLambdaInvocations(ctx, resourceARN, window, stat)
	}

	if metricName != LambdaInitDurationMetricName {
		// Slice 1 substrate scope: InitDuration + Invocations only.
		// Other names short-circuit to an empty result with no error
		// so the interface contract distinguishes "metric not
		// supported in slice 1" (empty result) from "API call failed"
		// (non-nil error). Slice 2 may broaden the routing as new
		// metric kinds land.
		return scanner.AggregateMetricResult{
			ResourceARN: resourceARN,
			MetricName:  metricName,
			Window:      window,
			Statistic:   stat,
		}, nil
	}

	functionName, err := extractLambdaFunctionName(resourceARN)
	if err != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("extract function name: %w", err)
	}

	if s.cwRateLimiter != nil {
		if err := s.cwRateLimiter.Wait(ctx); err != nil {
			return scanner.AggregateMetricResult{}, fmt.Errorf("rate limit: %w", err)
		}
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)
	extStat := mapMetricStatisticToCloudWatch(stat)
	periodSeconds := int32(cloudWatchMetricPeriodSeconds)

	input := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  awssdk.String(LambdaMetricNamespace),
		MetricName: awssdk.String(metricName),
		Dimensions: []cwtypes.Dimension{{
			Name:  awssdk.String("FunctionName"),
			Value: awssdk.String(functionName),
		}},
		StartTime:          awssdk.Time(startTime),
		EndTime:            awssdk.Time(endTime),
		Period:             &periodSeconds,
		ExtendedStatistics: []string{extStat},
	}

	out, callErr := s.callGetMetricStatisticsWithRetry(ctx, input)
	if callErr != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("cloudwatch get metric statistics: %w", callErr)
	}

	result := scanner.AggregateMetricResult{
		ResourceARN: resourceARN,
		MetricName:  metricName,
		Window:      window,
		Statistic:   stat,
		ObservedAt:  endTime,
	}
	if len(out.Datapoints) == 0 {
		// Acceptance test 2 — empty CloudWatch response is a real
		// "no datapoints" signal, not an error. Value stays 0,
		// SampleCount stays 0; the chunk-2 detection branch sees
		// SampleCount=0 and skips the comparison per the
		// ColdStartDetectionResult.ShouldFireRecommendation
		// contract.
		return result, nil
	}

	// CloudWatch returns one datapoint per `Period` interval, each
	// carrying the requested ExtendedStatistic (p95) for that
	// 5-minute window. The MAX across all periods in the window
	// gives the worst-case 5-minute P95 the function experienced,
	// which is the operator-visible signal the cold-start
	// recommendation reasons over. Slice 2 may adopt a more
	// sophisticated rollup (weighted-by-SampleCount mean of the
	// per-period P95s, or a re-aggregation against the raw
	// datapoints) once cross-cloud comparison work surfaces a
	// preference.
	maxVal := 0.0
	sampleCount := 0
	unit := ""
	for _, dp := range out.Datapoints {
		if dp.ExtendedStatistics == nil {
			continue
		}
		v, ok := dp.ExtendedStatistics[extStat]
		if !ok {
			continue
		}
		if v > maxVal {
			maxVal = v
		}
		if dp.SampleCount != nil {
			sampleCount += int(*dp.SampleCount)
		}
		if dp.Unit != "" && unit == "" {
			unit = string(dp.Unit)
		}
	}
	result.Value = maxVal
	result.SampleCount = sampleCount
	result.Unit = unit
	return result, nil
}

// queryLambdaInvocations is the sampling-rate-slice-1 sibling of the
// init-duration code path inside QueryAggregate. It mirrors the
// rate-limiter / throttle-retry / empty-result semantics but uses
// Statistics=["Sum"] rather than ExtendedStatistics=["p95"] and
// aggregates across per-period datapoints via SUM rather than MAX —
// the per-5-minute Sum values add up to the total invocations across
// the window, which is the denominator the sampling-rate detection
// branch (chunk 2) compares against the observed_span_count from
// traceindex.
//
// Empty datapoint handling matches the init-duration path: zero
// invocations in the window returns Value=0 / SampleCount=0 with no
// error. The chunk-2 detection branch additionally gates on the
// MIN_INVOCATION_COUNT (1000) floor per design doc §3 step 4, so an
// empty CloudWatch response naturally falls below the floor and the
// detection skips.
//
// See docs/proposals/sampling-rate-analysis-slice1.md §4.1.
func (s *Scanner) queryLambdaInvocations(
	ctx context.Context,
	resourceARN string,
	window time.Duration,
	stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	functionName, err := extractLambdaFunctionName(resourceARN)
	if err != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("extract function name: %w", err)
	}

	if s.cwRateLimiter != nil {
		if err := s.cwRateLimiter.Wait(ctx); err != nil {
			return scanner.AggregateMetricResult{}, fmt.Errorf("rate limit: %w", err)
		}
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)
	periodSeconds := int32(cloudWatchMetricPeriodSeconds)

	input := &cloudwatch.GetMetricStatisticsInput{
		Namespace:  awssdk.String(LambdaMetricNamespace),
		MetricName: awssdk.String(LambdaInvocationsMetricName),
		Dimensions: []cwtypes.Dimension{{
			Name:  awssdk.String("FunctionName"),
			Value: awssdk.String(functionName),
		}},
		StartTime: awssdk.Time(startTime),
		EndTime:   awssdk.Time(endTime),
		Period:    &periodSeconds,
		// Sum (counter), not ExtendedStatistics percentile —
		// Invocations is a count metric, percentile aggregation
		// would be a category error.
		Statistics: []cwtypes.Statistic{cwtypes.StatisticSum},
	}

	out, callErr := s.callGetMetricStatisticsWithRetry(ctx, input)
	if callErr != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("cloudwatch get metric statistics: %w", callErr)
	}

	result := scanner.AggregateMetricResult{
		ResourceARN: resourceARN,
		MetricName:  LambdaInvocationsMetricName,
		Window:      window,
		Statistic:   stat,
		ObservedAt:  endTime,
	}
	if len(out.Datapoints) == 0 {
		// No invocations in the window. SampleCount stays 0 so the
		// chunk-2 detection branch's MIN_INVOCATION_COUNT gate
		// trips and the sampling-rate detection skips this
		// resource.
		return result, nil
	}

	// SUM across per-period sums = total invocations across the
	// window. Mirrors the design doc §4.1 contract: "AWS/Lambda
	// Invocations (sum statistic over window)". The MAX-of-P95
	// rollup the init-duration path uses is the wrong rollup here
	// — we want the count denominator, not the worst-case
	// percentile.
	totalInvocations := 0.0
	sampleCount := 0
	unit := ""
	for _, dp := range out.Datapoints {
		if dp.Sum != nil {
			totalInvocations += *dp.Sum
		}
		if dp.SampleCount != nil {
			sampleCount += int(*dp.SampleCount)
		}
		if dp.Unit != "" && unit == "" {
			unit = string(dp.Unit)
		}
	}
	result.Value = totalInvocations
	result.SampleCount = sampleCount
	result.Unit = unit
	return result, nil
}

// callGetMetricStatisticsWithRetry wraps the SDK call with a small
// exponential-backoff retry loop scoped to ThrottlingException
// responses. Non-throttle errors surface immediately. The loop
// honours ctx cancellation between sleeps — a cancelled context
// during a backoff returns ctx.Err() rather than the throttle
// error, so the caller sees the cancellation reason rather than
// "throttled" noise.
func (s *Scanner) callGetMetricStatisticsWithRetry(
	ctx context.Context,
	input *cloudwatch.GetMetricStatisticsInput,
) (*cloudwatch.GetMetricStatisticsOutput, error) {
	backoff := cloudWatchThrottleInitialBackoff
	var lastErr error
	for attempt := 0; attempt <= cloudWatchThrottleMaxRetries; attempt++ {
		out, err := s.cwClient.GetMetricStatistics(ctx, input)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !isCloudWatchThrottleError(err) {
			return nil, err
		}
		if attempt == cloudWatchThrottleMaxRetries {
			break
		}
		// Honour ctx between retries.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, lastErr
}

// isCloudWatchThrottleError detects the CloudWatch throttle response.
// The SDK surfaces throttles as smithy.APIError with one of two error
// codes: "ThrottlingException" (the modern code) or "Throttling" (the
// legacy code some older AWS endpoints still emit). Both flip the
// retry loop on.
func isCloudWatchThrottleError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "ThrottlingException", "Throttling", "TooManyRequestsException":
		return true
	}
	return false
}

// mapMetricStatisticToCloudWatch converts the scanner.MetricStatistic
// enum into the CloudWatch ExtendedStatistic string. Slice 1 ships
// StatisticP95 + StatisticP99; the StatisticAverage / StatisticSum
// values are reserved for future slices and currently fall through to
// "p95" (the slice 1 detection rule's statistic). The fallback path
// is exercised by acceptance test 7 (TestQueryAggregate_PassesP95ExtendedStatistic).
func mapMetricStatisticToCloudWatch(stat scanner.MetricStatistic) string {
	switch stat {
	case scanner.StatisticP95:
		return "p95"
	case scanner.StatisticP99:
		return "p99"
	default:
		return "p95"
	}
}

// extractLambdaFunctionName parses the function name segment out of a
// Lambda function ARN. ARN format per the AWS docs:
//
//	arn:aws:lambda:<region>:<account>:function:<name>[:<qualifier>]
//
// Returns an error when the ARN doesn't match the expected shape —
// most commonly when the caller passed an unrelated resource ARN
// (an EC2 instance ID, a Lambda layer ARN, etc.). The error
// message includes the offending ARN so the chunk-2 detection branch's
// log surface points at the specific row that misfired.
//
// Pinned by metrics_test.go::TestQueryAggregate_InvalidLambdaARN_ReturnsError.
func extractLambdaFunctionName(arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) < 7 || parts[0] != "arn" || parts[2] != "lambda" || parts[5] != "function" {
		return "", fmt.Errorf("not a Lambda function ARN: %q", arn)
	}
	name := parts[6]
	if name == "" {
		return "", fmt.Errorf("not a Lambda function ARN: %q", arn)
	}
	return name, nil
}
