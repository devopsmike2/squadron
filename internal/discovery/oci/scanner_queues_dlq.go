// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// DLQ configuration analysis slice 1 chunk 4 (v0.89.166, #808
// Stream 205) — OCI Queue Service DLQ detection.
//
// OCI Queue Service has a managed DLQ primitive driven by a SINGLE
// field: deadLetterQueueDeliveryCount. The field has dual semantics
// per design doc §3 + §11.14-16:
//
//   - deadLetterQueueDeliveryCount > 0 → has_dlq=true. The OCI
//     Queue Service runtime auto-routes messages exceeding the
//     delivery-count threshold to a managed DLQ.
//   - deadLetterQueueDeliveryCount == 0 → has_dlq=false. Messages
//     redeliver indefinitely until the retention window expires.
//   - deadLetterQueueDeliveryCount > 0 also IS the retry count —
//     the same field gates DLQ presence AND counts retries.
//
// This is the cleanest of the four per-cloud DLQ shapes (a single
// field, no JSON parsing, no separate retry-count field). Both
// detection axes derive from the same source.
//
// Cold-start parity invariant: the chunk 4 patch is ADDITIVE only.
// The slice-9 Detail keys (lifecycle_state, compartment_id,
// has_log_group, visibility_in_seconds, retention_in_seconds,
// dead_letter_queue_delivery_count, kms_key_id_set) survive
// byte-identically. A caller that has not yet adopted the new DLQ
// axis keys sees byte-identical output to v0.89.165.

// DLQ retry-count band shared with the AWS chunk 1 + GCP chunk 2
// helpers. The shared band makes the per-cloud detection
// thresholds consistent across the queue tier; a future tuning
// slice that splits per-cloud thresholds will surface the
// per-cloud reasoning explicitly. Re-declared locally (rather
// than imported from aws or gcp) so the oci package has no
// per-DLQ-axis cross-package dependency — the design doc's
// "shared band" semantics are encoded by the IDENTICAL VALUE,
// not by import.
const (
	OCIQueuesDLQRetryCountBandMin = 2
	OCIQueuesDLQRetryCountBandMax = 50
)

// ociQueueDLQDetectionResult is the bare result of
// detectOCIQueueDLQ. Three fields directly mirror the three Detail
// keys the chunk 4 projection writes.
type ociQueueDLQDetectionResult struct {
	HasDLQ           bool
	RetryCount       int
	RetryCountInBand bool
}

// detectOCIQueueDLQ inspects an ociQueue value and returns the
// three slice 1 DLQ axis signals.
//
// Per design doc §3 + §11.14-16:
//
//   - DeadLetterQueueDeliveryCount > 0 → HasDLQ=true,
//     RetryCount=value, RetryCountInBand=true iff value is in
//     [OCIQueuesDLQRetryCountBandMin,
//     OCIQueuesDLQRetryCountBandMax] inclusive.
//   - DeadLetterQueueDeliveryCount == 0 → HasDLQ=false,
//     RetryCount=-1 (absent sentinel matches AWS chunk 1
//     convention), RetryCountInBand=false.
//
// Negative values are treated identically to 0 (defensive — the OCI
// API spec defines deadLetterQueueDeliveryCount as a non-negative
// integer; a negative value would indicate a contract violation
// from the runtime).
func detectOCIQueueDLQ(q ociQueue) ociQueueDLQDetectionResult {
	count := q.DeadLetterQueueDeliveryCount
	if count <= 0 {
		return ociQueueDLQDetectionResult{
			HasDLQ:           false,
			RetryCount:       -1,
			RetryCountInBand: false,
		}
	}
	inBand := count >= OCIQueuesDLQRetryCountBandMin && count <= OCIQueuesDLQRetryCountBandMax
	return ociQueueDLQDetectionResult{
		HasDLQ:           true,
		RetryCount:       count,
		RetryCountInBand: inBand,
	}
}

// applyOCIQueueDLQDetail writes the three slice 1 DLQ axis Detail
// keys onto an already-initialized snapshot. The caller
// (projectOCIQueue) is responsible for initializing snap.Detail;
// this helper writes directly without re-checking nil so it stays
// a thin layer atop the existing projection.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-9 existing Detail keys (lifecycle_state,
// compartment_id, has_log_group, visibility_in_seconds,
// retention_in_seconds, dead_letter_queue_delivery_count,
// kms_key_id_set).
//
// Pattern mirrors applySQSDLQDetail + applyCloudTasksDLQDetail +
// applyServiceBusDLQDetail in the per-cloud packages. All four
// helpers share the slice 1 DLQ axis shape (three Detail keys);
// the semantic differences live in the per-cloud detect*
// helpers.
func applyOCIQueueDLQDetail(snap *scanner.EventSourceInstanceSnapshot, q ociQueue) {
	res := detectOCIQueueDLQ(q)
	snap.Detail["has_dlq"] = res.HasDLQ
	snap.Detail["dlq_retry_count"] = res.RetryCount
	snap.Detail["dlq_retry_count_in_band"] = res.RetryCountInBand
}
