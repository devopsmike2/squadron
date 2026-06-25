// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-message rate analysis slice 3 chunk 3 (v0.89.175,
// #817 Stream 214) — Azure Service Bus poison-rate axis
// with §3.3 substrate-metric-dependence honest framing
// (also inherits §3.2 scanner-coverage-gap from DLQ slice 1
// chunk 3 + lag slice 2 chunk 3 — the namespace-level
// scanner doesn't reach the per-queue layer where the
// deadLetteredMessages metric ARM dimension lives).
//
// The per-queue poison rate requires the Azure Monitor
// metric Microsoft.ServiceBus/namespaces/queues
// DeadletteredMessages — deferred to a future slice that
// builds the Azure MetricQuerier integration (mirrors how
// the cold-start latency slice 2 chunk 2 built the Azure
// MetricQuerier).
//
// Cold-start parity invariant: ADDITIVE only. Slice-1 +
// slice-2 + slice-1-DLQ + slice-2-lag namespace-level keys
// survive byte-identically.

// serviceBusPoisonRateDetectionResult is the bare result
// of detectServiceBusPoisonRate. Two fields hard-coded to
// absent state.
type serviceBusPoisonRateDetectionResult struct {
	RatePerHour int
	HighBand    bool
}

// detectServiceBusPoisonRate returns the honest-framing
// absent state per design doc §3.3 + the inherited §3.2
// scanner-coverage-gap. The namespace argument is unused
// in slice 3 but accepted so the signature matches the
// future per-queue-walk + substrate-integrated extension.
func detectServiceBusPoisonRate(_ armServiceBusNamespace) serviceBusPoisonRateDetectionResult {
	return serviceBusPoisonRateDetectionResult{
		RatePerHour: -1,
		HighBand:    false,
	}
}

// applyServiceBusPoisonRateDetail writes the two slice 3
// honest-framing poison-rate axis Detail keys onto an
// already-initialized snapshot.
//
// Cold-start parity invariant: ADDS keys but never touches
// the slice-1 + slice-2 + slice-1-DLQ + slice-2-lag
// existing keys.
func applyServiceBusPoisonRateDetail(snap *scanner.EventSourceInstanceSnapshot, ns armServiceBusNamespace) {
	res := detectServiceBusPoisonRate(ns)
	snap.Detail["poison_rate_per_hour"] = res.RatePerHour
	snap.Detail["poison_rate_high_band"] = res.HighBand
}

// --- Poison-rate substrate slice 4 chunk 3a (v0.89.179, #821 Stream
// 218) — REAL Azure Monitor-backed detection that closes the Azure
// §3.3 deferral at NAMESPACE granularity. The honest-framing
// detectServiceBusPoisonRate above stays as the projection-time
// default (cold-start parity for the unwired path); the enrichment
// below overwrites the two Detail keys with real readings when an
// access token is available.
//
// SCOPE: chunk 3a closes §3.3 (substrate-metric-dependence) — Azure
// now reads a REAL metric. It does NOT yet close §3.2
// (scanner-coverage-gap): the reading is namespace-aggregated across
// all queues/topics. Per-queue attribution (the EntityName-dimension
// per-queue walk) is chunk 3b.

// ServiceBusPoisonRatePerHourHighThreshold is the inclusive lower
// bound (net dead-letter accumulation per hour) that flips
// poison_rate_high_band to true. 60/hour mirrors the AWS + GCP +
// slice-3 cross-cloud band. Pinned by
// servicebus_poison_rate_substrate_test.go::TestServiceBusPoisonRatePerHourHighThreshold_Constant.
const ServiceBusPoisonRatePerHourHighThreshold = 60

// ServiceBusPoisonRateWindowHours is the trailing observation window
// (hours) over which the DeadletteredMessages max-min delta is read.
const ServiceBusPoisonRateWindowHours = 1

