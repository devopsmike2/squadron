package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"encoding/base64"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/alerting"
	"github.com/devopsmike2/squadron/internal/alerts"
	"github.com/devopsmike2/squadron/internal/api"
	"github.com/devopsmike2/squadron/internal/api/handlers"
	"github.com/devopsmike2/squadron/internal/billing"
	"github.com/devopsmike2/squadron/internal/config"
	"github.com/devopsmike2/squadron/internal/configs"
	"github.com/devopsmike2/squadron/internal/costspikes"
	"github.com/devopsmike2/squadron/internal/deploy"
	"github.com/devopsmike2/squadron/internal/discovery"
	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scannerfactory"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/incidents"
	"github.com/devopsmike2/squadron/internal/insights"
	"github.com/devopsmike2/squadron/internal/inventory"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/opamp"
	"github.com/devopsmike2/squadron/internal/otlp/receiver"
	"github.com/devopsmike2/squadron/internal/pipelinehealth"
	"github.com/devopsmike2/squadron/internal/pricing"
	"github.com/devopsmike2/squadron/internal/proposer"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/rollouts"
	"github.com/devopsmike2/squadron/internal/selftel"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/siem"
	"github.com/devopsmike2/squadron/internal/silentagents"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/telemetrystore"
	"github.com/devopsmike2/squadron/internal/traceindex"
	"github.com/devopsmike2/squadron/internal/utils"
	"github.com/devopsmike2/squadron/internal/worker"
)

const (
	appName = "Squadron"
	version = "0.1.0"
)

