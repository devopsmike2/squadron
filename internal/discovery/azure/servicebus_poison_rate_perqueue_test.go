// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Poison-rate substrate slice 4 chunk 3b (v0.89.180, #822 Stream 219)
// — acceptance tests for PER-QUEUE attribution via the EntityName
// dimension split. Closes the §3.2 scanner-coverage-gap.

// perEntityResponse builds an armMetricsResponse with one timeseries
// per entity, each tagged with its entityname metadata and carrying
// (Maximum, Minimum) datapoint pairs.
func perEntityResponse(entities map[string][][2]float64) armMetricsResponse {
	tss := make([]armMetricsTimeseries, 0, len(entities))
	for name, pairs := range entities {
		dps := make([]armMetricsDatapoint, 0, len(pairs))
		for i, mm := range pairs {
			mx, mn := mm[0], mm[1]
			dps = append(dps, armMetricsDatapoint{
				TimeStamp: timeStampAt(i),
				Maximum:   fpPtr(mx),
				Minimum:   fpPtr(mn),
			})
		}
		tss = append(tss, armMetricsTimeseries{
			Data: dps,
			MetadataValues: []armMetricsMetadataValue{{
				Name:  armMetricsMetadataName{Value: "entityname"},
				Value: name,
			}},
		})
	}
	return armMetricsResponse{Value: []armMetricsValue{{Unit: "Count", Timeseries: tss}}}
}

// --- per-entity query: parses metadata + per-queue delta ---------

func TestQueryServiceBusDeadletterPerEntity_PerQueueDeltas(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: perEntityResponse(map[string][][2]float64{
			"orders":   {{10, 5}, {30, 12}},    // delta 25
			"payments": {{200, 50}, {210, 80}}, // delta 160
		}),
	}
	s := newMetricsScannerWithFake(t, fake)
	perEntity, err := s.queryServiceBusDeadletterPerEntity(context.Background(), testNamespaceARN, time.Hour)
	assert.NoError(t, err)
	assert.Equal(t, 25, perEntity["orders"])
	assert.Equal(t, 160, perEntity["payments"])

	// The call must request the EntityName split.
	if assert.GreaterOrEqual(t, len(fake.receivedReqs), 1) {
		assert.Contains(t, fake.receivedReqs[0].URL.Query().Get("$filter"), "EntityName eq '*'")
	}
}

func TestQueryServiceBusDeadletterPerEntity_SkipsEntitiesWithoutName(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: armMetricsResponse{Value: []armMetricsValue{{Timeseries: []armMetricsTimeseries{
			{Data: []armMetricsDatapoint{{Maximum: fpPtr(99), Minimum: fpPtr(1)}}}, // no metadata → skipped
		}}}},
	}
	s := newMetricsScannerWithFake(t, fake)
	perEntity, err := s.queryServiceBusDeadletterPerEntity(context.Background(), testNamespaceARN, time.Hour)
	assert.NoError(t, err)
	assert.Empty(t, perEntity, "timeseries without an entityname metadata value is skipped")
}

// --- worst-queue detection --------------------------------------

func TestDetectServiceBusQueuePoisonRate_PicksWorstQueue(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: perEntityResponse(map[string][][2]float64{
			"orders":   {{10, 5}},   // delta 5
			"payments": {{300, 10}}, // delta 290 → worst
			"audit":    {{40, 40}},  // delta 0
		}),
	}
	s := newMetricsScannerWithFake(t, fake)
	worst, err := s.DetectServiceBusQueuePoisonRate(context.Background(), testNamespaceARN)
	assert.NoError(t, err)
	assert.Equal(t, "payments", worst.WorstQueue)
	assert.Equal(t, 290, worst.WorstRatePerHr)
	assert.Equal(t, 3, worst.MeasuredQueues)
}

func TestDetectServiceBusQueuePoisonRate_NoEntitiesSignalsFallback(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: armMetricsResponse{Value: []armMetricsValue{{Timeseries: []armMetricsTimeseries{}}}},
	}
	s := newMetricsScannerWithFake(t, fake)
	worst, err := s.DetectServiceBusQueuePoisonRate(context.Background(), testNamespaceARN)
	assert.NoError(t, err)
	assert.Equal(t, 0, worst.MeasuredQueues, "no entities → caller falls back to namespace-level")
	assert.Equal(t, -1, worst.WorstRatePerHr)
}

// --- enrichment: worst-queue attribution + keys -----------------

func TestEnrichServiceBusPoisonRatePerQueue_AttributesWorstQueue(t *testing.T) {
	fake := &fakeAzureMetrics{
		cannedResponse: perEntityResponse(map[string][][2]float64{
			"orders":   {{10, 5}},  // delta 5
			"payments": {{130, 5}}, // delta 125 → worst, high band
		}),
	}
	s := newMetricsScannerWithFake(t, fake)
	snaps := []scanner.EventSourceInstanceSnapshot{sbSnap()}

	s.enrichServiceBusPoisonRatePerQueue(context.Background(), snaps, "fake-token")

	assert.Equal(t, 125, snaps[0].Detail["poison_rate_per_hour"], "worst queue's rate")
	assert.Equal(t, true, snaps[0].Detail["poison_rate_high_band"])
	assert.Equal(t, "payments", snaps[0].Detail["poison_rate_worst_queue"], "§3.2 per-queue attribution")
	assert.Equal(t, 2, snaps[0].Detail["poison_rate_measured_queue_count"])
}

func TestEnrichServiceBusPoisonRatePerQueue_FallsBackToNamespaceLevel(t *testing.T) {
	// First call (per-entity split) returns no entities; the fallback
	// namespace-level call returns a single aggregated series.
	callN := 0
	fake := &fakeAzureMetrics{
		responder: func(req *http.Request, n int) (int, interface{}) {
			callN++
			if callN == 1 {
				// per-entity split → empty
				return 200, armMetricsResponse{Value: []armMetricsValue{{Timeseries: []armMetricsTimeseries{}}}}
			}
			// namespace-level fallback → aggregated delta 70
			return 200, deadletterResponse([2]float64{70, 0})
		},
	}
	s := newMetricsScannerWithFake(t, fake)
	snaps := []scanner.EventSourceInstanceSnapshot{sbSnap()}

	s.enrichServiceBusPoisonRatePerQueue(context.Background(), snaps, "fake-token")

	assert.Equal(t, 70, snaps[0].Detail["poison_rate_per_hour"], "fell back to namespace-aggregated reading")
	assert.Equal(t, true, snaps[0].Detail["poison_rate_high_band"])
	// No per-queue keys on the fallback path.
	_, hasWorst := snaps[0].Detail["poison_rate_worst_queue"]
	assert.False(t, hasWorst, "no worst-queue key when per-entity data is unavailable")
}

func TestEnrichServiceBusPoisonRatePerQueue_EmptyTokenNoOp(t *testing.T) {
	fake := &fakeAzureMetrics{cannedResponse: perEntityResponse(map[string][][2]float64{"q": {{500, 0}}})}
	s := newMetricsScannerWithFake(t, fake)
	s.accessToken = "" // force unwired path
	snaps := []scanner.EventSourceInstanceSnapshot{sbSnap()}

	s.enrichServiceBusPoisonRatePerQueue(context.Background(), snaps, "")

	assert.Equal(t, -1, snaps[0].Detail["poison_rate_per_hour"], "cold-start parity: sentinel survives")
	_, hasWorst := snaps[0].Detail["poison_rate_worst_queue"]
	assert.False(t, hasWorst)
}
