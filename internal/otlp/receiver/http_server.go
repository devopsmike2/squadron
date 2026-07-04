package receiver

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/traceindex"
	"github.com/devopsmike2/squadron/internal/worker"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// TraceObserver is the minimal surface the receiver needs from the
// traceindex Index for the slice-1 chunk-2 wiring. The full Index
// type satisfies it; tests inject a recording fake. Keeping the
// interface local to the receiver (rather than reaching into
// traceindex.Index directly everywhere) means a future evolution of
// the Index — additional return values on Observe, batching, etc. —
// doesn't ripple through the receiver test suite.
type TraceObserver interface {
	Observe(ctx context.Context, obs traceindex.ResourceObservation)
}

// HTTPServer represents the HTTP OTLP receiver server
type HTTPServer struct {
	server     *http.Server
	logger     *zap.Logger
	metrics    *metrics.OTLPMetrics
	port       int
	workerPool *worker.Pool
	// traceIndex is the slice-1 chunk-2 wire-up. Nil is the
	// disabled-mode sentinel — handleOTLPTraces guards on it so the
	// SQUADRON_TRACEINDEX_DISABLED=true deployment path runs the
	// receiver unchanged. The hot-path Observe call must NOT block on
	// IO (design doc §5 + the chunk-2 prompt's constraint): the Index
	// is in-memory only, the background flusher handles the SQLite
	// transaction.
	traceIndex TraceObserver
	// qualityIndex is the span-quality slice-1 chunk-1 wire-up,
	// running side-by-side with traceIndex on the hot path. Same
	// nil-guard pattern — SQUADRON_SPANQUALITY_DISABLED leaves this
	// field zero and the handler skips the quality pass cleanly.
	qualityIndex QualityObserver
}

// NewHTTPServer creates a new HTTP server instance
func NewHTTPServer(port int, metricsInstance *metrics.OTLPMetrics, workerPool *worker.Pool, logger *zap.Logger) (*HTTPServer, error) {
	// Set Gin to release mode for better performance
	gin.SetMode(gin.ReleaseMode)

	// Create HTTP server
	s := &HTTPServer{
		logger:     logger,
		metrics:    metricsInstance,
		port:       port,
		workerPool: workerPool,
	}

	// Create Gin router
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(s.corsMiddleware())

	// Setup routes
	s.setupRoutes(router)

	// Create HTTP server
	s.server = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      router,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return s, nil
}

// SetTraceIndex wires the slice-1 chunk-2 traceindex Observer so the
// HTTP trace handler can fan out ResourceSpan-level observations to
// the in-memory index after unmarshal. A nil argument disables the
// dispatch path cleanly — the handler's existing nil-check keeps the
// SQUADRON_TRACEINDEX_DISABLED=true deployment shape working without
// special-casing in the constructor. The setter style (rather than a
// new constructor parameter) preserves binary compat with existing
// callers and matches the SetX accessor pattern the api server uses.
func (s *HTTPServer) SetTraceIndex(idx TraceObserver) {
	s.traceIndex = idx
}

// SetQualityIndex wires the span-quality slice-1 chunk-1 observer.
// Mirrors SetTraceIndex — nil disables the dispatch path cleanly so
// SQUADRON_SPANQUALITY_DISABLED=true at deploy keeps the receiver
// path untouched. The setter style preserves binary compat the same
// way SetTraceIndex does.
func (s *HTTPServer) SetQualityIndex(qual QualityObserver) {
	s.qualityIndex = qual
}

// setupRoutes configures the HTTP server with routes
func (s *HTTPServer) setupRoutes(router *gin.Engine) {
	// Health check endpoints
	router.GET("/health", s.healthCheck)
	router.GET("/ready", s.readyCheck)

	// Standard OTLP HTTP endpoints. The metrics middleware wires the
	// otlp_http_* request counters/histogram that were declared in
	// metrics.OTLPMetrics but never recorded — the v0.89 ingest stress
	// run surfaced them reading zero under 12k+ requests. Data routes
	// only; health probes would just add noise.
	otlp := router.Group("/v1", s.httpMetricsMiddleware())
	otlp.POST("/traces", s.handleOTLPTraces)
	otlp.POST("/metrics", s.handleOTLPMetrics)
	otlp.POST("/logs", s.handleOTLPLogs)

	s.logger.Info("OTLP HTTP routes registered")
}

// httpMetricsMiddleware records the HTTP receiver's request count,
// error count, and duration histogram. A 503 (worker queue full) is
// the designed backpressure signal and counts as an error here so
// operators can alert on shed load.
func (s *HTTPServer) httpMetricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.metrics == nil {
			c.Next()
			return
		}
		start := time.Now()
		c.Next()
		s.metrics.HTTPRequestsTotal.Inc(1)
		if c.Writer.Status() >= http.StatusBadRequest {
			s.metrics.HTTPRequestErrors.Inc(1)
		}
		s.metrics.HTTPRequestDuration.Record(time.Since(start))
	}
}

