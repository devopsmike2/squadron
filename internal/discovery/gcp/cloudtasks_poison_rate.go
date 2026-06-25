// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-message rate analysis slice 3 chunk 2 (v0.89.174,
// #816 Stream 213) — GCP Cloud Tasks poison-rate axis with
// §3.3 substrate-metric-dependence honest framing.
//
// Mirrors AWS chunk 1 shape. The per-queue poison rate
// requires the cloudtasks.googleapis.com/queue/task_attempt_count
// metric via Cloud Monitoring — deferred to a future slice
// that builds the GCP MetricQuerier integration (mirrors how
// the cold-start latency slice 2 chunk 1 built the GCP
// MetricQuerier).
//
// Cold-start parity invariant: ADDITIVE only. The slice-5 +
// slice-1-DLQ + slice-2-lag Cloud Tasks Detail keys survive
// byte-identically.

// cloudTasksPoisonRateDetectionResult is the bare result of
// detectCloudTasksPoisonRate. Two fields mirror two Detail
// keys, both hard-coded to absent state.
type cloudTasksPoisonRateDetectionResult struct {
	RatePerHour int
	HighBand    bool
}

// detectCloudTasksPoisonRate returns the honest-framing
// absent state per design doc §3.3. The queue argument is
// unused in slice 3 but accepted so the signature matches
// the future substrate-integrated extension.
func detectCloudTasksPoisonRate(_ *cloudTasksQueue) cloudTasksPoisonRateDetectionResult {
	return cloudTasksPoisonRateDetectionResult{
		RatePerHour: -1,
		HighBand:    false,
	}
}

// applyCloudTasksPoisonRateDetail writes the two slice 3
// honest-framing poison-rate axis Detail keys onto an
// already-initialized snapshot.
//
// Cold-start parity invariant: ADDS keys but never touches
// the slice-5 + slice-1-DLQ + slice-2-lag existing keys.
func applyCloudTasksPoisonRateDetail(snap *scanner.EventSourceInstanceSnapshot, q *cloudTasksQueue) {
	res := detectCloudTasksPoisonRate(q)
	if snap.Detail == nil {
		snap.Detail = map[string]any{}
	}
	snap.Detail["poison_rate_per_hour"] = res.RatePerHour
	snap.Detail["poison_rate_high_band"] = res.HighBand
}

// --- Poison-rate substrate slice 4 chunk 2 (v0.89.178, #820 Stream
// 217) — REAL Cloud Monitoring-backed detection that closes the GCP
// §3.3 deferral. The honest-framing detectCloudTasksPoisonRate above
// stays as the projection-time default (cold-start parity for the
// metricsClient==nil path); the enrichment below overwrites the two
// Detail keys with real readings when Cloud Monitoring is wired.

// CloudTasksPoisonRatePerHourHighThreshold is the inclusive lower
// bound (failed task attempts per hour) that flips
// poison_rate_high_band to true. 60/hour (1/min) mirrors the AWS
// PoisonRatePerHourHighThreshold + the slice-3 cross-cloud band for
// consistency; a future per-cloud tuning slice can shift it
// independently. Pinned by
// cloudtasks_poison_rate_substrate_test.go::TestCloudTasksPoisonRatePerHourHighThreshold_Constant.
const CloudTasksPoisonRatePerHourHighThreshold = 60

// CloudTasksPoisonRateWindowHours is the trailing observation window
// (in hours) over which the queue's failed task_attempt_count SUM is
// read. 1 hour matches the slice 3 framing and makes
// poison_rate_per_hour a direct per-hour rate.
const CloudTasksPoisonRateWindowHours = 1

// DetectCloudTasksPoisonRate reads the queue's failed
// task_attempt_count SUM over the trailing
// CloudTasksPoisonRateWindowHours window via the GCP MetricQuerier
// substrate and converts it into the poison-rate axis result. This
// is the REAL detection that replaces the slice 3 §3.3 honest-framing
// absent sentinel for GCP Cloud Tasks.
//
// Real-zero vs absent (design doc §3.1): SampleCount==0 means Cloud
// Monitoring has no data for this queue (queue too new / no failed
// attempts series yet) — we cannot assert a rate, so we return the
// absent sentinel (-1 / false). A SampleCount>0 reading with a SUM
// of 0 is a REAL "zero failed attempts this hour" verdict (0 /
// false). -1 always means "not measured", never "measured as zero".
//
// queueResourceName is the Cloud Tasks queue resource name
// ("projects/{p}/locations/{l}/queues/{q}", the snapshot's
// ResourceARN). The kind/name parsing + the queue_id filter live in
// QueryAggregate.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3 + §7.
func (s *Scanner) DetectCloudTasksPoisonRate(ctx context.Context, queueResourceName string) (cloudTasksPoisonRateDetectionResult, error) {
	res, err := s.QueryAggregate(
		ctx, queueResourceName, CloudTasksTaskAttemptCountMetricType,
		time.Duration(CloudTasksPoisonRateWindowHours)*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		return cloudTasksPoisonRateDetectionResult{}, fmt.Errorf("poison-rate metric query: %w", err)
	}
	if res.SampleCount == 0 {
		return cloudTasksPoisonRateDetectionResult{RatePerHour: -1, HighBand: false}, nil
	}
	rate := int(res.Value)
	return cloudTasksPoisonRateDetectionResult{
		RatePerHour: rate,
		HighBand:    rate >= CloudTasksPoisonRatePerHourHighThreshold,
	}, nil
}

// enrichCloudTasksPoisonRate is the post-projection enrichment pass
// that overwrites the honest-framing poison-rate Detail keys with
// real Cloud Monitoring readings. Mirrors the AWS enrichSQSPoisonRate
// posture (and the cold-start enrichment): nil-tolerant on
// metricsClient, per-row failures swallowed so one bad queue query
// does not sink the rest of the region's enrichment.
//
//   - metricsClient == nil → no-op. The projection's honest-framing
//     absent sentinels survive byte-identically (cold-start parity
//     for deployments without the Cloud Monitoring wiring).
//   - Unlike AWS SQS (poison rate measured on a separate DLQ with a
//     reachability check), the Cloud Tasks poison rate is measured on
//     the queue ITSELF (failed delivery attempts), so every queue
//     snapshot is queried directly — no DLQ/reachability gate.
//   - A per-queue query error leaves that snapshot's keys untouched
//     (absent sentinel preserved) and continues.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3-§5.
func (s *Scanner) enrichCloudTasksPoisonRate(ctx context.Context, snaps []scanner.EventSourceInstanceSnapshot) {
	if s.metricsClient == nil {
		return
	}
	for i := range snaps {
		detail := snaps[i].Detail
		if detail == nil {
			continue
		}
		arn := snaps[i].ResourceARN
		if arn == "" {
			continue
		}
		res, err := s.DetectCloudTasksPoisonRate(ctx, arn)
		if err != nil {
			continue
		}
		detail["poison_rate_per_hour"] = res.RatePerHour
		detail["poison_rate_high_band"] = res.HighBand
	}
}
