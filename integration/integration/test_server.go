// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/api"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/opamp"
	"github.com/devopsmike2/squadron/internal/otlp/receiver"
	"github.com/devopsmike2/squadron/internal/pipelinehealth"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
	"github.com/devopsmike2/squadron/internal/storage/telemetrystore"
	"github.com/devopsmike2/squadron/internal/storage/telemetrystore/duckdb"
	"github.com/devopsmike2/squadron/internal/worker"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// TestServer represents a test instance of Squadron for integration testing
type TestServer struct {
	// Configuration
	HTTPPort     int
	OpAMPPort    int
	OTLPGRPCPort int
	OTLPHTTPPort int

	// Storage
	appStoreFactory       applicationstore.ApplicationStoreFactory
	telemetryStoreFactory telemetrystore.TelemetryStoreFactory
	telemetryReader       telemetrystore.Reader
	telemetryWriter       telemetrystore.Writer

	// Services
	agentService      services.AgentService
	telemetryService  services.TelemetryQueryService
	savedQueryService services.SavedQueryService
	alertService      services.AlertService
	auditService      services.AuditService
	rolloutService    services.RolloutService
	authService       services.AuthService

	// Servers
	apiServer   *api.Server
	opampServer *opamp.Server
	grpcServer  *receiver.GRPCServer
	httpServer  *receiver.HTTPServer

	// Worker pool
	workerPool *worker.Pool

	// Metrics
	registry     *prometheus.Registry
	opampMetrics *metrics.OpAMPMetrics
	otlpMetrics  *metrics.OTLPMetrics

	// Event broker — wired in so the /events/stream endpoint works in the
	// test harness too, even though the integration tests don't subscribe.
	broker *events.Broker

	// Utilities
	logger  *zap.Logger
	baseURL string
	tempDir string
	t       *testing.T
}

// NewTestServer creates a new test server instance
func NewTestServer(t *testing.T, useMemory bool) *TestServer {
	logger := zap.NewNop()

	// Create temp directory for test databases
	tempDir := t.TempDir()

	// Allocate all four ports at once so they are guaranteed distinct (see
	// findFreePorts). Allocating them one at a time previously let the OS hand
	// the same ephemeral port to two fields, so two servers would try to bind
	// it -> flaky "bind: address already in use".
	ports := findFreePorts(4)
	ts := &TestServer{
		HTTPPort:     ports[0],
		OpAMPPort:    ports[1],
		OTLPGRPCPort: ports[2],
		OTLPHTTPPort: ports[3],
		logger:       logger,
		tempDir:      tempDir,
		t:            t,
	}

	ts.baseURL = fmt.Sprintf("http://localhost:%d", ts.HTTPPort)

	// Initialize storage
	if useMemory {
		ts.initMemoryStorage()
	} else {
		ts.initDatabaseStorage()
	}

	// Initialize metrics
	ts.initMetrics()

	// Initialize services
	ts.initServices()

	// Initialize servers
	ts.initServers()

	return ts
}

// initMemoryStorage initializes in-memory storage (fastest for tests)
func (ts *TestServer) initMemoryStorage() {
	// Application store
	appFactory := memory.NewFactory()
	if err := appFactory.Initialize(ts.logger); err != nil {
		ts.t.Fatalf("Failed to initialize memory app store: %v", err)
	}
	ts.appStoreFactory = appFactory

	// For telemetry, use a temp file for DuckDB
	telemetryDBPath := filepath.Join(ts.tempDir, "telemetry-mem.db")
	telemetryFactory := duckdb.NewFactory(telemetryDBPath)
	if err := telemetryFactory.Initialize(ts.logger); err != nil {
		ts.t.Fatalf("Failed to initialize memory telemetry store: %v", err)
	}
	ts.telemetryStoreFactory = telemetryFactory

	var err error
	ts.telemetryReader, err = telemetryFactory.CreateTelemetryReader()
	if err != nil {
		ts.t.Fatalf("Failed to create telemetry reader: %v", err)
	}

	ts.telemetryWriter, err = telemetryFactory.CreateTelemetryWriter()
	if err != nil {
		ts.t.Fatalf("Failed to create telemetry writer: %v", err)
	}
}

// initMetrics initializes metrics components on a single shared registry.
func (ts *TestServer) initMetrics() {
	ts.registry = prometheus.NewRegistry()
	metricsFactory := metrics.NewPrometheusFactory("squadron", ts.registry)
	ts.opampMetrics = metrics.NewOpAMPMetrics(metricsFactory)
	ts.otlpMetrics = metrics.NewOTLPMetrics(metricsFactory)
}

