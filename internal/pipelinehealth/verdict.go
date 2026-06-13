// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package pipelinehealth

import "fmt"

// Verdict thresholds. These are deliberately conservative — the
// goal is to flag obvious problems, not nitpick steady-state noise.
// A collector with a queue 50% full or a single send-failure in
// the last sample WILL get a Degraded badge; one that's losing
// data continuously gets Broken.
const (
	// QueueWarnRatio is the queue_size/queue_capacity threshold at
	// which we emit a Degraded signal. 50% means there's still
	// headroom but the destination is clearly slower than the
	// inbound rate.
	QueueWarnRatio = 0.5
	// QueueCriticalRatio is the threshold at which we emit a Broken
	// signal. 90% means the queue is about to start refusing items.
	QueueCriticalRatio = 0.9

	// SendFailedWarnAbsolute is the latest-sample value that emits
	// a Degraded signal. The collector reports cumulative counters,
	// so the latest value > 0 means failures have been observed at
	// some point in the agent's lifetime — not necessarily right now.
	// The Broken threshold uses a rate calculation handled by the
	// caller (we don't have two samples here).
	SendFailedWarnAbsolute = 1.0

	// ProcessorDropsWarnAbsolute is the latest-sample value that
	// emits a Degraded signal. Same caveat as SendFailedWarnAbsolute:
	// this is a counter, so non-zero means it's happened, not that
	// it's happening right now.
	ProcessorDropsWarnAbsolute = 1.0
)

// ComputeVerdict applies the threshold rules to a per-agent
// latest-value map and returns a verdict + the signals that
// contributed to it.
//
// Inputs:
//
//   - latest: map[metric_name] → []MetricRow. Each MetricRow is one
//     (label set, value) latest observation. The same metric_name
//     can have multiple rows because the collector reports one row
//     per exporter / receiver / processor.
//
// Output:
//
//   - Verdict — healthy / degraded / broken / unknown (only if
//     map is empty).
//   - []Signal — every threshold breach we found, ordered from
//     most-severe to least.
//
// The rules:
//
//  1. Queue saturation: for every (exporter) pair with a queue_size
//     and queue_capacity, ratio = size/capacity. ≥ critical → Broken.
//     ≥ warn → Degraded.
//
//  2. Send failures: any otelcol_exporter_send_failed_* > 0 →
//     Degraded. (Without two samples we can't compute a rate; the
//     alert evaluator in v0.32+ does the rate calculation across
//     polls.)
//
//  3. Processor drops: any otelcol_processor_dropped_* > 0 →
//     Degraded.
//
// Signals are accumulated; the worst signal severity drives the
// verdict.
func ComputeVerdict(latest map[string][]MetricRow) (Verdict, []Signal) {
	if len(latest) == 0 {
		return VerdictUnknown, nil
	}
	signals := []Signal{}
	worst := VerdictHealthy

	// Rule 1: queue saturation per exporter.
	queueSize := latest["otelcol_exporter_queue_size"]
	queueCap := latest["otelcol_exporter_queue_capacity"]
	if len(queueSize) > 0 && len(queueCap) > 0 {
		capByExp := indexByLabel(queueCap, "exporter")
		for _, sizeRow := range queueSize {
			exp := labelValue(sizeRow.Labels, "exporter")
			capRow, ok := capByExp[exp]
			if !ok || capRow.Value <= 0 {
				continue
			}
			ratio := sizeRow.Value / capRow.Value
			if ratio >= QueueCriticalRatio {
				signals = append(signals, Signal{
					Kind:     "queue_saturation",
					Severity: "critical",
					Message:  fmt.Sprintf("exporter %q queue %.0f%% full (%v/%v)", exp, ratio*100, sizeRow.Value, capRow.Value),
					Value:    ratio,
				})
				worst = worsen(worst, VerdictBroken)
			} else if ratio >= QueueWarnRatio {
				signals = append(signals, Signal{
					Kind:     "queue_saturation",
					Severity: "warn",
					Message:  fmt.Sprintf("exporter %q queue %.0f%% full (%v/%v)", exp, ratio*100, sizeRow.Value, capRow.Value),
					Value:    ratio,
				})
				worst = worsen(worst, VerdictDegraded)
			}
		}
	}

	// Rule 2: send failures observed.
	for _, name := range []string{
		"otelcol_exporter_send_failed_spans",
		"otelcol_exporter_send_failed_metric_points",
		"otelcol_exporter_send_failed_log_records",
	} {
		for _, row := range latest[name] {
			if row.Value < SendFailedWarnAbsolute {
				continue
			}
			signals = append(signals, Signal{
				Kind:     "send_failed",
				Severity: "warn",
				Message:  fmt.Sprintf("%s = %.0f via exporter %q", name, row.Value, labelValue(row.Labels, "exporter")),
				Value:    row.Value,
			})
			worst = worsen(worst, VerdictDegraded)
		}
	}

	// Rule 3: processor drops observed.
	for _, name := range []string{
		"otelcol_processor_dropped_spans",
		"otelcol_processor_dropped_metric_points",
		"otelcol_processor_dropped_log_records",
	} {
		for _, row := range latest[name] {
			if row.Value < ProcessorDropsWarnAbsolute {
				continue
			}
			signals = append(signals, Signal{
				Kind:     "processor_drops",
				Severity: "warn",
				Message:  fmt.Sprintf("%s = %.0f via processor %q", name, row.Value, labelValue(row.Labels, "processor")),
				Value:    row.Value,
			})
			worst = worsen(worst, VerdictDegraded)
		}
	}

	// Sort signals: critical first, then warn; otherwise stable.
	// Caller relies on signals[0] being the most actionable one.
	stableSort(signals)
	return worst, signals
}

// worsen returns the more severe of two verdicts. Order is:
// healthy < degraded < broken. Unknown is treated like healthy so
// it never overrides a real finding.
func worsen(current, candidate Verdict) Verdict {
	rank := func(v Verdict) int {
		switch v {
		case VerdictBroken:
			return 3
		case VerdictDegraded:
			return 2
		case VerdictHealthy:
			return 1
		default:
			return 0
		}
	}
	if rank(candidate) > rank(current) {
		return candidate
	}
	return current
}

// indexByLabel groups rows by the value of a single label key.
// Returns the FIRST row per key value, since for queue gauges the
// agent only reports one capacity per exporter.
func indexByLabel(rows []MetricRow, key string) map[string]MetricRow {
	out := map[string]MetricRow{}
	for _, r := range rows {
		v := labelValue(r.Labels, key)
		if _, ok := out[v]; !ok {
			out[v] = r
		}
	}
	return out
}

// labelValue returns the value for a given label key, or empty if
// the key is missing. We treat missing as a valid bucket so an
// agent without labelled exporters still gets a single ratio.
func labelValue(labels []KV, key string) string {
	for _, kv := range labels {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

// stableSort orders signals critical → warn, preserving input
// order within a severity. We avoid sort.SliceStable on the type
// directly to keep the dependency surface tight in this file.
func stableSort(signals []Signal) {
	for i := 1; i < len(signals); i++ {
		for j := i; j > 0; j-- {
			if signalLess(signals[j-1], signals[j]) {
				break
			}
			signals[j-1], signals[j] = signals[j], signals[j-1]
		}
	}
}

func signalLess(a, b Signal) bool {
	rank := func(s string) int {
		if s == "critical" {
			return 1
		}
		return 2
	}
	return rank(a.Severity) < rank(b.Severity)
}