// DetectServiceBusPoisonRate reads the namespace's DeadletteredMessages
// max-min delta over the trailing ServiceBusPoisonRateWindowHours
// window via the Azure MetricQuerier substrate and converts it into
// the poison-rate axis result. This is the REAL detection that
// replaces the slice 3 §3.3 honest-framing absent sentinel for Azure
// Service Bus (at namespace granularity).
//
// Real-zero vs absent (design doc §3.1): SampleCount==0 means Azure
// Monitor returned no DeadletteredMessages series for this namespace
// (no entities / metric not emitted yet) — absent sentinel (-1). A
// SampleCount>0 reading with a flat gauge (delta 0) is a REAL "zero
// new dead-letters this hour" verdict (0). -1 always means "not
// measured", never "measured as zero".
//
// namespaceResourceID is the ARM resource id
// (/subscriptions/.../providers/Microsoft.ServiceBus/namespaces/{ns},
// the snapshot's ResourceARN).
//
// See docs/proposals/poison-rate-substrate-slice4.md §3 + §7.
func (s *Scanner) DetectServiceBusPoisonRate(ctx context.Context, namespaceResourceID string) (serviceBusPoisonRateDetectionResult, error) {
	res, err := s.QueryAggregate(
		ctx, namespaceResourceID, ServiceBusDeadletteredMessagesMetric,
		time.Duration(ServiceBusPoisonRateWindowHours)*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		return serviceBusPoisonRateDetectionResult{}, fmt.Errorf("poison-rate metric query: %w", err)
	}
	if res.SampleCount == 0 {
		return serviceBusPoisonRateDetectionResult{RatePerHour: -1, HighBand: false}, nil
	}
	rate := int(res.Value)
	return serviceBusPoisonRateDetectionResult{
		RatePerHour: rate,
		HighBand:    rate >= ServiceBusPoisonRatePerHourHighThreshold,
	}, nil
}

// enrichServiceBusPoisonRate is the post-projection enrichment pass
// that overwrites the honest-framing poison-rate Detail keys with
// real Azure Monitor readings. Mirrors the AWS enrichSQSPoisonRate /
// GCP enrichCloudTasksPoisonRate posture: token-tolerant, per-row
// failures swallowed.
//
//   - accessToken == "" → no-op. The projection's honest-framing
//     absent sentinels survive byte-identically (cold-start parity
//     for deployments / paths without a token).
//   - The Azure metric substrate authenticates via s.accessToken
//     (the struct field doAzureMonitorMetricsCall reads). The
//     dispatcher event-source scan acquires its token locally rather
//     than storing it, so this wires the in-scope token onto the
//     Scanner when the field is empty — idempotent (same subscription
//     token) and mirrors how WithAccessToken arms the cold-start
//     branch.
//   - poison rate is read at the NAMESPACE resource (aggregated);
//     per-queue attribution is chunk 3b.
//   - A per-namespace query error leaves that snapshot untouched
//     (absent sentinel preserved) and continues.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3-§5.
func (s *Scanner) enrichServiceBusPoisonRate(ctx context.Context, snaps []scanner.EventSourceInstanceSnapshot, accessToken string) {
	if accessToken == "" {
		return
	}
	if s.accessToken == "" {
		s.accessToken = accessToken
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
		res, err := s.DetectServiceBusPoisonRate(ctx, arn)
		if err != nil {
			continue
		}
		detail["poison_rate_per_hour"] = res.RatePerHour
		detail["poison_rate_high_band"] = res.HighBand
	}
}

// --- Poison-rate substrate slice 4 chunk 3b (v0.89.180, #822 Stream
// 219) — PER-QUEUE attribution that closes the §3.2
// scanner-coverage-gap. Chunk 3a measured the namespace-aggregated
// dead-letter delta; chunk 3b splits the same DeadletteredMessages
// metric by the EntityName dimension (one Azure Monitor call,
// $filter="EntityName eq '*'") so the poison rate is attributed to
// the specific worst-offending queue — no separate ARM queue
// enumeration needed.

