// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package pipelinehealth

import "testing"

func TestComputeVerdict_Empty(t *testing.T) {
	got, signals := ComputeVerdict(nil)
	if got != VerdictUnknown {
		t.Fatalf("empty input → %s, want unknown", got)
	}
	if len(signals) != 0 {
		t.Fatalf("empty input produced %d signals", len(signals))
	}
}

func TestComputeVerdict_HealthyCollector(t *testing.T) {
	// Queue 10/1000 = 1%, no failures, no drops → healthy.
	latest := map[string][]MetricRow{
		"otelcol_exporter_queue_size": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 10},
		},
		"otelcol_exporter_queue_capacity": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 1000},
		},
		"otelcol_exporter_send_failed_metric_points": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 0},
		},
	}
	got, signals := ComputeVerdict(latest)
	if got != VerdictHealthy {
		t.Fatalf("got %s, want healthy. signals=%+v", got, signals)
	}
	if len(signals) != 0 {
		t.Fatalf("healthy result emitted signals: %+v", signals)
	}
}

func TestComputeVerdict_DegradedQueue(t *testing.T) {
	// Queue 600/1000 = 60% → degraded (warn).
	latest := map[string][]MetricRow{
		"otelcol_exporter_queue_size": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 600},
		},
		"otelcol_exporter_queue_capacity": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 1000},
		},
	}
	got, signals := ComputeVerdict(latest)
	if got != VerdictDegraded {
		t.Fatalf("got %s, want degraded", got)
	}
	if len(signals) != 1 || signals[0].Kind != "queue_saturation" || signals[0].Severity != "warn" {
		t.Fatalf("unexpected signals: %+v", signals)
	}
}

func TestComputeVerdict_BrokenQueue(t *testing.T) {
	// Queue 950/1000 = 95% → broken (critical).
	latest := map[string][]MetricRow{
		"otelcol_exporter_queue_size": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 950},
		},
		"otelcol_exporter_queue_capacity": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 1000},
		},
	}
	got, _ := ComputeVerdict(latest)
	if got != VerdictBroken {
		t.Fatalf("got %s, want broken", got)
	}
}

func TestComputeVerdict_SendFailedTriggersDegraded(t *testing.T) {
	latest := map[string][]MetricRow{
		"otelcol_exporter_send_failed_metric_points": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 42},
		},
	}
	got, signals := ComputeVerdict(latest)
	if got != VerdictDegraded {
		t.Fatalf("got %s, want degraded", got)
	}
	if signals[0].Kind != "send_failed" {
		t.Fatalf("expected send_failed signal, got %+v", signals)
	}
}

func TestComputeVerdict_BrokenWinsOverDegraded(t *testing.T) {
	// Queue saturation broken + send failures degraded → result is broken.
	latest := map[string][]MetricRow{
		"otelcol_exporter_queue_size": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 950},
		},
		"otelcol_exporter_queue_capacity": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 1000},
		},
		"otelcol_exporter_send_failed_metric_points": {
			{Labels: []KV{{Key: "exporter", Value: "otlp/datadog"}}, Value: 1},
		},
	}
	got, signals := ComputeVerdict(latest)
	if got != VerdictBroken {
		t.Fatalf("got %s, want broken", got)
	}
	// Critical-severity signal must sort to the front so the UI shows
	// the most actionable issue first.
	if signals[0].Severity != "critical" {
		t.Fatalf("signals not severity-sorted: %+v", signals)
	}
}

func TestComputeVerdict_ProcessorDropsTriggerDegraded(t *testing.T) {
	latest := map[string][]MetricRow{
		"otelcol_processor_dropped_metric_points": {
			{Labels: []KV{{Key: "processor", Value: "memory_limiter"}}, Value: 12},
		},
	}
	got, signals := ComputeVerdict(latest)
	if got != VerdictDegraded {
		t.Fatalf("got %s, want degraded", got)
	}
	if signals[0].Kind != "processor_drops" {
		t.Fatalf("expected processor_drops signal, got %+v", signals)
	}
}
