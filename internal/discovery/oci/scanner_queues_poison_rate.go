// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-message rate analysis slice 3 chunk 4 (v0.89.176, #818
// Stream 215) — OCI Queue Service poison-rate axis with §3.3
// substrate-metric-dependence honest framing. FINAL chunk of
// slice 3; CLOSES the poison-message rate arc.
//
// SUBSTRATE-METRIC-DEPENDENCE (design doc §3.2 + §3.3):
// The per-queue poison-message rate over a rolling 1-hour window
// requires the dead-letter delivery-count delta over time, which
// on OCI is only readable via the OCI Monitoring
// SummarizeMetricsData API (oci_queue namespace). Slice 3 chunk 4
// does NOT yet integrate with the OCI Monitoring substrate
// (mirrors how the cold-start latency slice 1 + 2 arc built the
// MetricQuerier substrate per cloud).
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
