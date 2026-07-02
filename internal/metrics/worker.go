// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

// WorkerMetrics tracks metrics for the async worker pool that processes
// OTLP payloads before they're written to telemetry storage. These metrics
// are most useful for catching write failures and queue saturation in
// production.
type WorkerMetrics struct {
	// Queue depth — useful for spotting backpressure / saturation.
	QueueDepth Gauge `metric:"worker_queue_depth" tags:"component=worker" help:"Current depth of the worker pool queue"`

	// Queue bytes — the volatile ack'd-but-unwritten backlog in DATA, the
	// real memory bound (queue_depth counts requests, whose per-request
	// item/byte size varies wildly). Bounded by worker.max_queue_bytes.
	QueueBytes Gauge `metric:"worker_queue_bytes" tags:"component=worker" help:"Current queued OTLP payload bytes awaiting write (volatile ack-to-durable backlog)"`

	// Write retry counters — per signal type.
	TraceWriteRetries  Counter `metric:"worker_trace_write_retries_total"  tags:"component=worker,signal=traces"  help:"Total trace write attempts that failed and were retried"`
	MetricWriteRetries Counter `metric:"worker_metric_write_retries_total" tags:"component=worker,signal=metrics" help:"Total metric write attempts that failed and were retried"`
	LogWriteRetries    Counter `metric:"worker_log_write_retries_total"    tags:"component=worker,signal=logs"    help:"Total log write attempts that failed and were retried"`

	// Dead-letter counters — incremented when retries are exhausted and an item is dropped.
	TraceDeadLetters  Counter `metric:"worker_trace_dead_letters_total"  tags:"component=worker,signal=traces"  help:"Trace writes that failed after exhausting retries"`
	MetricDeadLetters Counter `metric:"worker_metric_dead_letters_total" tags:"component=worker,signal=metrics" help:"Metric writes that failed after exhausting retries"`
	LogDeadLetters    Counter `metric:"worker_log_dead_letters_total"    tags:"component=worker,signal=logs"    help:"Log writes that failed after exhausting retries"`
}

// NewWorkerMetrics creates and initializes worker pool metrics.
func NewWorkerMetrics(factory Factory) *WorkerMetrics {
	m := &WorkerMetrics{}
	MustInit(m, factory, nil)
	return m
}
