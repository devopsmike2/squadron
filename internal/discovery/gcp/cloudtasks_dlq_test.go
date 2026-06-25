// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DLQ configuration analysis slice 1 chunk 2 (v0.89.164, #806
// Stream 203) — GCP Cloud Tasks acceptance tests per
// docs/proposals/dlq-configuration-analysis-slice1.md §11.6-8 plus
// the §3.1 honest-framing invariant.

// --- Test §11.6: no retryConfig → has_dlq_pattern_likely=false + retry out-of-band -

func TestScanCloudTasksQueues_NoRetryConfig_DLQAxesAllFalse(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	q := makeCloudTasksQueue(project, region, "no-retry")
	// q.RetryConfig stays nil.

	fake := newFakeCloudTasks()
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.Equal(t, false, out[0].Detail["has_dlq_pattern_likely"],
		"slice 1 §3.1 invariant: has_dlq_pattern_likely is ALWAYS false — Squadron cannot detect Cloud Tasks DLQ patterns from the admin API")
	assert.Equal(t, -1, out[0].Detail["dlq_retry_count"],
		"absent retryConfig → -1 absent sentinel")
	assert.Equal(t, false, out[0].Detail["dlq_retry_count_in_band"],
		"absent retryConfig fires cloudtasks-retry-count-bound")
}

// --- Test §11.7: in-band maxAttempts → retry_count_in_band=true -----
//
// has_dlq_pattern_likely STAYS false per §3.1 even when the retry
// count is in band — the chunks 2-4 reasoning text for
// cloudtasks-dlq-pattern-add fires anyway because Squadron cannot
// verify the consumer-side wiring.

func TestScanCloudTasksQueues_InBandMaxAttempts_DLQPatternStillFalse(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	q := makeCloudTasksQueue(project, region, "in-band")
	q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: 5}

	fake := newFakeCloudTasks()
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.Equal(t, false, out[0].Detail["has_dlq_pattern_likely"],
		"§3.1: has_dlq_pattern_likely is ALWAYS false regardless of retry count")
	assert.Equal(t, 5, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, true, out[0].Detail["dlq_retry_count_in_band"],
		"maxAttempts=5 sits inside the [2, 50] band")
}

// --- Test §11.8: maxAttempts=-1 (unlimited) → retry_count_in_band=false -

func TestScanCloudTasksQueues_UnlimitedRetries_RetryOutOfBand(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	q := makeCloudTasksQueue(project, region, "unlimited")
	q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: CloudTasksMaxAttemptsUnlimited}

	fake := newFakeCloudTasks()
	fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

	out := runCloudTasksScan(t, fake, project, region)
	require.Len(t, out, 1)
	assert.Equal(t, false, out[0].Detail["has_dlq_pattern_likely"])
	assert.Equal(t, -1, out[0].Detail["dlq_retry_count"],
		"unlimited retries operationally indistinguishable from absent for DLQ purposes — surfaces as -1")
	assert.Equal(t, false, out[0].Detail["dlq_retry_count_in_band"],
		"unlimited retries fire cloudtasks-retry-count-bound (poison messages never reach a downstream side channel)")
	assert.Equal(t, int32(-1), out[0].Detail["max_attempts"],
		"the slice-5 max_attempts Detail key still surfaces the raw unlimited value for the drilldown")
}

// --- Boundary checks: in-band edges (2 and 50) ----------------------

func TestScanCloudTasksQueues_RetryCount_BoundaryEdges(t *testing.T) {
	const (
		project = "test-project"
		region  = "us-central1"
	)
	cases := []struct {
		name         string
		maxAttempts  int32
		expectInBand bool
	}{
		{"lower-edge-2", 2, true},
		{"upper-edge-50", 50, true},
		{"just-below-lower", 1, false},
		{"just-above-upper", 51, false},
		{"explicit-zero", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := makeCloudTasksQueue(project, region, "edge")
			q.RetryConfig = &cloudTasksRetryConfig{MaxAttempts: tc.maxAttempts}

			fake := newFakeCloudTasks()
			fake.QueuesByRegion[region] = []*cloudTasksQueue{q}

			out := runCloudTasksScan(t, fake, project, region)
			require.Len(t, out, 1)
			assert.Equal(t, tc.expectInBand, out[0].Detail["dlq_retry_count_in_band"],
				"inclusive band [%d, %d] — count %d must produce in_band=%v",
				CloudTasksDLQRetryCountBandMin, CloudTasksDLQRetryCountBandMax,
				tc.maxAttempts, tc.expectInBand)
		})
	}
}

// --- Cold-start parity: slice-5 Detail keys preserved ---------------
//
// The slice-1 chunk 2 patch is ADDITIVE only. The slice-5 keys
// (max_attempts, max_retry_duration, stackdriver_sampling_ratio,
// max_dispatches_per_second, max_concurrent_dispatches,
// max_burst_size, state, purge_time) survive byte-identically.

func TestScanCloudTasksQueues_DLQAxisAdditive_PreservesSlice5Keys(t *testing.T) {
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

	// Slice-5 detail keys preserved byte-identically.
	assert.Equal(t, int32(7), out[0].Detail["max_attempts"])
	assert.Equal(t, "300s", out[0].Detail["max_retry_duration"])
	assert.Equal(t, 10.0, out[0].Detail["max_dispatches_per_second"])
	assert.Equal(t, int32(5), out[0].Detail["max_concurrent_dispatches"])
	assert.Equal(t, int32(100), out[0].Detail["max_burst_size"])
	assert.Equal(t, "RUNNING", out[0].Detail["state"])
	assert.True(t, out[0].HasTraceAxis, "slice-5 HasTraceAxis preserved")

	// Slice-1 DLQ keys also present.
	assert.Equal(t, false, out[0].Detail["has_dlq_pattern_likely"])
	assert.Equal(t, 7, out[0].Detail["dlq_retry_count"])
	assert.Equal(t, true, out[0].Detail["dlq_retry_count_in_band"])
}
