// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// DLQ configuration analysis slice 1 chunk 2 (v0.89.164, #806
// Stream 203) — GCP Cloud Tasks DLQ detection.
//
// CLOUD TASKS SPECIAL CASE (design doc §3.1):
// GCP Cloud Tasks does NOT have a managed DLQ primitive. The
// canonical operator pattern is consumer-side dead-letter routing:
// the HTTP target / App Engine handler catches the final retry's
// failure and writes the task payload to a separate "dead-letter"
// queue or storage destination. Squadron CANNOT detect this from the
// Cloud Tasks admin surface alone.
//
// Slice 1 chunk 2 ships the Cloud Tasks DLQ axis with HONEST FRAMING:
//   - has_dlq_pattern_likely is ALWAYS false in slice 1.
//   - The cloudtasks-dlq-pattern-add recommendation (chunk 4
//     iacpicker) emits a sibling Cloud Tasks queue named
//     `${original}-dlq` AND a reasoning text calling out that
//     Squadron CANNOT verify the consumer-side wiring — the operator
//     PR review is load-bearing.
//
// The honest-framing pattern is load-bearing for slice 12+
// substrate-dependent depth work where Squadron will repeatedly hit
// detection rules it cannot prove from the admin API alone.
//
// The retry-count axis (dlq_retry_count + dlq_retry_count_in_band)
// uses the same [DLQRetryCountBandMin, DLQRetryCountBandMax] band as
// AWS SQS (chunk 1), with two Cloud-Tasks-specific quirks:
//   - retryConfig absent → dlq_retry_count = -1 (absent sentinel,
//     matches the chunk 1 convention).
//   - retryConfig.maxAttempts == CloudTasksMaxAttemptsUnlimited (-1)
//     means UNLIMITED retries. Operationally this is indistinguishable
//     from "absent" for DLQ purposes: poison messages never reach a
//     downstream side channel. Slice 1 treats unlimited as
//     dlq_retry_count = -1 + in_band = false. The slice-5 max_attempts
//     Detail key (preserved unchanged) still surfaces the raw
//     unlimited value for the drilldown.

// DLQ retry-count band shared with the AWS chunk 1 helper. The
// shared band makes the per-cloud detection thresholds consistent
// across the queue tier; a future tuning slice that splits per-cloud
// thresholds will surface the per-cloud reasoning explicitly.
const (
	// CloudTasksDLQRetryCountBandMin mirrors the AWS chunk 1 band
	// lower bound. Re-declared here (rather than imported from the
	// aws package) so the gcp package has no per-DLQ-axis cross-
	// package dependency — the design doc's "shared band" semantics
	// are encoded by the IDENTICAL VALUE, not by import. A future
	// per-cloud tuning slice can shift the bounds independently.
	CloudTasksDLQRetryCountBandMin = 2

	// CloudTasksDLQRetryCountBandMax mirrors the AWS chunk 1 band
	// upper bound. Same independence semantics as
	// CloudTasksDLQRetryCountBandMin.
	CloudTasksDLQRetryCountBandMax = 50
)

// cloudTasksDLQDetectionResult is the bare result of
// detectCloudTasksDLQ. Three fields mirror the three Detail keys the
// chunk 2 projection writes.
type cloudTasksDLQDetectionResult struct {
	// HasDLQPatternLikely is ALWAYS false in slice 1 per design doc
	// §3.1 — Cloud Tasks has no managed DLQ primitive Squadron can
	// detect from the admin API. The recommendation
	// cloudtasks-dlq-pattern-add fires unconditionally for every
	// Cloud Tasks queue (with the decline path framed honestly:
	// operators using a non-Cloud-Tasks dead-letter destination
	// like Pub/Sub, GCS, or BigQuery streaming have explicit
	// decline cases).
	HasDLQPatternLikely bool

	// RetryCount is the retryConfig.MaxAttempts when retryConfig is
	// present AND MaxAttempts is not the unlimited sentinel; -1
	// otherwise. The -1 sentinel matches the AWS chunk 1 convention
	// and preserves the absent-vs-zero distinction the proposer's
	// reasoning text depends on (the slice-5 max_attempts Detail
	// key still surfaces the raw value for the drilldown).
	RetryCount int

	// RetryCountInBand is true ONLY when retryConfig is present,
	// MaxAttempts is not unlimited, AND MaxAttempts is in
	// [CloudTasksDLQRetryCountBandMin, CloudTasksDLQRetryCountBandMax]
	// inclusive. Unlimited + absent + explicit zero all fail the
	// band check and fire cloudtasks-retry-count-bound.
	RetryCountInBand bool
}

// detectCloudTasksDLQ inspects a cloudTasksQueue and returns the
// three slice 1 DLQ axis signals.
//
// Per design doc §3.1 + §11.6-8:
//   - HasDLQPatternLikely is ALWAYS false (slice 1 honest framing).
//   - retryConfig absent → RetryCount = -1, RetryCountInBand = false.
//   - retryConfig.MaxAttempts == CloudTasksMaxAttemptsUnlimited
//     (-1) → RetryCount = -1, RetryCountInBand = false. Unlimited
//     retries are operationally indistinguishable from absent for
//     DLQ purposes.
//   - retryConfig.MaxAttempts in [2, 50] inclusive → RetryCount =
//     value, RetryCountInBand = true.
//   - retryConfig.MaxAttempts in {0, 1, 51+} → RetryCount = value,
//     RetryCountInBand = false.
func detectCloudTasksDLQ(q *cloudTasksQueue) cloudTasksDLQDetectionResult {
	res := cloudTasksDLQDetectionResult{
		HasDLQPatternLikely: false,
		RetryCount:          -1,
		RetryCountInBand:    false,
	}
	if q == nil || q.RetryConfig == nil {
		return res
	}
	if q.RetryConfig.MaxAttempts == CloudTasksMaxAttemptsUnlimited {
		// Unlimited → treat as absent for DLQ purposes. The raw -1
		// still surfaces via the slice-5 max_attempts Detail key for
		// the drilldown layer.
		return res
	}
	count := int(q.RetryConfig.MaxAttempts)
	res.RetryCount = count
	res.RetryCountInBand = count >= CloudTasksDLQRetryCountBandMin && count <= CloudTasksDLQRetryCountBandMax
	return res
}

// applyCloudTasksDLQDetail writes the three slice 1 DLQ axis Detail
// keys onto an already-initialized snapshot. The caller
// (buildCloudTasksSnapshot) is responsible for initializing
// snap.Detail or accepting a nil map and letting this helper
// initialize it. Pattern mirrors applySQSDLQDetail in the aws
// package.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-5 existing keys (max_attempts,
// max_retry_duration, stackdriver_sampling_ratio,
// max_dispatches_per_second, max_concurrent_dispatches,
// max_burst_size, state, purge_time). A caller that does NOT yet
// read the new keys sees byte-identical output to v0.89.163.
func applyCloudTasksDLQDetail(snap *scanner.EventSourceInstanceSnapshot, q *cloudTasksQueue) {
	res := detectCloudTasksDLQ(q)
	if snap.Detail == nil {
		snap.Detail = map[string]any{}
	}
	snap.Detail["has_dlq_pattern_likely"] = res.HasDLQPatternLikely
	snap.Detail["dlq_retry_count"] = res.RetryCount
	snap.Detail["dlq_retry_count_in_band"] = res.RetryCountInBand
}
