// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-rate substrate slice 4 chunk 1 (v0.89.177, #819 Stream 216)
// — acceptance tests per docs/proposals/poison-rate-substrate-slice4.md
// §8. REAL CloudWatch-backed AWS SQS poison-rate detection that closes
// the slice-3 §3.3 deferral for AWS.

const testDLQARN = "arn:aws:sqs:us-east-1:123456789012:orders-dlq"

// --- §8.1 constants pins ----------------------------------------

func TestSQSMetricNamespace_Constant(t *testing.T) {
	assert.Equal(t, "AWS/SQS", SQSMetricNamespace)
}

func TestSQSNumberOfMessagesSentMetricName_Constant(t *testing.T) {
	assert.Equal(t, "NumberOfMessagesSent", SQSNumberOfMessagesSentMetricName)
}

func TestSQSPoisonRateWindowHours_Constant(t *testing.T) {
	assert.Equal(t, 1, SQSPoisonRateWindowHours)
}

// --- §8.1 extractSQSQueueName -----------------------------------

func TestExtractSQSQueueName(t *testing.T) {
	name, err := extractSQSQueueName(testDLQARN)
	assert.NoError(t, err)
	assert.Equal(t, "orders-dlq", name)

	for _, bad := range []string{
		"",
		"arn:aws:lambda:us-east-1:123456789012:function:fn",
		"arn:aws:sqs:us-east-1:123456789012:", // empty name
		"not-an-arn",
		"arn:aws:sqs:us-east-1:123456789012:name:extra", // 7 segments
	} {
		_, err := extractSQSQueueName(bad)
		assert.Error(t, err, "expected error for %q", bad)
	}
}

// --- §8.2 querySQSCounterSum sums across periods -----------------

func TestQuerySQSCounterSum_SumsOverWindow(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{
				{Sum: awssdk.Float64(40.0), SampleCount: awssdk.Float64(1), Unit: cwtypes.StandardUnitCount},
				{Sum: awssdk.Float64(50.0), SampleCount: awssdk.Float64(1), Unit: cwtypes.StandardUnitCount},
				{Sum: awssdk.Float64(30.0), SampleCount: awssdk.Float64(1), Unit: cwtypes.StandardUnitCount},
			},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	res, err := s.QueryAggregate(
		context.Background(), testDLQARN,
		SQSNumberOfMessagesSentMetricName,
		time.Hour,
		scanner.StatisticSum,
	)
	assert.NoError(t, err)
	assert.Equal(t, 120.0, res.Value, "SUM across the three periods")
	assert.Equal(t, 3, res.SampleCount)
	assert.Equal(t, SQSNumberOfMessagesSentMetricName, res.MetricName)

	// Verify the CloudWatch call used the AWS/SQS namespace +
	// QueueName dimension (NOT AWS/Lambda / FunctionName).
	if assert.Len(t, cw.receivedInputs, 1) {
		in := cw.receivedInputs[0]
		assert.Equal(t, SQSMetricNamespace, awssdk.ToString(in.Namespace))
		if assert.Len(t, in.Dimensions, 1) {
			assert.Equal(t, "QueueName", awssdk.ToString(in.Dimensions[0].Name))
			assert.Equal(t, "orders-dlq", awssdk.ToString(in.Dimensions[0].Value))
		}
	}
}

// --- §8.3 DetectSQSPoisonRate: honest absent sentinel (v0.89.229 revert) ---
// NumberOfMessagesSent does not capture redrive-moved DLQ messages, so
// DetectSQSPoisonRate returns the honest absent sentinel regardless of any
// CloudWatch reading and issues no metric query. See sqs_poison_rate.go.

func TestDetectSQSPoisonRate_AlwaysHonestSentinel(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{
				{Sum: awssdk.Float64(120.0), SampleCount: awssdk.Float64(2), Unit: cwtypes.StandardUnitCount},
			},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	res, err := s.DetectSQSPoisonRate(context.Background(), testDLQARN)
	assert.NoError(t, err)
	assert.Equal(t, -1, res.RatePerHour, "poison rate is not measurable from NumberOfMessagesSent; honest absent sentinel")
	assert.False(t, res.HighBand)
	assert.Empty(t, cw.receivedInputs, "DetectSQSPoisonRate must not issue a CloudWatch query")
}

// --- §8.4 enrichSQSPoisonRate -----------------------------------

func snapWithDLQ(dlqARN string) scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		Surface:     SQSSurface,
		ResourceARN: "arn:aws:sqs:us-east-1:123456789012:orders",
		Detail: map[string]any{
			// honest-framing projection defaults the enrichment overwrites:
			"poison_rate_per_hour":      -1,
			"poison_rate_high_band":     false,
			"redrive_policy_target_arn": dlqARN,
		},
	}
}

func TestEnrichSQSPoisonRate_PreservesSentinelNoOp(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{
				{Sum: awssdk.Float64(90.0), SampleCount: awssdk.Float64(1), Unit: cwtypes.StandardUnitCount},
			},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	snaps := []scanner.EventSourceInstanceSnapshot{snapWithDLQ(testDLQARN)}
	arnSet := map[string]struct{}{testDLQARN: {}}

	s.enrichSQSPoisonRate(context.Background(), snaps, arnSet)

	// Reverted to a no-op: honest absent sentinels survive, no CloudWatch query.
	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snaps[0].Detail["poison_rate_high_band"])
	assert.Empty(t, cw.receivedInputs, "enrichment must not query CloudWatch")
}

func TestEnrichSQSPoisonRate_NilClientNoOp(t *testing.T) {
	s := newMetricsTestScanner() // no CloudWatch client wired
	snaps := []scanner.EventSourceInstanceSnapshot{snapWithDLQ(testDLQARN)}
	arnSet := map[string]struct{}{testDLQARN: {}}

	s.enrichSQSPoisonRate(context.Background(), snaps, arnSet)

	// Cold-start parity: honest-framing sentinels survive untouched.
	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snaps[0].Detail["poison_rate_high_band"])
}

func TestEnrichSQSPoisonRate_UnreachableDLQSkipped(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{
				{Sum: awssdk.Float64(500.0), SampleCount: awssdk.Float64(1), Unit: cwtypes.StandardUnitCount},
			},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	snaps := []scanner.EventSourceInstanceSnapshot{snapWithDLQ(testDLQARN)}
	arnSet := map[string]struct{}{} // DLQ NOT reachable (cross-account / dangling)

	s.enrichSQSPoisonRate(context.Background(), snaps, arnSet)

	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"], "unreachable DLQ keeps the absent sentinel")
	assert.Equal(t, false, snaps[0].Detail["poison_rate_high_band"])
	assert.Empty(t, cw.receivedInputs, "no CloudWatch call for an unreachable DLQ")
}

func TestEnrichSQSPoisonRate_NoDLQSkipped(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{
				{Sum: awssdk.Float64(500.0), SampleCount: awssdk.Float64(1), Unit: cwtypes.StandardUnitCount},
			},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	// Snapshot with no redrive_policy_target_arn (no DLQ configured).
	snap := scanner.EventSourceInstanceSnapshot{
		Surface:     SQSSurface,
		ResourceARN: "arn:aws:sqs:us-east-1:123456789012:no-dlq",
		Detail: map[string]any{
			"poison_rate_per_hour":  -1,
			"poison_rate_high_band": false,
		},
	}
	snaps := []scanner.EventSourceInstanceSnapshot{snap}

	s.enrichSQSPoisonRate(context.Background(), snaps, map[string]struct{}{})

	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"])
	assert.Empty(t, cw.receivedInputs, "no DLQ → no CloudWatch call")
}