// initDatabaseStorage initializes file-based storage
func (ts *TestServer) initDatabaseStorage() {
	appDBPath := filepath.Join(ts.tempDir, "app.db")
	telemetryDBPath := filepath.Join(ts.tempDir, "telemetry.db")

	// Application store
	appFactory := sqlite.NewFactory(appDBPath)
	if err := appFactory.Initialize(ts.logger); err != nil {
		ts.t.Fatalf("Failed to initialize SQLite app store: %v", err)
	}
	ts.appStoreFactory = appFactory

	// Telemetry store
	telemetryFactory := duckdb.NewFactory(telemetryDBPath)
	if err := telemetryFactory.Initialize(ts.logger); err != nil {
		ts.t.Fatalf("Failed to initialize DuckDB telemetry store: %v", err)
	}
	ts.telemetryStoreFactory = telemetryFactory

	var err error
	ts.telemetryReader, err = telemetryFactory.CreateTelemetryReader()
	if err != nil {
		ts.t.Fatalf("Failed to create telemetry reader: %v", err)
	}

	ts.telemetryWriter, err = telemetryFactory.CreateTelemetryWriter()
	if err != nil {
		ts.t.Fatalf("Failed to create telemetry writer: %v", err)
	}
}

// initServices initializes service layer
func (ts *TestServer) initServices() {
	appStore, err := ts.appStoreFactory.CreateApplicationStore()
	if err != nil {
		ts.t.Fatalf("Failed to create app store: %v", err)
	}

	// Create the event broker first so services that publish can be wired
	// against it. Tests don't subscribe; this just keeps the production
	// wiring intact under test.
	ts.broker = events.NewBroker()

	// AuditService comes next — the agent service injects it for drift /
	// config events. Order matters: construct it before any service that
	// captures it by reference.
	ts.auditService = services.NewAuditService(appStore, ts.broker, ts.logger)

	// Create agent service without config sender initially
	ts.agentService = services.NewAgentService(appStore, nil, ts.broker, ts.auditService, ts.logger)
	ts.savedQueryService = services.NewSavedQueryService(appStore, ts.logger)
	ts.alertService = services.NewAlertService(appStore, ts.logger)
	// Rollout service is wired so /api/v1/rollouts routes are reachable.
	// The engine goroutine isn't started here — tests don't exercise the
	// background state machine.
	ts.rolloutService = services.NewRolloutService(appStore, ts.agentService, ts.auditService, ts.logger)
	// Auth service constructed but auth is left disabled in the test
	// server. Authn behavior is exercised by unit tests; the integration
	// suite focuses on cross-service workflows where auth would just be
	// boilerplate at every call site.
	ts.authService = services.NewAuthService(appStore, ts.logger)
}

// initServers initializes all servers
func (ts *TestServer) initServers() {
	// OpAMP Server components
	agents := opamp.NewAgents(ts.logger)

	// Create config sender (separate concern from AgentService)
	configSender := opamp.NewConfigSender(agents, ts.logger)

	opampServer, err := opamp.NewServer(agents, ts.agentService, ts.opampMetrics, "localhost:4317", "localhost:4318", ts.logger)
	if err != nil {
		ts.t.Fatalf("Failed to create OpAMP server: %v", err)
	}
	ts.opampServer = opampServer

	// Create telemetry service
	ts.telemetryService = services.NewTelemetryQueryService(ts.telemetryReader, ts.agentService, ts.logger)

	// API Server — uses the same registry as OpAMP/OTLP metrics, and the
	// same broker as the agent service so /events/stream sees publishes
	// from the rest of the harness.
	// nil configs tracer = no OTel push spans in the integration
	// harness. The tracer's nil-safety contract is exercised by the
	// configs unit tests; integration just verifies cross-service
	// behavior so the no-trace path is the right shape here.
	ts.apiServer = api.NewServer(ts.agentService, ts.telemetryService, ts.savedQueryService, ts.alertService, ts.auditService, ts.rolloutService, ts.authService, api.AuthConfig{Enabled: false}, configSender, ts.broker, nil, ts.registry, ts.logger)

	// Wire pipeline-health so the headline /pipeline-health/fleet surface is
	// reachable in the integration smoke gate. Uses the harness DuckDB reader +
	// agent service, mirroring the production wiring in cmd/all-in-one.
	ts.apiServer.SetPipelineHealth(pipelinehealth.NewService(ts.telemetryReader, smokeAgentLister{svc: ts.agentService}, ts.logger))

	// Create worker pool for async telemetry processing.
	// Using default values: queue_size=10000, workers=3, timeout=5s.
	// Worker metrics are nil — the pool falls back to a no-op WorkerMetrics
	// internally, so the test harness doesn't need to wire Prometheus.
	ts.workerPool = worker.NewPool(10000, 3, 5*time.Second, ts.telemetryWriter, ts.agentService, nil, ts.logger)
	ts.workerPool.Start()

	// OTLP Receivers - use worker pool for async processing
	grpcServer, err := receiver.NewGRPCServer(ts.OTLPGRPCPort, ts.otlpMetrics, ts.workerPool, ts.logger)
	if err != nil {
		ts.t.Fatalf("Failed to create gRPC server: %v", err)
	}
	ts.grpcServer = grpcServer

	httpServer, err := receiver.NewHTTPServer(ts.OTLPHTTPPort, ts.otlpMetrics, ts.workerPool, ts.logger)
	if err != nil {
		ts.t.Fatalf("Failed to create HTTP server: %v", err)
	}
	ts.httpServer = httpServer
}

