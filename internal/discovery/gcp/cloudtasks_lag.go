// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Consumer lag detection slice 2 chunk 2 (v0.89.169, #811
// Stream 208) — GCP Cloud Tasks lag axis with §3.1 honest framing.
//
// CLOUD TASKS LAG SPECIAL CASE (design doc §3.3):
// The Cloud Tasks admin API does NOT surface task count as a
// directly-queryable metric. The list endpoint paginates over
// tasks with a maximum page size of 1000; counting requires
// walking every page (which is too expensive for routine
// scans + violates the "no new API calls" constraint).
//
// Slice 2 chunk 2 ships the Cloud Tasks lag axis with HONEST
// FRAMING (same posture as the DLQ slice 1 chunk 2 §3.1 pattern):
//   - lag_backlog_depth is ALWAYS -1 (absent sentinel).
//   - lag_backlog_depth_high is ALWAYS false.
//   - lag_consumer_silence_seconds is ALWAYS -1 (absent sentinel).
//   - lag_consumer_silence_high is ALWAYS false.
//   - The recommendation cloudtasks-backlog-monitor-add fires
//     per Cloud Tasks queue with reasoning text explicitly
//     calling out that Squadron CANNOT verify the consumer is
//     keeping up; the operator wires Cloud Monitoring's
//     cloudtasks.googleapis.com/queue/task_count metric to an
//     alerting policy.
//
// This is the THIRD application of the §3.1 honest-framing
// pattern (managed-primitive-absence). DLQ slice 1 chunk 2
// established it (Cloud Tasks has no managed DLQ primitive).
// DLQ slice 1 chunk 3 established the §3.2 variant
// (scanner-coverage-gap, Azure Service Bus). Slice 2 chunk 2
// REUSES §3.1 for Cloud Tasks lag detection. The repeated
// reuse validates the pattern's load-bearing role for the
// per-axis depth horizon.
//
// Cold-start parity invariant: the chunk 2 patch is ADDITIVE
// only. The slice-5 (Event source) + slice-1 (DLQ) Cloud Tasks
// Detail keys survive byte-identically. A caller that has not
// yet adopted the new lag axis keys sees byte-identical output
// to v0.89.168.

// cloudTasksLagDetectionResult is the bare result of
// detectCloudTasksLag. Four fields mirror the four Detail keys
// the chunk 2 projection writes — all hard-coded to the
// honest-framing absent state in slice 2.
type cloudTasksLagDetectionResult struct {
	BacklogDepth           int
	BacklogDepthHigh       bool
	ConsumerSilenceSeconds int
	ConsumerSilenceHigh    bool
}

// detectCloudTasksLag inspects a cloudTasksQueue and returns the
// four namespace-level honest-framing lag axis signals.
//
// In slice 2, all four fields are constant regardless of queue
// shape because the per-queue task count + consumer activity
// signals the design doc §3 detection rules reference are not
// surfaced by the Cloud Tasks admin API. The honest framing is
// the load-bearing pattern: the cloudtasks-backlog-monitor-add
// recommendation explicitly calls out the gap in its reasoning
// text + prompts the operator to wire Cloud Monitoring's
// queue/task_count metric to an alerting policy.
//
// The queue argument is unused in slice 2 but accepted so the
// signature matches the future slice 3+ extension that reads
// per-queue metrics from a substrate MetricQuerier call.
func detectCloudTasksLag(_ *cloudTasksQueue) cloudTasksLagDetectionResult {
	return cloudTasksLagDetectionResult{
		BacklogDepth:           -1,
		BacklogDepthHigh:       false,
		ConsumerSilenceSeconds: -1,
		ConsumerSilenceHigh:    false,
	}
}

// applyCloudTasksLagDetail writes the four slice 2 honest-framing
// lag axis Detail keys onto an already-initialized snapshot. The
// caller (buildCloudTasksSnapshot) is responsible for initializing
// snap.Detail or accepting a nil map and letting this helper
// initialize it.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-5 + slice-1 (DLQ) existing keys
// (max_attempts, max_retry_duration, stackdriver_sampling_ratio,
// max_dispatches_per_second, max_concurrent_dispatches,
// max_burst_size, state, purge_time, has_dlq_pattern_likely,
// dlq_retry_count, dlq_retry_count_in_band).
//
// Pattern mirrors applyCloudTasksDLQDetail in the same package
// + applySQSLagDetail in the aws package. All chunk authors
// share the slice 2 lag axis shape (four Detail keys); the
// semantic differences (real detection vs. §3.1 honest framing)
// live in the per-cloud detect* helpers.
func applyCloudTasksLagDetail(snap *scanner.EventSourceInstanceSnapshot, q *cloudTasksQueue) {
	res := detectCloudTasksLag(q)
	if snap.Detail == nil {
		snap.Detail = map[string]any{}
	}
	snap.Detail["lag_backlog_depth"] = res.BacklogDepth
	snap.Detail["lag_backlog_depth_high"] = res.BacklogDepthHigh
	snap.Detail["lag_consumer_silence_seconds"] = res.ConsumerSilenceSeconds
	snap.Detail["lag_consumer_silence_high"] = res.ConsumerSilenceHigh
}

