package parser

import (
	"bytes"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/agentid"
	"github.com/devopsmike2/squadron/internal/otlp"
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

// OTLPParser handles parsing of OTLP protobuf data
type OTLPParser struct {
	logger *zap.Logger
}

// OTLP data structures for async processing
type OTLPTracesData struct {
	Traces []otlp.TraceData
}

type OTLPMetricsData struct {
	Sums       []otlp.MetricSumData
	Gauges     []otlp.MetricGaugeData
	Histograms []otlp.MetricHistogramData
}

type OTLPLogsData struct {
	Logs []otlp.LogData
}

// NewOTLPParser creates a new OTLP parser
func NewOTLPParser(logger *zap.Logger) *OTLPParser {
	return &OTLPParser{
		logger: logger,
	}
}

// ParseTraces parses OTLP traces data
func (p *OTLPParser) ParseTraces(data []byte) ([]otlp.TraceData, error) {
	var request coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(data, &request); err != nil {
		return nil, fmt.Errorf("failed to unmarshal traces: %w", err)
	}

	var traces []otlp.TraceData
	for _, resourceSpans := range request.ResourceSpans {
		resource := resourceSpans.Resource
		resourceAttrs := resourceAttributes(resource)

		// Extract agent ID from resource attributes
		extractedAgentID := agentid.Derive(resourceAttrs)

		for _, scopeSpans := range resourceSpans.ScopeSpans {
			scope := scopeSpans.Scope
			if scope == nil {
				// Scope is optional; substitute an empty one so direct field
				// reads (Name/Version) below are nil-safe.
				scope = &commonpb.InstrumentationScope{}
			}
			scopeAttrs := scopeAttributes(scope)

			for _, span := range scopeSpans.Spans {
				spanAttrs := attributesToMap(span.Attributes)

				// Convert events
				events := make([]otlp.EventData, len(span.Events))
				for i, event := range span.Events {
					events[i] = otlp.EventData{
						Name:       event.Name,
						Timestamp:  time.Unix(0, int64(event.TimeUnixNano)),
						Attributes: attributesToMap(event.Attributes),
					}
				}

				// Convert links
				links := make([]otlp.LinkData, len(span.Links))
				for i, link := range span.Links {
					links[i] = otlp.LinkData{
						TraceId:    formatTraceID(link.TraceId),
						SpanId:     formatSpanID(link.SpanId),
						TraceState: link.TraceState,
						Attributes: attributesToMap(link.Attributes),
					}
				}

				// Extract group information from resource attributes
				groupID, groupName := extractGroupInfo(resourceAttrs)

				// Convert span to trace data
				traceData := otlp.TraceData{
					Timestamp:          time.Unix(0, int64(span.StartTimeUnixNano)),
					TraceId:            formatTraceID(span.TraceId),
					SpanId:             formatSpanID(span.SpanId),
					ParentSpanId:       formatSpanID(span.ParentSpanId),
					TraceState:         span.TraceState,
					SpanName:           span.Name,
					SpanKind:           int32(span.Kind),
					ServiceName:        getServiceName(resourceAttrs),
					ResourceAttributes: resourceAttrs,
					ScopeName:          scope.Name,
					ScopeVersion:       scope.Version,
					ScopeAttributes:    scopeAttrs,
					SpanAttributes:     spanAttrs,
					Duration:           int64(span.EndTimeUnixNano - span.StartTimeUnixNano),
					StatusCode:         getStatusCode(span.Status),
					StatusMessage:      getStatusMessage(span.Status),
					Events:             events,
					Links:              links,
					AgentID:            extractedAgentID,
					GroupID:            groupID,
					GroupName:          groupName,
				}
				traces = append(traces, traceData)
			}
		}
	}

	return traces, nil
}

// ParseMetrics parses OTLP metrics data
// ParseMetrics decodes an OTLP metrics payload into the sum/gauge/histogram
// shapes Squadron persists. It also returns the number of data points DROPPED
// because their metric type isn't persisted yet (exponential histograms,
// summaries, or any unknown/unset type). Those points used to vanish silently
// while still counting toward "metrics received"; returning the count lets the
// worker surface it as an alertable metric instead of silent data loss.
func (p *OTLPParser) ParseMetrics(data []byte) ([]otlp.MetricSumData, []otlp.MetricGaugeData, []otlp.MetricHistogramData, int, error) {
	var request colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(data, &request); err != nil {
		return nil, nil, nil, 0, fmt.Errorf("failed to unmarshal metrics: %w", err)
	}

	var sumMetrics []otlp.MetricSumData
	var gaugeMetrics []otlp.MetricGaugeData
	var histogramMetrics []otlp.MetricHistogramData

	// Track data points we can't persist yet so the loss is visible (log +
	// returned count) rather than silent.
	droppedUnsupported := 0
	droppedByType := map[string]int{}

	for _, resourceMetrics := range request.ResourceMetrics {
		resource := resourceMetrics.Resource
		resourceAttrs := resourceAttributes(resource)

		// Extract agent ID from resource attributes
		extractedAgentID := agentid.Derive(resourceAttrs)

		for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
			scope := scopeMetrics.Scope
			if scope == nil {
				scope = &commonpb.InstrumentationScope{}
			}
			scopeAttrs := scopeAttributes(scope)

			for _, metric := range scopeMetrics.Metrics {
				serviceName := getServiceName(resourceAttrs)
				groupID, groupName := extractGroupInfo(resourceAttrs)

				// Process different metric types
				switch metric.Data.(type) {
				case *metricspb.Metric_Sum:
					sum := metric.GetSum()
					for _, dp := range sum.DataPoints {
						attrs := attributesToMap(dp.Attributes)
						metricData := otlp.MetricSumData{
							ResourceAttributes:     resourceAttrs,
							ResourceSchemaUrl:      "",
							ScopeName:              scope.Name,
							ScopeVersion:           scope.Version,
							ScopeAttributes:        scopeAttrs,
							ScopeDroppedAttrCount:  scope.DroppedAttributesCount,
							ScopeSchemaUrl:         "",
							ServiceName:            serviceName,
							MetricName:             metric.Name,
							MetricDescription:      metric.Description,
							MetricUnit:             metric.Unit,
							Attributes:             attrs,
							StartTimeUnix:          time.Unix(0, int64(dp.StartTimeUnixNano)),
							TimeUnix:               time.Unix(0, int64(dp.TimeUnixNano)),
							Value:                  getNumberDataPointValue(dp),
							Flags:                  uint32(dp.Flags),
							AggregationTemporality: int32(sum.AggregationTemporality),
							IsMonotonic:            sum.IsMonotonic,
							AgentID:                extractedAgentID,
							GroupID:                groupID,
							GroupName:              groupName,
						}
						sumMetrics = append(sumMetrics, metricData)
					}

				case *metricspb.Metric_Gauge:
					gauge := metric.GetGauge()
					for _, dp := range gauge.DataPoints {
						attrs := attributesToMap(dp.Attributes)
						metricData := otlp.MetricGaugeData{
							ResourceAttributes:     resourceAttrs,
							ResourceSchemaUrl:      "",
							ScopeName:              scope.Name,
							ScopeVersion:           scope.Version,
							ScopeAttributes:        scopeAttrs,
							ScopeDroppedAttrCount:  scope.DroppedAttributesCount,
							ScopeSchemaUrl:         "",
							ServiceName:            serviceName,
							MetricName:             metric.Name,
							MetricDescription:      metric.Description,
							MetricUnit:             metric.Unit,
							Attributes:             attrs,
							StartTimeUnix:          time.Unix(0, int64(dp.StartTimeUnixNano)),
							TimeUnix:               time.Unix(0, int64(dp.TimeUnixNano)),
							Value:                  getNumberDataPointValue(dp),
							Flags:                  uint32(dp.Flags),
							AggregationTemporality: 0,     // Not applicable for gauge
							IsMonotonic:            false, // Gauges are not monotonic
							AgentID:                extractedAgentID,
							GroupID:                groupID,
							GroupName:              groupName,
						}
						gaugeMetrics = append(gaugeMetrics, metricData)
					}

				case *metricspb.Metric_Histogram:
					histogram := metric.GetHistogram()
					for _, dp := range histogram.DataPoints {
						attrs := attributesToMap(dp.Attributes)
						metricData := otlp.MetricHistogramData{
							ResourceAttributes:     resourceAttrs,
							ResourceSchemaUrl:      "",
							ScopeName:              scope.Name,
							ScopeVersion:           scope.Version,
							ScopeAttributes:        scopeAttrs,
							ScopeDroppedAttrCount:  scope.DroppedAttributesCount,
							ScopeSchemaUrl:         "",
							ServiceName:            serviceName,
							MetricName:             metric.Name,
							MetricDescription:      metric.Description,
							MetricUnit:             metric.Unit,
							Attributes:             attrs,
							StartTimeUnix:          time.Unix(0, int64(dp.StartTimeUnixNano)),
							TimeUnix:               time.Unix(0, int64(dp.TimeUnixNano)),
							Count:                  dp.Count,
							Sum:                    dp.GetSum(),
							BucketCounts:           dp.BucketCounts,
							ExplicitBounds:         dp.ExplicitBounds,
							Flags:                  uint32(dp.Flags),
							Min:                    dp.GetMin(),
							Max:                    dp.GetMax(),
							AggregationTemporality: int32(histogram.AggregationTemporality),
							AgentID:                extractedAgentID,
							GroupID:                groupID,
							GroupName:              groupName,
						}
						histogramMetrics = append(histogramMetrics, metricData)
					}

				case *metricspb.Metric_ExponentialHistogram:
					// Not persisted yet (no exp-histogram storage table). Count
					// the points so the drop is visible instead of silent —
					// exponential histograms are the default for several modern
					// OTel SDKs, so this is real data if a customer uses them.
					if eh := metric.GetExponentialHistogram(); eh != nil {
						n := len(eh.DataPoints)
						droppedUnsupported += n
						droppedByType["exponential_histogram"] += n
					}

				case *metricspb.Metric_Summary:
					// Not persisted yet (Prometheus-compat summaries).
					if sm := metric.GetSummary(); sm != nil {
						n := len(sm.DataPoints)
						droppedUnsupported += n
						droppedByType["summary"] += n
					}

				default:
					// Unknown/unset metric type — malformed payload or a future
					// OTLP kind. Count it so it isn't silently swallowed.
					droppedUnsupported++
					droppedByType["unknown"]++
				}
			}
		}
	}

	if droppedUnsupported > 0 {
		p.logger.Warn("dropped unsupported OTLP metric data points (type not persisted yet)",
			zap.Int("dropped_data_points", droppedUnsupported),
			zap.Any("by_type", droppedByType))
	}

	return sumMetrics, gaugeMetrics, histogramMetrics, droppedUnsupported, nil
}

