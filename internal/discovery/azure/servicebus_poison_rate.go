// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
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