func main() {
	// Create root command
	rootCmd := &cobra.Command{
		Use:   "squadron",
		Short: "Squadron - OpenTelemetry observability platform",
		Long: `Squadron is a comprehensive observability platform that provides:
- OpenTelemetry data collection and processing
- Agent management via OpAMP protocol
- Real-time telemetry analysis
- Modern web interface for monitoring and management`,
		RunE: runSquadron,
	}

	// Add subcommands
	rootCmd.AddCommand(versionCommand())
	rootCmd.AddCommand(configCommand())

	// Add flags
	rootCmd.PersistentFlags().String("config", "./squadron.yaml", "Path to configuration file")
	rootCmd.PersistentFlags().String("log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().String("log-format", "json", "Log format (json, console)")

	// Bind flags to viper
	_ = viper.BindPFlags(rootCmd.PersistentFlags())

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func runSquadron(cmd *cobra.Command, args []string) error {
	// Load configuration
	configPath := viper.GetString("config")
	config, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	// Initialize logger
	logger, err := utils.NewLogger(config.Logging.Level, config.Logging.Format)
	if err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	logger.Info("Starting Squadron",
		zap.String("version", version),
		zap.String("config", configPath))

	// Create application store using meta factory
	appStoreFactory, err := applicationstore.NewFactoryFromAppConfig(config)
	if err != nil {
		logger.Fatal("Failed to create application store factory", zap.Error(err))
	}

	// Initialize the factory
	if err := appStoreFactory.Initialize(logger); err != nil {
		logger.Fatal("Failed to initialize application store factory", zap.Error(err))
	}

	// Create application store
	appStore, err := appStoreFactory.CreateApplicationStore()
	if err != nil {
		logger.Fatal("Failed to create application store", zap.Error(err))
	}

	// Ensure application store factory is properly closed on shutdown
	defer func() {
		if err := appStoreFactory.Close(); err != nil {
			logger.Error("Failed to close application store factory", zap.Error(err))
		}
	}()

	// Create telemetry store using meta factory
	telemetryStoreFactory, err := telemetrystore.NewFactoryFromAppConfig(config)
	if err != nil {
		logger.Fatal("Failed to create telemetry store factory", zap.Error(err))
	}

	// Initialize the factory
	if err := telemetryStoreFactory.Initialize(logger); err != nil {
		logger.Fatal("Failed to initialize telemetry store factory", zap.Error(err))
	}

	// Create telemetry reader
	telemetryReader, err := telemetryStoreFactory.CreateTelemetryReader()
	if err != nil {
		logger.Fatal("Failed to create telemetry reader", zap.Error(err))
	}

	// Create telemetry writer for OTLP receivers
	telemetryWriter, err := telemetryStoreFactory.CreateTelemetryWriter()
	if err != nil {
		logger.Fatal("Failed to create telemetry writer", zap.Error(err))
	}

	// Ensure telemetry store factory is properly closed on shutdown
	defer func() {
		if err := telemetryStoreFactory.Close(); err != nil {
			logger.Error("Failed to close telemetry store factory", zap.Error(err))
		}
	}()

	registry := prometheus.NewRegistry()
	metricsFactory := metrics.NewPrometheusFactory("squadron", registry)
	opampMetrics := metrics.NewOpAMPMetrics(metricsFactory)
	otlpMetrics := metrics.NewOTLPMetrics(metricsFactory)
	workerMetrics := metrics.NewWorkerMetrics(metricsFactory)
	driftMetrics := metrics.NewDriftMetrics(metricsFactory)
	alertMetrics := metrics.NewAlertMetrics(metricsFactory)

	// In-process event broker for SSE delivery of agent / alert state
	// changes to the UI. Lives for the whole process lifetime.
	eventBroker := events.NewBroker()

	agents := opamp.NewAgents(logger)

	// Determine which OTLP endpoints to offer to agents
	// If agent_*_endpoint is configured, use it; otherwise use the receiver endpoint
	agentGRPCEndpoint := config.OTLP.AgentGRPCEndpoint
	if agentGRPCEndpoint == "" {
		agentGRPCEndpoint = config.OTLP.GRPCEndpoint
	}
	agentHTTPEndpoint := config.OTLP.AgentHTTPEndpoint
	if agentHTTPEndpoint == "" {
		agentHTTPEndpoint = config.OTLP.HTTPEndpoint
	}

	// Self-telemetry publisher: when telemetry.enabled is true Squadron
	// exports each audit event as an OTel span AND bridges the
	// Prometheus /metrics surface (registry passed below) to OTLP
	// metrics on the same endpoint. Disabled here means a no-op
	// publisher — the audit service treats nil and no-op identically.
	selftelPub, err := selftel.New(context.Background(), selftel.Config{
		Enabled:        config.Telemetry.Enabled,
		ServiceName:    config.Telemetry.ServiceName,
		Endpoint:       config.Telemetry.OTLP.Endpoint,
		Protocol:       config.Telemetry.OTLP.Protocol,
		Headers:        config.Telemetry.OTLP.Headers,
		Insecure:       config.Telemetry.OTLP.Insecure,
		MetricInterval: config.Telemetry.MetricInterval,
	}, registry, logger)
	if err != nil {
		logger.Fatal("Failed to initialize self-telemetry", zap.Error(err))
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := selftelPub.Shutdown(shutdownCtx); err != nil {
			logger.Warn("self-telemetry shutdown failed", zap.Error(err))
		}
	}()

	// AuditService records every state change. Constructed before the
	// other services so they can publish into it via injection. Wired
	// with the same broker so timeline UIs append entries in real time
	// over SSE, and with the self-telemetry publisher so each entry
	// also surfaces in the operator's external observability stack
	// when configured.
	auditService := services.NewAuditServiceWithSelfTelemetry(appStore, eventBroker,
		&selftelAdapter{pub: selftelPub}, logger)

	// Create agent service. driftMetrics is wired so drift transitions and
	// fleet drift state appear on /metrics. The event broker receives
	// agent_registered and agent_drift_changed events for the SSE stream;
	// auditService records the same transitions as durable log entries.
	agentService := services.NewAgentService(appStore, driftMetrics, eventBroker, auditService, logger)
	savedQueryService := services.NewSavedQueryService(appStore, logger)
	alertService := services.NewAlertService(appStore, logger)
	authService := services.NewAuthService(appStore, logger)
	// v0.51 — wire audit fan-out for token issuance / revocation.
	// AuthServiceImpl exposes SetAuditService so the audit service
	// (constructed earlier in the graph) can be plugged in after the
	// fact. nil-safe: if the cast fails we just don't emit the
	// api_token.* events, which is the same behavior as before.
	if a, ok := authService.(interface {
		SetAuditService(services.AuditService)
	}); ok {
		a.SetAuditService(auditService)
	}

	// Rollout OTel tracer — bracketing spans per rollout + child
	// spans per stage. Reuses the self-telemetry tracer provider so
	// rollout traces show up in the same OTLP endpoint as audit
	// spans. The same tracer instance handles both engine-driven
	// spans and service-driven span events (pause / resume / abort
	// via the RolloutTracer interface) so a single rollout trace
	// captures every transition regardless of origin.
	rolloutTracer := rollouts.NewTracer(selftelPub.Tracer("squadron/rollouts"))
	// Config-push tracer — bracketing span per agent push. Shared
	// between the engine (rollout pushes + rollback pushes) and the
	// API handlers (direct + group pushes). Source attribute lets
	// operators filter by what triggered each push.
	configsTracer := configs.NewTracer(selftelPub.Tracer("squadron/configs"))
	rolloutService := services.NewRolloutServiceWithTracer(appStore, agentService, auditService, rolloutTracer, logger)
	// v0.52 — wire the build-specific extension points. The OSS
	// build installs no-op providers; the Compliance Pack build
	// installs real ones. main.go itself doesn't care which is
	// active. See cmd/all-in-one/wire_oss.go (default) and
	// wire_compliance.go (build tag: compliance) for the boundary.
	buildEdition := wireExtensions(rolloutService)
	logger.Info("squadron build edition", zap.String("edition", buildEdition))

	// Bootstrap an initial token if auth is enabled and the store has
	// none yet. Operators see this token in stderr on first start; they
	// copy it, use it to log in, create proper labeled tokens, and
	// revoke the bootstrap one. The check runs every start but only
	// emits a token when the store is empty so subsequent restarts are
	// quiet. See docs/auth.md for the recovery flow if every token is
	// lost.
	if config.Auth.Enabled {
		if err := bootstrapAuthToken(context.Background(), authService, logger); err != nil {
			logger.Fatal("Failed to bootstrap auth token", zap.Error(err))
		}
	} else {
		logger.Warn("API auth is disabled — every endpoint is open. " +
			"Set auth.enabled=true in squadron.yaml for production.")
	}

	// Create config sender (separate concern from AgentService)
	configSender := opamp.NewConfigSender(agents, logger)

	// Create OpAMP server with agent service (for persistence)
	// OpAMP connection tracer — long-lived span per (agent, connection).
	// Mirrors the rollout-tracer in-memory active-map pattern: spans
	// open on the first inbound message from an agent and close on
	// disconnect, with Shutdown flushing any in-flight ones.
	opampTracer := opamp.NewTracer(selftelPub.Tracer("squadron/opamp"))
	opampServer, err := opamp.NewServerWithTracer(agents, agentService, opampMetrics, opampTracer, agentGRPCEndpoint, agentHTTPEndpoint, logger)
	if err != nil {
		logger.Fatal("Failed to create OpAMP server", zap.Error(err))
	}

	// Create telemetry query service
	telemetryService := services.NewTelemetryQueryService(telemetryReader, agentService, logger)

	// Parse worker pool timeout
	workerTimeout, err := time.ParseDuration(config.Worker.Timeout)
	if err != nil {
		// Default to 5s if parsing fails
		workerTimeout = 5 * time.Second
		logger.Warn("Failed to parse worker timeout, using default", zap.Error(err))
	}

	// Initialize worker pool for async telemetry processing.
	// Pass workerMetrics so retry/dead-letter counters land on /metrics.
	workerPool := worker.NewPool(config.Worker.QueueSize, config.Worker.Workers, workerTimeout, telemetryWriter, agentService, workerMetrics, logger)

	// v0.36: passive OTLP discovery. The worker pool calls
	// discoverySvc.RegisterIfUnknown for each unique agent_id
	// it sees in incoming OTLP batches. Unknown ids become
	// "telemetry_only" agents in the agents list.
	discoverySvc := discovery.NewService(appStore, discovery.DefaultDedupWindow, logger)
	workerPool.SetDiscovery(discoverySvc)
	logger.Info("Passive OTLP discovery enabled (telemetry-only agents auto-register)")

	workerPool.Start()
	defer func() {
		if err := workerPool.Stop(30 * time.Second); err != nil {
			logger.Error("Failed to stop worker pool", zap.Error(err))
		}
	}()

	// Start OpAMP server
	if err := opampServer.Start(config.Server.OpAMPPort); err != nil {
		logger.Fatal("Failed to start OpAMP server", zap.Error(err))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = opampServer.Stop(ctx)
	}()

	// Initialize OTLP receivers. Ports come from the OTLP config so
	// operators can shift them when 4317/4318 conflict with a Docker
	// host port mapping or another collector on the box.
	// The yaml carries "host:port" strings; we parse the port off
	// and fall back to the defaults when unset.
	grpcPort := parseOTLPPort(config.OTLP.GRPCEndpoint, 4317)
	httpPort := parseOTLPPort(config.OTLP.HTTPEndpoint, 4318)

	// v0.89.75 (#706 Stream 104) — Trace integration slice 1 chunk 2.
	// Construct the in-memory traceindex Index BEFORE the OTLP
	// receivers so the SetTraceIndex calls below can hand the same
	// instance to both transports. The Index is fronted by appStore
	// (which satisfies traceindex.Store via the chunk-1
	// trace_resource_seen ApplicationStore methods); maxRows=0
	// selects the defaultMaxRows ceiling (100K — design doc §12),
	// overridable by SQUADRON_TRACEINDEX_MAX_ROWS in a later chunk.
	//
	// Disabled-mode escape hatch: SQUADRON_TRACEINDEX_DISABLED=true
	// skips Index construction entirely. The receivers' nil-check
	// guard in handleOTLPTraces / TraceService.Export handles the
	// disabled case without any extra branching on the hot path.
	var traceIndex *traceindex.Index
	if strings.EqualFold(os.Getenv("SQUADRON_TRACEINDEX_DISABLED"), "true") {
		logger.Info("Trace index disabled via SQUADRON_TRACEINDEX_DISABLED")
	} else {
		traceIndex = traceindex.NewIndex(appStore, 0, nil)
		logger.Info("Trace index enabled", zap.Int("max_rows", 100_000))
	}

	// v0.89.85 (#716 Stream 114) — Span quality slice 1 chunk 1.
	// Sibling of traceIndex: the OTLP receivers fan each span out to
	// the QualityObserver in addition to the per-ResourceSpan Index
	// pass. The detection runs on the same key derivation
	// (ComputeResourceKey) so the Discovery dashboard's per-resource
	// rollup joins cleanly across the two observers.
	//
	// Disabled-mode escape hatch: SQUADRON_SPANQUALITY_DISABLED=true
	// skips construction. The receivers' nil-guards handle the
	// disabled case without extra branching on the hot path.
	var qualityIndex *traceindex.Quality
	if strings.EqualFold(os.Getenv("SQUADRON_SPANQUALITY_DISABLED"), "true") {
		logger.Info("Quality index disabled via SQUADRON_SPANQUALITY_DISABLED")
	} else {
		qualityIndex = traceindex.NewQuality()
		logger.Info("Quality index enabled", zap.Duration("window", time.Hour))
	}

	grpcServer, err := receiver.NewGRPCServer(grpcPort, otlpMetrics, workerPool, logger)
	if err != nil {
		logger.Fatal("Failed to create gRPC server", zap.Error(err))
	}
	if traceIndex != nil {
		grpcServer.SetTraceIndex(traceIndex)
	}
	if qualityIndex != nil {
		grpcServer.SetQualityIndex(qualityIndex)
	}
	if err := grpcServer.Start(); err != nil {
		logger.Fatal("Failed to start gRPC server", zap.Error(err))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = grpcServer.Stop(ctx)
	}()

	httpServer, err := receiver.NewHTTPServer(httpPort, otlpMetrics, workerPool, logger)
	if err != nil {
		logger.Fatal("Failed to create HTTP server", zap.Error(err))
	}
	if traceIndex != nil {
		httpServer.SetTraceIndex(traceIndex)
	}
	if qualityIndex != nil {
		httpServer.SetQualityIndex(qualityIndex)
	}
	if err := httpServer.Start(); err != nil {
		logger.Fatal("Failed to start HTTP server", zap.Error(err))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Stop(ctx)
	}()

	// Trace index background flush goroutine. Bound to a cancellable
	// context the shutdown path cancels so the loop exits cleanly on
	// signal. The audit adapter shims the rich services.AuditService
	// interface down to the minimal AuditEmitter shape the traceindex
	// package consumes — keeps the traceindex package free of a
	// services import that would create a cycle with the storage
	// layer.
	traceFlushCtx, traceFlushCancel := context.WithCancel(context.Background())
	defer traceFlushCancel()
	if traceIndex != nil {
		flushAudit := &traceIndexAuditAdapter{audit: auditService}
		flusher := traceindex.NewBackgroundFlusher(traceIndex, 30*time.Second, flushAudit, logger)
		go flusher.Start(traceFlushCtx)
	}

	// Initialize HTTP API server.
	// Share the same Prometheus registry so that /metrics exposes OpAMP, OTLP,
	// worker, and API metrics in a single endpoint. The event broker is
	// shared with publishers so /events/stream reflects what they emit.
	apiServer := api.NewServer(agentService, telemetryService, savedQueryService, alertService, auditService, rolloutService, authService, api.AuthConfig{Enabled: config.Auth.Enabled}, configSender, eventBroker, configsTracer, registry, logger)
	// v0.52 — wire API-server-level Compliance Pack extension points.
	// OSS leaves the per-request access audit middleware unmounted;
	// Compliance Pack installs it. Called here before any other
	// SetX accessor so the middleware order in v1.Use() stays
	// deterministic (auth → access audit → handler).
	wireAPIServerExtensions(apiServer, auditService, logger)
	// v0.53 — wire action runner dependencies. The signer's key
	// loads from SQUADRON_ACTION_SIGNING_KEY (base64 of 32 raw
	// bytes); when unset we generate a fresh key at startup so dev
	// instances work without ceremony. Production deployments must
	// set this env var so runners pinned to a previous public key
	// keep working across restarts.
	var actionSigner *actions.Signer
	if seedB64 := os.Getenv("SQUADRON_ACTION_SIGNING_KEY"); seedB64 != "" {
		seed, err := base64.StdEncoding.DecodeString(seedB64)
		if err != nil {
			logger.Fatal("SQUADRON_ACTION_SIGNING_KEY is not valid base64", zap.Error(err))
		}
		actionSigner, err = actions.NewSigner(seed)
		if err != nil {
			logger.Fatal("SQUADRON_ACTION_SIGNING_KEY rejected", zap.Error(err))
		}
		logger.Info("action runner signer loaded from env", zap.String("fingerprint", actionSigner.Fingerprint()))
	} else {
		s, err := actions.GenerateSigner()
		if err != nil {
			logger.Fatal("failed to generate ephemeral action signing key", zap.Error(err))
		}
		actionSigner = s
		logger.Warn("SQUADRON_ACTION_SIGNING_KEY unset; generated ephemeral key (runners must re-enroll on restart)",
			zap.String("fingerprint", actionSigner.Fingerprint()))
	}
	apiServer.SetActionStoreAndSigner(appStore, actionSigner)

	// v0.27.1 Quickstart needs to know the OpAMP port so the
	// generated agent configs dial back to the right place.
	apiServer.SetOpAMPPort(config.Server.OpAMPPort)

	// v0.24 Telemetry Volume Insights — read-only query layer over
	// the otlp_batches table + sampled row-table aggregates. Wires
	// to the same telemetryReader used everywhere else, so it
	// shares DuckDB connection pooling and statement cache.
	insightsService := insights.NewService(telemetryReader, logger)
	apiServer.SetInsightsService(insightsService)

	// v0.25 Cost Recommendations — heuristic engine that turns the
	// insights surface into actionable advice (noisy attributes,
	// outlier agents, drop hotspots, empty signal branches). The
	// dismissals store lets operators hide a recommendation they've
	// already evaluated; the engine consults it via a tiny adapter.
	//
	// AgentNameResolver looks up labels for per-agent
	// recommendations. Best-effort: if the lookup fails or the
	// agent is unknown, the engine falls back to a short ID prefix.
	recsEngine := recommendations.NewEngine(
		insightsService,
		recsDismissalsAdapter{store: appStore},
		func(ctx context.Context, agentID string) string {
			parsed, err := uuid.Parse(agentID)
			if err != nil {
				return ""
			}
			a, err := appStore.GetAgent(ctx, parsed)
			if err != nil || a == nil {
				return ""
			}
			return a.Name
		},
		logger,
	)
	apiServer.SetRecommendationsEngine(recsEngine, appStore)

	// v0.27 Pricing — translate byte numbers into $/month. Default
	// rules cover the major destinations (Datadog/Honeycomb/etc.);
	// operators tune in squadron.yaml's pricing.rules. We construct
	// the projector even when disabled so /pricing/* routes can
	// gracefully report enabled=false rather than 503.
	pricingCfg := pricing.Config{
		Enabled:  config.Pricing.Enabled,
		Currency: config.Pricing.Currency,
	}
	if len(config.Pricing.Rules) > 0 {
		for _, r := range config.Pricing.Rules {
			pricingCfg.Rules = append(pricingCfg.Rules, pricing.Rule{
				Match: r.Match, Label: r.Label, PricePerGB: r.PricePerGB,
				Traces: r.Traces, Metrics: r.Metrics, Logs: r.Logs,
			})
		}
	} else if pricingCfg.Enabled {
		// Enabled but no rules → use built-in starter set.
		pricingCfg = pricing.DefaultConfig()
		pricingCfg.Enabled = true
	}
	projector, err := pricing.NewProjector(pricingCfg)
	if err != nil {
		logger.Fatal("Failed to construct pricing projector", zap.Error(err))
	}
	apiServer.SetPricer(projector)
	// Plumb the projector into the recommendations engine so each
	// rec gains a $/month figure alongside the byte estimate.
	recsEngine.SetPricer(projector)
	if projector.Enabled() {
		logger.Info("Pricing projection enabled",
			zap.String("currency", projector.Currency()),
			zap.Int("rules", len(projector.Rules())))
	} else {
		logger.Info("Pricing projection disabled (set pricing.enabled=true to enable $/month figures)")
	}

	// v0.29 cost-spike alerting. Detector polls the pricer + insights
	// every minute and opens a CostSpikeEvent when the projection
	// exceeds the warn/critical thresholds. Only wired when both
	// pricing and insights are present — without those two upstream
	// signals there's nothing meaningful to detect.
	if appStoreCostSpikes, ok := appStore.(handlers.CostSpikeStore); ok && projector.Enabled() && insightsService != nil {
		spikeStore, _ := appStore.(costspikes.SpikeStore)
		detector := costspikes.New(costspikes.DefaultConfig(), spikeStore, projector, insightsService)
		apiServer.SetCostSpikes(appStoreCostSpikes, detector)
		// Detector loop. Uses Background — main.go's shutdown
		// path closes the API server and then the process exits;
		// a stray tick during shutdown is harmless because
		// Detector.Tick is pure (no goroutines of its own).
		detectorCtx := context.Background()
		go func() {
			t := time.NewTicker(60 * time.Second)
			defer t.Stop()
			for range t.C {
				if err := detector.Tick(detectorCtx); err != nil {
					logger.Warn("cost-spike detector tick failed", zap.Error(err))
				}
			}
		}()
		logger.Info("Cost-spike alerting enabled (detector running every 60s)")
	} else {
		// Wire the read paths only — the routes still need a store
		// reference to serve list/ack even when the detector is off
		// (e.g. an operator viewing historical spikes).
		if storeForReads, ok := appStore.(handlers.CostSpikeStore); ok {
			apiServer.SetCostSpikes(storeForReads, nil)
		}
		logger.Info("Cost-spike alerting disabled (requires pricing.enabled + insights service)")
	}

	// v0.31 Pipeline Health — collector self-metrics surface. Reads
	// from the dedicated pipeline_health_samples table populated by
	// the worker pool's extractor. Needs an AgentLister so the fleet
	// summary can distinguish "agent reports OpAMP but no metrics
	// yet" (Unknown) from "the metric set is empty across the board"
	// (no agents).
	pipelineHealthSvc := pipelinehealth.NewService(
		telemetryReader,
		pipelineHealthAgentLister{store: appStore},
		logger,
	)
	apiServer.SetPipelineHealth(pipelineHealthSvc)
	logger.Info("Pipeline health surface enabled (collector self-metrics extracted into pipeline_health_samples)")

	// v0.32 inventory reconciliation — diff CI's expected hostlist
	// against the actual agents table. Always wired; the
	// reconciliation endpoint surfaces an empty report when neither
	// side has anything.
	inventorySvc := inventory.NewService(appStore, logger)
	apiServer.SetInventory(inventorySvc)
	logger.Info("Inventory reconciliation surface enabled")

	// v0.34 deploy integration (GitHub Actions). Disabled when
	// SQUADRON_DEPLOY_KEY is missing — the API will 503 with a
	// clear "set the key" message, and the UI hides deploy
	// affordances.
	if crypter, err := deploy.NewCrypterFromEnv(); err != nil {
		logger.Info("Deploy integration disabled (SQUADRON_DEPLOY_KEY unset). Generate with: head -c 32 /dev/urandom | base64")
	} else {
		// v0.41 — multi-provider router. GitHub Actions remains the
		// default; Azure DevOps Pipelines now sits alongside as a
		// peer backend. Both are constructed unconditionally because
		// the upstream PAT is per-target, not per-provider, and the
		// HTTP clients are cheap.
		ghProvider := deploy.NewGitHubProvider("")
		adoProvider := deploy.NewAzureDevOpsProvider("")
		towerProvider := deploy.NewAnsibleTowerProvider()
		provider := deploy.NewMultiProvider(ghProvider, adoProvider, towerProvider)
		deploySvc := deploy.NewService(appStore, provider, crypter, logger)
		deploySvc.SetCompletionWebhook(config.Deploy.CompletionWebhookURL)
		apiServer.SetDeploy(deploySvc)
		// Polling loop: every 60s the service walks queued +
		// in_progress runs and refreshes their status. v0.35 adds
		// a webhook receiver so this drops to a sync fallback.
		go func() {
			t := time.NewTicker(60 * time.Second)
			defer t.Stop()
			for range t.C {
				_ = deploySvc.SyncOpenRuns(context.Background())
			}
		}()
		logger.Info("Deploy integration enabled (GitHub Actions, polling every 60s)")

		// v0.36.1 GHA history walker. Periodically replays the deploy
		// target's workflow history and registers historical inventory
		// hosts as expected_agents. Composes with v0.32 reconciliation
		// — hosts seen in past deploys but not currently checking in
		// surface automatically.
		walker := discovery.NewGHAWalker(appStore, deploySvc, ghProvider,
			discovery.DefaultGHAWalkInterval, discovery.DefaultGHALookback, logger)
		go walker.Run(context.Background())
	}

	// v0.50 SIEM export. Same disabled-on-missing-key pattern as
	// deploy: if SQUADRON_SIEM_KEY isn't set, log + continue. The
	// /api/v1/siem/* routes return 503 in that state (clean UX
	// instead of a 500). When enabled, the dispatcher hot-reloads
	// destinations from storage every 60s so operator edits in the
	// UI apply without a process restart.
	// v0.52 — SIEM destination storage stays in the open core
	// (operators can configure destinations through the UI/API
	// regardless of build edition). The actual fan-out of audit
	// events to those destinations is the Compliance Pack feature.
	// wireSiemDispatcher installs a NoOp in the OSS build (events
	// stay local) and the real Splunk HEC + HMAC webhook
	// dispatcher in the Enterprise build.
	if siemCrypter, err := siem.NewCrypterFromEnv(); err != nil {
		logger.Info("SIEM destinations storage disabled (SQUADRON_SIEM_KEY unset). Generate with: head -c 32 /dev/urandom | base64")
	} else {
		siemSvc := services.NewSiemService(appStore, siemCrypter, auditService, logger)
		apiServer.SetSiemService(siemSvc)
		wireSiemDispatcher(auditService, siemSvc, logger)
	}

	// v0.42 Billing connectors. Splunk is the v0.42 ship; Datadog,
	// Honeycomb, New Relic slot in here later. Each connector is
	// optional — without config, the UI billing tile silently
	// disables.
	if config.Billing.Splunk.Enabled {
		splunkProvider, err := billing.NewSplunkSnapshotProvider(billing.SplunkConfig{
			SearchHead:         config.Billing.Splunk.SearchHead,
			Token:              config.Billing.Splunk.Token,
			WindowDays:         config.Billing.Splunk.WindowDays,
			InsecureSkipVerify: config.Billing.Splunk.InsecureSkipVerify,
		})
		if err != nil {
			logger.Warn("Splunk billing connector disabled", zap.Error(err))
		} else {
			apiServer.SetBillingProvider(splunkProvider)
			logger.Info("Splunk billing connector enabled")
		}
	}

	// v0.33 silent-agent watcher. Polls the agent table and fires
	// a webhook on healthy↔silent transitions. Disabled by default
	// — an operator opts in by setting silent_agents.enabled=true
	// + a webhook URL in squadron.yaml. The shape is the
	// silentagents.Event JSON, which an operator's webhook receiver
	// can handle alongside the existing alerting.NotificationPayload.
	if config.SilentAgents.Enabled {
		watcher := silentagents.New(silentagents.Config{
			SilenceThreshold: config.SilentAgents.SilenceThreshold,
			PollInterval:     config.SilentAgents.PollInterval,
			WebhookURL:       config.SilentAgents.WebhookURL,
			// v0.43 ChatOps. When DestinationType is unset, the
			// watcher preserves the legacy plain-JSON shape so
			// existing receivers don't break on upgrade.
			DestinationType:  config.SilentAgents.DestinationType,
			DestinationExtra: config.SilentAgents.DestinationExtra,
			PublicBaseURL:    config.SilentAgents.PublicBaseURL,
		}, appStore, logger)
		go watcher.Run(context.Background())
	} else {
		logger.Info("Silent-agent watcher disabled (set silent_agents.enabled=true to enable)")
	}

	// v0.26 AI assist — Anthropic Messages API wrapper. The
	// service is constructed unconditionally so /api/v1/ai/status
	// always responds; without an API key it just returns
	// enabled=false and every other AI route 503s with a clear
	// opt-in message. ANTHROPIC_API_KEY is the recommended
	// configuration path; LoadConfig pulls it from env at startup.
	aiService := ai.NewService(ai.Config{
		Enabled:      config.AI.Enabled,
		APIKey:       config.AI.APIKey,
		BaseURL:      config.AI.BaseURL,
		ExplainModel: config.AI.ExplainModel,
		MergeModel:   config.AI.MergeModel,
		MaxTokens:    config.AI.MaxTokens,
	}, logger)
	apiServer.SetAIService(aiService)
	if aiService.Enabled() {
		logger.Info("AI assist enabled",
			zap.String("explain_model", aiService.Capabilities().ExplainModel),
			zap.String("merge_model", aiService.Capabilities().MergeModel))
	} else {
		logger.Info("AI assist not configured (set ANTHROPIC_API_KEY + ai.enabled=true to enable)")
	}

	// v0.85 — universal discovery credential substrate. Wires the
	// credstore.Store + key onto the API server so /api/v1/discovery/*
	// routes become reachable. Opt-in: if SQUADRON_SECRETS_KEY is
	// unset, discovery stays 503 and Squadron continues to boot. This
	// keeps existing deployments working unchanged.
	if credKey, generated, err := credstore.LoadOrGenerateKey(filepath.Dir(config.Storage.App.Path)); err != nil {
		logger.Warn("discovery credstore: could not resolve a secrets key; discovery disabled",
			zap.Error(err),
			zap.String("hint", "set a valid base64 32-byte SQUADRON_SECRETS_KEY (head -c 32 /dev/urandom | base64), or ensure the data dir is writable"))
	} else {
		if generated {
			logger.Info("discovery credstore: no SQUADRON_SECRETS_KEY set — generated one and persisted it to the data dir; discovery is enabled out of the box. Set SQUADRON_SECRETS_KEY to supply your own key or share it across replicas.",
				zap.String("path", filepath.Join(filepath.Dir(config.Storage.App.Path), credstore.SecretsKeyFileName)))
		}
		credDBPath := filepath.Join(filepath.Dir(config.Storage.App.Path), "credstore.db")
		credDB, err := sql.Open("sqlite3", credDBPath)
		if err != nil {
			logger.Warn("discovery credstore: failed to open DB; discovery disabled", zap.Error(err))
		} else {
			backend := credstore.NewSQLiteSecretsBackend(credKey)
			credStore, err := credstore.NewStore(context.Background(), credDB, backend, auditService)
			if err != nil {
				logger.Warn("discovery credstore: NewStore failed; discovery disabled", zap.Error(err))
			} else {
				apiServer.SetDiscoveryCredStore(credStore)
				apiServer.SetDiscoveryCredKey(credKey)
				logger.Info("discovery credstore wired", zap.String("path", credDBPath))
			}
		}

		// v0.89.3 Stream 19 (#603) — Connect IaC repo substrate.
		// Shares the same SQUADRON_SECRETS_KEY as the credstore
		// above (the GitHub PAT is sealed by the same Key the AWS
		// credentials use). The data lives in a separate SQLite DB
		// (iacconnstore.db) so an operator wiping IaC connections
		// does not touch the credstore.
		iacDBPath := filepath.Join(filepath.Dir(config.Storage.App.Path), "iacconnstore.db")
		iacStore, ierr := iacconnstore.NewSQLiteStore(iacconnstore.Config{DBPath: iacDBPath, Logger: logger})
		if ierr != nil {
			logger.Warn("iac connection substrate: NewSQLiteStore failed; IaC connect disabled", zap.Error(ierr))
		} else {
			apiServer.SetIaCConnStore(iacStore)
			logger.Info("iac connection substrate wired", zap.String("path", iacDBPath))
		}

		// v0.89.23 #639 Stream 40 — wire the GitHub webhook listener
		// secret. The /api/v1/webhooks/github route is mounted
		// unconditionally by server.go; an empty secret leaves it in
		// a 503-with-guidance state so the operator's GitHub webhook
		// delivery log carries the actionable pointer rather than a
		// silent no-op. Slice 1 ships one shared deployment-wide
		// secret; per-connection secrets are slice 2. The secret
		// bytes are NEVER logged.
		if whSecret := os.Getenv("SQUADRON_GITHUB_WEBHOOK_SECRET"); whSecret != "" {
			apiServer.SetIaCGitHubWebhookSecret([]byte(whSecret))
			logger.Info("iac github webhook listener: secret wired (PR-merged events will be recorded)")
		} else {
			logger.Info("iac github webhook listener: SQUADRON_GITHUB_WEBHOOK_SECRET not set; /api/v1/webhooks/github will 503 until configured")
		}

		// v0.89.30 (#649) — wire the webhook delivery dedupe store
		// and start the background GC sweeper. The store is the same
		// applicationstore the rest of the server uses; the dedupe
		// table lives in app.db alongside audit_events. The GC
		// goroutine runs on a 24h ticker and deletes rows older than
		// 7 days (webhookDedupeRetention in the handler). Threat
		// closed: a compromised TLS terminator or intermediary proxy
		// captures + replays a legitimate signed delivery. See
		// docs/webhook-listener.md §"Slice 2 roadmap".
		apiServer.SetIaCGitHubWebhookStore(appStore)
		webhookDedupeGCCtx, webhookDedupeGCCancel := context.WithCancel(context.Background())
		handlers.StartWebhookDedupeGC(webhookDedupeGCCtx, appStore, logger)
		defer webhookDedupeGCCancel()
		logger.Info("iac github webhook listener: dedupe store wired and GC sweeper started",
			zap.Duration("retention", 7*24*time.Hour),
			zap.Duration("gc_interval", 24*time.Hour),
		)

		// v0.89.219 — per-cloud discovery connection substrates +
		// production scanner factories. AWS discovery rides the
		// universal credstore wired above; GCP / Azure / OCI each
		// carry a dedicated connection store + a ScannerFactory the
		// discovery trampolines in server.go late-bind. Composing them
		// here is the deferred "main.go composes the concrete factory
		// once both chunks land" step (see the
		// Set{GCP,Azure,OCI}DiscoveryScannerFactory godoc) — without it
		// the GCP / Azure / OCI discovery surfaces 503 with "<cloud>
		// discovery is not configured", which is what every release
		// since v0.89.47 / .52 / .57 shipped. Each store shares the
		// SQUADRON_SECRETS_KEY wired above (secrets sealed under the
		// same Key); each lives in its own SQLite DB so wiping one
		// cloud's connections never touches another's.
		discoveryBaseDir := filepath.Dir(config.Storage.App.Path)
		if gcpStore, gerr := gcpconnstore.NewSQLiteStore(gcpconnstore.Config{DBPath: filepath.Join(discoveryBaseDir, "gcpconnstore.db"), Logger: logger}); gerr != nil {
			logger.Warn("gcp discovery substrate: NewSQLiteStore failed; GCP discovery disabled", zap.Error(gerr))
		} else {
			apiServer.SetGCPDiscoveryStore(gcpStore)
			apiServer.SetGCPDiscoveryScannerFactory(scannerfactory.GCPFactory{})
			logger.Info("gcp discovery substrate wired", zap.String("path", filepath.Join(discoveryBaseDir, "gcpconnstore.db")))
		}
		if azStore, aerr := azureconnstore.NewSQLiteStore(azureconnstore.Config{DBPath: filepath.Join(discoveryBaseDir, "azureconnstore.db"), Logger: logger}); aerr != nil {
			logger.Warn("azure discovery substrate: NewSQLiteStore failed; Azure discovery disabled", zap.Error(aerr))
		} else {
			apiServer.SetAzureDiscoveryStore(azStore)
			apiServer.SetAzureDiscoveryScannerFactory(scannerfactory.AzureFactory{})
			logger.Info("azure discovery substrate wired", zap.String("path", filepath.Join(discoveryBaseDir, "azureconnstore.db")))
		}
		if ociStore, oerr := ociconnstore.NewSQLiteStore(ociconnstore.Config{DBPath: filepath.Join(discoveryBaseDir, "ociconnstore.db"), Logger: logger}); oerr != nil {
			logger.Warn("oci discovery substrate: NewSQLiteStore failed; OCI discovery disabled", zap.Error(oerr))
		} else {
			apiServer.SetOCIDiscoveryStore(ociStore)
			apiServer.SetOCIDiscoveryScannerFactory(scannerfactory.OCIFactory{})
			logger.Info("oci discovery substrate wired", zap.String("path", filepath.Join(discoveryBaseDir, "ociconnstore.db")))
		}
	}

	// v0.85 Stream 2F — wire the AI service onto the discovery
	// surface so the recommendations route can call
	// ProposeFromDiscoveryScan. Mirrors the credstore wire-up above;
	// the call is unconditional because *ai.Service short-circuits
	// with ErrDisabled when AI isn't configured (the trampoline 503s
	// for the recommendations route in that case). Keeping the
	// wire-up right beside the credstore block closes the gap that
	// prompted the credstore follow-up commit — if discovery is
	// wired but AI isn't passed through here, the recommendations
	// route returns a misleading "not configured" payload at the
	// trampoline rather than at the proposer's own ErrDisabled
	// surface.
	apiServer.SetDiscoveryAIService(aiService)

	// v0.54 Move 3 — incident drafter bridge. After an action runs
	// on a node, the bridge asks the AI drafter for a postmortem
	// style ticket draft. The operator reviews the draft through
	// the /api/v1/incidents/drafts endpoints (and the UI inbox in a
	// follow up chunk). Disabled when the AI service is disabled;
	// the bridge's tick is a clean no op in that case so we always
	// construct it.
	incidentsBridge, err := incidents.New(
		aiService,
		appStore,
		rolloutService,
		auditService,
		incidents.DefaultConfig(),
		logger,
	)
	if err != nil {
		logger.Fatal("failed to construct incidents bridge", zap.Error(err))
	}
	incidentsCtx, incidentsCancel := context.WithCancel(context.Background())
	incidentsBridge.Start(incidentsCtx)
	defer func() {
		incidentsCancel()
		incidentsBridge.Stop()
	}()
	logger.Info("incidents drafter bridge started",
		zap.Bool("ai_enabled", aiService.Enabled()))

	// SQ-3.9 — incident drafter publishers. Clipboard is always
	// registered. The GitHub Issues publisher is registered when
	// all three SQUADRON_GITHUB_ISSUES_* env vars are set. The
	// Linear publisher is registered when SQUADRON_LINEAR_API_KEY
	// and SQUADRON_LINEAR_TEAM_ID are set. Jira stays stamp only
	// for now; its UI option remains available and the handler
	// falls back to stamping the operator supplied external_id /
	// external_url.
	publishers := incidents.NewPublisherRegistry()
	if owner, repo, token := os.Getenv("SQUADRON_GITHUB_ISSUES_OWNER"),
		os.Getenv("SQUADRON_GITHUB_ISSUES_REPO"),
		os.Getenv("SQUADRON_GITHUB_ISSUES_TOKEN"); owner != "" && repo != "" && token != "" {
		var labels []string
		if raw := os.Getenv("SQUADRON_GITHUB_ISSUES_LABELS"); raw != "" {
			labels = strings.Split(raw, ",")
			for i := range labels {
				labels[i] = strings.TrimSpace(labels[i])
			}
		}
		ghPub, err := incidents.NewGitHubIssuesPublisher(incidents.GitHubIssuesConfig{
			Owner:  owner,
			Repo:   repo,
			Token:  token,
			Labels: labels,
		})
		if err != nil {
			logger.Warn("github issues publisher not enabled", zap.Error(err))
		} else {
			publishers.Register(ghPub)
			logger.Info("github issues publisher registered",
				zap.String("owner", owner),
				zap.String("repo", repo))
		}
	}
	// v0.55 N6 — Linear publisher. Reads SQUADRON_LINEAR_API_KEY
	// (personal API key, lin_api_xxxx) and SQUADRON_LINEAR_TEAM_ID.
	// Optional SQUADRON_LINEAR_LABEL_IDS is a comma separated list
	// of label IDs (not names; Linear references labels by ID).
	if apiKey, teamID := os.Getenv("SQUADRON_LINEAR_API_KEY"),
		os.Getenv("SQUADRON_LINEAR_TEAM_ID"); apiKey != "" && teamID != "" {
		var labelIDs []string
		if raw := os.Getenv("SQUADRON_LINEAR_LABEL_IDS"); raw != "" {
			labelIDs = strings.Split(raw, ",")
			for i := range labelIDs {
				labelIDs[i] = strings.TrimSpace(labelIDs[i])
			}
		}
		linPub, err := incidents.NewLinearPublisher(incidents.LinearConfig{
			APIKey:   apiKey,
			TeamID:   teamID,
			LabelIDs: labelIDs,
		})
		if err != nil {
			logger.Warn("linear publisher not enabled", zap.Error(err))
		} else {
			publishers.Register(linPub)
			logger.Info("linear publisher registered",
				zap.String("team_id", teamID))
		}
	}
	// v0.56 — Jira Cloud publisher. Reads SQUADRON_JIRA_BASE_URL
	// (tenant URL like https://acme.atlassian.net),
	// SQUADRON_JIRA_EMAIL, SQUADRON_JIRA_API_TOKEN,
	// SQUADRON_JIRA_PROJECT_KEY. Optional SQUADRON_JIRA_ISSUE_TYPE
	// (default Task) and SQUADRON_JIRA_LABELS (comma separated
	// names) refine routing.
	if baseURL, email, token, projectKey := os.Getenv("SQUADRON_JIRA_BASE_URL"),
		os.Getenv("SQUADRON_JIRA_EMAIL"),
		os.Getenv("SQUADRON_JIRA_API_TOKEN"),
		os.Getenv("SQUADRON_JIRA_PROJECT_KEY"); baseURL != "" && email != "" && token != "" && projectKey != "" {
		var labels []string
		if raw := os.Getenv("SQUADRON_JIRA_LABELS"); raw != "" {
			labels = strings.Split(raw, ",")
			for i := range labels {
				labels[i] = strings.TrimSpace(labels[i])
			}
		}
		jiraPub, err := incidents.NewJiraPublisher(incidents.JiraConfig{
			BaseURL:    baseURL,
			Email:      email,
			APIToken:   token,
			ProjectKey: projectKey,
			IssueType:  os.Getenv("SQUADRON_JIRA_ISSUE_TYPE"),
			Labels:     labels,
		})
		if err != nil {
			logger.Warn("jira publisher not enabled", zap.Error(err))
		} else {
			publishers.Register(jiraPub)
			logger.Info("jira publisher registered",
				zap.String("base_url", baseURL),
				zap.String("project_key", projectKey))
		}
	}
	apiServer.SetIncidentsPublishers(publishers)

	// SQ-1.4g — AI proposer bridge (Move 1). Polls open cost spike
	// events, asks the AI proposer to draft a staged rollout for
	// each, and posts the draft through services.RolloutService.
	// The proposal lands in pending_approval with proposed_by=ai;
	// a human approves; the existing rollout engine handles the
	// staged push. Disabled when the AI service is disabled; the
	// bridge's Start method is a clean no op in that case.
	proposerBridge := proposer.New(
		aiService,
		appStore,
		rolloutService,
		auditService,
		proposer.DefaultConfig(),
		logger,
	)
	proposerCtx, proposerCancel := context.WithCancel(context.Background())
	proposerBridge.Start(proposerCtx)
	defer func() {
		proposerCancel()
		_ = proposerBridge.Stop(5 * time.Second)
	}()
	logger.Info("AI proposer bridge started",
		zap.Bool("ai_enabled", aiService.Enabled()))

	// Start API server in a goroutine
	go func() {
		if err := apiServer.Start(fmt.Sprintf("%d", config.Server.HTTPPort)); err != nil {
			logger.Fatal("Failed to start API server", zap.Error(err))
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = apiServer.Stop(ctx)
	}()

	// Start alert evaluator. Evaluates each enabled rule on its configured
	// cadence and dispatches firing/resolved notifications, and publishes
	// AlertFired/AlertResolved events to the broker for the UI's live feed.
	// Alert evaluation tracer — span per evaluation cycle. Reuses
	// the self-telemetry tracer provider so alert evaluation traces
	// show up alongside rollouts and audit events. Nil tracer when
	// selftelPub is disabled.
	alertsTracer := alerts.NewTracer(selftelPub.Tracer("squadron/alerts"))
	alertEvaluator := alerting.NewEvaluatorWithTracer(alertService, telemetryService, alertMetrics, eventBroker, auditService, alertsTracer, logger)
	alertEvaluator.Start()
	defer func() {
		if err := alertEvaluator.Stop(10 * time.Second); err != nil {
			logger.Error("Failed to stop alert evaluator", zap.Error(err))
		}
	}()

	// Start the rollout engine. Walks active rollouts, advances stages,
	// and triggers automatic rollback when abort criteria fire. Uses the
	// OpAMP ConfigSender as its AgentCommander, the telemetry adapter for
	// error-rate criteria, and publishes RolloutStateChanged events to
	// the broker so the UI sees engine actions in real time.
	rolloutTelemetry := rollouts.NewTelemetryAdapter(telemetryReader)
	// rolloutTracer was constructed above alongside rolloutService —
	// reusing the same instance ensures service-layer span events
	// (pause / resume / abort) land on the same parent span the
	// engine opened.
	rolloutEngine := rollouts.NewEngine(rolloutService, agentService, auditService, appStore, rolloutTelemetry, configSender, eventBroker, rolloutTracer, configsTracer, logger)
	// v0.52 — wire engine-level Compliance Pack extension points
	// (currently the change-window provider). OSS installs NoOp;
	// Compliance Pack installs a store-backed provider.
	wireEngineExtensions(rolloutEngine)
	// v0.89.14 (#630) — wire the plan-engine boundary to the
	// action runner substrate. Pure rollout plans run identically
	// whether the dispatcher is wired or not; the boundary only
	// engages when a plan step has step_kind=action. When the
	// signer is nil (SQUADRON_ACTION_SIGNING_KEY unset and
	// SQUADRON_ACTION_SIGNER_AUTOGEN_ALLOW unset in production),
	// dispatcher construction is skipped — action-step plans
	// will fail dispatch at engine time with a clean log line and
	// abort the step.
	if actionSigner != nil {
		planActionDispatcher := services.NewPlanActionDispatcher(
			appStore, actionSigner, nil /* registry: actions.Default */, auditService, logger)
		rolloutEngine.SetActionDispatcher(planActionDispatcher)
		logger.Info("plan engine action dispatcher wired",
			zap.String("signer_fingerprint", actionSigner.Fingerprint()))
	}
	rolloutEngine.Start()
	defer func() {
		if err := rolloutEngine.Stop(10 * time.Second); err != nil {
			logger.Error("Failed to stop rollout engine", zap.Error(err))
		}
	}()

	// Start background services
	go startRollupGenerator(telemetryService, config, logger)
	go startCleanupTask(telemetryService, config, logger)

	logger.Info("Squadron is running",
		zap.Int("opamp_port", config.Server.OpAMPPort),
		zap.Int("otlp_grpc_port", grpcPort),
		zap.Int("otlp_http_port", httpPort),
		zap.Int("api_port", config.Server.HTTPPort))

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutting down Squadron...")
	return nil
}

// versionCommand returns the version subcommand
func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("%s v%s\n", appName, version)
		},
	}
}

