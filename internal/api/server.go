package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/api/handlers"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/services"
)

// AgentCommander defines the interface for sending commands to agents
type AgentCommander interface {
	SendConfigToAgent(agentId uuid.UUID, configContent string) error
	RestartAgent(agentId uuid.UUID) error
	RestartAgentsInGroup(groupId string) ([]uuid.UUID, []error)
	SendConfigToAgentsInGroup(groupId string, configContent string) ([]uuid.UUID, []error)
}

// Server represents the HTTP API server
type Server struct {
	router            *gin.Engine
	agentService      services.AgentService
	telemetryService  services.TelemetryQueryService
	savedQueryService services.SavedQueryService
	alertService      services.AlertService
	auditService      services.AuditService
	commander         AgentCommander
	broker            *events.Broker
	logger            *zap.Logger
	httpServer        *http.Server
	metrics           *metrics.APIMetrics
	registry          *prometheus.Registry
}

// NewServer creates a new API server.
//
// The caller owns the Prometheus registry — pass the same registry used to
// register OpAMP, OTLP, and worker metrics so that /metrics exposes a single,
// unified view of the process. (Previously this constructor created its own
// registry, which silently hid every non-API metric from /metrics.)
func NewServer(agentService services.AgentService, telemetryService services.TelemetryQueryService, savedQueryService services.SavedQueryService, alertService services.AlertService, auditService services.AuditService, commander AgentCommander, broker *events.Broker, registry *prometheus.Registry, logger *zap.Logger) *Server {
	// Set Gin to release mode for production
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()

	// Initialize API metrics on the caller-provided registry.
	metricsFactory := metrics.NewPrometheusFactory("squadron", registry)
	apiMetrics := metrics.NewAPIMetrics(metricsFactory)

	// Add middleware
	router.Use(gin.Recovery())
	router.Use(corsMiddleware())
	router.Use(loggingMiddleware(logger))

	server := &Server{
		router:            router,
		agentService:      agentService,
		telemetryService:  telemetryService,
		savedQueryService: savedQueryService,
		alertService:      alertService,
		auditService:      auditService,
		commander:         commander,
		broker:            broker,
		logger:            logger,
		metrics:           apiMetrics,
		registry:          registry,
	}

	// Add metrics middleware
	router.Use(server.metricsMiddleware())

	// Register routes
	server.registerRoutes()

	return server
}

// Start starts the HTTP server
func (s *Server) Start(port string) error {
	s.httpServer = &http.Server{
		Addr:    ":" + port,
		Handler: s.router,
	}

	s.logger.Info("Starting HTTP API server", zap.String("port", port))
	return s.httpServer.ListenAndServe()
}

// Stop gracefully stops the HTTP server
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("Stopping HTTP API server")

	// Create a context with timeout for graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return s.httpServer.Shutdown(shutdownCtx)
}

