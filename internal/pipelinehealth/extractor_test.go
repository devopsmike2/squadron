// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package pipelinehealth

import (
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/otlp"
)

func TestIsCaptured(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		// Captured names
		{"otelcol_exporter_queue_size", true},
		{"otelcol_exporter_send_failed_metric_points", true},
		{"otelcol_process_uptime", true},
		// Wrong prefix
		{"prometheus_engine_query_duration_seconds", false},
		{"http_request_total", false},
		// Right prefix, not in allow list (an obscure scope-instrumentation metric)
		{"otelcol_scrape_failed_total", false},
		{"otelcol_exporter_compression_ratio", false},
		// Empty
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCaptured(tc.name); got != tc.want {
				t.Fatalf("IsCaptured(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestHasAny_FastPath(t *testing.T) {
	// All-user-metrics batch → false (the fast-path the worker uses
	// to skip Extract entirely).
	sums := []otlp.MetricSumData{
		{MetricName: "http_request_total", AgentID: "a1"},
	}
	gauges := []otlp.MetricGaugeData{
		{MetricName: "http_active_connections", AgentID: "a1"},
	}
	if HasAny(sums, gauges) {
		t.Fatal("HasAny returned true for a batch with no otelcol_* metrics")
	}

	// One captured metric → true.
	gauges = append(gauges, otlp.MetricGaugeData{
		MetricName: "otelcol_exporter_queue_size", AgentID: "a1",
	})
	if !HasAny(sums, gauges) {
		t.Fatal("HasAny returned false for a batch with otelcol_* metrics")
	}
}

func TestExtract_ProducesSamples(t *testing.T) {
	now := time.Now().UTC()
	sums := []otlp.MetricSumData{
		{
			MetricName: "otelcol_exporter_send_failed_metric_points",
			AgentID:    "agent-1",
			Attributes: map[string]string{"exporter": "otlp/datadog"},
			TimeUnix:   now,
			Value:      42,
			MetricUnit: "1",
		},
		// User metric mixed into the batch — should be skipped.
		{
			MetricName: "http_request_total",
			AgentID:    "agent-1",
			Value:      1000,
		},
		// otelcol_* but not on the allow list — should be skipped.
		{
			MetricName: "otelcol_scrape_failed_total",
			AgentID:    "agent-1",
			Value:      5,
		},
		// Agent ID missing — should be silently dropped.
		{
			MetricName: "otelcol_exporter_sent_metric_points",
			AgentID:    "",
			Value:      99,
		},
	}
	gauges := []otlp.MetricGaugeData{
		{
			MetricName: "otelcol_exporter_queue_size",
			AgentID:    "agent-1",
			Attributes: map[string]string{"exporter": "otlp/datadog"},
			TimeUnix:   now,
			Value:      512,
		},
	}

	samples := Extract(sums, gauges)
	if len(samples) != 2 {
		t.Fatalf("Extract returned %d samples, want 2: %+v", len(samples), samples)
	}

	for _, s := range samples {
		if s.AgentID != "agent-1" {
			t.Errorf("sample has wrong agent_id: %s", s.AgentID)
		}
		if s.LabelsHash == "" {
			t.Errorf("sample has no labels hash: %+v", s)
		}
		if s.Timestamp.IsZero() {
			t.Errorf("sample has zero timestamp: %+v", s)
		}
		if s.Labels["exporter"] != "otlp/datadog" {
			t.Errorf("sample lost the exporter label: %+v", s.Labels)
		}
	}
}

func TestHashLabels_Stable(t *testing.T) {
	// Two maps with the same content but built in different orders
	// MUST produce the same hash. The implementation sorts before
	// hashing — this test exists so a refactor doesn't accidentally
	// drop the sort.
	a := map[string]string{
		"exporter":     "otlp/datadog",
		"data_type":    "metric_points",
		"service.name": "otelcol",
	}
	b := map[string]string{
		"service.name": "otelcol",
		"exporter":     "otlp/datadog",
		"data_type":    "metric_points",
	}
	if HashLabels(a) != HashLabels(b) {
		t.Fatalf("HashLabels not stable across map iteration order: %s vs %s",
			HashLabels(a), HashLabels(b))
	}

	// Different content → different hash.
	c := map[string]string{
		"exporter":     "otlp/honeycomb",
		"data_type":    "metric_points",
		"service.name": "otelcol",
	}
	if HashLabels(a) == HashLabels(c) {
		t.Fatal("HashLabels collided on different inputs")
	}

	// Empty → "_" sentinel.
	if HashLabels(nil) != "_" {
		t.Fatalf("HashLabels(nil) = %q, want '_'", HashLabels(nil))
	}
}
