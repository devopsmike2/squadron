// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"time"

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

// --- Poison-rate substrate slice 4 chunk 1 (v0.89.177, #819 Stream
// 216) — REAL CloudWatch-backed detection that closes the AWS §3.3
// deferral. The honest-framing detectSQSPoisonRate above stays as
// the projection-time default (cold-start parity for the
// cwClient==nil path); the enrichment below overwrites the two
// Detail keys with real readings when CloudWatch is wired.

// SQSPoisonRateWindowHours is the trailing observation window (in
// hours) over which the DLQ's NumberOfMessagesSent SUM is read to
// compute the poison-message rate. 1 hour matches the slice 3 design
// framing ("rolling 1-hour window") and makes poison_rate_per_hour a
// direct rate (sum over one hour = messages per hour). Pinned by
// sqs_poison_rate_test.go::TestSQSPoisonRateWindowHours_Constant.
const SQSPoisonRateWindowHours = 1

// DetectSQSPoisonRate reads the dead-letter queue's
// NumberOfMessagesSent SUM over the trailing SQSPoisonRateWindowHours
// window via the AWS MetricQuerier substrate and converts it into the
// poison-rate axis result. This is the REAL detection that replaces
// the slice 3 §3.3 honest-framing absent sentinel for AWS SQS.
//
// Real-zero vs absent (design doc §3.1): the MetricQuerier empty-
// result contract surfaces "no datapoints" as SampleCount==0. A
// SampleCount==0 reading means CloudWatch has no data for this DLQ
// (queue too new, metric not yet emitted) — we cannot assert a rate,
// so we return the honest-framing absent sentinel (-1 / false). A
// SampleCount>0 reading with a SUM of 0 is a REAL "zero poison
// messages this hour" verdict (0 / false). This keeps -1 meaning
// "not measured", never "measured as zero".
//
// The dlqARN argument is the dead-letter queue ARN (the source
// queue's redrive_policy_target_arn). Callers are responsible for
// confirming the DLQ is reachable (in the scanned account's ARN set)
// before calling — DetectSQSPoisonRate does not re-validate
// reachability.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3.
func (s *Scanner) DetectSQSPoisonRate(ctx context.Context, dlqARN string) (sqsPoisonRateDetectionResult, error) {
	res, err := s.QueryAggregate(
		ctx, dlqARN, SQSNumberOfMessagesSentMetricName,
		time.Duration(SQSPoisonRateWindowHours)*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		return sqsPoisonRateDetectionResult{}, fmt.Errorf("poison-rate metric query: %w", err)
	}
	if res.SampleCount == 0 {
		// No datapoints — keep the honest-framing absent sentinel.
		return sqsPoisonRateDetectionResult{RatePerHour: -1, HighBand: false}, nil
	}
	rate := int(res.Value)
	return sqsPoisonRateDetectionResult{
		RatePerHour: rate,
		HighBand:    rate >= PoisonRatePerHourHighThreshold,
	}, nil
}

// enrichSQSPoisonRate is the post-projection enrichment pass that
// overwrites the honest-framing poison-rate Detail keys with real
// CloudWatch readings. Mirrors the cold-start enrichment posture
// (runColdStartDetectionForServerless): nil-tolerant on cwClient,
// per-row failures are swallowed so one bad DLQ query does not sink
// the rest of the region's enrichment.
//
//   - cwClient == nil → no-op. The projection's honest-framing
//     absent sentinels survive byte-identically (cold-start parity
//     for unwired deployments).
//   - A snapshot with no redrive_policy_target_arn (no DLQ) is left
//     untouched — there is no DLQ to measure, so -1 stands.
//   - A snapshot whose DLQ ARN is NOT in arnSet (cross-account /
//     dangling target) is left untouched — we did not enumerate that
//     queue, so we cannot read its metrics; -1 stands.
//   - A per-queue query error leaves that snapshot's keys untouched
//     (absent sentinel preserved) and continues.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3-§5.
func (s *Scanner) enrichSQSPoisonRate(ctx context.Context, snaps []scanner.EventSourceInstanceSnapshot, arnSet map[string]struct{}) {
	if s.cwClient == nil {
		return
	}
	for i := range snaps {
		detail := snaps[i].Detail
		if detail == nil {
			continue
		}
		targetRaw, ok := detail["redrive_policy_target_arn"]
		if !ok {
			continue
		}
		dlqARN, ok := targetRaw.(string)
		if !ok || dlqARN == "" {
			continue
		}
		if _, reachable := arnSet[dlqARN]; !reachable {
			// Cross-account / dangling DLQ — cannot read its metrics.
			continue
		}
		res, err := s.DetectSQSPoisonRate(ctx, dlqARN)
		if err != nil {
			// Transient CloudWatch error — keep the absent sentinel
			// for this row and move on.
			continue
		}
		detail["poison_rate_per_hour"] = res.RatePerHour
		detail["poison_rate_high_band"] = res.HighBand
	}
}
