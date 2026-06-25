// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package pipelinehealth converts OTLP self-metrics emitted by the
// OpenTelemetry Collector (the `otelcol_*` metric family) into a
// per-agent health snapshot.
//
// The package has two responsibilities:
//
//  1. EXTRACT — given a freshly-parsed OTLP metrics batch, pull out
//     the otelcol_* data points and turn them into
//     telemetrytypes.PipelineHealthSample rows. This lets the worker
//     pool fan-out: regular metrics still land in metrics_sum /
//     metrics_gauge so users can query them via SquadronQL, and a
//     copy of just the self-metric subset lands in
//     pipeline_health_samples for the dedicated dashboard.
//
//  2. VERDICT — given the latest sample per (metric_name, labels)
//     for an agent, compute a coarse health verdict (healthy /
//     degraded / broken) and a list of contributing signals. This
//     is what the fleet view and the agent drawer surface.
//
// The extractor is deliberately small. It does NOT subscribe to the
// OTLP stream itself — the worker pool calls Extract after its
// existing ParseMetrics step. That keeps the regular-telemetry hot
// path independent of pipeline-health concerns.
package pipelinehealth

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/devopsmike2/squadron/internal/otlp"
	telemetrytypes "github.com/devopsmike2/squadron/internal/storage/telemetrystore/types"
)

// MetricNamePrefix is the prefix every OpenTelemetry Collector
// self-metric carries. The collector_contrib distribution also
// emits a few process_* metrics (process_uptime, process_runtime_*)
// — we capture those too because uptime + memory headroom are
// useful signals for the verdict.
const MetricNamePrefix = "otelcol_"

// captureNames is the allow-list of metric names we route into the
// pipeline_health table. Restricting to this list keeps storage
// small and the dashboard queryable on a known schema. The full
// otelcol_* surface (including per-receiver per-format counters) is
// still written to metrics_sum / metrics_gauge, so power users can
// still query it via SquadronQL without us re-storing every datapoint.
//
// Each entry maps the metric name to whether it's a counter (we
// derive rate from successive samples in the verdict layer) or a
// gauge (latest value is what matters).
var captureNames = map[string]struct{}{
	// Receiver throughput + refusal. Refusal counts are the canonical
	// "the collector couldn't keep up" signal at the front of the
	// pipeline.
	"otelcol_receiver_accepted_spans":         {},
	"otelcol_receiver_refused_spans":          {},
	"otelcol_receiver_accepted_metric_points": {},
	"otelcol_receiver_refused_metric_points":  {},
	"otelcol_receiver_accepted_log_records":   {},
	"otelcol_receiver_refused_log_records":    {},

	// Exporter throughput + failure. send_failed_* is the canonical
	// "the data isn't making it to the destination" signal at the
	// back of the pipeline.
	"otelcol_exporter_sent_spans":                 {},
	"otelcol_exporter_send_failed_spans":          {},
	"otelcol_exporter_sent_metric_points":         {},
	"otelcol_exporter_send_failed_metric_points":  {},
	"otelcol_exporter_sent_log_records":           {},
	"otelcol_exporter_send_failed_log_records":    {},
	"otelcol_exporter_enqueue_failed_spans":       {},
	"otelcol_exporter_enqueue_failed_log_records": {},

	// Queue gauges. queue_size approaches queue_capacity means the
	// exporter is backed up — usually because the destination is
	// slow or down.
	"otelcol_exporter_queue_size":     {},
	"otelcol_exporter_queue_capacity": {},

	// Processor drops. Dropped points usually mean a filter
	// processor or a memory_limiter is throwing data away.
	"otelcol_processor_dropped_spans":         {},
	"otelcol_processor_dropped_metric_points": {},
	"otelcol_processor_dropped_log_records":   {},
	"otelcol_processor_refused_spans":         {},
	"otelcol_processor_refused_metric_points": {},
	"otelcol_processor_refused_log_records":   {},

	// Process health. uptime makes it easy to detect a restart loop;
	// memory_rss + cpu_seconds are the "is the collector itself
	// healthy" signals.
	"otelcol_process_uptime":                    {},
	"otelcol_process_memory_rss":                {},
	"otelcol_process_runtime_heap_alloc_bytes":  {},
	"otelcol_process_runtime_total_alloc_bytes": {},
	"otelcol_process_cpu_seconds":               {},
}

// IsCaptured reports whether a metric name is one we route into the
// pipeline_health table. Exposed so the worker can short-circuit
// before allocating a slice for the extracted samples.
func IsCaptured(metricName string) bool {
	if !strings.HasPrefix(metricName, MetricNamePrefix) {
		return false
	}
	_, ok := captureNames[metricName]
	return ok
}

// HasAny reports whether any metric in the parsed batch is one we
// would capture. Used by the worker pool to avoid the allocation
// cost of Extract when a batch is pure user telemetry.
func HasAny(sums []otlp.MetricSumData, gauges []otlp.MetricGaugeData) bool {
	for i := range sums {
		if IsCaptured(sums[i].MetricName) {
			return true
		}
	}
	for i := range gauges {
		if IsCaptured(gauges[i].MetricName) {
			return true
		}
	}
	return false
}

// Extract walks a freshly-parsed OTLP metrics batch and returns
// the pipeline-health samples for the captured metric names. The
// caller is the worker pool's metrics path; both sums (counters)
// and gauges produce samples.
//
// Samples missing an agent ID are dropped silently — there's no
// useful way to attribute them in the dashboard.
func Extract(sums []otlp.MetricSumData, gauges []otlp.MetricGaugeData) []telemetrytypes.PipelineHealthSample {
	if len(sums) == 0 && len(gauges) == 0 {
		return nil
	}
	out := make([]telemetrytypes.PipelineHealthSample, 0, 8)
	for i := range sums {
		s := &sums[i]
		if !IsCaptured(s.MetricName) || s.AgentID == "" {
			continue
		}
		labels := stringLabels(s.Attributes)
		out = append(out, telemetrytypes.PipelineHealthSample{
			Timestamp:  s.TimeUnix,
			AgentID:    s.AgentID,
			MetricName: s.MetricName,
			Labels:     labels,
			LabelsHash: HashLabels(labels),
			Value:      s.Value,
			Unit:       s.MetricUnit,
		})
	}
	for i := range gauges {
		g := &gauges[i]
		if !IsCaptured(g.MetricName) || g.AgentID == "" {
			continue
		}
		labels := stringLabels(g.Attributes)
		out = append(out, telemetrytypes.PipelineHealthSample{
			Timestamp:  g.TimeUnix,
			AgentID:    g.AgentID,
			MetricName: g.MetricName,
			Labels:     labels,
			LabelsHash: HashLabels(labels),
			Value:      g.Value,
			Unit:       g.MetricUnit,
		})
	}
	return out
}

// stringLabels copies the OTLP attribute map (string→string) into a
// fresh map. We deliberately don't keep non-string values — the
// otelcol_* metrics only use string attributes (exporter name,
// receiver name, signal type), and storing only strings keeps the
// JSON column tidy.
func stringLabels(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		out[k] = v
	}
	return out
}

// HashLabels returns a short stable digest of a label set. We use
// it as the third part of the (agent_id, metric_name, labels_hash)
// natural key — two exporters on the same agent will produce
// different hashes, so their time series are properly separated.
//
// The implementation is sha256 truncated to 16 hex chars. Collisions
// are vanishingly unlikely at the scale of "label sets per agent"
// (low hundreds at the absolute most). We sort keys before hashing
// so map iteration order doesn't change the digest.
func HashLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "_"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(labels[k]))
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}