// ParseLogs parses OTLP logs data
func (p *OTLPParser) ParseLogs(data []byte) ([]otlp.LogData, error) {
	var request collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(data, &request); err != nil {
		return nil, fmt.Errorf("failed to unmarshal logs: %w", err)
	}

	var logs []otlp.LogData
	for _, resourceLogs := range request.ResourceLogs {
		resource := resourceLogs.Resource
		resourceAttrs := resourceAttributes(resource)

		// Extract agent ID from resource attributes
		extractedAgentID := agentid.Derive(resourceAttrs)

		for _, scopeLogs := range resourceLogs.ScopeLogs {
			scope := scopeLogs.Scope
			if scope == nil {
				scope = &commonpb.InstrumentationScope{}
			}
			scopeAttrs := scopeAttributes(scope)

			// Extract group information from resource attributes
			groupID, groupName := extractGroupInfo(resourceAttrs)

			for _, logRecord := range scopeLogs.LogRecords {
				logAttrs := attributesToMap(logRecord.Attributes)

				// Convert log record to storage format
				logData := otlp.LogData{
					Timestamp:          time.Unix(0, int64(logRecord.TimeUnixNano)),
					TraceId:            formatTraceID(logRecord.TraceId),
					SpanId:             formatSpanID(logRecord.SpanId),
					TraceFlags:         uint32(logRecord.Flags),
					SeverityText:       logRecord.SeverityText,
					SeverityNumber:     int32(logRecord.SeverityNumber),
					ServiceName:        getServiceName(resourceAttrs),
					Body:               getLogBody(logRecord),
					ResourceSchemaUrl:  "",
					ResourceAttributes: resourceAttrs,
					ScopeSchemaUrl:     "",
					ScopeName:          scope.Name,
					ScopeVersion:       scope.Version,
					ScopeAttributes:    scopeAttrs,
					LogAttributes:      logAttrs,
					AgentID:            extractedAgentID,
					GroupID:            groupID,
					GroupName:          groupName,
				}
				logs = append(logs, logData)
			}
		}
	}

	return logs, nil
}

