package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/alerting"
	"github.com/devopsmike2/squadron/internal/api"
	"github.com/devopsmike2/squadron/internal/config"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/rollouts"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/opamp"
	"github.com/devopsmike2/squadron/internal/otlp/receiver"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/telemetrystore"
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

	// AuditService records every state change. Constructed before the
	// other services so they can publish into it via injection. Wired
	// with the same broker so timeline UIs append entries in real time
	// over SSE.
	auditService := services.NewAuditService(appStore, eventBroker, logger)

	// Create agent service. driftMetrics is wired so drift transitions and
	// fleet drift state appear on /metrics. The event broker receives
	// agent_registered and agent_drift_changed events for the SSE stream;
	// auditService records the same transitions as durable log entries.
	agentService := services.NewAgentService(appStore, driftMetrics, eventBroker, auditService, logger)
	savedQueryService := services.NewSavedQueryService(appStore, logger)
	alertService := services.NewAlertService(appStore, logger)
	rolloutService := services.NewRolloutService(appStore, agentService, auditService, logger)

	// Create config sender (separate concern from AgentService)
	configSender := opamp.NewConfigSender(agents, logger)

	// Create OpAMP server with agent service (for persistence)
	opampServer, err := opamp.NewServer(agents, agentService, opampMetrics, agentGRPCEndpoint, agentHTTPEndpoint, logger)
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

	// Initialize OTLP receivers (parsing and enrichment happen in worker pool)
	grpcServer, err := receiver.NewGRPCServer(4317, otlpMetrics, workerPool, logger)
	if err != nil {
		logger.Fatal("Failed to create gRPC server", zap.Error(err))
	}
	if err := grpcServer.Start(); err != nil {
		logger.Fatal("Failed to start gRPC server", zap.Error(err))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = grpcServer.Stop(ctx)
	}()

	httpServer, err := receiver.NewHTTPServer(4318, otlpMetrics, workerPool, logger)
	if err != nil {
		logger.Fatal("Failed to create HTTP server", zap.Error(err))
	}
	if err := httpServer.Start(); err != nil {
		logger.Fatal("Failed to start HTTP server", zap.Error(err))
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Stop(ctx)
	}()

	// Initialize HTTP API server.
	// Share the same Prometheus registry so that /metrics exposes OpAMP, OTLP,
	// worker, and API metrics in a single endpoint. The event broker is
	// shared with publishers so /events/stream reflects what they emit.
	apiServer := api.NewServer(agentService, telemetryService, savedQueryService, alertService, auditService, rolloutService, configSender, eventBroker, registry, logger)

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
	alertEvaluator := alerting.NewEvaluator(alertService, telemetryService, alertMetrics, eventBroker, auditService, logger)
	alertEvaluator.Start()
	defer func() {
		if err := alertEvaluator.Stop(10 * time.Second); err != nil {
			logger.Error("Failed to stop alert evaluator", zap.Error(err))
		}
	}()

	// Start the rollout engine. Walks active rollouts, advances stages,
	// and triggers automatic rollback when abort criteria fire. Uses the
	// OpAMP ConfigSender as its AgentCommander.
	rolloutEngine := rollouts.NewEngine(rolloutService, agentService, auditService, appStore, configSender, logger)
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
		zap.Int("otlp_grpc_port", 4317),
		zap.Int("otlp_http_port", 4318),
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