// serviceBusWorstQueuePoisonRate is the chunk-3b per-queue attribution
// result: the worst-offending queue's name + its net dead-letter
// accumulation rate, plus how many entities carried measurable data.
type serviceBusWorstQueuePoisonRate struct {
	WorstQueue     string
	WorstRatePerHr int
	MeasuredQueues int
}

// DetectServiceBusQueuePoisonRate splits the namespace's
// DeadletteredMessages metric by EntityName and returns the
// worst-offending queue's poison rate. Closes the §3.2 per-queue
// attribution gap.
//
// MeasuredQueues == 0 means Azure returned no per-entity series (the
// caller falls back to the namespace-level chunk-3a reading). The
// real-zero vs absent contract is preserved by the caller via
// MeasuredQueues.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3 + §7.
func (s *Scanner) DetectServiceBusQueuePoisonRate(ctx context.Context, namespaceResourceID string) (serviceBusWorstQueuePoisonRate, error) {
	perEntity, err := s.queryServiceBusDeadletterPerEntity(
		ctx, namespaceResourceID,
		time.Duration(ServiceBusPoisonRateWindowHours)*time.Hour,
	)
	if err != nil {
		return serviceBusWorstQueuePoisonRate{}, fmt.Errorf("per-queue poison-rate query: %w", err)
	}
	// MeasuredQueues == 0 (the empty-map case) signals the caller to
	// fall back to the namespace-level reading; WorstRatePerHr stays
	// at the absent sentinel.
	res := serviceBusWorstQueuePoisonRate{WorstRatePerHr: -1}
	if len(perEntity) == 0 {
		return res, nil
	}
	for queue, rate := range perEntity {
		res.MeasuredQueues++
		if res.WorstQueue == "" || rate > res.WorstRatePerHr {
			res.WorstQueue = queue
			res.WorstRatePerHr = rate
		}
	}
	return res, nil
}

// enrichServiceBusPoisonRatePerQueue is the chunk-3b enrichment pass.
// It supersedes the chunk-3a namespace-level enrichment: for each
// namespace it attributes the poison rate to the worst-offending
// queue (via the EntityName split) and records the queue name +
// measured-queue count. When the per-entity split returns nothing
// (older API surface, or genuinely no entities), it falls back to the
// chunk-3a namespace-aggregated reading so no capability is lost.
//
//   - accessToken == "" → no-op (cold-start parity).
//   - poison_rate_per_hour / poison_rate_high_band → worst queue's
//     rate / band (or the namespace-aggregate on fallback).
//   - poison_rate_worst_queue / poison_rate_measured_queue_count →
//     ADDED only when per-entity data exists (enrichment-only keys;
//     absent on the unwired path, preserving cold-start parity).
//   - per-namespace query error → snapshot left untouched, continue.
//
// See docs/proposals/poison-rate-substrate-slice4.md §3-§5.
func (s *Scanner) enrichServiceBusPoisonRatePerQueue(ctx context.Context, snaps []scanner.EventSourceInstanceSnapshot, accessToken string) {
	if accessToken == "" {
		return
	}
	if s.accessToken == "" {
		s.accessToken = accessToken
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
		worst, err := s.DetectServiceBusQueuePoisonRate(ctx, arn)
		if err != nil {
			continue
		}
		if worst.MeasuredQueues == 0 {
			// No per-entity data — fall back to the chunk-3a
			// namespace-aggregated reading so Azure still reports a
			// real metric (or the absent sentinel) at namespace
			// granularity.
			if res, ferr := s.DetectServiceBusPoisonRate(ctx, arn); ferr == nil {
				detail["poison_rate_per_hour"] = res.RatePerHour
				detail["poison_rate_high_band"] = res.HighBand
			}
			continue
		}
		detail["poison_rate_per_hour"] = worst.WorstRatePerHr
		detail["poison_rate_high_band"] = worst.WorstRatePerHr >= ServiceBusPoisonRatePerHourHighThreshold
		detail["poison_rate_worst_queue"] = worst.WorstQueue
		detail["poison_rate_measured_queue_count"] = worst.MeasuredQueues
	}
}
