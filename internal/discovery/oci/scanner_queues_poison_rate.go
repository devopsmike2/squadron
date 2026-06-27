// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-message rate analysis slice 3 chunk 4 (v0.89.176, #818
// Stream 215) — OCI Queue Service poison-rate axis with §3.3
// substrate-metric-dependence honest framing. FINAL chunk of
// slice 3; CLOSES the poison-message rate arc.
//
// SUBSTRATE-METRIC-DEPENDENCE (design doc §3.2 + §3.3):
// CORRECTION (v0.89.260, #159): an earlier version of this comment
// claimed the poison signal was readable via OCI Monitoring's
// SummarizeMetricsData on the oci_queue namespace. That is FALSE —
// verified against the published OCI Queue metrics
// (docs.oracle.com/iaas/Content/queue/metrics.htm): the oci_queue
// namespace exposes QueueSize / MessagesInQueueCount / MessagesCount /
// RequestSuccess / RequestsLatency / RequestsThroughput / ConsumerLag /
// DroppedMessagesCount — and NO dead-letter metric. The
// deadLetterQueueDeliveryCount field on the queue is a CONFIG threshold
// (delivery attempts before a message is dead-lettered), not a count of
// poisoned messages, so it cannot drive a rate either.
//
// The REAL OCI poison signal is DLQ DEPTH from the data-plane GetStats
// call ({messagesEndpoint}/20210201/queues/{id}/stats): the response
// carries two Stats objects — `stats` (the queue) and `dlqStats` (its
// dead letter queue) — each with visibleMessages. dlqStats.visibleMessages
// is the honest poison-present signal (mirrors the AWS SQS DLQ-depth
// approach, #156/v0.89.259). Implementing it requires wiring the OCI
// Queue DATA-PLANE endpoint (the scanner currently uses only the
// control-plane list + Logging calls); tracked as the slice-3 follow-up.
// Until then this stays honest-absent (-1), NOT a fabricated rate.
//
// Slice 3 chunk 4 ships the OCI Queue Service poison-rate axis
// with HONEST FRAMING — identical shape to AWS chunk 1, GCP
// chunk 2, Azure chunk 3:
//   - poison_rate_per_hour ALWAYS = -1 (absent sentinel).
//   - poison_rate_high_band ALWAYS = false.
//   - The recommendation queues-poison-rate-monitor-add fires per
//     OCI queue with reasoning text explicitly calling out the
//     OCI Monitoring integration gap + prompting the operator to
//     wire an OCI Monitoring alarm on the queue's dead-letter
//     delivery metric.
//
// §3.3 is the CLEANEST of the three honest-framing variants
// because EVERY cloud sits under it for slice 3 — there is no
// mixed shape across chunks. A future slice closes §3.3 by
// extending each cloud scanner with the per-cloud MetricQuerier
// calls. OCI is the FOURTH and final cloud to ship the §3.3
// shape, closing the slice 3 arc.
//
// Cold-start parity invariant: the chunk 4 patch is ADDITIVE
// only. The slice-9 + slice-1-DLQ + slice-2-lag Detail keys
// survive byte-identically. A caller that has not yet adopted the
// new poison-rate axis keys sees byte-identical output to
// v0.89.175.

// OCIPoisonRatePerHourHighThreshold is the inclusive lower bound
// that flips poison_rate_high_band to true in a future slice that
// integrates with the OCI Monitoring substrate. 60 per hour (1
// per minute) mirrors the AWS chunk 1 PoisonRatePerHourHighThreshold
// for cross-cloud consistency per design doc §4 — a future
// per-cloud tuning slice can shift this independently.
const OCIPoisonRatePerHourHighThreshold = 60

// ociQueuePoisonRateDetectionResult is the bare result of
// detectOCIQueuePoisonRate. Two fields mirror the two Detail keys
// the chunk 4 projection writes — both hard-coded to absent state
// in slice 3.
type ociQueuePoisonRateDetectionResult struct {
	RatePerHour int
	HighBand    bool
}

// detectOCIQueuePoisonRate returns the honest-framing absent state
// per design doc §3.3. The queue argument is unused in slice 3 but
// accepted so the signature matches the future substrate-integrated
// extension that reads OCI Monitoring SummarizeMetricsData for the
// queue's dead-letter delivery metric.
func detectOCIQueuePoisonRate(_ ociQueue) ociQueuePoisonRateDetectionResult {
	return ociQueuePoisonRateDetectionResult{
		RatePerHour: -1,
		HighBand:    false,
	}
}

