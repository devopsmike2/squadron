// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package parser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func setupParserTest() *OTLPParser {
	logger := zap.NewNop()
	return NewOTLPParser(logger)
}

func makeResourceAttributes() []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		{
			Key: "service.name",
			Value: &commonpb.AnyValue{
				Value: &commonpb.AnyValue_StringValue{StringValue: "test-service"},
			},
		},
		{
			Key: "service.instance.id",
			Value: &commonpb.AnyValue{
				Value: &commonpb.AnyValue_StringValue{StringValue: "11111111-1111-4111-8111-111111111111"},
			},
		},
		{
			Key: "agent.group_id",
			Value: &commonpb.AnyValue{
				Value: &commonpb.AnyValue_StringValue{StringValue: "test-group-id"},
			},
		},
		{
			Key: "agent.group_name",
			Value: &commonpb.AnyValue{
				Value: &commonpb.AnyValue_StringValue{StringValue: "test-group"},
			},
		},
	}
}

// Trace parsing tests

func TestParseTraces_Success(t *testing.T) {
	parser := setupParserTest()

	// Create test trace data
	traceID := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	parentSpanID := []byte{9, 10, 11, 12, 13, 14, 15, 16}

	request := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource: &resourcepb.Resource{
					Attributes: makeResourceAttributes(),
				},
				ScopeSpans: []*tracepb.ScopeSpans{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0.0",
						},
						Spans: []*tracepb.Span{
							{
								TraceId:           traceID,
								SpanId:            spanID,
								ParentSpanId:      parentSpanID,
								Name:              "test-span",
								Kind:              tracepb.Span_SPAN_KIND_SERVER,
								StartTimeUnixNano: uint64(time.Now().UnixNano()),
								EndTimeUnixNano:   uint64(time.Now().Add(time.Second).UnixNano()),
								Status: &tracepb.Status{
									Code:    tracepb.Status_STATUS_CODE_OK,
									Message: "OK",
								},
								Attributes: []*commonpb.KeyValue{
									{
										Key: "http.method",
										Value: &commonpb.AnyValue{
											Value: &commonpb.AnyValue_StringValue{StringValue: "GET"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Marshal to bytes
	data, err := proto.Marshal(request)
	require.NoError(t, err)

	// Parse traces
	traces, err := parser.ParseTraces(data)
	require.NoError(t, err)
	require.Len(t, traces, 1)

	// Verify parsed trace
	trace := traces[0]
	assert.Equal(t, "test-span", trace.SpanName)
	assert.Equal(t, "test-service", trace.ServiceName)
	assert.Equal(t, "11111111-1111-4111-8111-111111111111", trace.AgentID)
	assert.Equal(t, "test-group-id", trace.GroupID)
	assert.Equal(t, "test-group", trace.GroupName)
	assert.Equal(t, "STATUS_CODE_OK", trace.StatusCode)
	assert.NotEmpty(t, trace.TraceId)
	assert.NotEmpty(t, trace.SpanId)
}

// TestParseTraces_DurationClampsUnderflow verifies span durations don't wrap to
// a garbage value when the end timestamp is unset (0) or precedes the start
// (clock skew) — both are clamped to 0 instead of ~1.8e19 ns.
func TestParseTraces_DurationClampsUnderflow(t *testing.T) {
	parser := setupParserTest()
	start := uint64(time.Now().UnixNano())

	span := func(name string, startN, endN uint64) *tracepb.Span {
		return &tracepb.Span{
			TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			SpanId:            []byte{1, 2, 3, 4, 5, 6, 7, 8},
			Name:              name,
			StartTimeUnixNano: startN,
			EndTimeUnixNano:   endN,
		}
	}

	request := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: makeResourceAttributes()},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "test-scope"},
				Spans: []*tracepb.Span{
					span("unset-end", start, 0),                // end missing
					span("skewed", start, start-1_000_000),     // end < start
					span("normal", start, start+2_000_000_000), // +2s
				},
			}},
		}},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)
	traces, err := parser.ParseTraces(data)
	require.NoError(t, err)
	require.Len(t, traces, 3)

	byName := map[string]int64{}
	for _, tr := range traces {
		byName[tr.SpanName] = tr.Duration
	}
	assert.Equal(t, int64(0), byName["unset-end"], "unset end → 0, not wrapped")
	assert.Equal(t, int64(0), byName["skewed"], "end<start → 0, not wrapped")
	assert.Equal(t, int64(2_000_000_000), byName["normal"], "normal duration intact")
}

