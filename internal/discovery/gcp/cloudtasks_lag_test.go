// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Consumer lag detection slice 2 chunk 2 (v0.89.169, #811
// Stream 208) — GCP Cloud Tasks honest-framing tests per design
// doc §3.3 + §11.5.

// --- Test §3.3.A: queue-shape invariance — all queue shapes return absent state.

func TestDetectCloudTasksLag_AlwaysReturnsHonestFramingAbsentState(t *testing.T) {
	cases := []struct {
		name string
		q    *cloudTasksQueue
	}{
		{name: "nil queue", q: nil},
		{name: "empty queue", q: &cloudTasksQueue{}},
		{
			name: "queue with retry config",
			q: &cloudTasksQueue{
				Name:        "projects/p/locations/us-central1/queues/with-retry",
				RetryConfig: &cloudTasksRetryConfig{MaxAttempts: 5},
			},
		},
		{
			name: "queue with stackdriver logging",
			q: &cloudTasksQueue{
				Name: "projects/p/locations/us-central1/queues/with-logging",
				StackdriverLoggingConfig: &cloudTasksStackdriverLoggingConfig{
					SamplingRatio: 1.0,
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := detectCloudTasksLag(tc.q)
			assert.Equal(t, -1, res.BacklogDepth,
				"slice 2 §3.3 invariant: admin API does not surface task count")
			assert.Equal(t, false, res.BacklogDepthHigh)
			assert.Equal(t, -1, res.ConsumerSilenceSeconds,
				"slice 2 §3.3 invariant: admin API does not surface consumer-side activity")
			assert.Equal(t, false, res.ConsumerSilenceHigh)
		})
	}
}

// --- Test §3.3.B: applyCloudTasksLagDetail writes all four honest-framing keys.

func TestApplyCloudTasksLagDetail_WritesAllFourAxisKeys(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	q := makeCloudTasksQueue(project, region, "any-queue")

	fake := newFakeCloudTasks()
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.Equal(t, -1, out[0].Detail["lag_backlog_depth"])
	assert.Equal(t, false, out[0].Detail["lag_backlog_depth_high"])
	assert.Equal(t, -1, out[0].Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, false, out[0].Detail["lag_consumer_silence_high"])
}

// --- Test §3.3.C: cold-start parity — slice-5 + slice-1 DLQ keys preserved.

func TestApplyCloudTasksLagDetail_AdditiveOnly_PreservesPriorKeys(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	q := makeCloudTasksQueue(project, region, "parity")
	q.RetryConfig = &cloudTasksRetryConfig{
		MaxAttempts:      7,
		MaxRetryDuration: "300s",
	}
	q.RateLimits = &cloudTasksRateLimits{
		MaxDispatchesPerSecond:  10.0,
		MaxConcurrentDispatches: 5,
		MaxBurstSize:            100,
	}
	q.State = "RUNNING"

	fake := newFakeCloudTasks()
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)

	// Slice-5 keys preserved byte-identically.
	assert.Equal(t, int32(7), out[0].Detail["max_attempts"])
	assert.Equal(t, "300s", out[0].Detail["max_retry_duration"])
	assert.Equal(t, 10.0, out[0].Detail["max_dispatches_per_second"])
	assert.Equal(t, int32(5), out[0].Detail["max_concurrent_dispatches"])
	assert.Equal(t, int32(100), out[0].Detail["max_burst_size"])
	assert.Equal(t, "RUNNING", out[0].Detail["state"])

	// Slice-1 DLQ axis keys preserved byte-identically.
	assert.Equal(t, false, out[0].Detail["has_dlq_pattern_likely"])
	assert.Equal(t, 7, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, true, out[0].Detail["dlq_retry_count_in_band"])

	// Slice-2 lag axis honest-framing keys also present.
	assert.Equal(t, -1, out[0].Detail["lag_backlog_depth"])
	assert.Equal(t, false, out[0].Detail["lag_backlog_depth_high"])
	assert.Equal(t, -1, out[0].Detail["lag_consumer_silence_seconds"])
	assert.Equal(t, false, out[0].Detail["lag_consumer_silence_high"])
}
