package receiver

import (
	"context"

	"github.com/devopsmike2/squadron/internal/otlp"
)

// TelemetryWriter defines the interface for writing telemetry data to storage
type TelemetryWriter interface {
	// WriteTraces writes trace data to storage
	WriteTraces(ctx context.Context, traces []otlp.TraceData) error

	// WriteMetrics writes metric data to storage
	WriteMetrics(ctx context.Context, sums []otlp.MetricSumData, gauges []otlp.MetricGaugeData, histograms []otlp.MetricHistogramData) error

	// WriteLogs writes log data to storage
	WriteLogs(ctx context.Context, logs []otlp.LogData) error
}