// selftelAdapter bridges the selftel.Publisher to the
// services.SelfTelemetryPublisher interface. The two packages can't
// directly know about each other's types (services/ lives below
// selftel/ in the dependency graph), so this thin wiring layer
// translates one entry struct to the other at runtime.
type selftelAdapter struct {
	pub *selftel.Publisher
}

func (a *selftelAdapter) PublishAuditEvent(ctx context.Context, entry services.SelfTelemetryEntry) {
	a.pub.PublishAuditEvent(ctx, selftel.AuditEntry{
		Actor:      entry.Actor,
		EventType:  entry.EventType,
		TargetType: entry.TargetType,
		TargetID:   entry.TargetID,
		Action:     entry.Action,
		Payload:    entry.Payload,
	})
}

// traceIndexAuditAdapter bridges the rich services.AuditService
// interface down to the minimal traceindex.AuditEmitter shape. The
// shim exists because internal/traceindex cannot import
// internal/services without creating a cycle (services depends on
// the application store which depends on traceindex.ResourceRow).
// Each Record call lands an AuditEntry shaped per design doc §8 —
// actor "system", TargetType empty (fleet-wide), Action
// "flushed" (the verb the timeline humanizer renders).
type traceIndexAuditAdapter struct {
	audit services.AuditService
}

