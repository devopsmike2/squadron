// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
)

// Consumer lag detection slice 2 chunk 4 (v0.89.171, #813
// Stream 210) — OCI Queue Service acceptance tests per
// docs/proposals/consumer-lag-detection-slice2.md §11.7-8.

// --- Test §11.7: large backlog + old state change → both axes fire.

func TestDetectOCIQueueLag_LargeBacklogOldState_BothAxesFire(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-400 * time.Second).Format(time.RFC3339)
	q := ociQueue{
		ID:          "ocid1.queue.oc1.iad.stalled",
		DisplayName: "stalled",
		RuntimeMetadata: &ociQueueRuntimeMetadata{
			VisibleMessages:      2000,
			TimeStateLastChanged: old,
		},
	}
	res := detectOCIQueueLag(q, now)
	assert.Equal(t, 2000, res.BacklogDepth)
	assert.Equal(t, true, res.BacklogDepthHigh,
		"visibleMessages=2000 ≥ OCIBacklogDepthHighThreshold=1000 — queues-backlog-monitor-add fires")
	assert.Equal(t, 400, res.ConsumerSilenceSeconds)
	assert.Equal(t, true, res.ConsumerSilenceHigh,
		"silence=400s ≥ OCIConsumerSilenceHighThreshold=300s — queues-consumer-silence-investigate fires")
}

// --- Test §11.8: small backlog → no backlog-high firing.

func TestDetectOCIQueueLag_SmallBacklog_NoBacklogFiring(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	q := ociQueue{
		RuntimeMetadata: &ociQueueRuntimeMetadata{
			VisibleMessages:      500,
			TimeStateLastChanged: now.Add(-60 * time.Second).Format(time.RFC3339),
		},
	}
	res := detectOCIQueueLag(q, now)
	assert.Equal(t, 500, res.BacklogDepth)
	assert.Equal(t, false, res.BacklogDepthHigh)
	assert.Equal(t, 60, res.ConsumerSilenceSeconds)
	assert.Equal(t, false, res.ConsumerSilenceHigh)
}

// --- Defensive: runtimeMetadata absent → absent sentinels.

func TestDetectOCIQueueLag_RuntimeMetadataAbsent_AbsentSentinels(t *testing.T) {
	q := ociQueue{
		ID:          "ocid1.queue.oc1.iad.no-meta",
		DisplayName: "no-meta",
		// RuntimeMetadata stays nil.
	}
	res := detectOCIQueueLag(q, time.Now())
	assert.Equal(t, -1, res.BacklogDepth)
	assert.Equal(t, false, res.BacklogDepthHigh)
	assert.Equal(t, -1, res.ConsumerSilenceSeconds)
	assert.Equal(t, false, res.ConsumerSilenceHigh)
}

// --- Defensive: malformed timeStateLastChanged → silence axes stay at absent sentinel.

func TestDetectOCIQueueLag_MalformedTimestamp_SilenceAxesAbsent(t *testing.T) {
	q := ociQueue{
		RuntimeMetadata: &ociQueueRuntimeMetadata{
			VisibleMessages:      1500,
			TimeStateLastChanged: "not-a-valid-rfc3339-timestamp",
		},
	}
	res := detectOCIQueueLag(q, time.Now())
	// Backlog axis still surfaces despite the timestamp parse
	// failure — defensive isolation.
	assert.Equal(t, 1500, res.BacklogDepth)
	assert.Equal(t, true, res.BacklogDepthHigh)
	// Silence axes default to absent sentinel.
	assert.Equal(t, -1, res.ConsumerSilenceSeconds)
	assert.Equal(t, false, res.ConsumerSilenceHigh)
}

// --- Boundary check: in/out-of-band edges -----------------------

