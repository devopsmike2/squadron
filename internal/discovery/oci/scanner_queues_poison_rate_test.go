// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
)

// Poison-message rate analysis slice 3 chunk 4 (v0.89.176, #818
// Stream 215) — OCI Queue Service acceptance tests per
// docs/proposals/poison-message-rate-slice3.md §11 (queue-shape
// invariance + cold-start parity). FINAL chunk; CLOSES slice 3.

// --- §3.3 invariance: ANY queue shape → absent state.
//
// The §3.3 honest-framing contract is that detection is
// substrate-metric-dependent and therefore ALWAYS returns the
// absent sentinel regardless of queue shape. This is the OCI
// analog of the AWS chunk 1 / GCP chunk 2 / Azure chunk 3
// invariance tests.

func TestDetectOCIQueuePoisonRate_InvariantAbsentAcrossShapes(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		q    ociQueue
	}{
		{
			name: "empty-queue",
			q:    ociQueue{},
		},
		{
			name: "no-runtime-metadata",
			q: ociQueue{
				ID:          "ocid1.queue.oc1.iad.no-meta",
				DisplayName: "no-meta",
			},
		},
		{
			name: "large-backlog-old-state-with-dlq",
			q: ociQueue{
				ID:                           "ocid1.queue.oc1.iad.busy",
				DisplayName:                  "busy",
				LifecycleState:               "ACTIVE",
				DeadLetterQueueDeliveryCount: 10,
				RuntimeMetadata: &ociQueueRuntimeMetadata{
					VisibleMessages:      50000,
					TimeStateLastChanged: now.Add(-2 * time.Hour).Format(time.RFC3339),
				},
			},
		},
		{
			name: "empty-runtime-metadata-zero-values",
			q: ociQueue{
				RuntimeMetadata: &ociQueueRuntimeMetadata{
					VisibleMessages:      0,
					TimeStateLastChanged: "",
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := detectOCIQueuePoisonRate(tc.q)
			assert.Equal(t, -1, res.RatePerHour,
				"§3.3 substrate-metric-dependence — poison_rate_per_hour is ALWAYS the absent sentinel; "+
					"OCI Monitoring SummarizeMetricsData integration deferred to a future slice")
			assert.Equal(t, false, res.HighBand,
				"§3.3 — poison_rate_high_band is ALWAYS false until the substrate integration lands; "+
					"OCIPoisonRatePerHourHighThreshold=%d is the future firing bound", OCIPoisonRatePerHourHighThreshold)
		})
	}
}

// --- applyOCIQueuePoisonRateDetail writes both honest-framing keys.

func TestApplyOCIQueuePoisonRateDetail_WritesAbsentKeys(t *testing.T) {
	q := ociQueue{
		ID:          "ocid1.queue.oc1.iad.keys",
		DisplayName: "keys",
	}
	snap := &scanner.EventSourceInstanceSnapshot{Detail: map[string]any{}}
	applyOCIQueuePoisonRateDetail(snap, q)
	assert.Equal(t, -1, snap.Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snap.Detail["poison_rate_high_band"])
}

// --- Cold-start parity: applyOCIQueuePoisonRateDetail leaves the
// slice-9 + slice-1-DLQ + slice-2-lag keys byte-identical.
//
// The chunk 4 patch is ADDITIVE: stacking the poison-rate helper
// on top of the prior DLQ + lag helpers must preserve every
// pre-existing Detail key. This is the load-bearing cold-start
// parity guarantee for slice 3 chunk 4.

func TestApplyOCIQueuePoisonRateDetail_AdditiveOnly_PreservesPriorKeys(t *testing.T) {
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
	// Replicate the full projection stack order: slice-1-DLQ, then
	// slice-2-lag, then slice-3-poison-rate.
	applyOCIQueueDLQDetail(snap, q)
	applyOCIQueueLagDetail(snap, q)
	applyOCIQueuePoisonRateDetail(snap, q)

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

	// Slice-2-lag keys preserved byte-identically.
	assert.Equal(t, 1500, snap.Detail["lag_backlog_depth"])
	assert.Equal(t, true, snap.Detail["lag_backlog_depth_high"])
	assert.GreaterOrEqual(t, snap.Detail["lag_consumer_silence_seconds"], 449,
		"silence parsed from RFC3339 (allowing for sub-second drift)")
	assert.Equal(t, true, snap.Detail["lag_consumer_silence_high"])

	// Slice-3 poison-rate axis keys present + honest-framing absent.
	assert.Equal(t, -1, snap.Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snap.Detail["poison_rate_high_band"])
}
