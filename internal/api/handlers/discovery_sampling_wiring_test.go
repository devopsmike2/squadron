// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/traceindex"
)

// discovery_sampling_wiring_test.go — slice 2 of #295. Pins the
// span-counter type-assertion the GCP + OCI sampling wiring depends on:
// the server holds the span index as a QualitySnapshotIndex, and the
// setter must recover the SpanCountLast24h surface (which the production
// *traceindex.Quality has) — or leave the annotation a no-op when the
// index doesn't provide it.

// samplingFullQualityIndex satisfies BOTH QualitySnapshotIndex AND
// proposer.SamplingRateSpanCounter — the production *traceindex.Quality
// shape.
type samplingFullQualityIndex struct{ spans uint64 }

func (samplingFullQualityIndex) SnapshotAll() []traceindex.QualityCountersSnapshot { return nil }
func (samplingFullQualityIndex) SnapshotKey(string) (traceindex.QualityCountersSnapshot, bool) {
	return traceindex.QualityCountersSnapshot{}, false
}
func (f samplingFullQualityIndex) SpanCountLast24h(string) (uint64, bool) { return f.spans, true }

// samplingOnlyQualityIndex satisfies QualitySnapshotIndex but NOT the span
// counter — the wiring must leave sampling off rather than panic.
type samplingOnlyQualityIndex struct{}

func (samplingOnlyQualityIndex) SnapshotAll() []traceindex.QualityCountersSnapshot { return nil }
func (samplingOnlyQualityIndex) SnapshotKey(string) (traceindex.QualityCountersSnapshot, bool) {
	return traceindex.QualityCountersSnapshot{}, false
}

func TestWithGCPSamplingSpanCounter_TypeAssertion(t *testing.T) {
	full := (&DiscoveryGCPHandlers{}).WithGCPSamplingSpanCounter(samplingFullQualityIndex{spans: 5})
	if full.samplingSpanCounter == nil {
		t.Error("a quality index exposing SpanCountLast24h should wire the sampling span counter")
	}
	only := (&DiscoveryGCPHandlers{}).WithGCPSamplingSpanCounter(samplingOnlyQualityIndex{})
	if only.samplingSpanCounter != nil {
		t.Error("an index without SpanCountLast24h must leave sampling a no-op")
	}
	none := (&DiscoveryGCPHandlers{}).WithGCPSamplingSpanCounter(nil)
	if none.samplingSpanCounter != nil {
		t.Error("a nil index must leave sampling a no-op")
	}
}

func TestWithOCISamplingSpanCounter_TypeAssertion(t *testing.T) {
	full := (&DiscoveryOCIHandlers{}).WithOCISamplingSpanCounter(samplingFullQualityIndex{spans: 7})
	if full.samplingSpanCounter == nil {
		t.Error("a quality index exposing SpanCountLast24h should wire the sampling span counter")
	}
	only := (&DiscoveryOCIHandlers{}).WithOCISamplingSpanCounter(samplingOnlyQualityIndex{})
	if only.samplingSpanCounter != nil {
		t.Error("an index without SpanCountLast24h must leave sampling a no-op")
	}
	none := (&DiscoveryOCIHandlers{}).WithOCISamplingSpanCounter(nil)
	if none.samplingSpanCounter != nil {
		t.Error("a nil index must leave sampling a no-op")
	}
}

// TestWithAzureSamplingSpanCounter_TypeAssertion mirrors GCP/OCI — Azure
// native sampling (Option 2) recovers SpanCountLast24h the same way.
func TestWithAzureSamplingSpanCounter_TypeAssertion(t *testing.T) {
	full := (&DiscoveryAzureHandlers{}).WithAzureSamplingSpanCounter(samplingFullQualityIndex{spans: 9})
	if full.samplingSpanCounter == nil {
		t.Error("a quality index exposing SpanCountLast24h should wire the sampling span counter")
	}
	only := (&DiscoveryAzureHandlers{}).WithAzureSamplingSpanCounter(samplingOnlyQualityIndex{})
	if only.samplingSpanCounter != nil {
		t.Error("an index without SpanCountLast24h must leave sampling a no-op")
	}
}

// TestWithAzureServerlessMetricDetection_Gate pins the explicit opt-in gate
// Azure needs (its QueryAggregate always has the token, so the annotation
// is gated on the flag, not on metric-client absence like the other clouds).
func TestWithAzureServerlessMetricDetection_Gate(t *testing.T) {
	on := (&DiscoveryAzureHandlers{}).WithAzureServerlessMetricDetection(true)
	if !on.serverlessMetricDetectionEnabled {
		t.Error("WithAzureServerlessMetricDetection(true) must enable the gate")
	}
	off := (&DiscoveryAzureHandlers{}).WithAzureServerlessMetricDetection(false)
	if off.serverlessMetricDetectionEnabled {
		t.Error("default/off must leave Azure native sampling inactive")
	}
}