// registerRoutes registers all API routes
func (s *Server) registerRoutes() {
	// Initialize handlers
	agentHandlers := handlers.NewAgentHandlers(s.agentService, s.commander, s.logger)
	configHandlers := handlers.NewConfigHandlers(s.agentService, s.commander, s.logger)
	telemetryHandlers := handlers.NewTelemetryHandlers(s.telemetryService, s.logger)
	squadronQLHandlers := handlers.NewSquadronQLHandlers(s.telemetryService, s.logger)
	groupHandlers := handlers.NewGroupHandlers(s.agentService, s.commander, s.logger)
	savedQueryHandlers := handlers.NewSavedQueryHandlers(s.savedQueryService, s.logger)
	topologyHandlers := handlers.NewTopologyHandlers(s.agentService, s.telemetryService, s.logger)
	healthHandlers := handlers.NewHealthHandlers(s.agentService, s.telemetryService, s.logger)
	alertHandlers := handlers.NewAlertHandlers(s.alertService, s.logger)
	auditHandlers := handlers.NewAuditHandlers(s.auditService, s.logger)
	eventsHandlers := handlers.NewEventsHandlers(s.broker, s.logger)

	// Metrics endpoint
	s.router.GET("/metrics", gin.WrapH(promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})))

	// Health check
	s.router.GET("/health", healthHandlers.HandleHealth)

	// API v1 routes
	v1 := s.router.Group("/api/v1")
	{
		// Agent routes
		agents := v1.Group("/agents")
		{
			agents.GET("", agentHandlers.HandleGetAgents)
			agents.GET("/stats", agentHandlers.HandleGetAgentStats) // Must come before /:id
			agents.GET("/:id", agentHandlers.HandleGetAgent)
			agents.PATCH("/:id/group", agentHandlers.HandleUpdateAgentGroup)
			agents.POST("/:id/config", agentHandlers.HandleSendConfigToAgent)
			agents.POST("/:id/restart", agentHandlers.HandleRestartAgent)
		}

		// Config routes
		configs := v1.Group("/configs")
		{
			configs.GET("", configHandlers.HandleGetConfigs)
			configs.POST("", configHandlers.HandleCreateConfig)
			configs.POST("/validate", configHandlers.HandleValidateConfig) // Must come before /:id
			configs.POST("/lint", configHandlers.HandleLintConfig)         // Anti-pattern + structural lint
			configs.GET("/templates", configHandlers.HandleGetConfigTemplates)
			configs.GET("/templates/:id", configHandlers.HandleGetConfigTemplate)
			configs.GET("/versions", configHandlers.HandleGetConfigVersions)
			configs.GET("/:id", configHandlers.HandleGetConfig)
			configs.PUT("/:id", configHandlers.HandleUpdateConfig)
			configs.DELETE("/:id", configHandlers.HandleDeleteConfig)
		}

		// Telemetry routes
		telemetry := v1.Group("/telemetry")
		{
			// Legacy endpoints
			telemetry.POST("/metrics/query", telemetryHandlers.HandleQueryMetrics)			// Saved query library
			savedQueries := telemetry.Group("/saved-queries")
			{
				savedQueries.GET("", savedQueryHandlers.HandleListSavedQueries)
				savedQueries.POST("", savedQueryHandlers.HandleCreateSavedQuery)
				savedQueries.PUT("/:id", savedQueryHandlers.HandleUpdateSavedQuery)
				savedQueries.DELETE("/:id", savedQueryHandlers.HandleDeleteSavedQuery)
			}


			telemetry.POST("/logs/query", telemetryHandlers.HandleQueryLogs)
			telemetry.POST("/traces/query", telemetryHandlers.HandleQueryTraces)
			telemetry.GET("/overview", telemetryHandlers.HandleGetTelemetryOverview)
			telemetry.GET("/services", telemetryHandlers.HandleGetServices)

			// Squadron QL endpoints
			telemetry.POST("/query", squadronQLHandlers.HandleSquadronQLQuery)
			telemetry.POST("/query/validate", squadronQLHandlers.HandleValidateQuery)
			telemetry.POST("/query/suggestions", squadronQLHandlers.HandleGetSuggestions)
			telemetry.GET("/query/templates", squadronQLHandlers.HandleGetTemplates)
			telemetry.GET("/query/functions", squadronQLHandlers.HandleGetFunctions)
		}

		// Group routes
		groups := v1.Group("/groups")
		{
			groups.GET("", groupHandlers.HandleGetGroups)
			groups.POST("", groupHandlers.HandleCreateGroup)
			groups.GET("/:id", groupHandlers.HandleGetGroup)
			groups.PUT("/:id", groupHandlers.HandleUpdateGroup)
			groups.DELETE("/:id", groupHandlers.HandleDeleteGroup)
			groups.POST("/:id/config", groupHandlers.HandleAssignConfig)
			groups.GET("/:id/config", groupHandlers.HandleGetGroupConfig)
			groups.GET("/:id/agents", groupHandlers.HandleGetGroupAgents)
			groups.POST("/:id/restart", groupHandlers.HandleRestartGroup)
		}

		// Topology routes
		topology := v1.Group("/topology")
		{
			topology.GET("", topologyHandlers.HandleGetTopology)
			topology.GET("/agent/:id", topologyHandlers.HandleGetAgentTopology)
			topology.GET("/group/:id", topologyHandlers.HandleGetGroupTopology)
		}

		// Alert rule routes
		alerts := v1.Group("/alerts/rules")
		{
			alerts.GET("", alertHandlers.HandleListAlertRules)
			alerts.POST("", alertHandlers.HandleCreateAlertRule)
			alerts.GET("/:id", alertHandlers.HandleGetAlertRule)
			alerts.PUT("/:id", alertHandlers.HandleUpdateAlertRule)
			alerts.DELETE("/:id", alertHandlers.HandleDeleteAlertRule)
		}

		// Real-time event stream (Server-Sent Events).
		v1.GET("/events/stream", eventsHandlers.HandleStream)

		// Audit log — read-only.
		audit := v1.Group("/audit")
		{
			audit.GET("/events", auditHandlers.HandleListAuditEvents)
		}
	}

	// Serve static files for the UI
	s.router.Static("/assets", "./ui/dist/assets")

	// SPA catch-all route - must be last
	s.router.NoRoute(func(c *gin.Context) {
		// Check if file exists
		filePath := filepath.Join("./ui/dist", c.Request.URL.Path)
		if _, err := os.Stat(filePath); err == nil {
			c.File(filePath)
			return
		}

		// Serve index.html for all other routes (SPA routing)
		c.File("./ui/dist/index.html")
	})
}