// Start starts all servers
func (ts *TestServer) Start() {
	// Start API server
	go func() {
		if err := ts.apiServer.Start(fmt.Sprintf("%d", ts.HTTPPort)); err != nil && err != http.ErrServerClosed {
			ts.t.Logf("API server error: %v", err)
		}
	}()

	// Start OpAMP server
	if err := ts.opampServer.Start(ts.OpAMPPort); err != nil {
		ts.t.Fatalf("Failed to start OpAMP server: %v", err)
	}

	// Start OTLP receivers
	if err := ts.grpcServer.Start(); err != nil {
		ts.t.Fatalf("Failed to start gRPC server: %v", err)
	}

	if err := ts.httpServer.Start(); err != nil {
		ts.t.Fatalf("Failed to start HTTP server: %v", err)
	}

	// Wait for servers to be ready
	ts.WaitForReady()
}

// Stop stops all servers
func (ts *TestServer) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if ts.apiServer != nil {
		_ = ts.apiServer.Stop(ctx)
	}

	if ts.opampServer != nil {
		_ = ts.opampServer.Stop(ctx)
	}

	if ts.grpcServer != nil {
		_ = ts.grpcServer.Stop(ctx)
	}

	if ts.httpServer != nil {
		_ = ts.httpServer.Stop(ctx)
	}

	// Stop worker pool
	if ts.workerPool != nil {
		_ = ts.workerPool.Stop(5 * time.Second)
	}

	// Close storage factories
	if closer, ok := ts.appStoreFactory.(applicationstore.Closer); ok {
		closer.Close()
	}
	if closer, ok := ts.telemetryStoreFactory.(interface{ Close() error }); ok {
		closer.Close()
	}

	// Clean up temp directory
	os.RemoveAll(ts.tempDir)
}

// WaitForReady waits for the server to be ready to accept requests
func (ts *TestServer) WaitForReady() {
	maxAttempts := 30
	for i := 0; i < maxAttempts; i++ {
		resp, err := http.Get(ts.baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	ts.t.Fatal("Server did not become ready in time")
}

// GET makes an HTTP GET request
func (ts *TestServer) GET(path string) (*http.Response, error) {
	return http.Get(ts.baseURL + path)
}

// POST makes an HTTP POST request
func (ts *TestServer) POST(path string, contentType string, body io.Reader) (*http.Response, error) {
	return http.Post(ts.baseURL+path, contentType, body)
}

// DELETE makes an HTTP DELETE request
func (ts *TestServer) DELETE(path string) (*http.Response, error) {
	req, err := http.NewRequest("DELETE", ts.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// findFreePorts returns n distinct free TCP ports. It opens all n listeners
// SIMULTANEOUSLY before closing any of them, so the OS cannot hand the same
// ephemeral port back to two calls — the bug that made the servers collide
// (e.g. OTLPGRPCPort == OTLPHTTPPort -> "bind: address already in use").
// Allocating one at a time (open→close→open) let the freshly-closed port be
// re-handed to the next call. Ports are still bound-then-released here, so a
// tiny release→re-bind window remains, but the intra-set duplication that
// caused the flake is gone. See knowledge/ci-gotchas.md.
func findFreePorts(n int) []int {
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			panic(err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	// Release only after all n are reserved, so all n ports are distinct.
	for _, l := range listeners {
		_ = l.Close()
	}
	return ports
}

// smokeAgentLister adapts the agent service to pipelinehealth.AgentLister for
// the integration harness, mirroring pipelineHealthAgentLister in cmd/all-in-one.
type smokeAgentLister struct{ svc services.AgentService }

func (l smokeAgentLister) AllAgentIDs(ctx context.Context) ([]string, error) {
	agents, err := l.svc.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(agents))
	for _, a := range agents {
		if a != nil {
			ids = append(ids, a.ID.String())
		}
	}
	return ids, nil
}
