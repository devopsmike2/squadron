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
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/api/handlers"
	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/configs"
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

// AuthConfig controls the API auth middleware. When Enabled is true,
// every /api/v1/* request must carry a valid Bearer token; /metrics
// and /health stay public. When false, no auth middleware is mounted
// and the API behaves as it did pre-v0.8 — useful for development,
// dangerous in production.
type AuthConfig struct {
	Enabled bool
}

// Server represents the HTTP API server
type Server struct {
	router            *gin.Engine
	agentService      services.AgentService
	telemetryService  services.TelemetryQueryService
	savedQueryService services.SavedQueryService
	alertService      services.AlertService
	auditService      services.AuditService
	rolloutService    services.RolloutService
	authService       services.AuthService
	authConfig        AuthConfig
	commander         AgentCommander
	broker            *events.Broker
	configsTracer     *configs.Tracer // optional; nil disables config-push spans on direct handler pushes
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
func NewServer(agentService services.AgentService, telemetryService services.TelemetryQueryService, savedQueryService services.SavedQueryService, alertService services.AlertService, auditService services.AuditService, rolloutService services.RolloutService, authService services.AuthService, authConfig AuthConfig, commander AgentCommander, broker *events.Broker, configsTracer *configs.Tracer, registry *prometheus.Registry, logger *zap.Logger) *Server {
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
	// OTel trace propagation: extracts the W3C traceparent header on
	// inbound requests into the gin context, and creates a server
	// span named by the route. When selftel is disabled, the global
	// propagator + tracer are no-ops so this layer is effectively
	// free. Mounted ABOVE auth so the span exists even for 401
	// rejections — operators trace-debugging a misauthed CI run can
	// still find their request in the trace UI.
	router.Use(otelgin.Middleware("squadron"))

	server := &Server{
		router:            router,
		agentService:      agentService,
		telemetryService:  telemetryService,
		savedQueryService: savedQueryService,
		alertService:      alertService,
		auditService:      auditService,
		rolloutService:    rolloutService,
		authService:       authService,
		authConfig:        authConfig,
		commander:         commander,
		broker:            broker,
		configsTracer:     configsTracer,
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
	agentHandlers := handlers.NewAgentHandlersWithTracer(s.agentService, s.commander, s.configsTracer, s.logger)
	configHandlers := handlers.NewConfigHandlers(s.agentService, s.commander, s.logger)
	telemetryHandlers := handlers.NewTelemetryHandlers(s.telemetryService, s.logger)
	squadronQLHandlers := handlers.NewSquadronQLHandlers(s.telemetryService, s.logger)
	groupHandlers := handlers.NewGroupHandlers(s.agentService, s.commander, s.logger)
	savedQueryHandlers := handlers.NewSavedQueryHandlers(s.savedQueryService, s.logger)
	topologyHandlers := handlers.NewTopologyHandlers(s.agentService, s.telemetryService, s.logger)
	healthHandlers := handlers.NewHealthHandlers(s.agentService, s.telemetryService, s.logger)
	alertHandlers := handlers.NewAlertHandlers(s.alertService, s.logger)
	auditHandlers := handlers.NewAuditHandlers(s.auditService, s.logger)
	rolloutHandlers := handlers.NewRolloutHandlers(s.rolloutService, s.logger)
	eventsHandlers := handlers.NewEventsHandlers(s.broker, s.logger)
	authHandlers := handlers.NewAuthHandlers(s.authService, s.logger)

	// Metrics endpoint — public so scrapers don't need a token.
	s.router.GET("/metrics", gin.WrapH(promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})))

	// Health check — public so load balancers can probe.
	s.router.GET("/health", healthHandlers.HandleHealth)

	// API v1 routes
	v1 := s.router.Group("/api/v1")
	if s.authConfig.Enabled {
		// When auth is enabled, every /api/v1/* request must carry a
		// valid Bearer token. /metrics and /health above stay public.
		v1.Use(middleware.RequireBearer(s.authService, s.logger))
	}
	{
		// Auth token management lives under /api/v1/auth/tokens.
		// Bootstrap problem: the first token has to be created without
		// a token. That's handled by the bootstrap-token-on-first-start
		// flow in main.go — by the time operators reach this endpoint
		// they should already have a token to authenticate with.
		auth := v1.Group("/auth/tokens")
		{
			auth.GET("", middleware.RequireScope(services.ScopeAuthRead), authHandlers.HandleListTokens)
			auth.POST("", middleware.RequireScope(services.ScopeAuthWrite), authHandlers.HandleCreateToken)
			auth.POST("/:id/revoke", middleware.RequireScope(services.ScopeAuthWrite), authHandlers.HandleRevokeToken)
		}

		// Agent routes. GET is read; PATCH/POST modify the agent (group
		// assignment, config push, restart) and require write.
		agents := v1.Group("/agents")
		{
			agents.GET("", middleware.RequireScope(services.ScopeAgentsRead), agentHandlers.HandleGetAgents)
			agents.GET("/stats", middleware.RequireScope(services.ScopeAgentsRead), agentHandlers.HandleGetAgentStats)
			agents.GET("/:id", middleware.RequireScope(services.ScopeAgentsRead), agentHandlers.HandleGetAgent)
			agents.PATCH("/:id/group", middleware.RequireScope(services.ScopeAgentsWrite), agentHandlers.HandleUpdateAgentGroup)
			agents.POST("/:id/config", middleware.RequireScope(services.ScopeAgentsWrite), agentHandlers.HandleSendConfigToAgent)
			agents.POST("/:id/restart", middleware.RequireScope(services.ScopeAgentsWrite), agentHandlers.HandleRestartAgent)
		}

		// Config routes. validate/lint/templates are read-shaped (they
		// don't mutate state even though they're POSTs by API design),
		// so they require configs:read. Create/update/delete need write.
		configs := v1.Group("/configs")
		{
			configs.GET("", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfigs)
			configs.POST("", middleware.RequireScope(services.ScopeConfigsWrite), configHandlers.HandleCreateConfig)
			configs.POST("/validate", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleValidateConfig)
			configs.POST("/lint", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleLintConfig)
			configs.GET("/templates", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfigTemplates)
			configs.GET("/templates/:id", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfigTemplate)
			configs.GET("/versions", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfigVersions)
			configs.GET("/:id", middleware.RequireScope(services.ScopeConfigsRead), configHandlers.HandleGetConfig)
			configs.PUT("/:id", middleware.RequireScope(services.ScopeConfigsWrite), configHandlers.HandleUpdateConfig)
			configs.DELETE("/:id", middleware.RequireScope(services.ScopeConfigsWrite), configHandlers.HandleDeleteConfig)
		}

		// Telemetry routes are all read-shaped (POSTs are queries, not
		// mutations). Saved queries are a CRUD library that piggybacks
		// on the same scope — there's no separate "saved query write"
		// scope for v0.10; if operators want stricter isolation, that's
		// a future scope subdivision.
		telemetry := v1.Group("/telemetry")
		telemetry.Use(middleware.RequireScope(services.ScopeTelemetryRead))
		{
			telemetry.POST("/metrics/query", telemetryHandlers.HandleQueryMetrics)
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

			telemetry.POST("/query", squadronQLHandlers.HandleSquadronQLQuery)
			telemetry.POST("/query/validate", squadronQLHandlers.HandleValidateQuery)
			telemetry.POST("/query/suggestions", squadronQLHandlers.HandleGetSuggestions)
			telemetry.GET("/query/templates", squadronQLHandlers.HandleGetTemplates)
			telemetry.GET("/query/functions", squadronQLHandlers.HandleGetFunctions)
		}

		// Group routes. Restart is a write because it triggers an
		// operational change on every agent in the group.
		groups := v1.Group("/groups")
		{
			groups.GET("", middleware.RequireScope(services.ScopeGroupsRead), groupHandlers.HandleGetGroups)
			groups.POST("", middleware.RequireScope(services.ScopeGroupsWrite), groupHandlers.HandleCreateGroup)
			groups.GET("/:id", middleware.RequireScope(services.ScopeGroupsRead), groupHandlers.HandleGetGroup)
			groups.PUT("/:id", middleware.RequireScope(services.ScopeGroupsWrite), groupHandlers.HandleUpdateGroup)
			groups.DELETE("/:id", middleware.RequireScope(services.ScopeGroupsWrite), groupHandlers.HandleDeleteGroup)
			groups.POST("/:id/config", middleware.RequireScope(services.ScopeGroupsWrite), groupHandlers.HandleAssignConfig)
			groups.GET("/:id/config", middleware.RequireScope(services.ScopeGroupsRead), groupHandlers.HandleGetGroupConfig)
			groups.GET("/:id/agents", middleware.RequireScope(services.ScopeAgentsRead), groupHandlers.HandleGetGroupAgents)
			groups.POST("/:id/restart", middleware.RequireScope(services.ScopeAgentsWrite), groupHandlers.HandleRestartGroup)
		}

		// Topology routes are read-only views over agents + telemetry.
		topology := v1.Group("/topology")
		topology.Use(middleware.RequireScope(services.ScopeAgentsRead))
		{
			topology.GET("", topologyHandlers.HandleGetTopology)
			topology.GET("/agent/:id", topologyHandlers.HandleGetAgentTopology)
			topology.GET("/group/:id", topologyHandlers.HandleGetGroupTopology)
		}

		// Alert rule routes
		alerts := v1.Group("/alerts/rules")
		{
			alerts.GET("", middleware.RequireScope(services.ScopeAlertsRead), alertHandlers.HandleListAlertRules)
			alerts.POST("", middleware.RequireScope(services.ScopeAlertsWrite), alertHandlers.HandleCreateAlertRule)
			alerts.GET("/:id", middleware.RequireScope(services.ScopeAlertsRead), alertHandlers.HandleGetAlertRule)
			alerts.PUT("/:id", middleware.RequireScope(services.ScopeAlertsWrite), alertHandlers.HandleUpdateAlertRule)
			alerts.DELETE("/:id", middleware.RequireScope(services.ScopeAlertsWrite), alertHandlers.HandleDeleteAlertRule)
		}

		// Real-time event stream (Server-Sent Events). Stream carries
		// state-change events across every domain; the audit:read scope
		// is the closest match since the events are largely the same
		// shapes the audit log records.
		v1.GET("/events/stream", middleware.RequireScope(services.ScopeAuditRead), eventsHandlers.HandleStream)

		// Audit log — read-only.
		audit := v1.Group("/audit")
		audit.Use(middleware.RequireScope(services.ScopeAuditRead))
		{
			audit.GET("/events", auditHandlers.HandleListAuditEvents)
		}

		// Rollouts — safe staged config deployment with automatic rollback.
		rollouts := v1.Group("/rollouts")
		{
			rollouts.GET("", middleware.RequireScope(services.ScopeRolloutsRead), rolloutHandlers.HandleListRollouts)
			rollouts.POST("", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleCreateRollout)
			rollouts.GET("/:id", middleware.RequireScope(services.ScopeRolloutsRead), rolloutHandlers.HandleGetRollout)
			rollouts.POST("/:id/abort", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleAbortRollout)
			rollouts.POST("/:id/pause", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandlePauseRollout)
			rollouts.POST("/:id/resume", middleware.RequireScope(services.ScopeRolloutsWrite), rolloutHandlers.HandleResumeRollout)
		}

		// Rollout recipe cookbook. Sibling of /rollouts (not nested)
		// to avoid Gin's static-vs-parametric route conflict with
		// /rollouts/:id. Both endpoints are cache-friendly — they
		// change only on Squadron upgrade. rollouts:read gates them so
		// a read-only operator can see what shapes are available even
		// though they can't create rollouts.
		v1.GET("/rollout-recipes/abort-criteria",
			middleware.RequireScope(services.ScopeRolloutsRead),
			rolloutHandlers.HandleListAbortCriteriaRecipes)
		v1.GET("/rollout-recipes/templates",
			middleware.RequireScope(services.ScopeRolloutsRead),
			rolloutHandlers.HandleListRolloutTemplates)

		// Rollout preview — diff + lint between a group's current
		// effective config and a target config, for the create-form
		// "are you sure?" pane. Sibling of /rollouts for the same
		// routing-conflict reason.
		v1.GET("/rollout-preview",
			middleware.RequireScope(services.ScopeRolloutsRead),
			rolloutHandlers.HandlePreviewRollout)
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
