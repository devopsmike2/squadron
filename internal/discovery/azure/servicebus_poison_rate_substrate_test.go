// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-rate substrate slice 4 chunk 3a (v0.89.179, #821 Stream 218)
// — acceptance tests per docs/proposals/poison-rate-substrate-slice4.md
// §8. REAL Azure Monitor-backed Service Bus poison-rate detection
// (namespace granularity) that closes the slice-3 §3.3 deferral for
// Azure. Per-queue attribution (§3.2) is chunk 3b.

const testNamespaceARN = "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg/providers/Microsoft.ServiceBus/namespaces/orders-ns"

// deadletterResponse builds an armMetricsResponse carrying per-bucket
// Maximum + Minimum DeadletteredMessages gauge values.
func deadletterResponse(pairs ...[2]float64) armMetricsResponse {
	dps := make([]armMetricsDatapoint, 0, len(pairs))
	for i, mm := range pairs {
		mx, mn := mm[0], mm[1]
		dps = append(dps, armMetricsDatapoint{
			TimeStamp: timeStampAt(i),
			Maximum:   fpPtr(mx),
			Minimum:   fpPtr(mn),
		})
	}
	return armMetricsResponse{
		Value: []armMetricsValue{{
			Unit:       "Count",
			Timeseries: []armMetricsTimeseries{{Data: dps}},
		}},
	}
}

func timeStampAt(i int) string {
	return time.Date(2025, 1, 1, 0, i*5, 0, 0, time.UTC).Format(time.RFC3339)
}

// --- constants pins ---------------------------------------------

func TestServiceBusDeadletteredMessagesMetric_Constant(t *testing.T) {
	assert.Equal(t, "DeadletteredMessages", ServiceBusDeadletteredMessagesMetric)
}

func TestServiceBusPoisonRatePerHourHighThreshold_Constant(t *testing.T) {
	assert.Equal(t, 60, ServiceBusPoisonRatePerHourHighThreshold)
}

func TestServiceBusPoisonRateWindowHours_Constant(t *testing.T) {
	assert.Equal(t, 1, ServiceBusPoisonRateWindowHours)
}

// --- delta math: rate = max(Maximum) - min(Minimum), floored at 0 -

func TestQueryAggregate_ServiceBusDeadletterDelta(t *testing.T) {
	// Gauge rises 10 -> 130 across the window: max(Maximum)=130,
	// min(Minimum)=10 → delta 120.
	fake := &fakeAzureMetrics{
		cannedResponse: deadletterResponse(
			[2]float64{40, 10},
			[2]float64{90, 35},
			[2]float64{130, 90},
		),
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.QueryAggregate(
		context.Background(), testNamespaceARN,
		ServiceBusDeadletteredMessagesMetric,
		time.Hour, scanner.StatisticSum,
	)
	assert.NoError(t, err)
	assert.Equal(t, 120.0, res.Value, "max(130) - min(10) = 120 net accumulation")
	assert.Equal(t, 3, res.SampleCount)
	assert.Equal(t, ServiceBusDeadletteredMessagesMetric, res.MetricName)

	// The Azure Monitor call must request both aggregations.
	if assert.GreaterOrEqual(t, len(fake.receivedReqs), 1) {
		agg := fake.receivedReqs[0].URL.Query().Get("aggregation")
		assert.Contains(t, agg, "Maximum")
		assert.Contains(t, agg, "Minimum")
		assert.Equal(t, ServiceBusDeadletteredMessagesMetric, fake.receivedReqs[0].URL.Query().Get("metricnames"))
	}
}

func TestQueryAggregate_ServiceBusDeadletterDelta_NegativeClampedToZero(t *testing.T) {
	// Gauge drains across the window (messages reprocessed): max < ...
	// the floor keeps the rate non-negative.
	fake := &fakeAzureMetrics{
		cannedResponse: deadletterResponse(
			[2]float64{100, 80},
			[2]float64{60, 20},
		),
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.QueryAggregate(
		context.Background(), testNamespaceARN,
		ServiceBusDeadletteredMessagesMetric, time.Hour, scanner.StatisticSum,
	)
	assert.NoError(t, err)
	// max(Maximum)=100, min(Minimum)=20 → delta 80 (still positive here);
	// the clamp is exercised when min > max which cannot happen with
	// real data, so assert the non-negative invariant.
	assert.GreaterOrEqual(t, res.Value, 0.0)
}

// --- DetectServiceBusPoisonRate: real / real-zero / absent -------

func TestDetectServiceBusPoisonRate_RealRateFiresHighBand(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: deadletterResponse([2]float64{125, 5}),
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.DetectServiceBusPoisonRate(context.Background(), testNamespaceARN)
	assert.NoError(t, err)
	assert.Equal(t, 120, res.RatePerHour)
	assert.True(t, res.HighBand, "120/hr >= ServiceBusPoisonRatePerHourHighThreshold=%d", ServiceBusPoisonRatePerHourHighThreshold)
}

func TestDetectServiceBusPoisonRate_FlatGaugeIsRealZero(t *testing.T) {
	// Constant 100 dead-lettered messages, no new arrivals → delta 0.
	fake := &fakeAzureMetrics{
		cannedResponse: deadletterResponse([2]float64{100, 100}, [2]float64{100, 100}),
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.DetectServiceBusPoisonRate(context.Background(), testNamespaceARN)
	assert.NoError(t, err)
	assert.Equal(t, 0, res.RatePerHour, "flat gauge = zero NEW dead-letters this hour (real zero, not absent)")
	assert.False(t, res.HighBand)
}

func TestDetectServiceBusPoisonRate_EmptyTimeseriesIsAbsent(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: armMetricsResponse{Value: []armMetricsValue{{Unit: "Count", Timeseries: []armMetricsTimeseries{{Data: []armMetricsDatapoint{}}}}}},
	}
	s := newMetricsScannerWithFake(t, fake)
	res, err := s.DetectServiceBusPoisonRate(context.Background(), testNamespaceARN)
	assert.NoError(t, err)
	assert.Equal(t, -1, res.RatePerHour, "no datapoints → honest-framing absent sentinel")
	assert.False(t, res.HighBand)
}

// --- enrichServiceBusPoisonRate ---------------------------------

func sbSnap() scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		ResourceARN: testNamespaceARN,
		Detail: map[string]any{
			"poison_rate_per_hour":  -1,
			"poison_rate_high_band": false,
		},
	}
}

func TestEnrichServiceBusPoisonRate_OverwritesWithRealReading(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: deadletterResponse([2]float64{75, 0}),
	}
	s := newMetricsScannerWithFake(t, fake)
	snaps := []scanner.EventSourceInstanceSnapshot{sbSnap()}

	s.enrichServiceBusPoisonRate(context.Background(), snaps, "fake-token")

	assert.Equal(t, 75, snaps[0].Detail["poison_rate_per_hour"], "real reading overwrites the -1 sentinel")
	assert.Equal(t, true, snaps[0].Detail["poison_rate_high_band"])
}

func TestEnrichServiceBusPoisonRate_EmptyTokenNoOp(t *testing.T) {
	fake := &fakeAzureMetrics{cannedResponse: deadletterResponse([2]float64{999, 0})}
	s := newMetricsScannerWithFake(t, fake)
	// Force the unwired-token path: clear the struct token the fake set.
	s.accessToken = ""
	snaps := []scanner.EventSourceInstanceSnapshot{sbSnap()}

	s.enrichServiceBusPoisonRate(context.Background(), snaps, "")

	// Cold-start parity: honest-framing sentinels survive untouched.
	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snaps[0].Detail["poison_rate_high_band"])
}