// --- Consumer-lag substrate slice 5 chunk 1 (v0.89.182, #824 Stream
// 221) — REAL Cloud Monitoring-backed BACKLOG detection that closes
// the GCP §3.1 consumer-lag deferral. The honest-framing
// detectCloudTasksLag above stays as the projection-time default
// (cold-start parity for the unwired path); the enrichment below
// overwrites the two BACKLOG keys with real readings when Cloud
// Monitoring is wired. The two SILENCE keys stay honest-framed —
// Cloud Tasks exposes no clean per-queue oldest-task-age metric, so
// silence remains a documented deferral (design doc §2).

// CloudTasksBacklogDepthHighThreshold is the inclusive lower bound
// (tasks in queue) that flips lag_backlog_depth_high to true. 1000
// mirrors the AWS BacklogDepthHighThreshold + OCI
// OCIBacklogDepthHighThreshold for cross-cloud consistency.
const CloudTasksBacklogDepthHighThreshold = 1000

// CloudTasksBacklogWindowHours is the trailing window over which the
// peak queue depth is read (ALIGN_MAX). 1 hour gives the recent peak
// backlog — a queue that spiked in the last hour is backing up.
const CloudTasksBacklogWindowHours = 1

// DetectCloudTasksBacklog reads the queue's peak depth
// (cloudtasks.googleapis.com/queue/depth, gauge) over the trailing
// CloudTasksBacklogWindowHours window via the GCP MetricQuerier
// substrate. This is the REAL detection that replaces the slice 2
// §3.1 honest-framing absent sentinel for the Cloud Tasks BACKLOG
// axis.
//
// Returns (depth, sampleCount). sampleCount==0 means Cloud Monitoring
// returned no datapoints (queue too new / metric-name mismatch) — the
// caller keeps the honest-framing absent sentinel (-1). A non-empty
// series with depth 0 is a real "empty queue" reading.
//
// queueResourceName is the Cloud Tasks queue resource name
// ("projects/{p}/locations/{l}/queues/{q}", the snapshot ResourceARN).
//
// See docs/proposals/consumer-lag-substrate-slice5.md §3.
func (s *Scanner) DetectCloudTasksBacklog(ctx context.Context, queueResourceName string) (depth, sampleCount int, err error) {
	res, qerr := s.QueryAggregate(
		ctx, queueResourceName, CloudTasksQueueDepthMetricType,
		time.Duration(CloudTasksBacklogWindowHours)*time.Hour,
		scanner.StatisticP95, // routing keys on metric name; gauge path ignores stat
	)
	if qerr != nil {
		return 0, 0, fmt.Errorf("backlog metric query: %w", qerr)
	}
	if res.SampleCount == 0 {
		return -1, 0, nil
	}
	return int(res.Value), res.SampleCount, nil
}

// enrichCloudTasksLag is the post-projection enrichment pass that
// overwrites the honest-framing BACKLOG lag keys with real Cloud
// Monitoring readings. Mirrors the poison-rate enrichment posture:
// nil-tolerant on metricsClient, per-row failures swallowed.
//
//   - metricsClient == nil → no-op (cold-start parity).
//   - overwrites lag_backlog_depth + lag_backlog_depth_high only.
//     The two SILENCE keys (lag_consumer_silence_seconds /
//     lag_consumer_silence_high) are left at their honest-framing -1
//     — Cloud Tasks has no per-queue oldest-task-age metric (design
//     doc §2 deferral).
//   - per-queue query error leaves the snapshot untouched, continues.
//
// See docs/proposals/consumer-lag-substrate-slice5.md §3-§5.
func (s *Scanner) enrichCloudTasksLag(ctx context.Context, snaps []scanner.EventSourceInstanceSnapshot) {
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
		depth, samples, err := s.DetectCloudTasksBacklog(ctx, arn)
		if err != nil {
			continue
		}
		if samples == 0 {
			// No datapoints — keep the honest-framing absent sentinel.
			continue
		}
		detail["lag_backlog_depth"] = depth
		detail["lag_backlog_depth_high"] = depth >= CloudTasksBacklogDepthHighThreshold
	}
}
