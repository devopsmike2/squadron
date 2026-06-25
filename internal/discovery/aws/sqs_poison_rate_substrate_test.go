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

// --- §8.3 DetectSQSPoisonRate: real rate / real-zero / absent ---

func TestDetectSQSPoisonRate_RealRateFiresHighBand(t *testing.T) {
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
	assert.Equal(t, 120, res.RatePerHour)
	assert.True(t, res.HighBand, "120/hr >= PoisonRatePerHourHighThreshold=%d", PoisonRatePerHourHighThreshold)
}

func TestDetectSQSPoisonRate_RealZeroIsNotAbsent(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{
				{Sum: awssdk.Float64(0.0), SampleCount: awssdk.Float64(1), Unit: cwtypes.StandardUnitCount},
			},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	res, err := s.DetectSQSPoisonRate(context.Background(), testDLQARN)
	assert.NoError(t, err)
	assert.Equal(t, 0, res.RatePerHour, "SampleCount>0 with SUM 0 is a REAL zero, not the absent sentinel")
	assert.False(t, res.HighBand)
}

func TestDetectSQSPoisonRate_NoDatapointsKeepsAbsentSentinel(t *testing.T) {
	cw := &cwFake{
		respondWith: &cloudwatch.GetMetricStatisticsOutput{
			Datapoints: []cwtypes.Datapoint{},
		},
	}
	s := newMetricsTestScannerWithCW(t, cw)
	res, err := s.DetectSQSPoisonRate(context.Background(), testDLQARN)
	assert.NoError(t, err)
	assert.Equal(t, -1, res.RatePerHour, "SampleCount==0 → honest-framing absent sentinel, never measured-as-zero")
	assert.False(t, res.HighBand)
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

func TestEnrichSQSPoisonRate_OverwritesForReachableDLQ(t *testing.T) {
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

	assert.Equal(t, 90, snaps[0].Detail["poison_rate_per_hour"], "real reading overwrites the -1 sentinel")
	assert.Equal(t, true, snaps[0].Detail["poison_rate_high_band"])
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
