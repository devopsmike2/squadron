// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package selftel

import (
	"context"
	"testing"

	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// readBridgedMetrics drives the Prometheus -> OTel bridge once with the
// supplied registry and returns the produced metric scopes. Bypasses
// the network exporter entirely — exercises only the producer + reader
// path that selftel.New wires up in production. Keeps the test
// hermetic and synchronous.
func readBridgedMetrics(t *testing.T, registry *prometheus.Registry) []metricdata.ScopeMetrics {
	t.Helper()
	producer := prombridge.NewMetricProducer(prombridge.WithGatherer(registry))
	reader := sdkmetric.NewManualReader(sdkmetric.WithProducer(producer))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm),
		"reader.Collect should succeed against any Prometheus registry")
	return rm.ScopeMetrics
}

// findMetric returns the first metric in any scope whose name matches
// `name`, or nil if absent. The bridge prefixes nothing — Prometheus
// metric names come through verbatim.
func findMetric(scopes []metricdata.ScopeMetrics, name string) *metricdata.Metrics {
	for _, scope := range scopes {
		for i := range scope.Metrics {
			if scope.Metrics[i].Name == name {
				return &scope.Metrics[i]
			}
		}
	}
	return nil
}

func TestMetricBridge_CounterRoundTrip(t *testing.T) {
	// A Prometheus counter incremented by N should appear on the OTel
	// side as a Sum data point with value N. Label key/values must
	// propagate as attributes.
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "squadron_test_widgets_total",
		Help: "Widgets produced.",
	}, []string{"shape"})
	reg.MustRegister(counter)
	counter.WithLabelValues("square").Add(3)
	counter.WithLabelValues("circle").Add(7)

	scopes := readBridgedMetrics(t, reg)
	m := findMetric(scopes, "squadron_test_widgets_total")
	require.NotNil(t, m, "expected squadron_test_widgets_total in bridged metrics")

	sum, ok := m.Data.(metricdata.Sum[float64])
	require.True(t, ok, "expected Sum[float64], got %T", m.Data)
	assert.True(t, sum.IsMonotonic, "counter must bridge to a monotonic Sum")

	values := map[string]float64{}
	for _, dp := range sum.DataPoints {
		shape, _ := dp.Attributes.Value("shape")
		values[shape.AsString()] = dp.Value
	}
	assert.Equal(t, 3.0, values["square"])
	assert.Equal(t, 7.0, values["circle"])
}

func TestMetricBridge_GaugeRoundTrip(t *testing.T) {
	// Gauges should bridge to OTel Gauge data points with the current
	// value. A subsequent set on the same gauge should reflect on
	// the next Collect, not be summed.
	reg := prometheus.NewRegistry()
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "squadron_test_in_flight",
		Help: "Currently in flight.",
	})
	reg.MustRegister(gauge)
	gauge.Set(42)

	scopes := readBridgedMetrics(t, reg)
	m := findMetric(scopes, "squadron_test_in_flight")
	require.NotNil(t, m)

	g, ok := m.Data.(metricdata.Gauge[float64])
	require.True(t, ok, "expected Gauge[float64], got %T", m.Data)
	require.Len(t, g.DataPoints, 1)
	assert.Equal(t, 42.0, g.DataPoints[0].Value)
}

func TestMetricBridge_HistogramRoundTrip(t *testing.T) {
	// Prometheus histograms with explicit buckets should bridge to OTel
	// Histogram data points carrying the same bucket boundaries +
	// counts. The bridge preserves the Prometheus le-bucket semantics.
	reg := prometheus.NewRegistry()
	hist := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "squadron_test_duration_seconds",
		Help:    "Operation duration.",
		Buckets: []float64{0.1, 0.5, 1, 5},
	})
	reg.MustRegister(hist)
	for _, v := range []float64{0.05, 0.2, 0.3, 2, 10} {
		hist.Observe(v)
	}

	scopes := readBridgedMetrics(t, reg)
	m := findMetric(scopes, "squadron_test_duration_seconds")
	require.NotNil(t, m)

	h, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok, "expected Histogram[float64], got %T", m.Data)
	require.Len(t, h.DataPoints, 1)
	dp := h.DataPoints[0]
	assert.EqualValues(t, 5, dp.Count, "5 observations should land in the histogram")
	assert.InDelta(t, 12.55, dp.Sum, 1e-6, "sum of observations must propagate")
}

func TestMetricBridge_DisabledPath_NoExport(t *testing.T) {
	// Pin the disabled invariant: when telemetry.enabled = false, the
	// Publisher is constructed with mp == nil and the global
	// MeterProvider is never installed. Operators must not pay any
	// scrape cost when self-telemetry is off.
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "squadron_test_disabled_counter",
		Help: "Should never reach OTLP.",
	}))

	pub, err := New(context.Background(), Config{Enabled: false}, reg, zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, pub)
	assert.Nil(t, pub.mp, "disabled Publisher must not construct a MeterProvider")

	// Shutdown the no-op publisher to confirm the metric-shutdown path
	// is also nil-safe.
	require.NoError(t, pub.Shutdown(context.Background()))
}

func TestMetricBridge_NilGatherer_TracesStillExport(t *testing.T) {
	// Passing nil for the Gatherer means "I want traces but not the
	// metric bridge" — a valid intermediate state for callers that
	// haven't wired the registry yet, or for tests. The publisher
	// must construct successfully with tp != nil and mp == nil.
	pub, err := New(context.Background(), Config{
		Enabled:  true,
		Endpoint: "127.0.0.1:0", // unreachable on purpose; trace exporter dials lazily
		Protocol: "grpc",
		Insecure: true,
	}, nil, zap.NewNop())
	require.NoError(t, err, "trace-only init should not fail with nil Gatherer")
	require.NotNil(t, pub)
	assert.NotNil(t, pub.tp, "trace provider must be set when Enabled=true")
	assert.Nil(t, pub.mp, "metric provider must be nil when Gatherer is nil")
	require.NoError(t, pub.Shutdown(context.Background()))
}
