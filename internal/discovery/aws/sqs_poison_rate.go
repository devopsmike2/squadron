// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-message rate analysis slice 3 chunk 1 (v0.89.173,
// #815 Stream 212) — AWS SQS poison-rate axis with §3.3
// honest framing.
//
// SUBSTRATE-METRIC-DEPENDENCE (design doc §3.2 + §3.3):
// The per-queue poison-message rate over a rolling 1-hour
// window requires the DLQ ApproximateNumberOfMessages
// metric delta over time, which is only readable via
// CloudWatch GetMetricStatistics. Slice 3 chunk 1 does NOT
// yet integrate with the CloudWatch substrate (mirrors how
// the cold-start latency slice 1 + 2 arc built the
// MetricQuerier substrate per cloud).
//
// Slice 3 chunk 1 ships the AWS SQS poison-rate axis with
// HONEST FRAMING:
//   - poison_rate_per_hour ALWAYS = -1 (absent sentinel).
//   - poison_rate_high_band ALWAYS = false.
//   - The recommendation sqs-poison-rate-monitor-add fires
//     per SQS queue with reasoning text explicitly calling
//     out the CloudWatch integration gap + prompting the
//     operator to wire a CloudWatch alarm on the DLQ's
//     ApproximateNumberOfMessages metric.
//
// THIRD HONEST-FRAMING VARIANT in the taxonomy:
//   §3.1 (DLQ slice 1 chunk 2; lag slice 2 chunk 2):
//        managed-primitive-absence.
//   §3.2 (DLQ slice 1 chunk 3; lag slice 2 chunk 3):
//        scanner-coverage-gap.
//   §3.3 (NEW, slice 3 all chunks): substrate-metric-
//        dependence.
//
// §3.3 is the CLEANEST of the three because EVERY cloud
// sits under it for slice 3 — there is no mixed shape
// across chunks. A future slice closes §3.3 by extending
// each cloud scanner with the MetricQuerier calls.
//
// Cold-start parity invariant: the chunk 1 patch is
// ADDITIVE only. The slice-4 + slice-1-DLQ + slice-2-lag
// Detail keys survive byte-identically. A caller that has
// not yet adopted the new poison-rate axis keys sees
// byte-identical output to v0.89.172.

// PoisonRatePerHourHighThreshold is the inclusive lower
// bound that flips poison_rate_high_band to true in a
// future slice that integrates with the substrate
// MetricQuerier. 60 per hour (1 per minute) is heuristic
// per design doc §4.
const PoisonRatePerHourHighThreshold = 60

// sqsPoisonRateDetectionResult is the bare result of
// detectSQSPoisonRate. Two fields mirror the two Detail
// keys the chunk 1 projection writes — both hard-coded
// to absent state in slice 3.
type sqsPoisonRateDetectionResult struct {
	RatePerHour int
	HighBand    bool
}

// detectSQSPoisonRate returns the honest-framing absent
// state per design doc §3.3. The queueAttributes argument
// is unused in slice 3 but accepted so the signature
// matches the future substrate-integrated extension that
// reads CloudWatch GetMetricStatistics for the queue's
// DLQ ApproximateNumberOfMessages metric.
func detectSQSPoisonRate(_ queueAttributes) sqsPoisonRateDetectionResult {
	return sqsPoisonRateDetectionResult{
		RatePerHour: -1,
		HighBand:    false,
	}
}

// applySQSPoisonRateDetail writes the two slice 3
// honest-framing poison-rate axis Detail keys
// (poison_rate_per_hour, poison_rate_high_band) onto an
// already-initialized snapshot.
//
// Cold-start parity invariant: this function ADDS keys
// but never touches the slice-4 + slice-1-DLQ +
// slice-2-lag existing keys.
//
// Pattern mirrors applySQSDLQDetail + applySQSLagDetail in
// the same package + the analogous per-cloud helpers in
// chunks 2-4.
func applySQSPoisonRateDetail(snap *scanner.EventSourceInstanceSnapshot, qa queueAttributes) {
	res := detectSQSPoisonRate(qa)
	snap.Detail["poison_rate_per_hour"] = res.RatePerHour
	snap.Detail["poison_rate_high_band"] = res.HighBand
}