func (a *traceIndexAuditAdapter) Record(ctx context.Context, eventType, actor string, payload map[string]any) {
	if a == nil || a.audit == nil {
		return
	}
	_ = a.audit.Record(ctx, services.AuditEntry{
		Actor:     actor,
		EventType: eventType,
		Action:    "flushed",
		Payload:   payload,
	})
}

// bootstrapAuthToken issues a labeled token when the application store
// is empty. This solves the "first run" chicken-and-egg: with auth
// enabled, the operator can't reach the token-creation API without a
// token, so Squadron emits one to stderr at startup. The label
// "bootstrap" is intentionally generic — operators are expected to
// rotate to properly-labeled tokens and revoke this one immediately.
//
// Quiet on every subsequent start; only fires when the tokens table is
// empty. Recovery from "all tokens lost" is documented in docs/auth.md
// (revoke at the SQLite level + restart).
func bootstrapAuthToken(ctx context.Context, authService services.AuthService, logger *zap.Logger) error {
	tokens, err := authService.List(ctx)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	if len(tokens) > 0 {
		return nil
	}
	// Bootstrap token gets the wildcard so the operator can do
	// anything — including create properly-scoped replacement tokens.
	// No expiry: a bootstrap token's job is to recover the auth flow
	// after upgrades, and one expiring in the middle of an incident
	// would defeat the purpose. Operators are expected to revoke it
	// after issuing proper scoped tokens.
	_, plaintext, err := authService.Issue(ctx, "bootstrap", []string{services.ScopeWildcard}, nil)
	if err != nil {
		return fmt.Errorf("issue bootstrap token: %w", err)
	}
	// Loud on purpose. Operators NEED to see this — it's the only path
	// to authenticating to a freshly-enabled Squadron. We write to the
	// logger at Warn level so it shows up under default log levels and
	// in any aggregator. Operators should revoke this token after
	// creating properly-labeled ones via the /auth/tokens UI.
	logger.Warn("API auth is enabled and no tokens exist yet — issued a bootstrap token. Revoke it after creating your real tokens.",
		zap.String("bootstrap_token", plaintext))
	return nil
}

