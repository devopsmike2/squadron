// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Consumer lag detection slice 2 chunk 3 (v0.89.170, #812
// Stream 209) — Azure Service Bus lag axis with §3.2 inherited
// honest framing.
//
// AZURE SERVICE BUS LAG SCANNER-COVERAGE-GAP (design doc §3.4):
// INHERITS the §3.2 scanner-coverage-gap pattern from DLQ slice 1
// chunk 3 (v0.89.165). The slice 1 + slice 2 Azure Service Bus
// scanner walks Microsoft.ServiceBus/namespaces — NOT individual
// queues. The per-queue activeMessageCount + per-queue consumer
// silence proxies sit at the Microsoft.ServiceBus/namespaces/queues
// ARM sub-resource which the scanner has not yet walked.
//
// Slice 2 chunk 3 ships the Azure Service Bus lag axis at the
// NAMESPACE level with honest framing:
//   - lag_backlog_depth ALWAYS = -1 (absent sentinel).
//   - lag_backlog_depth_high ALWAYS = false.
//   - lag_consumer_silence_seconds ALWAYS = -1.
//   - lag_consumer_silence_high ALWAYS = false.
//   - The recommendation servicebus-backlog-queue-walk-prerequisite
//     fires per-namespace with reasoning text explicitly calling
//     out the scanner coverage gap (same posture as the DLQ slice 1
//     chunk 3 servicebus-dlq-queue-walk-prerequisite kind).
//
// STRATEGIC NOTE: a future Azure Service Bus per-queue walk slice
// closes BOTH the DLQ slice 1 chunk 3 deferrals AND the slice 2
// chunk 3 deferrals in a single API extension. One scanner change
// unblocks two per-axis detection rules.
//
// FOURTH APPLICATION of the honest-framing patterns (§3.1 + §3.2
// together). DLQ slice 1 chunks 2 + 3 established the patterns.
// Slice 2 chunks 2 + 3 reuse them. The repeated reuse validates
// the pattern's load-bearing role and demonstrates the per-axis-
// depth horizon's predictable shape: AWS + OCI consistently ship
// real detection; GCP + Azure consistently ship honest framing
// for the queue tier.
//
// Cold-start parity invariant: the chunk 3 patch is ADDITIVE only.
// The slice-1 + slice-2 + slice-1-DLQ namespace-level Detail keys
// (source_type, has_trace, has_log, sku,
// has_dlq_queue_walk_available, dlq_retry_count,
// dlq_retry_count_in_band) survive byte-identically. A caller that
// has not yet adopted the new lag axis keys sees byte-identical
// output to v0.89.169.

// serviceBusLagDetectionResult is the bare result of
// detectServiceBusLag. Four fields directly mirror the four
// namespace-level honest-framing Detail keys.
type serviceBusLagDetectionResult struct {
	BacklogDepth           int
	BacklogDepthHigh       bool
	ConsumerSilenceSeconds int
	ConsumerSilenceHigh    bool
}

// detectServiceBusLag inspects an armServiceBusNamespace and
// returns the four namespace-level honest-framing lag axis
// signals.
//
// In slice 2, all four fields are constant regardless of namespace
// shape because the per-queue lag fields the design doc §3
// detection rules reference are at an unwalked ARM sub-resource.
// The honest framing is the load-bearing pattern: the
// servicebus-backlog-queue-walk-prerequisite recommendation
// explicitly calls out the scanner coverage gap in its reasoning
// text.
//
// The namespace argument is unused in slice 2 but accepted so the
// signature matches the future slice that adds the queue walk +
// reads per-queue activeMessageCount.
func detectServiceBusLag(_ armServiceBusNamespace) serviceBusLagDetectionResult {
	return serviceBusLagDetectionResult{
		BacklogDepth:           -1,
		BacklogDepthHigh:       false,
		ConsumerSilenceSeconds: -1,
		ConsumerSilenceHigh:    false,
	}
}

// applyServiceBusLagDetail writes the four slice 2 honest-framing
// lag axis Detail keys onto an already-initialized snapshot. The
// caller (projectServiceBusNamespace) is responsible for
// initializing snap.Detail; this helper writes directly without
// re-checking nil so it stays a thin layer atop the existing
// projection.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-1 + slice-2 + slice-1-DLQ existing
// namespace-level keys (source_type, has_trace, has_log, sku,
// has_dlq_queue_walk_available, dlq_retry_count,
// dlq_retry_count_in_band).
//
// Pattern mirrors applyServiceBusDLQDetail in the same package +
// applyCloudTasksLagDetail in the gcp package + applySQSLagDetail
// in the aws package. All chunk authors share the slice 2 lag
// axis shape (four Detail keys per namespace / queue); the
// semantic differences (real detection vs. §3.1 vs. §3.2 honest
// framing) live in the per-cloud detect* helpers.
func applyServiceBusLagDetail(snap *scanner.EventSourceInstanceSnapshot, ns armServiceBusNamespace) {
	res := detectServiceBusLag(ns)
	snap.Detail["lag_backlog_depth"] = res.BacklogDepth
	snap.Detail["lag_backlog_depth_high"] = res.BacklogDepthHigh
	snap.Detail["lag_consumer_silence_seconds"] = res.ConsumerSilenceSeconds
	snap.Detail["lag_consumer_silence_high"] = res.ConsumerSilenceHigh
}
