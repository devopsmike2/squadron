// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-rate substrate slice 4 chunk 2 (v0.89.178, #820 Stream 217)
// — acceptance tests per docs/proposals/poison-rate-substrate-slice4.md
// §8. REAL Cloud Monitoring-backed GCP Cloud Tasks poison-rate
// detection that closes the slice-3 §3.3 deferral for GCP.

const testQueueResourceName = "projects/test-project/locations/us-central1/queues/orders"

// --- constants pins ---------------------------------------------

func TestCloudTasksTaskAttemptCountMetricType_Constant(t *testing.T) {
	assert.Equal(t, "cloudtasks.googleapis.com/queue/task_attempt_count", CloudTasksTaskAttemptCountMetricType)
}

func TestCloudTasksPoisonRatePerHourHighThreshold_Constant(t *testing.T) {
	assert.Equal(t, 60, CloudTasksPoisonRatePerHourHighThreshold)
}

func TestCloudTasksPoisonRateWindowHours_Constant(t *testing.T) {
	assert.Equal(t, 1, CloudTasksPoisonRateWindowHours)
}

// --- metric routing: filter scopes to queue_id + failed attempts -

func TestQueryAggregate_CloudTasksFailedAttempts_FilterAndSum(t *testing.T) {
	f := &metricsFake{
		respondWith: []TimeSeriesPoint{
			{Value: 40, SampleCount: 1},
			{Value: 50, SampleCount: 1},
		},
	}
	s := newMetricsTestScannerWithFake(t, f)
	res, err := s.QueryAggregate(
		context.Background(), testQueueResourceName,
		CloudTasksTaskAttemptCountMetricType,
		60*60*1000*1000*1000, // 1h in ns
		scanner.StatisticSum,
	)
	assert.NoError(t, err)
	assert.Equal(t, 90.0, res.Value, "SUM of per-period deltas across the window")
	assert.Equal(t, 2, res.SampleCount)

	if assert.Len(t, f.receivedFilter, 1) {
		flt := f.receivedFilter[0]
		assert.Contains(t, flt, `resource.labels.queue_id = "orders"`)
		assert.Contains(t, flt, `metric.labels.response_code != "OK"`)
		assert.Contains(t, flt, CloudTasksTaskAttemptCountMetricType)
	}
	if assert.Len(t, f.receivedStat, 1) {
		assert.Equal(t, "ALIGN_DELTA", f.receivedStat[0], "count metric uses ALIGN_DELTA")
	}
}

// --- DetectCloudTasksPoisonRate: real / real-zero / absent -------

func TestDetectCloudTasksPoisonRate_RealRateFiresHighBand(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 120, SampleCount: 2}}}
	s := newMetricsTestScannerWithFake(t, f)
	res, err := s.DetectCloudTasksPoisonRate(context.Background(), testQueueResourceName)
	assert.NoError(t, err)
	assert.Equal(t, 120, res.RatePerHour)
	assert.True(t, res.HighBand, "120/hr >= CloudTasksPoisonRatePerHourHighThreshold=%d", CloudTasksPoisonRatePerHourHighThreshold)
}

func TestDetectCloudTasksPoisonRate_RealZeroIsNotAbsent(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 0, SampleCount: 1}}}
	s := newMetricsTestScannerWithFake(t, f)
	res, err := s.DetectCloudTasksPoisonRate(context.Background(), testQueueResourceName)
	assert.NoError(t, err)
	assert.Equal(t, 0, res.RatePerHour, "SampleCount>0 with SUM 0 is a REAL zero, not absent")
	assert.False(t, res.HighBand)
}

func TestDetectCloudTasksPoisonRate_NoDatapointsKeepsAbsentSentinel(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{}}
	s := newMetricsTestScannerWithFake(t, f)
	res, err := s.DetectCloudTasksPoisonRate(context.Background(), testQueueResourceName)
	assert.NoError(t, err)
	assert.Equal(t, -1, res.RatePerHour, "SampleCount==0 → honest-framing absent sentinel")
	assert.False(t, res.HighBand)
}

// --- enrichCloudTasksPoisonRate ---------------------------------

func ctSnap() scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		ResourceARN: testQueueResourceName,
		Detail: map[string]any{
			"poison_rate_per_hour":  -1,
			"poison_rate_high_band": false,
		},
	}
}

func TestEnrichCloudTasksPoisonRate_OverwritesWithRealReading(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 75, SampleCount: 1}}}
	s := newMetricsTestScannerWithFake(t, f)
	snaps := []scanner.EventSourceInstanceSnapshot{ctSnap()}

	s.enrichCloudTasksPoisonRate(context.Background(), snaps)

	assert.Equal(t, 75, snaps[0].Detail["poison_rate_per_hour"], "real reading overwrites the -1 sentinel")
	assert.Equal(t, true, snaps[0].Detail["poison_rate_high_band"])
}

func TestEnrichCloudTasksPoisonRate_NilClientNoOp(t *testing.T) {
	s := newMetricsTestScanner() // no Cloud Monitoring client wired
	snaps := []scanner.EventSourceInstanceSnapshot{ctSnap()}

	s.enrichCloudTasksPoisonRate(context.Background(), snaps)

	// Cold-start parity: honest-framing sentinels survive untouched.
	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snaps[0].Detail["poison_rate_high_band"])
}

func TestEnrichCloudTasksPoisonRate_EmptyARNSkipped(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 999, SampleCount: 1}}}
	s := newMetricsTestScannerWithFake(t, f)
	snap := scanner.EventSourceInstanceSnapshot{
		ResourceARN: "",
		Detail:      map[string]any{"poison_rate_per_hour": -1, "poison_rate_high_band": false},
	}
	snaps := []scanner.EventSourceInstanceSnapshot{snap}

	s.enrichCloudTasksPoisonRate(context.Background(), snaps)

	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, 0, f.calls, "no metric query for an empty ARN")
}

// --- guard: the failed-attempt filter excludes OK, not all codes -

func TestCloudTasksFilter_ExcludesOnlyOKResponses(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 1, SampleCount: 1}}}
	s := newMetricsTestScannerWithFake(t, f)
	_, _ = s.DetectCloudTasksPoisonRate(context.Background(), testQueueResourceName)
	flt := f.receivedFilter[0]
	assert.True(t, strings.Contains(flt, `!= "OK"`), "filter must count non-OK (failed) attempts only")
}
