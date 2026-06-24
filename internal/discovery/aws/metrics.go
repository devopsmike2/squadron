// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// AWSCloudWatchRateLimitRPS pins the per-account rate limit the slice 1
// substrate enforces for CloudWatch GetMetricStatistics requests.
// CloudWatch GetMetricStatistics is rate-limited per AWS account at
// ~50 RPS (varies by account size); the slice 1 substrate sits well
// under that ceiling at 10 RPS so multi-instance Squadron deployments
// scanning the same account stay below the throttle limit.
//
// Chunk 1 (v0.89.113) defines the constant; chunk 2 (v0.89.114) wires
// the rate limiter that consults it. Pinned to 10 by the
// metrics_test.go::TestAWSCloudWatchRateLimitRPS_Constant test —
// changes to the value have to update the test (and the runbook).
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
// metrics_test.go::TestLambdaInitDurationMetricName_Constant. The
// constant lives at package scope so the chunk 2 CloudWatch wiring,
// the chunk 3 proposer prompt, and the chunk 4 runbook all reference
// the same string.
const LambdaInitDurationMetricName = "InitDuration"

// LambdaMetricNamespace is the CloudWatch namespace for AWS Lambda
// metrics. CloudWatch GetMetricStatistics requires (namespace,
// metricName) as a tuple to disambiguate metrics from different
// services; the namespace lives at package scope so the chunk 2
// wiring constructs the request identically every call.
//
// Pinned to "AWS/Lambda" by
// metrics_test.go::TestLambdaMetricNamespace_Constant.
const LambdaMetricNamespace = "AWS/Lambda"

// QueryAggregate satisfies the scanner.MetricQuerier interface for the
// AWS provider. Chunk 1 (v0.89.113) returns scanner.ErrMetricNotImplemented;
// chunk 2 (v0.89.114) wires the CloudWatch GetMetricStatistics call,
// the rate limiter (capped at AWSCloudWatchRateLimitRPS), pagination
// handling, and the empty-result-set semantics the interface contract
// specifies.
//
// The skeleton echoes the caller's input fields (ResourceARN,
// MetricName, Window, Statistic) into the returned AggregateMetricResult
// alongside the sentinel error so callers that ignore the error path
// during testing still observe the request shape. The Value, Unit,
// SampleCount, and ObservedAt fields stay at their zero values
// because the skeleton has no real data to populate them with.
//
// The interface is stable across chunks 1 → 4 so that chunk 3
// (proposer + UI) and chunk 4 (runbook) can be written against the
// v0.89.113 shape without waiting on chunk 2 to ship first. Callers
// SHOULD detect the not-yet-implemented state via errors.Is(err,
// scanner.ErrMetricNotImplemented) rather than string-matching the
// error message.
//
// See docs/proposals/cold-start-latency-slice1.md §5, §10 (chunk 1
// contract).
func (s *Scanner) QueryAggregate(
	ctx context.Context,
	resourceARN string,
	metricName string,
	window time.Duration,
	stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	return scanner.AggregateMetricResult{
		ResourceARN: resourceARN,
		MetricName:  metricName,
		Window:      window,
		Statistic:   stat,
	}, scanner.ErrMetricNotImplemented
}
