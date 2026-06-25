// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// DLQ configuration analysis slice 1 chunk 1 (v0.89.163, #805
// Stream 202) — AWS SQS DLQ detection helpers.
//
// First per-axis-depth slice chunk after the cross-cloud event source
// widening pass closed at 3-3-3-3 / 12 surfaces. See
// docs/proposals/dlq-configuration-analysis-slice1.md.
//
// Two detection rules per design doc §3:
//
//   - Missing DLQ: queue has no DLQ configured (RedrivePolicy
//     attribute absent or malformed). Poison messages redeliver
//     indefinitely. Fires sqs-dlq-attach.
//   - Inappropriate retry count: queue has DLQ but maxReceiveCount
//     is OUTSIDE [DLQRetryCountBandMin, DLQRetryCountBandMax]. Counts
//     below 2 send transient failures straight to the DLQ; counts
//     above 50 defer DLQ routing past the typical
//     consumer-restart-and-retry horizon. Fires
//     sqs-dlq-retry-count-bound.
//
// Implementation note: chunk 1 leaves the slice-4 detection axes
// (HasTraceAxis ← HasRedrivePolicy + HasLogAxis ← reachable DLQ ARN)
// completely unchanged. The DLQ axis Detail keys layer ADDITIVELY on
// top of the existing keys (redrive_policy_target_arn,
// redrive_policy_max_receive_count, etc.) so cold-start parity is
// preserved byte-identically for any caller that does not yet read
// the new keys.

// DLQRetryCountBandMin is the inclusive lower bound on the
// DLQ retry-count band per design doc §3. Counts below this value
// (typically 1) route transient failures straight to the DLQ on the
// first failed delivery, which is too aggressive for the typical
// consumer-restart-and-retry horizon. The band is heuristic; the
// constant lives in package-scope so a future tuning slice has an
// auditable starting point.
const DLQRetryCountBandMin = 2

// DLQRetryCountBandMax is the inclusive upper bound on the
// DLQ retry-count band per design doc §3. Counts above this value
// (typically a few hundred to "unlimited") defer DLQ routing past the
// typical consumer-restart-and-retry horizon, leaving poison messages
// to consume per-message processing budget for hours or days before
// the DLQ side-channel ever fires. Heuristic; same auditability rule
// as DLQRetryCountBandMin.
const DLQRetryCountBandMax = 50

// sqsDLQDetectionResult is the bare result of detectSQSDLQ. Three
// fields directly mirror the three Detail keys the chunk 1
// projection writes:
//
//   - HasDLQ ↔ Detail["has_dlq"]
//   - RetryCount ↔ Detail["dlq_retry_count"] (-1 sentinel when DLQ
//     is absent — preserves the absent-vs-zero distinction that
//     callers downstream may need to surface differently).
//   - RetryCountInBand ↔ Detail["dlq_retry_count_in_band"] (always
//     false when HasDLQ is false; the band check is meaningless when
//     the DLQ itself is absent).
type sqsDLQDetectionResult struct {
	HasDLQ           bool
	RetryCount       int
	RetryCountInBand bool
}

// detectSQSDLQ inspects a queueAttributes value and returns the
// three slice 1 DLQ axis signals.
//
// Detection rules per design doc §3 + §11.1-5 of the per-cloud test
// list:
//
//   - HasRedrivePolicy true → HasDLQ true. The slice 4 scanner
//     already gated HasRedrivePolicy on the
//     deadLetterTargetArn-non-empty + RedrivePolicy-parses
//     conditions, so a HasRedrivePolicy true value is already
//     defensively filtered (malformed JSON returns
//     HasRedrivePolicy=false from extractQueueAttributes, satisfying
//     test §11.5).
//   - When HasDLQ is true, RetryCount is the
//     RedrivePolicy.MaxReceiveCount value (zero-valued when the
//     operator explicitly set 0 — test §11.3 pins the out-of-band
//     case at value 1; explicit-0 is also out-of-band by the same
//     rule).
//   - RetryCountInBand is true ONLY when HasDLQ AND RetryCount is in
//     [DLQRetryCountBandMin, DLQRetryCountBandMax] inclusive.
//
// When HasDLQ is false, RetryCount returns -1 as the absent
// sentinel. This preserves the absent-vs-zero distinction that
// Detail-bag consumers downstream may need to surface differently
// (the proposer's reasoning text for sqs-dlq-attach explicitly does
// NOT include a retry-count number; conflating absent with explicit
// zero would muddy that decline-path framing).
func detectSQSDLQ(qa queueAttributes) sqsDLQDetectionResult {
	if !qa.HasRedrivePolicy || qa.RedrivePolicy == nil {
		return sqsDLQDetectionResult{
			HasDLQ:           false,
			RetryCount:       -1,
			RetryCountInBand: false,
		}
	}
	count := qa.RedrivePolicy.MaxReceiveCount
	inBand := count >= DLQRetryCountBandMin && count <= DLQRetryCountBandMax
	return sqsDLQDetectionResult{
		HasDLQ:           true,
		RetryCount:       count,
		RetryCountInBand: inBand,
	}
}

// applySQSDLQDetail writes the three slice 1 DLQ axis Detail keys
// (has_dlq, dlq_retry_count, dlq_retry_count_in_band) onto an
// already-initialized snapshot. The caller (buildSQSSnapshot) is
// responsible for initializing snap.Detail; this helper writes
// directly without re-checking nil so it stays a thin layer atop the
// existing projection.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-4 existing keys (redrive_policy_target_arn,
// redrive_policy_max_receive_count, kms_master_key_id, fifo_queue,
// content_based_deduplication, attribute_fetch_failed). A caller
// that does NOT yet read the new keys sees byte-identical output
// to v0.89.162.
func applySQSDLQDetail(snap *scanner.EventSourceInstanceSnapshot, qa queueAttributes) {
	res := detectSQSDLQ(qa)
	snap.Detail["has_dlq"] = res.HasDLQ
	snap.Detail["dlq_retry_count"] = res.RetryCount
	snap.Detail["dlq_retry_count_in_band"] = res.RetryCountInBand
}
