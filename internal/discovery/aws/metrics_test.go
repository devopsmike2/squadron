// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"
	"time"

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
