// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Consumer lag detection slice 2 chunk 4 (v0.89.171, #813
// Stream 210) — OCI Queue Service lag detection. FINAL chunk of
// slice 2; CLOSES the consumer lag arc.
//
// OCI Queue Service's runtimeMetadata block carries the lag axis
// source fields:
//
//   - visibleMessages → backlog depth (analog of AWS SQS
//     ApproximateNumberOfMessages).
//   - timeStateLastChanged → a surrogate for consumer silence
//     (when did the queue last change state? if a queue has
//     visibleMessages but timeStateLastChanged > 5 minutes ago,
//     the consumer is not draining).
//
// runtimeMetadata is OPTIONAL in the queue list response. When
// absent, slice 2 chunk 4 returns the absent sentinel (-1) for
// both axes — same defensive posture as the slice 1 chunk 1 (AWS
// DLQ) malformed RedrivePolicy case + the slice 1 chunk 4 (OCI
// DLQ) zero/negative deadLetterQueueDeliveryCount case.
//
// Cold-start parity invariant: the chunk 4 patch is ADDITIVE only.
// The slice-9 + slice-1-DLQ existing Detail keys (lifecycle_state,
// compartment_id, has_log_group, visibility_in_seconds,
// retention_in_seconds, dead_letter_queue_delivery_count,
// kms_key_id_set, has_dlq, dlq_retry_count, dlq_retry_count_in_band)
// survive byte-identically. A caller that has not yet adopted the
// lag axis keys sees byte-identical output to v0.89.170.

// OCIBacklogDepthHighThreshold is the inclusive lower bound that
// flips lag_backlog_depth_high to true. 1000 mirrors the AWS chunk
// 1 BacklogDepthHighThreshold for cross-cloud consistency — a
// future per-cloud tuning slice can shift this independently.
const OCIBacklogDepthHighThreshold = 1000

// OCIConsumerSilenceHighThreshold is the inclusive lower bound (in
// seconds) that flips lag_consumer_silence_high to true. 300
// seconds (5 minutes) mirrors the AWS chunk 1
// ConsumerSilenceHighThreshold.
const OCIConsumerSilenceHighThreshold = 300

// ociQueueLagDetectionResult is the bare result of
// detectOCIQueueLag. Four fields mirror the four Detail keys.
type ociQueueLagDetectionResult struct {
	BacklogDepth           int
	BacklogDepthHigh       bool
	ConsumerSilenceSeconds int
	ConsumerSilenceHigh    bool
}

// detectOCIQueueLag inspects an ociQueue value and returns the
// four slice 2 lag axis signals.
//
// Detection rules per design doc §3 + §11.7-8:
//
//   - runtimeMetadata absent → -1 / false / -1 / false (absent
//     sentinels).
//   - runtimeMetadata.visibleMessages ≥
//     OCIBacklogDepthHighThreshold → BacklogDepthHigh = true.
//   - runtimeMetadata.timeStateLastChanged > now - 300s →
//     ConsumerSilenceSeconds = (now - timeStateLastChanged),
//     ConsumerSilenceHigh = true.
//
// The `now` function argument supports deterministic testing —
// callers pass a fixed time during tests, the production
// projection passes time.Now (slice 2 §11 acceptance pin).
func detectOCIQueueLag(q ociQueue, now time.Time) ociQueueLagDetectionResult {
	res := ociQueueLagDetectionResult{
		BacklogDepth:           -1,
		BacklogDepthHigh:       false,
		ConsumerSilenceSeconds: -1,
		ConsumerSilenceHigh:    false,
	}
	if q.RuntimeMetadata == nil {
		return res
	}
	// Backlog depth: visibleMessages → BacklogDepth +
	// BacklogDepthHigh. Defensive: visibleMessages of 0 is a
	// legitimate "queue is empty" reading, NOT an absent sentinel,
	// so it surfaces as BacklogDepth=0 + BacklogDepthHigh=false.
	res.BacklogDepth = q.RuntimeMetadata.VisibleMessages
	res.BacklogDepthHigh = q.RuntimeMetadata.VisibleMessages >= OCIBacklogDepthHighThreshold

	// Consumer silence: parse timeStateLastChanged (RFC3339).
	// Defensive: parse failures leave the silence axes at the
	// absent sentinel (-1 / false).
	if q.RuntimeMetadata.TimeStateLastChanged != "" {
		if ts, err := time.Parse(time.RFC3339, q.RuntimeMetadata.TimeStateLastChanged); err == nil {
			seconds := int(now.Sub(ts).Seconds())
			res.ConsumerSilenceSeconds = seconds
			res.ConsumerSilenceHigh = seconds >= OCIConsumerSilenceHighThreshold
		}
	}
	return res
}

// applyOCIQueueLagDetail writes the four slice 2 lag axis Detail
// keys onto an already-initialized snapshot. The caller
// (projectOCIQueue) is responsible for initializing snap.Detail.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-9 + slice-1-DLQ existing keys.
//
// Pattern mirrors applySQSLagDetail in the aws package +
// applyCloudTasksLagDetail / applyServiceBusLagDetail in the gcp
// / azure packages. All chunk authors share the slice 2 lag axis
// shape (four Detail keys); the OCI chunk uses real detection (vs
// honest framing) per design doc §3.
func applyOCIQueueLagDetail(snap *scanner.EventSourceInstanceSnapshot, q ociQueue) {
	res := detectOCIQueueLag(q, time.Now())
	snap.Detail["lag_backlog_depth"] = res.BacklogDepth
	snap.Detail["lag_backlog_depth_high"] = res.BacklogDepthHigh
	snap.Detail["lag_consumer_silence_seconds"] = res.ConsumerSilenceSeconds
	snap.Detail["lag_consumer_silence_high"] = res.ConsumerSilenceHigh
}