// Start starts the HTTP server
func (s *HTTPServer) Start() error {
	s.logger.Info("Starting HTTP OTLP receiver", zap.Int("port", s.port))

	// Start serving
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("HTTP server error", zap.Error(err))
		}
	}()

	return nil
}

// Stop gracefully stops the HTTP server
func (s *HTTPServer) Stop(ctx context.Context) error {
	s.logger.Info("Stopping HTTP OTLP receiver...")

	if err := s.server.Shutdown(ctx); err != nil {
		s.logger.Error("HTTP server shutdown error", zap.Error(err))
		return err
	}

	s.logger.Info("HTTP server stopped gracefully")
	return nil
}

const (
	// maxOTLPRequestBytes caps the raw (possibly gzip-compressed) OTLP/HTTP
	// request body. Without a cap, io.ReadAll buffers an arbitrarily large
	// body into memory before it is handed to the worker pool as
	// WorkItem.RawData — the queue-bounds work caps the NUMBER of queued
	// items, not the bytes per item, so an unbounded body is an
	// unauthenticated memory-exhaustion DoS on the ingest port. 16 MiB is
	// well above a normal OTLP batch (the gRPC receiver's implicit ceiling
	// is gRPC's 4 MiB default MaxRecvMsgSize) while bounding per-request RAM.
	maxOTLPRequestBytes = 16 << 20 // 16 MiB
	// maxOTLPDecompressedBytes caps the DECOMPRESSED size when the client
	// sends Content-Encoding: gzip. gzip can expand ~1000x, so capping only
	// the compressed body is insufficient — a few-MiB upload would expand to
	// gigabytes (a decompression bomb). This bounds the expanded stream. 64
	// MiB leaves generous headroom for large legitimate batches.
	maxOTLPDecompressedBytes = 64 << 20 // 64 MiB
)