// configCommand returns the config subcommand
func configCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Print current configuration",
		Run: func(cmd *cobra.Command, args []string) {
			configPath := viper.GetString("config")
			_, err := config.LoadConfig(configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
				os.Exit(1)
			}
			// TODO: Pretty print configuration
			fmt.Printf("Configuration loaded from: %s\n", configPath)
		},
	}
}

// startRollupGenerator periodically generates rollups for metrics
func startRollupGenerator(telemetryService services.TelemetryQueryService, config *config.Config, logger *zap.Logger) {
	if !config.Rollups.Enabled {
		logger.Info("Rollup generation is disabled")
		return
	}

	logger.Info("Starting rollup generator")
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		ctx := context.Background()
		now := time.Now()

		// Generate rollups based on time intervals
		if err := generateRollup(ctx, telemetryService, "1m", now, logger); err != nil {
			logger.Error("Failed to generate 1m rollup", zap.Error(err))
		}

		if now.Minute()%5 == 0 {
			if err := generateRollup(ctx, telemetryService, "5m", now, logger); err != nil {
				logger.Error("Failed to generate 5m rollup", zap.Error(err))
			}
		}

		if now.Minute() == 0 {
			if err := generateRollup(ctx, telemetryService, "1h", now, logger); err != nil {
				logger.Error("Failed to generate 1h rollup", zap.Error(err))
			}
		}
	}
}