func TestParseTraces_InvalidData(t *testing.T) {
	parser := setupParserTest()

	// Parse invalid data
	_, err := parser.ParseTraces([]byte("invalid data"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal traces")
}

func TestParseTraces_EmptyRequest(t *testing.T) {
	parser := setupParserTest()

	request := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)

	traces, err := parser.ParseTraces(data)
	require.NoError(t, err)
	assert.Empty(t, traces)
}

// Metrics parsing tests

func TestParseMetrics_Sum(t *testing.T) {
	parser := setupParserTest()

	now := time.Now()
	request := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{
			{
				Resource: &resourcepb.Resource{
					Attributes: makeResourceAttributes(),
				},
				ScopeMetrics: []*metricspb.ScopeMetrics{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0.0",
						},
						Metrics: []*metricspb.Metric{
							{
								Name:        "test.counter",
								Description: "A test counter",
								Data: &metricspb.Metric_Sum{
									Sum: &metricspb.Sum{
										AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
										DataPoints: []*metricspb.NumberDataPoint{
											{
												TimeUnixNano: uint64(now.UnixNano()),
												Value: &metricspb.NumberDataPoint_AsDouble{
													AsDouble: 42.5,
												},
												Attributes: []*commonpb.KeyValue{
													{
														Key: "label",
														Value: &commonpb.AnyValue{
															Value: &commonpb.AnyValue_StringValue{StringValue: "value"},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)

	sums, gauges, histograms, _, err := parser.ParseMetrics(data)
	require.NoError(t, err)
	require.Len(t, sums, 1)
	assert.Empty(t, gauges)
	assert.Empty(t, histograms)

	sum := sums[0]
	assert.Equal(t, "test.counter", sum.MetricName)
	assert.Equal(t, "A test counter", sum.MetricDescription)
	assert.Equal(t, 42.5, sum.Value)
	assert.Equal(t, "test-service", sum.ServiceName)
	assert.Equal(t, "11111111-1111-4111-8111-111111111111", sum.AgentID)
}

func TestParseMetrics_Gauge(t *testing.T) {
	parser := setupParserTest()

	now := time.Now()
	request := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{
			{
				Resource: &resourcepb.Resource{
					Attributes: makeResourceAttributes(),
				},
				ScopeMetrics: []*metricspb.ScopeMetrics{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0.0",
						},
						Metrics: []*metricspb.Metric{
							{
								Name: "test.gauge",
								Data: &metricspb.Metric_Gauge{
									Gauge: &metricspb.Gauge{
										DataPoints: []*metricspb.NumberDataPoint{
											{
												TimeUnixNano: uint64(now.UnixNano()),
												Value: &metricspb.NumberDataPoint_AsDouble{
													AsDouble: 100.0,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)

	sums, gauges, histograms, _, err := parser.ParseMetrics(data)
	require.NoError(t, err)
	assert.Empty(t, sums)
	require.Len(t, gauges, 1)
	assert.Empty(t, histograms)

	gauge := gauges[0]
	assert.Equal(t, "test.gauge", gauge.MetricName)
	assert.Equal(t, 100.0, gauge.Value)
}

// TestParseMetrics_UnsupportedTypesDropped verifies that exponential-histogram
// and summary data points (which Squadron doesn't persist yet) are COUNTED as
// dropped rather than silently swallowed, while supported metrics still parse.
func TestParseMetrics_UnsupportedTypesDropped(t *testing.T) {
	parser := setupParserTest()
	now := time.Now()

	request := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{
			{
				Resource: &resourcepb.Resource{Attributes: makeResourceAttributes()},
				ScopeMetrics: []*metricspb.ScopeMetrics{
					{
						Scope: &commonpb.InstrumentationScope{Name: "test-scope"},
						Metrics: []*metricspb.Metric{
							{
								Name: "supported.counter",
								Data: &metricspb.Metric_Sum{
									Sum: &metricspb.Sum{
										DataPoints: []*metricspb.NumberDataPoint{
											{
												TimeUnixNano: uint64(now.UnixNano()),
												Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 1},
											},
										},
									},
								},
							},
							{
								Name: "dropped.exp_histogram",
								Data: &metricspb.Metric_ExponentialHistogram{
									ExponentialHistogram: &metricspb.ExponentialHistogram{
										DataPoints: []*metricspb.ExponentialHistogramDataPoint{
											{TimeUnixNano: uint64(now.UnixNano())},
											{TimeUnixNano: uint64(now.UnixNano())},
										},
									},
								},
							},
							{
								Name: "dropped.summary",
								Data: &metricspb.Metric_Summary{
									Summary: &metricspb.Summary{
										DataPoints: []*metricspb.SummaryDataPoint{
											{TimeUnixNano: uint64(now.UnixNano())},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)

	sums, gauges, histograms, dropped, err := parser.ParseMetrics(data)
	require.NoError(t, err)
	// Supported metric still lands.
	require.Len(t, sums, 1)
	assert.Empty(t, gauges)
	assert.Empty(t, histograms)
	// 2 exp-histogram points + 1 summary point counted as dropped, not silently
	// swallowed.
	assert.Equal(t, 3, dropped)
}

func TestParseMetrics_Histogram(t *testing.T) {
	parser := setupParserTest()

	now := time.Now()
	request := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{
			{
				Resource: &resourcepb.Resource{
					Attributes: makeResourceAttributes(),
				},
				ScopeMetrics: []*metricspb.ScopeMetrics{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0.0",
						},
						Metrics: []*metricspb.Metric{
							{
								Name: "test.histogram",
								Data: &metricspb.Metric_Histogram{
									Histogram: &metricspb.Histogram{
										DataPoints: []*metricspb.HistogramDataPoint{
											{
												TimeUnixNano:   uint64(now.UnixNano()),
												Count:          100,
												Sum:            proto.Float64(1000.0),
												BucketCounts:   []uint64{10, 20, 30, 40},
												ExplicitBounds: []float64{0.1, 0.5, 1.0},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)

	sums, gauges, histograms, _, err := parser.ParseMetrics(data)
	require.NoError(t, err)
	assert.Empty(t, sums)
	assert.Empty(t, gauges)
	require.Len(t, histograms, 1)

	histogram := histograms[0]
	assert.Equal(t, "test.histogram", histogram.MetricName)
	assert.Equal(t, uint64(100), histogram.Count)
	assert.Equal(t, 1000.0, histogram.Sum)
	assert.Len(t, histogram.BucketCounts, 4)
	assert.Len(t, histogram.ExplicitBounds, 3)
}

func TestParseMetrics_InvalidData(t *testing.T) {
	parser := setupParserTest()

	_, _, _, _, err := parser.ParseMetrics([]byte("invalid data"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal metrics")
}

// Logs parsing tests

func TestParseLogs_Success(t *testing.T) {
	parser := setupParserTest()

	now := time.Now()
	request := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: makeResourceAttributes(),
				},
				ScopeLogs: []*logspb.ScopeLogs{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0.0",
						},
						LogRecords: []*logspb.LogRecord{
							{
								TimeUnixNano:   uint64(now.UnixNano()),
								SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
								SeverityText:   "INFO",
								Body: &commonpb.AnyValue{
									Value: &commonpb.AnyValue_StringValue{StringValue: "test log message"},
								},
								Attributes: []*commonpb.KeyValue{
									{
										Key: "log.attr",
										Value: &commonpb.AnyValue{
											Value: &commonpb.AnyValue_StringValue{StringValue: "value"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)

	logs, err := parser.ParseLogs(data)
	require.NoError(t, err)
	require.Len(t, logs, 1)

	log := logs[0]
	assert.Equal(t, "INFO", log.SeverityText)
	assert.Equal(t, int32(logspb.SeverityNumber_SEVERITY_NUMBER_INFO), log.SeverityNumber)
	assert.Equal(t, "test log message", log.Body)
	assert.Equal(t, "test-service", log.ServiceName)
	assert.Equal(t, "11111111-1111-4111-8111-111111111111", log.AgentID)
	assert.Contains(t, log.LogAttributes, "log.attr")
}

func TestParseLogs_WithTraceContext(t *testing.T) {
	parser := setupParserTest()

	traceID := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := []byte{1, 2, 3, 4, 5, 6, 7, 8}

	now := time.Now()
	request := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{
				Resource: &resourcepb.Resource{
					Attributes: makeResourceAttributes(),
				},
				ScopeLogs: []*logspb.ScopeLogs{
					{
						Scope: &commonpb.InstrumentationScope{
							Name:    "test-scope",
							Version: "1.0.0",
						},
						LogRecords: []*logspb.LogRecord{
							{
								TimeUnixNano: uint64(now.UnixNano()),
								SeverityText: "INFO",
								Body: &commonpb.AnyValue{
									Value: &commonpb.AnyValue_StringValue{StringValue: "test log"},
								},
								TraceId: traceID,
								SpanId:  spanID,
							},
						},
					},
				},
			},
		},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)

	logs, err := parser.ParseLogs(data)
	require.NoError(t, err)
	require.Len(t, logs, 1)

	log := logs[0]
	assert.NotEmpty(t, log.TraceId)
	assert.NotEmpty(t, log.SpanId)
}

func TestParseLogs_InvalidData(t *testing.T) {
	parser := setupParserTest()

	_, err := parser.ParseLogs([]byte("invalid data"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to unmarshal logs")
}

func TestParseLogs_EmptyRequest(t *testing.T) {
	parser := setupParserTest()

	request := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{},
	}

	data, err := proto.Marshal(request)
	require.NoError(t, err)

	logs, err := parser.ParseLogs(data)
	require.NoError(t, err)
	assert.Empty(t, logs)
}

// TestParse_NilSafety proves the parser survives non-compliant OTLP payloads
// (nil Resource, nil Scope, nil KeyValue, nil attribute Value, nil ArrayValue)
// without panicking. Before the nil-safety hardening these crashed the ingest
// worker, which has no recover() — a single malformed client could take down
// telemetry ingest.
func TestParse_NilSafety(t *testing.T) {
	p := setupParserTest()

	badAttrs := []*commonpb.KeyValue{
		nil,                    // nil KeyValue
		{Key: "k", Value: nil}, // nil Value
		{Key: "", Value: nil},  // empty key
		{Key: "arr", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: nil}}},  // nil ArrayValue
		{Key: "kv", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: nil}}}, // nil KvlistValue
	}

	traceReq := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{Resource: nil, ScopeSpans: []*tracepb.ScopeSpans{
				{Scope: nil, Spans: []*tracepb.Span{{Name: "s", Attributes: badAttrs}}},
			}},
		},
	}
	tb, err := proto.Marshal(traceReq)
	require.NoError(t, err)
	require.NotPanics(t, func() {
		_, err := p.ParseTraces(tb)
		assert.NoError(t, err)
	})

	metricReq := &colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{
			{Resource: nil, ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: nil}}},
		},
	}
	mb, err := proto.Marshal(metricReq)
	require.NoError(t, err)
	require.NotPanics(t, func() {
		_, _, _, _, err := p.ParseMetrics(mb)
		assert.NoError(t, err)
	})

	logReq := &collogspb.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{
			{Resource: nil, ScopeLogs: []*logspb.ScopeLogs{
				{Scope: nil, LogRecords: []*logspb.LogRecord{{Attributes: badAttrs}}},
			}},
		},
	}
	lb, err := proto.Marshal(logReq)
	require.NoError(t, err)
	require.NotPanics(t, func() {
		_, err := p.ParseLogs(lb)
		assert.NoError(t, err)
	})
}
