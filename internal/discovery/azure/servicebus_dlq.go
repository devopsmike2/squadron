// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// DLQ configuration analysis slice 1 chunk 3 (v0.89.165, #807
// Stream 204) — Azure Service Bus DLQ detection (namespace-level
// honest framing per design doc §3.2).
//
// AZURE SERVICE BUS SCANNER-COVERAGE-GAP (design doc §3.2):
// The slice 1 Azure Service Bus scanner walks
// Microsoft.ServiceBus/namespaces — NOT individual queues. The
// queue-level DLQ fields
// (forwardDeadLetteredMessagesTo, enableDeadLetteringOnMessageExpiration,
// maxDeliveryCount) sit at the
// Microsoft.ServiceBus/namespaces/queues ARM sub-resource which the
// scanner has not yet walked. The pre-revision design doc
// incorrectly claimed these fields were already read; the v0.89.165
// revision establishes the HONEST SCOPE.
//
// Slice 1 chunk 3 ships Azure Service Bus DLQ axes at the NAMESPACE
// level with honest framing:
//   - has_dlq_queue_walk_available is ALWAYS false in slice 1.
//   - dlq_retry_count is ALWAYS -1 (absent sentinel).
//   - dlq_retry_count_in_band is ALWAYS false.
//   - The recommendation
//     servicebus-dlq-queue-walk-prerequisite fires per-namespace
//     with reasoning text explicitly calling out the scanner
//     coverage gap and framing the prerequisite for the future
//     slice that adds the
//     Microsoft.ServiceBus/namespaces/queues walk.
//
// This is the SECOND HONEST FRAMING precedent. §3.1 established the
// pattern for managed-primitive-absence (Cloud Tasks has no DLQ
// primitive Squadron can detect). §3.2 establishes the pattern for
// scanner-coverage-gap (Squadron's current scan view doesn't reach
// the per-resource layer where the field lives). Both patterns are
// load-bearing for slice 12+ substrate-dependent depth work.
//
// Cold-start parity invariant: the chunk 3 patch is ADDITIVE only.
// The slice-1 + slice-2 namespace-level Detail keys (source_type,
// has_trace, has_log, sku, propagation notes via PropagationNotes)
// survive byte-identically. A caller that has not yet adopted the
// new DLQ axis keys sees byte-identical output to v0.89.164.

// serviceBusDLQDetectionResult is the bare result of
// detectServiceBusDLQ. Three fields directly mirror the three
// namespace-level honest-framing Detail keys.
type serviceBusDLQDetectionResult struct {
	// HasDLQQueueWalkAvailable is ALWAYS false in slice 1 per
	// §3.2 — the namespace-level scanner does not reach the
	// per-queue config. The recommendation
	// servicebus-dlq-queue-walk-prerequisite fires unconditionally
	// for every namespace. Future slice 2+ chunk that adds the
	// queue walk flips this to true and decomposes the axis into
	// per-queue has_dlq + dlq_retry_count fields.
	HasDLQQueueWalkAvailable bool

	// RetryCount is ALWAYS -1 (absent sentinel) at the namespace
	// level. The per-queue maxDeliveryCount value is only readable
	// after the queue walk lands in a future slice.
	RetryCount int

	// RetryCountInBand is ALWAYS false at the namespace level. The
	// band check is meaningless without per-queue
	// maxDeliveryCount.
	RetryCountInBand bool
}

// detectServiceBusDLQ inspects an armServiceBusNamespace and returns
// the three namespace-level honest-framing DLQ axis signals.
//
// In slice 1, all three fields are constant regardless of
// namespace shape because the per-queue config the design doc §3
// detection rules reference is at an unwalked ARM sub-resource. The
// honest framing is the load-bearing pattern: the
// servicebus-dlq-queue-walk-prerequisite recommendation explicitly
// calls out the scanner coverage gap in its reasoning text.
//
// The namespace argument is unused in slice 1 but accepted so the
// signature matches the future slice 2+ extension that reads
// per-queue config from a queue walk attached to the namespace
// projection.
func detectServiceBusDLQ(_ armServiceBusNamespace) serviceBusDLQDetectionResult {
	return serviceBusDLQDetectionResult{
		HasDLQQueueWalkAvailable: false,
		RetryCount:               -1,
		RetryCountInBand:         false,
	}
}

// applyServiceBusDLQDetail writes the three slice 1 honest-framing
// DLQ axis Detail keys onto an already-initialized snapshot. The
// caller (projectServiceBusNamespace) is responsible for
// initializing snap.Detail; this helper writes directly without
// re-checking nil so it stays a thin layer atop the existing
// projection.
//
// Cold-start parity invariant: this function ADDS keys but never
// touches the slice-1 + slice-2 existing namespace-level keys
// (source_type, has_trace, has_log, sku). A caller that has not
// yet adopted the new DLQ axis keys sees byte-identical output to
// v0.89.164.
//
// Pattern mirrors applySQSDLQDetail in the aws package +
// applyCloudTasksDLQDetail in the gcp package. All three helpers
// share the slice 1 DLQ axis shape (three Detail keys per
// namespace / queue); the semantic differences live in the
// per-cloud detect* helpers.
func applyServiceBusDLQDetail(snap *scanner.EventSourceInstanceSnapshot, ns armServiceBusNamespace) {
	res := detectServiceBusDLQ(ns)
	snap.Detail["has_dlq_queue_walk_available"] = res.HasDLQQueueWalkAvailable
	snap.Detail["dlq_retry_count"] = res.RetryCount
	snap.Detail["dlq_retry_count_in_band"] = res.RetryCountInBand
}
