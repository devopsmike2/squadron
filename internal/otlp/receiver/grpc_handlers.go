package receiver

import (
	"context"
	"time"

	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/worker"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TraceService implements the OTLP Trace Service gRPC interface
type TraceService struct {
	coltracepb.UnimplementedTraceServiceServer
	logger     *zap.Logger
	metrics    *metrics.OTLPMetrics
	workerPool *worker.Pool
	// traceIndex is the slice-1 chunk-2 wire-up — same shape as the
	// HTTPServer field. Nil disables the dispatch path cleanly.
	traceIndex TraceObserver
	// qualityIndex is the span-quality slice-1 chunk-1 wire-up,
	// mirroring HTTPServer.qualityIndex. Nil disables cleanly.
	qualityIndex QualityObserver
	// tenant is the ADR 0012 §1 ingest tenant this instance binds all
	// OTLP-ingested items to. Empty => the worker stamps DefaultTenant.
	// Set from ingest.otlp.tenant_id via SetTenant (setter style keeps
	// NewTraceService binary-compatible, matching SetTraceIndex).
	tenant string
}

// SetTenant binds this service's submitted WorkItems to the ADR 0012 §1
// ingest tenant. Empty leaves the worker to stamp DefaultTenant (inert in
// OSS). Mirrors SetTraceIndex so cmd/all-in-one wires from one call site.
func (s *TraceService) SetTenant(tenant string) { s.tenant = tenant }

// SetTraceIndex wires the traceindex Observer onto the gRPC trace
// service. Mirrors HTTPServer.SetTraceIndex so the chunk-2 wiring
// in cmd/all-in-one can hand the same Index to both transports.
func (s *TraceService) SetTraceIndex(idx TraceObserver) {
	s.traceIndex = idx
}

// SetQualityIndex wires the span-quality Observer onto the gRPC
// trace service. Mirrors HTTPServer.SetQualityIndex so the chunk-1
// wiring in cmd/all-in-one can hand the same Quality to both
// transports.
func (s *TraceService) SetQualityIndex(qual QualityObserver) {
	s.qualityIndex = qual
}

// MetricsService implements the OTLP Metrics Service gRPC interface
type MetricsService struct {
	colmetricspb.UnimplementedMetricsServiceServer
	logger     *zap.Logger
	metrics    *metrics.OTLPMetrics
	workerPool *worker.Pool
	// tenant — see TraceService.tenant (ADR 0012 §1). Set via SetTenant.
	tenant string
}

// SetTenant binds this service's submitted WorkItems to the ADR 0012 §1
// ingest tenant. Empty => DefaultTenant (inert in OSS).
func (s *MetricsService) SetTenant(tenant string) { s.tenant = tenant }

// LogsService implements the OTLP Logs Service gRPC interface
type LogsService struct {
	collogspb.UnimplementedLogsServiceServer
	logger     *zap.Logger
	metrics    *metrics.OTLPMetrics
	workerPool *worker.Pool
	// tenant — see TraceService.tenant (ADR 0012 §1). Set via SetTenant.
	tenant string
}

// SetTenant binds this service's submitted WorkItems to the ADR 0012 §1
// ingest tenant. Empty => DefaultTenant (inert in OSS).
func (s *LogsService) SetTenant(tenant string) { s.tenant = tenant }

// NewTraceService creates a new TraceService instance
func NewTraceService(metricsInstance *metrics.OTLPMetrics, workerPool *worker.Pool, logger *zap.Logger) *TraceService {
	return &TraceService{
		logger:     logger,
		metrics:    metricsInstance,
		workerPool: workerPool,
	}
}

// NewMetricsService creates a new MetricsService instance
func NewMetricsService(metricsInstance *metrics.OTLPMetrics, workerPool *worker.Pool, logger *zap.Logger) *MetricsService {
	return &MetricsService{
		logger:     logger,
		metrics:    metricsInstance,
		workerPool: workerPool,
	}
}

// NewLogsService creates a new LogsService instance
func NewLogsService(metricsInstance *metrics.OTLPMetrics, workerPool *worker.Pool, logger *zap.Logger) *LogsService {
	return &LogsService{
		logger:     logger,
		metrics:    metricsInstance,
		workerPool: workerPool,
	}
}

// Export handles trace export requests via gRPC
func (s *TraceService) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	start := time.Now()
	s.logger.Debug("Processing gRPC trace export request",
		zap.Int("resource_spans_count", len(req.ResourceSpans)))

	// Track gRPC request
	if s.metrics != nil {
		s.metrics.GRPCRequestsTotal.Inc(1)
	}

	// Slice 1 chunk 2 (#706 Stream 104) — fan the per-ResourceSpan
	// observation out to the traceindex BEFORE the worker pool
	// dispatch, matching the HTTP handler's ordering. See
	// http_server.go::handleOTLPTraces for the rationale (observation
	// should land even when the queue submit subsequently fails).
	if s.traceIndex != nil {
		observeResourceSpans(ctx, s.traceIndex, req.ResourceSpans, start)
	}
	// Span-quality slice-1 chunk-1 (#716 Stream 114) — runs after
	// the traceindex pass. Same nil-guard pattern as the HTTP path.
	if s.qualityIndex != nil {
		observeQualitySpans(s.qualityIndex, req.ResourceSpans)
	}

	// Serialize the request to protobuf bytes
	data, err := proto.Marshal(req)
	if err != nil {
		s.logger.Error("Failed to marshal trace request", zap.Error(err))
		if s.metrics != nil {
			s.metrics.GRPCRequestErrors.Inc(1)
			s.metrics.TracesErrors.Inc(1)
		}
		return &coltracepb.ExportTraceServiceResponse{
			PartialSuccess: &coltracepb.ExportTracePartialSuccess{
				RejectedSpans: int64(countSpans(req.ResourceSpans)),
				ErrorMessage:  "Failed to serialize request",
			},
		}, nil
	}

	// Track bytes received
	if s.metrics != nil {
		s.metrics.TraceBytes.Inc(int64(len(data)))
	}

	// Track received traces. The counter measures SPANS ("trace spans
	// received" per the metric help), not ResourceSpans containers — a batch
	// with one ResourceSpans holding 100 spans is 100, not 1.
	if s.metrics != nil {
		s.metrics.TracesReceived.Inc(int64(countSpans(req.ResourceSpans)))
	}

	// Submit raw bytes to worker pool for async processing
	item := worker.WorkItem{
		Type:      worker.WorkItemTypeTraces,
		RawData:   data,
		Timestamp: time.Now(),
		Tenant:    s.tenant, // ADR 0012 §1 — bind ingest to the configured tenant
	}

	if err := s.workerPool.Submit(item); err != nil {
		s.logger.Error("Failed to queue traces", zap.Error(err))
		if s.metrics != nil {
			s.metrics.StorageWriteErrors.Inc(1)
			s.metrics.TracesErrors.Inc(1)
		}
		return &coltracepb.ExportTraceServiceResponse{
			PartialSuccess: &coltracepb.ExportTracePartialSuccess{
				RejectedSpans: int64(countSpans(req.ResourceSpans)),
				ErrorMessage:  "Queue full",
			},
		}, nil
	}

	// Track queued traces (span count, per the metric help — see above).
	if s.metrics != nil {
		s.metrics.TracesProcessed.Inc(int64(countSpans(req.ResourceSpans)))
	}

	duration := time.Since(start)
	s.logger.Debug("Successfully queued trace export request",
		zap.Int("resource_spans_count", len(req.ResourceSpans)),
		zap.Int("bytes", len(data)),
		zap.Duration("duration", duration))

	// Track request duration
	if s.metrics != nil {
		s.metrics.GRPCRequestDuration.Record(duration)
		s.metrics.TraceProcessDuration.Record(duration)
	}

	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// Export handles metrics export requests via gRPC
func (s *MetricsService) Export(ctx context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	start := time.Now()
	s.logger.Debug("Processing gRPC metrics export request",
		zap.Int("resource_metrics_count", len(req.ResourceMetrics)))

	// Serialize the request to protobuf bytes
	data, err := proto.Marshal(req)
	if err != nil {
		s.logger.Error("Failed to marshal metrics request", zap.Error(err))
		return &colmetricspb.ExportMetricsServiceResponse{
			PartialSuccess: &colmetricspb.ExportMetricsPartialSuccess{
				RejectedDataPoints: int64(countMetricDataPoints(req.ResourceMetrics)),
				ErrorMessage:       "Failed to serialize request",
			},
		}, nil
	}

	// Submit raw bytes to worker pool for async processing
	item := worker.WorkItem{
		Type:      worker.WorkItemTypeMetrics,
		RawData:   data,
		Timestamp: time.Now(),
		Tenant:    s.tenant, // ADR 0012 §1 — bind ingest to the configured tenant
	}

	if err := s.workerPool.Submit(item); err != nil {
		s.logger.Error("Failed to queue metrics", zap.Error(err))
		return &colmetricspb.ExportMetricsServiceResponse{
			PartialSuccess: &colmetricspb.ExportMetricsPartialSuccess{
				RejectedDataPoints: int64(countMetricDataPoints(req.ResourceMetrics)),
				ErrorMessage:       "Queue full",
			},
		}, nil
	}

	duration := time.Since(start)
	s.logger.Debug("Successfully queued metrics export request",
		zap.Int("resource_metrics_count", len(req.ResourceMetrics)),
		zap.Int("bytes", len(data)),
		zap.Duration("duration", duration))

	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

// Export handles logs export requests via gRPC
func (s *LogsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	start := time.Now()
	s.logger.Debug("Processing gRPC logs export request",
		zap.Int("resource_logs_count", len(req.ResourceLogs)))

	// Serialize the request to protobuf bytes
	data, err := proto.Marshal(req)
	if err != nil {
		s.logger.Error("Failed to marshal logs request", zap.Error(err))
		return &collogspb.ExportLogsServiceResponse{
			PartialSuccess: &collogspb.ExportLogsPartialSuccess{
				RejectedLogRecords: int64(countLogRecords(req.ResourceLogs)),
				ErrorMessage:       "Failed to serialize request",
			},
		}, nil
	}

	// Submit raw bytes to worker pool for async processing
	item := worker.WorkItem{
		Type:      worker.WorkItemTypeLogs,
		RawData:   data,
		Timestamp: time.Now(),
		Tenant:    s.tenant, // ADR 0012 §1 — bind ingest to the configured tenant
	}

	if err := s.workerPool.Submit(item); err != nil {
		s.logger.Error("Failed to queue logs", zap.Error(err))
		return &collogspb.ExportLogsServiceResponse{
			PartialSuccess: &collogspb.ExportLogsPartialSuccess{
				RejectedLogRecords: int64(countLogRecords(req.ResourceLogs)),
				ErrorMessage:       "Queue full",
			},
		}, nil
	}

	duration := time.Since(start)
	s.logger.Debug("Successfully queued logs export request",
		zap.Int("resource_logs_count", len(req.ResourceLogs)),
		zap.Int("bytes", len(data)),
		zap.Duration("duration", duration))

	return &collogspb.ExportLogsServiceResponse{}, nil
}

// countSpans counts the total number of spans across the resource spans,
// walking ResourceSpans → ScopeSpans → Spans. The received/processed counters
// (help: "trace spans received/processed") and the OTLP
// PartialSuccess.RejectedSpans field are span counts by definition — NOT
// ResourceSpans-container counts. Mirrors countMetricDataPoints /
// countLogRecords, which the metrics and logs paths already use; the traces
// path was the sole holdout still reporting len(ResourceSpans), which
// undercounts throughput by the batch's spans-per-resource factor.
func countSpans(resourceSpans []*tracepb.ResourceSpans) int {
	count := 0
	for _, rs := range resourceSpans {
		for _, ss := range rs.ScopeSpans {
			count += len(ss.Spans)
		}
	}
	return count
}

// countMetricDataPoints counts the total number of data points in resource metrics
func countMetricDataPoints(resourceMetrics []*metricspb.ResourceMetrics) int {
	count := 0
	for _, rm := range resourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch data := m.Data.(type) {
				case *metricspb.Metric_Gauge:
					count += len(data.Gauge.DataPoints)
				case *metricspb.Metric_Sum:
					count += len(data.Sum.DataPoints)
				case *metricspb.Metric_Histogram:
					count += len(data.Histogram.DataPoints)
				case *metricspb.Metric_ExponentialHistogram:
					count += len(data.ExponentialHistogram.DataPoints)
				case *metricspb.Metric_Summary:
					count += len(data.Summary.DataPoints)
				}
			}
		}
	}
	return count
}

// countLogRecords counts the total number of log records in resource logs
func countLogRecords(resourceLogs []*logspb.ResourceLogs) int {
	count := 0
	for _, rl := range resourceLogs {
		for _, sl := range rl.ScopeLogs {
			count += len(sl.LogRecords)
		}
	}
	return count
}
