// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// OCI Queue poison-DEPTH signal (#159, v0.89.305) — the honest,
// always-available poison-present signal that closes the slice-3
// §3.3 deferral the rate axis could not (the oci_queue Monitoring
// namespace has NO dead-letter metric; see scanner_queues_poison_rate.go
// for the verified namespace inventory).
//
// The REAL OCI poison signal is DLQ DEPTH read from the Queue Service
// DATA-PLANE GetStats call:
//
//	GET {messagesEndpoint}/20210201/queues/{queueId}/stats
//
// The response carries two Stats objects — `stats` (the queue) and
// `dlqStats` (its managed dead-letter queue) — each with a
// `visibleMessages` count. dlqStats.visibleMessages is the honest
// "poison present" signal: a non-empty DLQ means messages exceeded
// the deadLetterQueueDeliveryCount delivery threshold and were
// dead-lettered. This mirrors the AWS SQS DLQ-depth approach
// (#156 / v0.89.259, applySQSPoisonDepthDetail) one-for-one.
//
// Semantics (identical to the AWS analog):
//   - poison_dlq_depth: the DLQ's current visibleMessages, or -1 when
//     the queue has no DLQ configured (deadLetterQueueDeliveryCount==0),
//     the data-plane messagesEndpoint is unknown, or the GetStats call
//     fails this scan (unreachable).
//   - poison_dlq_nonempty: true when the reachable DLQ currently holds
//     >=1 message — a direct "poison present" verdict. It is a depth
//     proxy, NOT a rate: a drained DLQ reads empty even if a burst
//     occurred earlier.
//
// Cold-start parity invariant: this is ADDITIVE only. The slice-9 +
// slice-1-DLQ + slice-2-lag + slice-3-poison-rate Detail keys survive
// byte-identically; callers that have not adopted the two depth keys
// see no change to any prior key.

// ociQueueStats is one Stats object from the Queue Service GetStats
// data-plane response. Only visibleMessages is consumed; other
// fields (inFlightMessages, sizeInBytes, ...) are intentionally
// ignored so an additive OCI payload change cannot break parsing.
type ociQueueStats struct {
	VisibleMessages int `json:"visibleMessages"`
}

// ociQueueStatsResponse is the GetStats data-plane envelope. dlqStats
// is a pointer so an absent block (queue without a DLQ) is
// distinguishable from a present-but-empty DLQ (visibleMessages==0).
type ociQueueStatsResponse struct {
	Stats    *ociQueueStats `json:"stats"`
	DlqStats *ociQueueStats `json:"dlqStats"`
}

// parseOCIQueueDLQDepth extracts the dead-letter queue's
// visibleMessages from a GetStats response body. Returns -1 (absent
// sentinel) when the dlqStats block is absent or the body does not
// parse — never a fabricated zero. A present dlqStats with
// visibleMessages==0 is a REAL "DLQ currently empty" reading (0).
func parseOCIQueueDLQDepth(body []byte) (int, error) {
	var resp ociQueueStatsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return -1, fmt.Errorf("parse queue stats: %w", err)
	}
	if resp.DlqStats == nil {
		return -1, nil
	}
	return resp.DlqStats.VisibleMessages, nil
}

// queueDLQDepth reads the queue's DLQ depth from the Queue Service
// data-plane GetStats call. Best-effort and nil-tolerant: any reason
// the depth cannot be read this scan (no DLQ configured, unknown
// messagesEndpoint, signing/network/HTTP error, unparseable body)
// returns the absent sentinel -1 rather than failing the projection.
//
// The DLQ-configured gate (deadLetterQueueDeliveryCount > 0) mirrors
// the AWS "no redrive policy -> absent -1" branch: a queue with no
// DLQ has nothing to measure.
func (s *Scanner) queueDLQDepth(ctx context.Context, sk *SigningKey, q ociQueue) int {
	if q.DeadLetterQueueDeliveryCount <= 0 {
		return -1
	}
	if q.MessagesEndpoint == "" || q.ID == "" {
		return -1
	}
	url := fmt.Sprintf("%s/20210201/queues/%s/stats", q.MessagesEndpoint, q.ID)
	body, err := s.doSignedGET(ctx, sk, url)
	if err != nil {
		return -1
	}
	depth, perr := parseOCIQueueDLQDepth(body)
	if perr != nil {
		return -1
	}
	return depth
}

// applyOCIQueuePoisonDepthDetail writes the two depth/presence poison
// Detail keys (#159). Mirrors applySQSPoisonDepthDetail (AWS #156).
// The caller (projectOCIQueue) is responsible for initializing
// snap.Detail; this helper adds keys without touching any prior key.
func applyOCIQueuePoisonDepthDetail(snap *scanner.EventSourceInstanceSnapshot, dlqDepth int) {
	snap.Detail["poison_dlq_depth"] = dlqDepth
	snap.Detail["poison_dlq_nonempty"] = dlqDepth > 0
}