// generateRollup creates pre-aggregated rollups for the given interval over the
// most recently completed window. The window aligned to the interval boundary
// — e.g. for a "1m" interval called at 10:23:15, we roll up the window starting
// at 10:22:00.
func generateRollup(ctx context.Context, telemetryService services.TelemetryQueryService, interval string, now time.Time, logger *zap.Logger) error {
	var (
		rollupInterval services.RollupInterval
		windowStart    time.Time
	)
	switch interval {
	case "1m":
		rollupInterval = services.RollupInterval1m
		windowStart = now.Truncate(time.Minute).Add(-time.Minute)
	case "5m":
		rollupInterval = services.RollupInterval5m
		windowStart = now.Truncate(5 * time.Minute).Add(-5 * time.Minute)
	case "1h":
		rollupInterval = services.RollupInterval1h
		windowStart = now.Truncate(time.Hour).Add(-time.Hour)
	default:
		return fmt.Errorf("unsupported rollup interval %q", interval)
	}

	logger.Debug("Generating rollup",
		zap.String("interval", interval),
		zap.Time("window_start", windowStart))
	return telemetryService.CreateRollups(ctx, windowStart, rollupInterval)
}

// startCleanupTask periodically cleans up old data using retention configured
// in config.Retention. The current telemetry store interface accepts a single
// duration covering all signals, so we use the longest of the configured
// retentions as a conservative ceiling — nothing is deleted before any signal
// type's retention has expired. Per-signal retention enforcement is tracked
// for a follow-up storage-interface change.
func startCleanupTask(telemetryService services.TelemetryQueryService, config *config.Config, logger *zap.Logger) {
	retention := cleanupRetention(config.Retention, logger)
	if retention <= 0 {
		logger.Info("Cleanup task disabled (no retention configured)")
		return
	}

	logger.Info("Starting cleanup task", zap.Duration("retention", retention))
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		ctx := context.Background()
		if err := telemetryService.CleanupOldData(ctx, retention); err != nil {
			logger.Error("Failed to cleanup old telemetry data", zap.Error(err))
		} else {
			logger.Debug("Cleaned up old telemetry data", zap.Duration("retention", retention))
		}
	}
}

