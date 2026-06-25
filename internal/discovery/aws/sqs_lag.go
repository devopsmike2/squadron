// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"strconv"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Consumer lag detection slice 2 chunk 1 (v0.89.168, #810
// Stream 207) — AWS SQS lag axis detection.
//
// FIRST per-axis-depth axis after DLQ slice 1 closed at v0.89.166.
// Mirrors the slice 1 chunk 1 helper shape: detect* helper that
// returns a result struct, apply* helper that writes Detail keys
// onto an already-initialized snapshot.
//
// Detection rules per docs/proposals/consumer-lag-detection-slice2.md
// §3 + §11.1-4:
//
//   - Backlog depth: ApproximateNumberOfMessages > 1000 →
//     lag_backlog_depth_high = true.
//   - Consumer silence: ApproximateAgeOfOldestMessage > 300
//     seconds → lag_consumer_silence_high = true.
//
// Both signals are derived from the existing GetQueueAttributes
// call which already requests QueueAttributeNameAll — slice 2 just
// reads more fields from the same response payload. NO new API
// call, NO IAM extension.
//
// Cold-start parity invariant: the chunk 1 patch is ADDITIVE only.
// The slice-4 + slice-1 (DLQ) Detail keys
// (redrive_policy_target_arn, redrive_policy_max_receive_count,
// kms_master_key_id, fifo_queue, content_based_deduplication,
// attribute_fetch_failed, has_dlq, dlq_retry_count,
// dlq_retry_count_in_band) survive byte-identically. A caller that
// has not yet adopted the new lag axis keys sees byte-identical
// output to v0.89.166.

// SQSApproximateNumberOfMessagesAttr — the SQS attribute name for
// the queue's approximate backlog depth. Returned by
// GetQueueAttributes when AttributeNames includes "All" or
// "ApproximateNumberOfMessages".
const SQSApproximateNumberOfMessagesAttr = "ApproximateNumberOfMessages"

// SQSApproximateAgeOfOldestMessageAttr — the SQS attribute name for
// the age in seconds of the oldest non-deleted message in the queue.
// A consumer-side silence surrogate: high values indicate the
// consumer isn't draining the queue quickly enough.
const SQSApproximateAgeOfOldestMessageAttr = "ApproximateAgeOfOldestMessage"

// BacklogDepthHighThreshold is the inclusive lower bound that flips
// lag_backlog_depth_high to true. 1000 is the heuristic per design
// doc §4; a future tuning slice can shift this.
const BacklogDepthHighThreshold = 1000

// ConsumerSilenceHighThreshold is the inclusive lower bound (in
// seconds) that flips lag_consumer_silence_high to true. 300
// seconds (5 minutes) is the heuristic per design doc §4.
const ConsumerSilenceHighThreshold = 300

// sqsLagDetectionResult is the bare result of detectSQSLag. Four
// fields mirror the four Detail keys the chunk 1 projection writes.
type sqsLagDetectionResult struct {
	BacklogDepth          int
	BacklogDepthHigh      bool
	ConsumerSilenceSeconds int
	ConsumerSilenceHigh   bool
}

// detectSQSLag inspects the raw GetQueueAttributes attributes map
// and returns the four slice 2 lag axis signals.
//
// Defensive posture: attributes the API failed to surface OR that
// fail integer parsing produce the absent sentinel (-1) and the
// corresponding *_high boolean stays false. This mirrors the
// chunk 1 (DLQ) HasDLQ=false defensive posture for malformed
// RedrivePolicy.
func detectSQSLag(qa queueAttributes) sqsLagDetectionResult {
	res := sqsLagDetectionResult{
		BacklogDepth:           -1,
		BacklogDepthHigh:       false,
		ConsumerSilenceSeconds: -1,
		ConsumerSilenceHigh:    false,
	}
	if qa.Attributes == nil {
		return res
	}
	if depthStr, ok := qa.Attributes[SQSApproximateNumberOfMessagesAttr]; ok {
		if depth, err := strconv.Atoi(depthStr); err == nil {
			res.BacklogDepth = depth
			res.BacklogDepthHigh = depth >= BacklogDepthHighThreshold
		}
	}
	if ageStr, ok := qa.Attributes[SQSApproximateAgeOfOldestMessageAttr]; ok {
		if age, err := strconv.Atoi(ageStr); err == nil {
			res.ConsumerSilenceSeconds = age
			res.ConsumerSilenceHigh = age >= ConsumerSilenceHighThreshold
		}
	}
	return res
}

// applySQSLagDetail writes the four slice 2 lag axis Detail keys
// (lag_backlog_depth, lag_backlog_depth_high,
// lag_consumer_silence_seconds, lag_consumer_silence_high) onto an
// already-initialized snapshot. The caller (buildSQSSnapshot) is
// responsible for initializing snap.Detail; this helper writes
// directly without re-checking nil so it stays a thin layer atop
// the existing projection.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-4 + slice-1 (DLQ) existing keys. A caller that
// has not yet adopted the new lag axis keys sees byte-identical
// output to v0.89.166.
//
// Pattern mirrors applySQSDLQDetail in the same package + the per-
// cloud lag helpers in chunks 2-4 (which use honest-framing
// constants for the cloud surfaces where the field is unreachable).
func applySQSLagDetail(snap *scanner.EventSourceInstanceSnapshot, qa queueAttributes) {
	res := detectSQSLag(qa)
	snap.Detail["lag_backlog_depth"] = res.BacklogDepth
	snap.Detail["lag_backlog_depth_high"] = res.BacklogDepthHigh
	snap.Detail["lag_consumer_silence_seconds"] = res.ConsumerSilenceSeconds
	snap.Detail["lag_consumer_silence_high"] = res.ConsumerSilenceHigh
}