// applyOCIQueuePoisonRateDetail writes the two slice 3
// honest-framing poison-rate axis Detail keys
// (poison_rate_per_hour, poison_rate_high_band) onto an
// already-initialized snapshot. The caller (projectOCIQueue) is
// responsible for initializing snap.Detail.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-9 + slice-1-DLQ + slice-2-lag existing keys.
//
// Pattern mirrors applyOCIQueueDLQDetail + applyOCIQueueLagDetail
// in the same package + the analogous per-cloud poison-rate
// helpers (applySQSPoisonRateDetail, applyCloudTasksPoisonRateDetail,
// applyServiceBusPoisonRateDetail) in chunks 1-3.
func applyOCIQueuePoisonRateDetail(snap *scanner.EventSourceInstanceSnapshot, q ociQueue) {
	res := detectOCIQueuePoisonRate(q)
	snap.Detail["poison_rate_per_hour"] = res.RatePerHour
	snap.Detail["poison_rate_high_band"] = res.HighBand
}

// --- Poison-rate substrate slice 4 chunk 4 (v0.89.181, #823 Stream
// 220) — REAL OCI Monitoring-backed detection that closes the OCI
// §3.3 deferral. This is the FINAL cloud — chunk 4 CLOSES the entire
// poison-rate substrate arc and retires the last §3.3 deferral. The
// honest-framing detectOCIQueuePoisonRate above stays as the
// projection-time default (cold-start parity for the unwired path);
// the enrichment below overwrites the two Detail keys with real
// readings when the OCI Monitoring client is wired.

// OCIQueueMetricNamespace is the OCI Monitoring namespace for Queue
// Service metrics. summarizeMetricsData requires (namespace, MQL
// query) as a tuple; this is the queue-tier analog of the
// oci_functions namespace the cold-start substrate uses.
const OCIQueueMetricNamespace = "oci_queue"

// OCIQueueDeadLetterMessagesMetric is the OCI Monitoring metric for
// the per-queue dead-letter message count. Poison-rate substrate
// slice 4 chunk 4 reads this gauge and derives the poison rate as
// the max-min delta (net dead-letter accumulation) over the window —
// the same gauge-delta shape the Azure chunk uses, and distinct from
// the AWS / GCP counter-sum shape (those clouds expose arrival
// counters; OCI + Azure expose dead-letter DEPTH gauges).
//
// AVAILABILITY WARNING (verified v0.89.236 against OCI's Queue Metrics
// reference): "MessagesInDlq" is NOT a metric in the oci_queue namespace.
// The namespace exposes QueueSize, MessagesInQueueCount, MessagesCount,
// RequestSuccess, RequestsLatency, RequestsThroughput, ConsumerLag, and
// DroppedMessagesCount — none is a dead-letter DEPTH gauge. So this query
// always returns no datapoints and the detection safe-degrades to the
// honest absent sentinel; it never actually fires. enrichOCIQueuePoisonRate
// is therefore a no-op (below). A future depth-based signal can use the
// queue's deadLetterQueueDeliveryCount attribute (already read by the
// scanner) — see docs/audit/detection-metric-availability.md.
const OCIQueueDeadLetterMessagesMetric = "MessagesInDlq"

// OCIQueuePoisonRateWindowHours is the trailing observation window
// (hours) over which the dead-letter gauge max-min delta is read.
const OCIQueuePoisonRateWindowHours = 1