func TestDetectOCIQueueLag_BoundaryEdges(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name              string
		visibleMessages   int
		silenceSeconds    int
		expectBacklogHigh bool
		expectSilenceHigh bool
	}{
		{"backlog-just-below-1000", 999, 0, false, false},
		{"backlog-equal-1000-inclusive", 1000, 0, true, false},
		{"silence-just-below-300", 0, 299, false, false},
		{"silence-equal-300-inclusive", 0, 300, false, true},
		{"empty-queue-zero-not-absent", 0, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := now.Add(-time.Duration(tc.silenceSeconds) * time.Second).Format(time.RFC3339)
			q := ociQueue{
				RuntimeMetadata: &ociQueueRuntimeMetadata{
					VisibleMessages:      tc.visibleMessages,
					TimeStateLastChanged: ts,
				},
			}
			res := detectOCIQueueLag(q, now)
			assert.Equal(t, tc.expectBacklogHigh, res.BacklogDepthHigh,
				"OCIBacklogDepthHighThreshold=%d — visibleMessages=%d must produce backlog_high=%v",
				OCIBacklogDepthHighThreshold, tc.visibleMessages, tc.expectBacklogHigh)
			assert.Equal(t, tc.expectSilenceHigh, res.ConsumerSilenceHigh,
				"OCIConsumerSilenceHighThreshold=%d — silence=%d must produce silence_high=%v",
				OCIConsumerSilenceHighThreshold, tc.silenceSeconds, tc.expectSilenceHigh)
		})
	}
}

// --- Cold-start parity: applyOCIQueueLagDetail leaves slice-9 + slice-1-DLQ keys alone.

func TestApplyOCIQueueLagDetail_AdditiveOnly_PreservesPriorKeys(t *testing.T) {
	q := ociQueue{
		ID:                           "ocid1.queue.oc1.iad.parity",
		DisplayName:                  "parity",
		LifecycleState:               "ACTIVE",
		VisibilityInSeconds:          30,
		RetentionInSeconds:           345600,
		DeadLetterQueueDeliveryCount: 7,
		CustomEncryptionKeyID:        "ocid1.key.oc1.iad.aaaa",
		RuntimeMetadata: &ociQueueRuntimeMetadata{
			VisibleMessages:      1500,
			TimeStateLastChanged: time.Now().Add(-450 * time.Second).Format(time.RFC3339),
		},
	}
	snap := &scanner.EventSourceInstanceSnapshot{
		Detail: map[string]any{
			"lifecycle_state":                  q.LifecycleState,
			"compartment_id":                   "ocid1.compartment.oc1.iad.aaaa",
			"has_log_group":                    false,
			"visibility_in_seconds":            q.VisibilityInSeconds,
			"retention_in_seconds":             q.RetentionInSeconds,
			"dead_letter_queue_delivery_count": q.DeadLetterQueueDeliveryCount,
			"kms_key_id_set":                   q.CustomEncryptionKeyID != "",
		},
	}
	// Replicate the slice-1-DLQ Detail writes.
	applyOCIQueueDLQDetail(snap, q)
	applyOCIQueueLagDetail(snap, q)

	// Slice-9 keys preserved byte-identically.
	assert.Equal(t, "ACTIVE", snap.Detail["lifecycle_state"])
	assert.Equal(t, "ocid1.compartment.oc1.iad.aaaa", snap.Detail["compartment_id"])
	assert.Equal(t, 30, snap.Detail["visibility_in_seconds"])
	assert.Equal(t, 345600, snap.Detail["retention_in_seconds"])
	assert.Equal(t, 7, snap.Detail["dead_letter_queue_delivery_count"])

	// Slice-1-DLQ keys preserved byte-identically.
	assert.Equal(t, true, snap.Detail["has_dlq"])
	assert.Equal(t, 7, snap.Detail["dlq_retry_count"])
	assert.Equal(t, true, snap.Detail["dlq_retry_count_in_band"])

	// Slice-2 lag axis keys also present.
	assert.Equal(t, 1500, snap.Detail["lag_backlog_depth"])
	assert.Equal(t, true, snap.Detail["lag_backlog_depth_high"])
	assert.GreaterOrEqual(t, snap.Detail["lag_consumer_silence_seconds"], 449,
		"silence parsed from RFC3339 (allowing for sub-second drift)")
	assert.Equal(t, true, snap.Detail["lag_consumer_silence_high"])
}