// readRequestBody reads the OTLP request body, transparently
// decompressing it when the client sets Content-Encoding: gzip. The
// OTLP/HTTP spec permits gzip-compressed payloads and the upstream
// otelcol otlphttp exporter enables gzip by default; without this the
// compressed bytes reach proto.Unmarshal and fail with "invalid
// wire-format data" (HTTP 400), silently dropping telemetry from any
// standard OTLP client.
//
// The read is bounded on two axes to stop unauthenticated DoS: the raw
// body via http.MaxBytesReader (maxOTLPRequestBytes), and — for gzip —
// the decompressed stream via io.LimitReader (maxOTLPDecompressedBytes).
// Both are needed: the compressed cap alone can't stop a gzip bomb.
func (s *HTTPServer) readRequestBody(c *gin.Context) ([]byte, error) {
	var reader io.Reader = http.MaxBytesReader(c.Writer, c.Request.Body, maxOTLPRequestBytes)
	if strings.EqualFold(c.GetHeader("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		// Read one byte past the cap so an over-limit body is detected
		// (rejected) rather than silently truncated.
		reader = io.LimitReader(gz, maxOTLPDecompressedBytes+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(data) > maxOTLPDecompressedBytes {
		return nil, fmt.Errorf("otlp request body exceeds decompressed size limit of %d bytes", maxOTLPDecompressedBytes)
	}
	return data, nil
}

// unmarshalOTLPRequest decodes an OTLP/HTTP request body into msg, selecting
// the wire encoding from the request's Content-Type. The OTLP/HTTP spec
// (opentelemetry.io/docs/specs/otlp/#binary-protobuf-encoding /
// #json-protobuf-encoding) defines TWO encodings a compliant server must
// accept: binary protobuf (Content-Type: application/x-protobuf, the default)
// and Protobuf-JSON (Content-Type: application/json). The receiver previously
// ran proto.Unmarshal unconditionally, so any standard OTLP/JSON client —
// browser-based OTel Web SDKs (JSON-only), language SDKs configured for JSON,
// and curl/script-based smoke tests — hit the binary decoder with a JSON body
// and got HTTP 400 "invalid wire-format data", silently dropping all their
// telemetry. This mirrors the Content-Encoding: gzip fix in readRequestBody:
// honor what standard clients actually send. An absent or unrecognized
// Content-Type falls back to protobuf, matching the spec's default and
// preserving the prior behavior for existing clients.
func unmarshalOTLPRequest(contentType string, body []byte, msg proto.Message) error {
	if isJSONContentType(contentType) {
		return protojson.Unmarshal(body, msg)
	}
	return proto.Unmarshal(body, msg)
}

// isJSONContentType reports whether the Content-Type header names the OTLP/JSON
// encoding. The media type may carry parameters (e.g. "application/json;
// charset=utf-8"), so only the type/subtype before any ';' is compared, case-
// insensitively per RFC 9110.
func isJSONContentType(contentType string) bool {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.EqualFold(strings.TrimSpace(contentType), "application/json")
}

// handleOTLPTraces handles OTLP traces ingestion
func (s *HTTPServer) handleOTLPTraces(c *gin.Context) {
	start := time.Now()

	// Read raw body
	body, err := s.readRequestBody(c)
	if err != nil {
		s.logger.Error("Failed to read traces request body", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read body"})
		return
	}

	// Unmarshal to validate it's valid OTLP
	var req coltracepb.ExportTraceServiceRequest
	if err := unmarshalOTLPRequest(c.GetHeader("Content-Type"), body, &req); err != nil {
		s.logger.Error("Failed to unmarshal traces request", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid OTLP traces data"})
		return
	}

	// Slice 1 chunk 2 (#706 Stream 104) — fan the per-ResourceSpan
	// observation out to the traceindex BEFORE dispatching to the
	// worker pool. Two reasons for the ordering:
	//   - A worker-pool queue-full failure (the next block) returns
	//     503 to the sender; the index observation should still land
	//     because the receiver did successfully unmarshal the batch.
	//     "Did Squadron see spans from this resource?" is independent
	//     of "did Squadron get them into long-term storage?".
	//   - The Observe call is O(1) under an Index-internal lock and
	//     does no IO, so it adds ~microseconds per ResourceSpan —
	//     fine to land before the queue submit. (Design doc §5 +
	//     chunk-2 prompt: hot path MUST NOT block on IO.)
	// nil traceIndex is the disabled-mode sentinel; the existing
	// receiver path is unchanged in that mode.
	if s.traceIndex != nil {
		observeResourceSpans(c.Request.Context(), s.traceIndex, req.ResourceSpans, time.Now())
	}
	// Span-quality slice-1 chunk-1 (#716 Stream 114) — runs after the
	// traceindex pass so a panic in quality detection cannot starve
	// the index. nil qualityIndex is the disabled-mode sentinel; the
	// observeQualitySpans guard handles it without extra branching
	// here.
	if s.qualityIndex != nil {
		observeQualitySpans(s.qualityIndex, req.ResourceSpans)
	}

	// Submit raw bytes to worker pool for async processing
	item := worker.WorkItem{
		Type:      worker.WorkItemTypeTraces,
		RawData:   body,
		Timestamp: time.Now(),
	}

	if err := s.workerPool.Submit(item); err != nil {
		s.logger.Error("Failed to queue traces", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Queue full, try again"})
		return
	}

	duration := time.Since(start)
	s.logger.Debug("Successfully queued traces request",
		zap.Int("body_size", len(body)),
		zap.Int("queue_depth", s.workerPool.QueueDepth()),
		zap.Duration("duration", duration))

	c.Status(http.StatusAccepted)
}

// handleOTLPMetrics handles OTLP metrics ingestion
func (s *HTTPServer) handleOTLPMetrics(c *gin.Context) {
	start := time.Now()

	// Read raw body
	body, err := s.readRequestBody(c)
	if err != nil {
		s.logger.Error("Failed to read metrics request body", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read body"})
		return
	}

	// Unmarshal to validate it's valid OTLP
	var req colmetricspb.ExportMetricsServiceRequest
	if err := unmarshalOTLPRequest(c.GetHeader("Content-Type"), body, &req); err != nil {
		s.logger.Error("Failed to unmarshal metrics request", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid OTLP metrics data"})
		return
	}

	// Submit raw bytes to worker pool for async processing
	item := worker.WorkItem{
		Type:      worker.WorkItemTypeMetrics,
		RawData:   body,
		Timestamp: time.Now(),
	}

	if err := s.workerPool.Submit(item); err != nil {
		s.logger.Error("Failed to queue metrics", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Queue full, try again"})
		return
	}

	duration := time.Since(start)
	s.logger.Debug("Successfully queued metrics request",
		zap.Int("body_size", len(body)),
		zap.Int("queue_depth", s.workerPool.QueueDepth()),
		zap.Duration("duration", duration))

	c.Status(http.StatusAccepted)
}

// handleOTLPLogs handles OTLP logs ingestion
func (s *HTTPServer) handleOTLPLogs(c *gin.Context) {
	start := time.Now()

	// Read raw body
	body, err := s.readRequestBody(c)
	if err != nil {
		s.logger.Error("Failed to read logs request body", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read body"})
		return
	}

	// Unmarshal to validate it's valid OTLP
	var req collogspb.ExportLogsServiceRequest
	if err := unmarshalOTLPRequest(c.GetHeader("Content-Type"), body, &req); err != nil {
		s.logger.Error("Failed to unmarshal logs request", zap.Error(err))
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid OTLP logs data"})
		return
	}

	// Submit raw bytes to worker pool for async processing
	item := worker.WorkItem{
		Type:      worker.WorkItemTypeLogs,
		RawData:   body,
		Timestamp: time.Now(),
	}

	if err := s.workerPool.Submit(item); err != nil {
		s.logger.Error("Failed to queue logs", zap.Error(err))
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Queue full, try again"})
		return
	}

	duration := time.Since(start)
	s.logger.Debug("Successfully queued logs request",
		zap.Int("body_size", len(body)),
		zap.Int("queue_depth", s.workerPool.QueueDepth()),
		zap.Duration("duration", duration))

	c.Status(http.StatusAccepted)
}

// healthCheck returns server health status
func (s *HTTPServer) healthCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyCheck returns server readiness status
func (s *HTTPServer) readyCheck(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}

// corsMiddleware adds CORS headers
func (s *HTTPServer) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