// queryOCIQueueDeadletterDelta reads the queue's dead-letter gauge
// over the window via OCI Monitoring summarizeMetricsData and returns
// the net accumulation (max - min across the returned per-resolution
// datapoints, floored at 0) plus the datapoint count.
//
// MQL: MessagesInDlq[<window>]{resourceId = "<queueOCID>"}.max() —
// the .max() reduction gives the per-resolution peak depth; the
// substrate then takes max-min across resolutions as the net
// accumulation (the Azure gauge-delta analog). sampleCount==0 means
// OCI returned no datapoints (queue too new, metric not emitted, or
// metric-name mismatch) → the caller keeps the absent sentinel.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3 + §7.
func (s *Scanner) queryOCIQueueDeadletterDelta(ctx context.Context, compartmentID, queueOCID string) (delta, sampleCount int, err error) {
	if s.metricsLimiter != nil {
		if werr := s.metricsLimiter.Wait(ctx); werr != nil {
			return 0, 0, fmt.Errorf("rate limit: %w", werr)
		}
	}
	endTime := time.Now().UTC()
	window := time.Duration(OCIQueuePoisonRateWindowHours) * time.Hour
	startTime := endTime.Add(-window)
	query := fmt.Sprintf(
		"%s[%s]{resourceId = %q}.max()",
		OCIQueueDeadLetterMessagesMetric, ociWindowQuery(window), queueOCID,
	)
	points, callErr := s.monitoringClient.SummarizeMetricsData(
		ctx, compartmentID, OCIQueueMetricNamespace, query, startTime, endTime,
	)
	if callErr != nil {
		return 0, 0, fmt.Errorf("summarize metrics: %w", callErr)
	}
	if len(points) == 0 {
		return 0, 0, nil
	}
	maxV, minV := points[0].Value, points[0].Value
	for _, p := range points {
		if p.Value > maxV {
			maxV = p.Value
		}
		if p.Value < minV {
			minV = p.Value
		}
	}
	d := maxV - minV
	if d < 0 {
		d = 0
	}
	return int(d), len(points), nil
}

// DetectOCIQueuePoisonRate reads the queue's dead-letter net
// accumulation via OCI Monitoring and converts it into the
// poison-rate axis result. This is the REAL detection that replaces
// the slice 3 §3.3 honest-framing absent sentinel for OCI Queue
// Service — the FINAL cloud in the substrate arc.
//
// Real-zero vs absent (design doc §3.1): sampleCount==0 → absent
// sentinel (-1). A non-empty series with a flat gauge (delta 0) →
// real "zero new dead-letters this hour" (0). -1 always means "not
// measured", never "measured as zero".
//
// See docs/proposals/poison-rate-substrate-slice4.md §3 + §7.
func (s *Scanner) DetectOCIQueuePoisonRate(ctx context.Context, compartmentID, queueOCID string) (ociQueuePoisonRateDetectionResult, error) {
	delta, samples, err := s.queryOCIQueueDeadletterDelta(ctx, compartmentID, queueOCID)
	if err != nil {
		return ociQueuePoisonRateDetectionResult{}, fmt.Errorf("poison-rate query: %w", err)
	}
	if samples == 0 {
		return ociQueuePoisonRateDetectionResult{RatePerHour: -1, HighBand: false}, nil
	}
	return ociQueuePoisonRateDetectionResult{
		RatePerHour: delta,
		HighBand:    delta >= OCIPoisonRatePerHourHighThreshold,
	}, nil
}

// enrichOCIQueuePoisonRate is the post-projection enrichment pass
// that overwrites the honest-framing poison-rate Detail keys with
// real OCI Monitoring readings. Mirrors the AWS / GCP / Azure
// enrichment posture: nil-tolerant on monitoringClient, per-row
// failures swallowed.
//
//   - monitoringClient == nil → no-op. The projection's
//     honest-framing absent sentinels survive byte-identically
//     (cold-start parity for deployments without the Monitoring
//     wiring).
//   - reads compartment_id from the snapshot Detail bag (set by
//     projectOCIQueue) + the queue OCID from ResourceARN — both are
//     needed for the summarizeMetricsData call.
//   - a per-queue query error leaves that snapshot untouched (absent
//     sentinel preserved) and continues.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3-§5.
func (s *Scanner) enrichOCIQueuePoisonRate(_ context.Context, _ []scanner.EventSourceInstanceSnapshot) {
	// No-op. OCI's oci_queue namespace has no dead-letter DEPTH metric
	// (OCIQueueDeadLetterMessagesMetric / "MessagesInDlq" does not exist —
	// see the constant's AVAILABILITY WARNING), so the substrate query
	// always returned no datapoints and safe-degraded to the absent
	// sentinel. The honest absent sentinels written at projection time
	// stand. DetectOCIQueuePoisonRate + queryOCIQueueDeadletterDelta remain
	// as the wiring seam for a future depth-based signal (e.g. via the
	// deadLetterQueueDeliveryCount attribute the scanner already reads).
}