// Helper functions

// resourceAttributes returns the flattened attribute map for a resource,
// tolerating a nil resource. Per the OTLP spec a ResourceSpans/Metrics/Logs
// may carry a nil Resource (metadata-only or partial batches from
// non-compliant exporters); the receiver already skips these, so the parser
// must not panic dereferencing resource.Attributes.
func resourceAttributes(r *resourcepb.Resource) map[string]string {
	if r == nil {
		return map[string]string{}
	}
	return attributesToMap(r.Attributes)
}

// scopeAttributes returns the flattened attribute map for an instrumentation
// scope, tolerating a nil scope (Scope is optional on every Scope* container).
func scopeAttributes(s *commonpb.InstrumentationScope) map[string]string {
	if s == nil {
		return map[string]string{}
	}
	return attributesToMap(s.Attributes)
}

func attributesToMap(attrs []*commonpb.KeyValue) map[string]string {
	result := make(map[string]string)
	for _, attr := range attrs {
		// A nil KeyValue, empty key, or nil Value can all appear in
		// non-compliant payloads; skip rather than panic. getAttributeValue
		// also guards nil defensively.
		if attr == nil || attr.Key == "" {
			continue
		}
		result[attr.Key] = getAttributeValue(attr.Value)
	}
	return result
}

func getAttributeValue(value *commonpb.AnyValue) string {
	if value == nil {
		return ""
	}
	switch v := value.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return v.StringValue
	case *commonpb.AnyValue_BoolValue:
		if v.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprintf("%d", v.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%f", v.DoubleValue)
	case *commonpb.AnyValue_ArrayValue:
		// Convert array to string representation. ArrayValue itself can be nil
		// in a malformed payload even though the oneof arm is set.
		if v.ArrayValue == nil {
			return "[]"
		}
		var items []string
		for _, item := range v.ArrayValue.Values {
			items = append(items, getAttributeValue(item))
		}
		return fmt.Sprintf("[%s]", joinStrings(items, ","))
	case *commonpb.AnyValue_KvlistValue:
		// Convert key-value list to string representation (nil-guarded).
		if v.KvlistValue == nil {
			return "{}"
		}
		var pairs []string
		for _, kv := range v.KvlistValue.Values {
			if kv == nil {
				continue
			}
			pairs = append(pairs, fmt.Sprintf("%s=%s", kv.Key, getAttributeValue(kv.Value)))
		}
		return fmt.Sprintf("{%s}", joinStrings(pairs, ","))
	default:
		return ""
	}
}

