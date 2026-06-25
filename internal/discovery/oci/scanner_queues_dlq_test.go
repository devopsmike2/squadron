// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/stretchr/testify/assert"
)

// DLQ configuration analysis slice 1 chunk 4 (v0.89.166, #808
// Stream 205) — OCI Queue Service acceptance tests per
// docs/proposals/dlq-configuration-analysis-slice1.md §11.14-16.

// --- Test §11.14: deadLetterQueueDeliveryCount=0 → has_dlq=false --

func TestDetectOCIQueueDLQ_ZeroDeliveryCount_HasDLQFalse(t *testing.T) {
	q := ociQueue{
		ID:                           "ocid1.queue.oc1.iad.aaaaaaaa",
		DisplayName:                  "no-dlq",
		DeadLetterQueueDeliveryCount: 0,
	}
	res := detectOCIQueueDLQ(q)
	assert.Equal(t, false, res.HasDLQ,
		"deadLetterQueueDeliveryCount=0 → has_dlq=false, queues-dlq-attach fires")
	assert.Equal(t, -1, res.RetryCount,
		"absent sentinel matches AWS chunk 1 convention")
	assert.Equal(t, false, res.RetryCountInBand,
		"band check meaningless when DLQ absent")
}

// --- Test §11.15: in-band deliveryCount → has_dlq=true + in_band=true -

func TestDetectOCIQueueDLQ_InBandDeliveryCount_HasDLQTrue_InBandTrue(t *testing.T) {
	q := ociQueue{
		ID:                           "ocid1.queue.oc1.iad.in-band",
		DisplayName:                  "in-band",
		DeadLetterQueueDeliveryCount: 5,
	}
	res := detectOCIQueueDLQ(q)
	assert.Equal(t, true, res.HasDLQ)
	assert.Equal(t, 5, res.RetryCount)
	assert.Equal(t, true, res.RetryCountInBand,
		"deadLetterQueueDeliveryCount=5 sits inside [2, 50] — queues-dlq-retry-count-bound does NOT fire")
}

// --- Test §11.16: above-band deliveryCount → has_dlq=true + in_band=false -

func TestDetectOCIQueueDLQ_AboveBandDeliveryCount_OutOfBand_FiresBound(t *testing.T) {
	q := ociQueue{
		ID:                           "ocid1.queue.oc1.iad.lenient",
		DisplayName:                  "too-lenient",
		DeadLetterQueueDeliveryCount: 100,
	}
	res := detectOCIQueueDLQ(q)
	assert.Equal(t, true, res.HasDLQ)
	assert.Equal(t, 100, res.RetryCount)
	assert.Equal(t, false, res.RetryCountInBand,
		"deadLetterQueueDeliveryCount=100 above OCIQueuesDLQRetryCountBandMax=50 — queues-dlq-retry-count-bound fires")
}

// --- Boundary checks: in-band edges (2 and 50) ----------------------

func TestDetectOCIQueueDLQ_RetryCount_BoundaryEdges(t *testing.T) {
	cases := []struct {
		name         string
		count        int
		expectHasDLQ bool
		expectInBand bool
	}{
		{"lower-edge-2", 2, true, true},
		{"upper-edge-50", 50, true, true},
		{"just-below-lower", 1, true, false},
		{"just-above-upper", 51, true, false},
		{"explicit-zero", 0, false, false},
		{"negative-defensive", -1, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := ociQueue{
				DeadLetterQueueDeliveryCount: tc.count,
			}
			res := detectOCIQueueDLQ(q)
			assert.Equal(t, tc.expectHasDLQ, res.HasDLQ,
				"deliveryCount=%d should produce has_dlq=%v", tc.count, tc.expectHasDLQ)
			assert.Equal(t, tc.expectInBand, res.RetryCountInBand,
				"inclusive band [%d, %d] — count %d must produce in_band=%v",
				OCIQueuesDLQRetryCountBandMin, OCIQueuesDLQRetryCountBandMax,
				tc.count, tc.expectInBand)
		})
	}
}

// --- Cold-start parity: applyOCIQueueDLQDetail leaves slice-9 keys alone ---

func TestApplyOCIQueueDLQDetail_AdditiveOnly_PreservesSlice9Keys(t *testing.T) {
	q := ociQueue{
		ID:                           "ocid1.queue.oc1.iad.parity",
		DisplayName:                  "parity",
		LifecycleState:               "ACTIVE",
		VisibilityInSeconds:          30,
		RetentionInSeconds:           345600,
		DeadLetterQueueDeliveryCount: 7,
		CustomEncryptionKeyID:        "ocid1.key.oc1.iad.aaaa",
	}
	// Replicate projectOCIQueue's Detail bag shape (slice-9 keys),
	// then apply the chunk 4 helper and confirm the slice-9 keys
	// survive byte-identically alongside the new chunk 4 DLQ axis
	// keys.
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

	applyOCIQueueDLQDetail(snap, q)

	// Slice-9 keys preserved byte-identically.
	assert.Equal(t, "ACTIVE", snap.Detail["lifecycle_state"])
	assert.Equal(t, "ocid1.compartment.oc1.iad.aaaa", snap.Detail["compartment_id"])
	assert.Equal(t, false, snap.Detail["has_log_group"])
	assert.Equal(t, 30, snap.Detail["visibility_in_seconds"])
	assert.Equal(t, 345600, snap.Detail["retention_in_seconds"])
	assert.Equal(t, 7, snap.Detail["dead_letter_queue_delivery_count"])
	assert.Equal(t, true, snap.Detail["kms_key_id_set"])

	// Slice-1 chunk 4 DLQ axis keys also present.
	assert.Equal(t, true, snap.Detail["has_dlq"])
	assert.Equal(t, 7, snap.Detail["dlq_retry_count"])
	assert.Equal(t, true, snap.Detail["dlq_retry_count_in_band"])
}
