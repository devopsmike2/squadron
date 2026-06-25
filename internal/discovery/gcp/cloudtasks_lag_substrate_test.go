// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Consumer-lag substrate slice 5 chunk 1 (v0.89.182, #824 Stream 221)
// — acceptance tests per docs/proposals/consumer-lag-substrate-slice5.md
// §8. REAL Cloud Monitoring-backed GCP Cloud Tasks BACKLOG detection
// that closes the slice-2 §3.1 lag deferral (backlog half).

const testLagQueueResourceName = "projects/test-project/locations/us-central1/queues/orders"

// --- constants pins ---------------------------------------------

func TestCloudTasksQueueDepthMetricType_Constant(t *testing.T) {
	assert.Equal(t, "cloudtasks.googleapis.com/queue/depth", CloudTasksQueueDepthMetricType)
}

func TestCloudTasksBacklogDepthHighThreshold_Constant(t *testing.T) {
	assert.Equal(t, 1000, CloudTasksBacklogDepthHighThreshold)
}

// --- gauge routing: ALIGN_MAX aligner + queue_id filter ----------

func TestQueryAggregate_CloudTasksDepth_GaugeMaxRollup(t *testing.T) {
	// Peak depth across periods: MAX(200, 1500, 900) = 1500.
	f := &metricsFake{
		respondWith: []TimeSeriesPoint{
			{Value: 200, SampleCount: 1},
			{Value: 1500, SampleCount: 1},
			{Value: 900, SampleCount: 1},
		},
	}
	s := newMetricsTestScannerWithFake(t, f)
	res, err := s.QueryAggregate(
		context.Background(), testLagQueueResourceName,
		CloudTasksQueueDepthMetricType, time.Hour, scanner.StatisticP95,
	)
	assert.NoError(t, err)
	assert.Equal(t, 1500.0, res.Value, "gauge uses MAX rollup (peak depth)")

	if assert.Len(t, f.receivedStat, 1) {
		assert.Equal(t, "ALIGN_MAX", f.receivedStat[0], "gauge uses the ALIGN_MAX aligner")
	}
	if assert.Len(t, f.receivedFilter, 1) {
		flt := f.receivedFilter[0]
		assert.Contains(t, flt, `resource.labels.queue_id = "orders"`)
		assert.Contains(t, flt, CloudTasksQueueDepthMetricType)
		assert.NotContains(t, flt, "response_code", "depth counts ALL tasks, no failure filter")
	}
}

// --- DetectCloudTasksBacklog: real / empty-queue / absent --------

func TestDetectCloudTasksBacklog_RealDepthFiresHighBand(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 1500, SampleCount: 1}}}
	s := newMetricsTestScannerWithFake(t, f)
	depth, samples, err := s.DetectCloudTasksBacklog(context.Background(), testLagQueueResourceName)
	assert.NoError(t, err)
	assert.Equal(t, 1500, depth)
	assert.Equal(t, 1, samples)
}

func TestDetectCloudTasksBacklog_EmptyQueueIsRealZero(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 0, SampleCount: 1}}}
	s := newMetricsTestScannerWithFake(t, f)
	depth, samples, err := s.DetectCloudTasksBacklog(context.Background(), testLagQueueResourceName)
	assert.NoError(t, err)
	assert.Equal(t, 0, depth, "SampleCount>0 with depth 0 is a real empty queue, not absent")
	assert.Equal(t, 1, samples)
}

func TestDetectCloudTasksBacklog_NoDatapointsIsAbsent(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{}}
	s := newMetricsTestScannerWithFake(t, f)
	depth, samples, err := s.DetectCloudTasksBacklog(context.Background(), testLagQueueResourceName)
	assert.NoError(t, err)
	assert.Equal(t, -1, depth, "no datapoints → honest-framing absent sentinel")
	assert.Equal(t, 0, samples)
}

// --- enrichment: overwrites backlog, leaves silence honest-framed -

func lagSnap() scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		ResourceARN: testLagQueueResourceName,
		Detail: map[string]any{
			"lag_backlog_depth":            -1,
			"lag_backlog_depth_high":       false,
			"lag_consumer_silence_seconds": -1,
			"lag_consumer_silence_high":    false,
		},
	}
}

func TestEnrichCloudTasksLag_OverwritesBacklogLeavesSilence(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 1200, SampleCount: 1}}}
	s := newMetricsTestScannerWithFake(t, f)
	snaps := []scanner.EventSourceInstanceSnapshot{lagSnap()}

	s.enrichCloudTasksLag(context.Background(), snaps)

	assert.Equal(t, 1200, snaps[0].Detail["lag_backlog_depth"], "backlog now real")
	assert.Equal(t, true, snaps[0].Detail["lag_backlog_depth_high"])
	// Silence keys stay honest-framed (Cloud Tasks has no oldest-age metric).
	assert.Equal(t, -1, snaps[0].Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, false, snaps[0].Detail["lag_consumer_silence_high"])
}

func TestEnrichCloudTasksLag_NilClientNoOp(t *testing.T) {
	s := newMetricsTestScanner() // no Cloud Monitoring client wired
	snaps := []scanner.EventSourceInstanceSnapshot{lagSnap()}

	s.enrichCloudTasksLag(context.Background(), snaps)

	// Cold-start parity: all four honest-framing keys survive.
	assert.Equal(t, -1, snaps[0].Detail["lag_backlog_depth"])
	assert.Equal(t, false, snaps[0].Detail["lag_backlog_depth_high"])
	assert.Equal(t, -1, snaps[0].Detail["lag_consumer_silence_seconds"])
}

func TestEnrichCloudTasksLag_NoDatapointsKeepsSentinel(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{}} // empty → absent
	s := newMetricsTestScannerWithFake(t, f)
	snaps := []scanner.EventSourceInstanceSnapshot{lagSnap()}

	s.enrichCloudTasksLag(context.Background(), snaps)

	assert.Equal(t, -1, snaps[0].Detail["lag_backlog_depth"], "no data → sentinel kept, not overwritten to 0")
}