func getServiceName(attrs map[string]string) string {
	if serviceName, exists := attrs["service.name"]; exists {
		return serviceName
	}
	return "unknown-service"
}

// extractGroupInfo extracts group ID and name from resource attributes
// OSS version: simplified without backend group resolution
func extractGroupInfo(attrs map[string]string) (groupID, groupName string) {
	// Check if we have a group_id in the attributes
	if id, exists := attrs["agent.group_id"]; exists && id != "" {
		if len(id) >= 8 && len(id) <= 128 {
			groupID = id
		}
	}

	// Check if we have a group_name for display
	if name, exists := attrs["agent.group_name"]; exists && name != "" {
		if len(name) <= 128 && isValidGroupName(name) {
			groupName = name
		}
	}

	return groupID, groupName
}

// isValidGroupName validates group name according to constraints
func isValidGroupName(name string) bool {
	if name == "" {
		return false
	}

	// Check for valid characters: [a-zA-Z0-9\-_.]
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}

	return true
}

func formatTraceID(traceID []byte) string {
	if len(traceID) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", traceID)
}

func formatSpanID(spanID []byte) string {
	if len(spanID) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", spanID)
}

func getLogBody(logRecord *logspb.LogRecord) string {
	if logRecord.Body != nil {
		switch body := logRecord.Body.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			return body.StringValue
		case *commonpb.AnyValue_BoolValue:
			if body.BoolValue {
				return "true"
			}
			return "false"
		case *commonpb.AnyValue_IntValue:
			return fmt.Sprintf("%d", body.IntValue)
		case *commonpb.AnyValue_DoubleValue:
			return fmt.Sprintf("%f", body.DoubleValue)
		}
	}
	return ""
}

func joinStrings(strs []string, sep string) string {
	if len(strs) == 0 {
		return ""
	}
	if len(strs) == 1 {
		return strs[0]
	}

	var buf bytes.Buffer
	buf.WriteString(strs[0])
	for i := 1; i < len(strs); i++ {
		buf.WriteString(sep)
		buf.WriteString(strs[i])
	}
	return buf.String()
}

// getNumberDataPointValue extracts the value from a NumberDataPoint
func getNumberDataPointValue(dp *metricspb.NumberDataPoint) float64 {
	if dp == nil {
		return 0.0
	}

	switch v := dp.Value.(type) {
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt)
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble
	case nil:
		return 0.0
	default:
		return 0.0
	}
}

// getStatusCode safely extracts status code from span status
func getStatusCode(status *tracepb.Status) string {
	if status == nil {
		return "STATUS_CODE_UNSET"
	}
	return status.Code.String()
}

// getStatusMessage safely extracts status message from span status
func getStatusMessage(status *tracepb.Status) string {
	if status == nil {
		return ""
	}
	return status.Message
}