// corsMiddleware adds CORS headers
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// loggingMiddleware adds request logging with reduced verbosity
func loggingMiddleware(logger *zap.Logger) gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		// Skip logging for health checks and other frequent, low-value requests
		if param.Path == "/health" || param.Path == "/ready" {
			return ""
		}

		// Log errors at INFO level
		if param.StatusCode >= 400 {
			logger.Info("HTTP Request Error",
				zap.String("method", param.Method),
				zap.String("path", param.Path),
				zap.Int("status", param.StatusCode),
				zap.Duration("latency", param.Latency),
				zap.String("client_ip", param.ClientIP),
			)
			return ""
		}

		// Log all other requests at DEBUG level to reduce noise
		logger.Debug("HTTP Request",
			zap.String("method", param.Method),
			zap.String("path", param.Path),
			zap.Int("status", param.StatusCode),
			zap.Duration("latency", param.Latency),
			zap.String("client_ip", param.ClientIP),
		)
		return ""
	})
}

// metricsMiddleware tracks request metrics
func (s *Server) metricsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// Process request
		c.Next()

		// Track metrics
		duration := time.Since(start)
		s.metrics.RequestCount.Inc(1)
		s.metrics.RequestDuration.Record(duration)

		// Track errors
		if c.Writer.Status() >= 400 {
			s.metrics.RequestErrors.Inc(1)
		}

		// Track specific endpoint metrics
		path := c.FullPath()
		switch {
		case path == "/health":
			s.metrics.HealthCheckCount.Inc(1)
		case path == "/api/v1/agents/:id":
			s.metrics.AgentGetCount.Inc(1)
		case path == "/api/v1/agents":
			s.metrics.AgentListCount.Inc(1)
		case path == "/api/v1/groups/:id":
			s.metrics.GroupGetCount.Inc(1)
		case path == "/api/v1/groups":
			if c.Request.Method == "GET" {
				s.metrics.GroupListCount.Inc(1)
			} else if c.Request.Method == "POST" {
				s.metrics.GroupCreateCount.Inc(1)
			}
		case path == "/api/v1/configs/:id":
			s.metrics.ConfigGetCount.Inc(1)
		case path == "/api/v1/configs":
			if c.Request.Method == "GET" {
				s.metrics.ConfigListCount.Inc(1)
			} else if c.Request.Method == "POST" {
				s.metrics.ConfigCreateCount.Inc(1)
			}
		case path == "/api/v1/telemetry/metrics/query":
			s.metrics.TelemetryQueryCount.Inc(1)
			s.metrics.TelemetryQueryDuration.Record(duration)
		case path == "/api/v1/topology":
			s.metrics.TopologyQueryCount.Inc(1)
			s.metrics.TopologyQueryDuration.Record(duration)
		}
	}
}