// cleanupRetention returns the longest configured retention across all signal
// classes, falling back to 24h if nothing is parseable. Unparseable individual
// fields are logged and ignored rather than crashing the cleanup loop.
func cleanupRetention(r config.RetentionConfig, logger *zap.Logger) time.Duration {
	const fallback = 24 * time.Hour
	candidates := map[string]string{
		"raw_metrics": r.RawMetrics,
		"raw_logs":    r.RawLogs,
		"rollups_1m":  r.Rollups1m,
		"rollups_5m":  r.Rollups5m,
	}

	var max time.Duration
	for name, raw := range candidates {
		if raw == "" {
			continue
		}
		d, err := config.ParseDuration(raw)
		if err != nil {
			logger.Warn("Failed to parse retention setting; ignoring",
				zap.String("setting", name),
				zap.String("value", raw),
				zap.Error(err))
			continue
		}
		if d > max {
			max = d
		}
	}
	if max == 0 {
		logger.Warn("No valid retention setting found; using fallback", zap.Duration("retention", fallback))
		return fallback
	}
	return max
}

// recsDismissalsAdapter bridges the application store's dismissals
// CRUD to the recommendations.Dismissals interface. The engine has
// no reason to know about the wider applicationstore — keeping this
// adapter tiny and in main.go means the engine package stays
// import-free of the storage layer.
type recsDismissalsAdapter struct {
	store interface {
		IsRecommendationDismissed(ctx context.Context, recommendationID string) (bool, error)
	}
}

