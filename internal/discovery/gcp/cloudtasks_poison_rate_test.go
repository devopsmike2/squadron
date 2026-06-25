// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Poison-message rate analysis slice 3 chunk 2 (v0.89.174,
// #816 Stream 213) — GCP Cloud Tasks honest-framing tests
// per design doc §3.3 + §11.3.

func TestDetectCloudTasksPoisonRate_AlwaysHonestFramingState(t *testing.T) {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := detectCloudTasksPoisonRate(tc.q)
			assert.Equal(t, -1, res.RatePerHour,
				"slice 3 §3.3 invariant: Cloud Monitoring substrate integration deferred")
			assert.Equal(t, false, res.HighBand)
		})
	}
}

func TestApplyCloudTasksPoisonRateDetail_AdditiveOnly_PreservesPriorKeys(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	q := makeCloudTasksQueue(project, region, "parity")
	q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: 7}
	q.RateLimits = &cloudTasksRateLimits{MaxBurstSize: 100}
	q.State = "RUNNING"

	fake := newFakeCloudTasks()
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)

	// Slice-5 keys preserved.
	assert.Equal(t, int32(7), out[0].Detail["max_attempts"])
	assert.Equal(t, "RUNNING", out[0].Detail["state"])

	// Slice-1-DLQ keys preserved.
	assert.Equal(t, false, out[0].Detail["has_dlq_pattern_likely"])
	assert.Equal(t, 7, out[0].Detail["dlq_retry_count"])

	// Slice-2-lag honest-framing keys preserved.
	assert.Equal(t, -1, out[0].Detail["lag_backlog_depth"])

	// Slice-3 poison-rate honest-framing keys also present.
	assert.Equal(t, -1, out[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, false, out[0].Detail["poison_rate_high_band"])
}
