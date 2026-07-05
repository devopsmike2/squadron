package receiver

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/worker"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/gzip" // Register gzip compressor
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

// GRPCServer represents the gRPC OTLP receiver server
type GRPCServer struct {
	server         *grpc.Server
	listener       net.Listener
	logger         *zap.Logger
	port           int
	traceService   *TraceService
	metricsService *MetricsService
	logsService    *LogsService
}

// NewGRPCServer creates a new gRPC server instance
func NewGRPCServer(port int, metricsInstance *metrics.OTLPMetrics, workerPool *worker.Pool, logger *zap.Logger) (*GRPCServer, error) {
	// Create gRPC server with keepalive settings
	server := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    10 * time.Second,
			Timeout: 5 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)

	// Register OTLP services
	traceService := NewTraceService(metricsInstance, workerPool, logger)
	metricsService := NewMetricsService(metricsInstance, workerPool, logger)
	logsService := NewLogsService(metricsInstance, workerPool, logger)

	coltracepb.RegisterTraceServiceServer(server, traceService)
	colmetricspb.RegisterMetricsServiceServer(server, metricsService)
	collogspb.RegisterLogsServiceServer(server, logsService)

	// Enable gRPC reflection for debugging
	reflection.Register(server)

	return &GRPCServer{
		server:         server,
		logger:         logger,
		port:           port,
		traceService:   traceService,
		metricsService: metricsService,
		logsService:    logsService,
	}, nil
}

// SetTenant binds all three OTLP gRPC services (traces/metrics/logs) to the
// ADR 0012 §1 ingest tenant, so cmd/all-in-one wires ingest.otlp.tenant_id
// from a single call site regardless of transport. Empty leaves the worker
// to stamp DefaultTenant (inert in OSS). Mirrors SetTraceIndex.
func (s *GRPCServer) SetTenant(tenant string) {
	if s.traceService != nil {
		s.traceService.SetTenant(tenant)
	}
	if s.metricsService != nil {
		s.metricsService.SetTenant(tenant)
	}
	if s.logsService != nil {
		s.logsService.SetTenant(tenant)
	}
}

// SetTraceIndex wires the slice-1 chunk-2 traceindex Observer onto
// the underlying TraceService. The GRPCServer holds the reference
// so cmd/all-in-one can set the index from a single call site
// regardless of which transport (HTTP / gRPC) it's wiring.
func (s *GRPCServer) SetTraceIndex(idx TraceObserver) {
	if s.traceService != nil {
		s.traceService.SetTraceIndex(idx)
	}
}

// SetQualityIndex wires the span-quality slice-1 chunk-1 observer
// onto the underlying TraceService. The GRPCServer holds the
// reference so cmd/all-in-one can set the index from a single call
// site regardless of which transport (HTTP / gRPC) it's wiring.
func (s *GRPCServer) SetQualityIndex(qual QualityObserver) {
	if s.traceService != nil {
		s.traceService.SetQualityIndex(qual)
	}
}

// Start starts the gRPC server
func (s *GRPCServer) Start() error {
	// Listen on the gRPC port
	address := fmt.Sprintf(":%d", s.port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", address, err)
	}

	s.listener = listener
	s.logger.Info("Starting gRPC OTLP receiver", zap.String("address", address))

	// Start serving
	go func() {
		if err := s.server.Serve(listener); err != nil {
			s.logger.Error("gRPC server error", zap.Error(err))
		}
	}()

	return nil
}

// Stop gracefully stops the gRPC server
func (s *GRPCServer) Stop(ctx context.Context) error {
	s.logger.Info("Stopping gRPC OTLP receiver...")

	// Graceful shutdown with timeout
	done := make(chan struct{})
	go func() {
		s.server.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
		s.logger.Info("gRPC server stopped gracefully")
		return nil
	case <-ctx.Done():
		s.logger.Warn("gRPC server shutdown timeout, forcing stop")
		s.server.Stop()
		return ctx.Err()
	}
}

// GetPort returns the port the server is listening on
func (s *GRPCServer) GetPort() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return fmt.Sprintf("%d", s.port)
}