func (a recsDismissalsAdapter) IsDismissed(ctx context.Context, recID string) (bool, error) {
	return a.store.IsRecommendationDismissed(ctx, recID)
}

// parseOTLPPort extracts the port from a "host:port" string. Empty
// or malformed values fall through to the supplied default. Used to
// honor the otlp.grpc_endpoint / otlp.http_endpoint config so
// operators can shift ports without editing the binary.
func parseOTLPPort(endpoint string, def int) int {
	if endpoint == "" {
		return def
	}
	_, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return def
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return def
	}
	return port
}

// pipelineHealthAgentLister wraps the application store as a
// pipelinehealth.AgentLister. Keeping this adapter in main.go keeps
// the pipelinehealth package import-free of the storage layer — the
// service only needs string IDs, not full Agent records, so we map
// down to that shape here.
type pipelineHealthAgentLister struct {
	store interface {
		ListAgents(ctx context.Context) ([]*applicationstore.Agent, error)
	}
}

// AllAgentIDs returns every known agent UUID as a string. Errors
// from the underlying store are propagated — the pipeline-health
// service treats them as non-fatal and falls back to surfacing only
// agents with samples.
func (a pipelineHealthAgentLister) AllAgentIDs(ctx context.Context) ([]string, error) {
	agents, err := a.store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(agents))
	for _, ag := range agents {
		if ag == nil {
			continue
		}
		ids = append(ids, ag.ID.String())
	}
	return ids, nil
}
