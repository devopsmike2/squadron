// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
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
