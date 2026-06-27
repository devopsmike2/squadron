// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-rate substrate slice 4 chunk 4 (v0.89.181, #823 Stream 220)
// — acceptance tests per docs/proposals/poison-rate-substrate-slice4.md
// §8. REAL OCI Monitoring-backed Queue Service poison-rate detection
// that closes the slice-3 §3.3 deferral for OCI — the FINAL cloud,
// CLOSING the substrate arc.

const (
	testQueueOCID   = "ocid1.queue.oc1.phx.aaaaorders"
	testCompartment = "ocid1.compartment.oc1..aaaa"
)

func dlqPoints(values ...float64) []ociMetricDataPoint {
	out := make([]ociMetricDataPoint, 0, len(values))
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	for i, v := range values {
		out = append(out, ociMetricDataPoint{
			Timestamp:   base.Add(time.Duration(i) * 5 * time.Minute),
			Value:       v,
			SampleCount: 1,
		})
	}
	return out
}

// --- constants pins ---------------------------------------------

func TestOCIQueueMetricNamespace_Constant(t *testing.T) {
	assert.Equal(t, "oci_queue", OCIQueueMetricNamespace)
}

func TestOCIQueuePoisonRateWindowHours_Constant(t *testing.T) {
	assert.Equal(t, 1, OCIQueuePoisonRateWindowHours)
}

// --- metric query: namespace + MQL form + max-min delta ----------

func TestQueryOCIQueueDeadletterDelta_DeltaAndQueryShape(t *testing.T) {
	mf := &monitoringFake{
		respondWith: dlqPoints(10, 45, 130, 90), // max 130, min 10 → delta 120
	}
	s := newMetricsTestScanner(t, mf)
	delta, samples, err := s.queryOCIQueueDeadletterDelta(context.Background(), testCompartment, testQueueOCID)
	assert.NoError(t, err)
	assert.Equal(t, 120, delta, "max(130) - min(10)")
	assert.Equal(t, 4, samples)

	if assert.GreaterOrEqual(t, len(mf.receivedQuery), 1) {
		q := mf.receivedQuery[0]
		assert.Contains(t, q, OCIQueueDeadLetterMessagesMetric)
		assert.Contains(t, q, testQueueOCID)
		assert.True(t, strings.Contains(q, ".max()"), "uses the .max() reduction")
	}
	if assert.GreaterOrEqual(t, len(mf.receivedNS), 1) {
		assert.Equal(t, OCIQueueMetricNamespace, mf.receivedNS[0])
	}
}

// --- DetectOCIQueuePoisonRate: real / real-zero / absent ---------

func TestDetectOCIQueuePoisonRate_RealRateFiresHighBand(t *testing.T) {
	mf := &monitoringFake{respondWith: dlqPoints(5, 125)} // delta 120
	s := newMetricsTestScanner(t, mf)
	res, err := s.DetectOCIQueuePoisonRate(context.Background(), testCompartment, testQueueOCID)
	assert.NoError(t, err)
	assert.Equal(t, 120, res.RatePerHour)
	assert.True(t, res.HighBand, "120/hr >= OCIPoisonRatePerHourHighThreshold=%d", OCIPoisonRatePerHourHighThreshold)
}

func TestDetectOCIQueuePoisonRate_FlatGaugeIsRealZero(t *testing.T) {
	mf := &monitoringFake{respondWith: dlqPoints(50, 50, 50)} // delta 0
	s := newMetricsTestScanner(t, mf)
	res, err := s.DetectOCIQueuePoisonRate(context.Background(), testCompartment, testQueueOCID)
	assert.NoError(t, err)
	assert.Equal(t, 0, res.RatePerHour, "flat gauge = zero NEW dead-letters (real zero, not absent)")
	assert.False(t, res.HighBand)
}

func TestDetectOCIQueuePoisonRate_NoDatapointsIsAbsent(t *testing.T) {
	mf := &monitoringFake{respondWith: nil} // empty → absent
	s := newMetricsTestScanner(t, mf)
	res, err := s.DetectOCIQueuePoisonRate(context.Background(), testCompartment, testQueueOCID)
	assert.NoError(t, err)
	assert.Equal(t, -1, res.RatePerHour, "no datapoints (incl. metric-name mismatch) → safe absent sentinel")
	assert.False(t, res.HighBand)
}

// --- enrichOCIQueuePoisonRate -----------------------------------

func ociQueueSnap() scanner.EventSourceInstanceSnapshot {
	return scanner.EventSourceInstanceSnapshot{
		ResourceARN: testQueueOCID,
		Detail: map[string]any{
			"compartment_id":        testCompartment,
			"poison_rate_per_hour":  -1,
			"poison_rate_high_band": false,
		},
	}
}

func TestEnrichOCIQueuePoisonRate_NoOpPreservesSentinel(t *testing.T) {
	// Reverted to a no-op (v0.89.236): "MessagesInDlq" is not a valid
	// oci_queue metric, so the honest absent sentinels stand and no
	// OCI Monitoring query is issued.
	mf := &monitoringFake{respondWith: dlqPoints(0, 80)}
	s := newMetricsTestScanner(t, mf)
	snaps := []scanner.EventSourceInstanceSnapshot{ociQueueSnap()}

	s.enrichOCIQueuePoisonRate(context.Background(), snaps)

	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snaps[0].Detail["poison_rate_high_band"])
	assert.Equal(t, 0, mf.calls, "enrichment must not query OCI Monitoring")
}

func TestEnrichOCIQueuePoisonRate_NilClientNoOp(t *testing.T) {
	// Scanner with no monitoring client wired.
	s := &Scanner{TenancyOCID: "ocid1.tenancy.oc1..aaa", Region: "us-phoenix-1"}
	snaps := []scanner.EventSourceInstanceSnapshot{ociQueueSnap()}

	s.enrichOCIQueuePoisonRate(context.Background(), snaps)

	// Cold-start parity: honest-framing sentinels survive untouched.
	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"])
	assert.Equal(t, false, snaps[0].Detail["poison_rate_high_band"])
}

func TestEnrichOCIQueuePoisonRate_MissingCompartmentSkipped(t *testing.T) {
	mf := &monitoringFake{respondWith: dlqPoints(0, 500)}
	s := newMetricsTestScanner(t, mf)
	snap := scanner.EventSourceInstanceSnapshot{
		ResourceARN: testQueueOCID,
		Detail:      map[string]any{"poison_rate_per_hour": -1, "poison_rate_high_band": false},
	}
	snaps := []scanner.EventSourceInstanceSnapshot{snap}

	s.enrichOCIQueuePoisonRate(context.Background(), snaps)

	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"], "no compartment_id → cannot query, sentinel kept")
	assert.Equal(t, 0, mf.calls, "no metric call without a compartment")
}
