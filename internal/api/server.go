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

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/api/handlers"
	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/configs"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/billing"
	"github.com/devopsmike2/squadron/internal/deploy"
	"github.com/devopsmike2/squadron/internal/insights"
	"github.com/devopsmike2/squadron/internal/inventory"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/costspikes"
	"github.com/devopsmike2/squadron/internal/pipelinehealth"
	"github.com/devopsmike2/squadron/internal/pricing"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
)

// AgentCommander defines the interface for sending commands to agents.
//
// SendConfigToAgentWithContext is the trace-aware variant used by the
// per-agent direct-push handler — it propagates the per-push span
// context into the OpAMP CustomMessage so an OTel-aware agent can join
// the originating trace. SendConfigToAgent stays for non-traced and
// group-fanout callers and to preserve back-compat for downstream
// embedders.
type AgentCommander interface {
	SendConfigToAgent(agentId uuid.UUID, configContent string) error
	SendConfigToAgentWithContext(ctx context.Context, agentId uuid.UUID, configContent string) error
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
	configsTracer     *configs.Tracer  // optional; nil disables config-push spans on direct handler pushes
	opampPort         int               // v0.27.1: the OpAMP port we tell quickstart-generated agents to dial
	insightsService   *insights.Service // optional; nil disables the /api/v1/insights/* routes (no telemetry reader configured)
	recsEngine        *recommendations.Engine
	recsDismissals    handlers.DismissalStore // optional; nil disables /api/v1/recommendations/* (paired with recsEngine)
	aiService         *ai.Service             // optional; nil keeps /api/v1/ai/status responsive (returns enabled=false) but mutation routes 503
	pricer            *pricing.Projector      // optional; v0.27 $/month projection. nil → /api/v1/pricing/* returns enabled=false
	costSpikes        handlers.CostSpikeStore // optional; v0.29 cost-spike alerting storage
	costSpikeDetector *costspikes.Detector    // optional; nil disables /tick + the background detector loop
	pipelineHealth    *pipelinehealth.Service // optional; v0.31 collector self-metrics surface — nil → /api/v1/pipeline-health/* returns 503
	inventory         *inventory.Service      // optional; v0.32 expected-vs-actual reconciliation — nil → /api/v1/inventory/* returns 503
	deploy            *deploy.Service         // optional; v0.34 GitHub Actions deploy trigger — nil or Enabled()==false → /api/v1/deploy/* returns 503
	billingProvider   billing.SnapshotProvider // optional; v0.42 — nil → /api/v1/billing/snapshot returns 204
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

// insightsTrampoline late-binds an insights handler call so the
// route table can be registered before SetInsightsService is
// called. Returns a gin.HandlerFunc that resolves s.insightsService
// at request time; 503s with a clear error if still nil.
func (s *Server) insightsTrampoline(fn func(*handlers.InsightsHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.insightsService == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Telemetry Volume Insights are not available — no telemetry backend wired",
			})
			return
		}
		h := handlers.NewInsightsHandlers(s.insightsService, s.logger)
		fn(h, c)
	}
}

// SetInsightsService wires the (optional) insights query service.
// When unset, the /api/v1/insights/* routes return 503 — operators
// running without a telemetry backend (e.g. a test harness) don't
// see the routes break, just respond "not available".
//
// Setter pattern rather than a constructor argument so existing
// callers (test_server.go and friends) don't grow yet another
// positional parameter for an optional feature.
func (s *Server) SetInsightsService(svc *insights.Service) {
	s.insightsService = svc
}

// SetRecommendationsEngine wires the v0.25 recommendations engine
// + its dismissals store. Both go together — an engine without a
// store can still Evaluate but can't honor dismissals. When unset,
// the /api/v1/recommendations/* routes return 503 with a clear
// message (same trampoline pattern as the insights routes).
func (s *Server) SetRecommendationsEngine(engine *recommendations.Engine, dismissals handlers.DismissalStore) {
	s.recsEngine = engine
	s.recsDismissals = dismissals
}

// SetOpAMPPort tells the Server which port the OpAMP server is
// listening on. v0.27.1 uses this to construct the dial URL that
// the Quickstart wizard hands to operators (the API runs on a
// different port from OpAMP, so the request Host alone isn't
// enough). Defaults to 4320 if never set.
func (s *Server) SetOpAMPPort(p int) { s.opampPort = p }

// SetPricer wires the v0.27 pricing projector. Always non-nil at
// runtime (main.go always constructs one — disabled state lives
// inside the projector). The pricingTrampoline still guards
// against nil for the test_server.go path.
func (s *Server) SetPricer(p *pricing.Projector) { s.pricer = p }

// SetCostSpikes wires the v0.29 cost-spike alerting layer: the
// storage slice (always the application store) + an optional
// detector. When the detector is non-nil, the server's Start
// will also launch the background Tick loop. When the store is
// nil, the /alerts/cost-spikes routes 503.
func (s *Server) SetCostSpikes(store handlers.CostSpikeStore, det *costspikes.Detector) {
	s.costSpikes = store
	s.costSpikeDetector = det
}

// SetPipelineHealth wires the v0.31 collector-self-metrics surface.
// nil disables the /api/v1/pipeline-health/* routes (503) — this is
// the right state for the test_server.go path that doesn't have a
// telemetry reader, since the service needs DuckDB to function.
func (s *Server) SetPipelineHealth(svc *pipelinehealth.Service) {
	s.pipelineHealth = svc
}

// SetInventory wires the v0.32 expected-vs-actual reconciliation
// surface. Always non-nil at production runtime (main.go constructs
// it unconditionally against the application store). The
// nil-guard exists for the test_server.go path.
func (s *Server) SetInventory(svc *inventory.Service) {
	s.inventory = svc
}

// SetDeploy wires the v0.34 GitHub Actions deploy surface. Pass
// nil (or a service whose Enabled() returns false) to disable the
// /api/v1/deploy/* routes (they 503). Disabled is the right state
// when SQUADRON_DEPLOY_KEY is unset — main.go decides.
// SetBillingProvider wires the v0.42 billing connector. Pass nil to
// disable — the /api/v1/billing/snapshot endpoint returns 204 in
// that case and the UI's billing tile silently hides.
func (s *Server) SetBillingProvider(p billing.SnapshotProvider) {
	s.billingProvider = p
}

func (s *Server) SetDeploy(svc *deploy.Service) {
	s.deploy = svc
}

// pricingTrampoline mirrors insightsTrampoline. The /pricing/*
// routes are read-only and gracefully degrade — when the pricer
// is unwired we 503; when it's wired but disabled the handler
// returns enabled=false at 200.
func (s *Server) pricingTrampoline(fn func(*handlers.PricingHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.pricer == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "Pricing service is not wired",
				"enabled": false,
			})
			return
		}
		// PricingHandlers needs insights for the projection
		// endpoint. Reuse the one already wired via SetInsightsService.
		// When insights is nil, projection returns zero — that's fine
		// for the test_server.go path.
		h := handlers.NewPricingHandlers(s.pricer, s.insightsService, s.logger)
		fn(h, c)
	}
}

// SetAIService wires the (optional) v0.26 AI-assist service. The
// service is constructed unconditionally in main.go — it
// short-circuits with ErrDisabled when no API key is configured —
// so passing a non-nil service here is the right default. The
// nil-guard exists for the test_server.go path that doesn't wire
// AI at all.
func (s *Server) SetAIService(svc *ai.Service) {
	s.aiService = svc
}

// aiTrampoline late-binds an AI handler call. Same shape as the
// other trampolines, but with a softer 503 — the /status route in
// particular needs to remain responsive even when the service is
// unwired so the UI's capability probe doesn't fail at app load.
// We let the /status handler decide its own response shape; other
// handlers go through the standard nil-check.
func (s *Server) aiTrampoline(fn func(*handlers.AIHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.aiService == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "AI assist is not configured",
				"enabled": false,
			})
			return
		}
		h := handlers.NewAIHandlers(s.aiService, s.logger)
		fn(h, c)
	}
}

// aiStatusTrampoline is the special case for /api/v1/ai/status —
// when the service is nil, return enabled=false rather than 503
// so the UI's capability probe stays simple.
func (s *Server) aiStatusTrampoline() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.aiService == nil {
			c.JSON(http.StatusOK, ai.Capabilities{Enabled: false})
			return
		}
		h := handlers.NewAIHandlers(s.aiService, s.logger)
		h.HandleStatus(c)
	}
}

// recommendationsTrampoline late-binds a recs handler call so the
// route table can be registered before SetRecommendationsEngine
// runs. Mirrors insightsTrampoline; 503s with a clear error
// message when the engine is still nil.
func (s *Server) recommendationsTrampoline(fn func(*handlers.RecommendationsHandlers, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.recsEngine == nil || s.recsDismissals == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Cost Recommendations are not available — engine not wired",
			})
			return
		}
		h := handlers.NewRecommendationsHandlers(s.recsEngine, s.recsDismissals, s.logger)
		fn(h, c)
	}
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
			// v0.35: hard-delete the agent record for hosts that
			// have been retired from the fleet. Audit-logged via
			// the agent service's existing event publish.
			agents.DELETE("/:id", middleware.RequireScope(services.ScopeAgentsWrite), agentHandlers.HandleDecommissionAgent)
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

		// v0.40.0 Timeline — postmortem view that merges audit,
		// deploy, and cost-spike events into one chronologically
		// sorted stream. Read-only; gated by audit-read since the
		// merged data is a strict subset of what the audit log
		// already exposes.
		v1.GET("/timeline",
			middleware.RequireScope(services.ScopeAuditRead),
			func(c *gin.Context) {
				handlers.NewTimelineHandlers(
					s.auditService,
					s.deploy,
					s.costSpikes,
					s.logger,
				).HandleList(c)
			})

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

		// Telemetry Volume Insights (v0.24+). Read-only data
		// surface for "where are my telemetry bytes going". The
		// v0.25 cost-recommendation engine reads from these
		// endpoints; keep the response shapes stable. Mounted
		// behind ScopeAgentsRead — same scope that gates the
		// agents list, since the insights are an aggregation of
		// the same underlying telemetry.
		//
		// Routes are registered unconditionally; each handler
		// re-checks s.insightsService at request time and 503s
		// if it's still nil. This lets main.go wire the service
		// via SetInsightsService AFTER NewServer constructs the
		// route table (the alternative — make every existing
		// caller of NewServer take another argument — has more
		// blast radius than this trampoline).
		v1.GET("/insights/volume",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleFleetVolume(c) }))
		v1.GET("/insights/volume/agents",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleTopAgents(c) }))
		v1.GET("/insights/volume/agents/:id",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleAgentVolume(c) }))
		v1.GET("/insights/volume/attributes",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleTopAttributes(c) }))
		v1.GET("/insights/volume/drops",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.insightsTrampoline(func(h *handlers.InsightsHandlers, c *gin.Context) { h.HandleDrops(c) }))

		// Cost Recommendations (v0.25). Heuristic advice layered on
		// top of the v0.24 insights surface. Reads are
		// ScopeAgentsRead (same gating as the underlying volume
		// data); dismiss/restore mutations require ScopeAgentsWrite
		// because they shape what other operators see.
		v1.GET("/recommendations",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleList(c) }))
		v1.GET("/recommendations/agents/:id",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleListForAgent(c) }))
		v1.GET("/recommendations/dismissals",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleListDismissals(c) }))
		v1.POST("/recommendations/:id/dismiss",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleDismiss(c) }))
		v1.POST("/recommendations/:id/restore",
			middleware.RequireScope(services.ScopeAgentsWrite),
			s.recommendationsTrampoline(func(h *handlers.RecommendationsHandlers, c *gin.Context) { h.HandleRestore(c) }))

		// v0.26 AI assist. Wraps the Anthropic Messages API; off
		// by default. All routes behind ScopeAgentsRead since
		// they're read-only assistive surfaces (no state changes,
		// no fan-out, no agent commands). The /status route stays
		// responsive even when AI is unwired so the UI's
		// capability probe is a single round-trip; the other
		// routes 503 with an opt-in message.
		v1.GET("/ai/status",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiStatusTrampoline())
		v1.POST("/ai/explain",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleExplainSnippet(c) }))
		v1.POST("/ai/merge",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleMergeIntoConfig(c) }))
		v1.POST("/ai/explain-config",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleExplainConfig(c) }))
		// v0.44 — natural-language fleet query. Same agents-read
		// scope since the result is just filter params for /agents.
		v1.POST("/ai/fleet-query",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleFleetQuery(c) }))
		// v0.44 — auto-remediate lint warnings. The remediated YAML
		// flows through the normal save / rollout path, so no new
		// scope is needed — config-write happens at save time.
		v1.POST("/ai/remediate-lint",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.aiTrampoline(func(h *handlers.AIHandlers, c *gin.Context) { h.HandleRemediateLint(c) }))

		// v0.27 Pricing projection. Turns the v0.24 byte numbers
		// into $/month figures. Read-only; same scope as the rest
		// of the cost-insights surface.
		v1.GET("/pricing/config",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.pricingTrampoline(func(h *handlers.PricingHandlers, c *gin.Context) { h.HandleConfig(c) }))
		v1.GET("/pricing/projection",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.pricingTrampoline(func(h *handlers.PricingHandlers, c *gin.Context) { h.HandleProjection(c) }))
		// v0.39.0 month-end spend forecast. Same projection math
		// pro-rated across the calendar month into elapsed +
		// remaining buckets so the Savings page can render a
		// "projected $X by EOM" tile.
		v1.GET("/pricing/forecast",
			middleware.RequireScope(services.ScopeAgentsRead),
			s.pricingTrampoline(func(h *handlers.PricingHandlers, c *gin.Context) { h.HandleForecast(c) }))

		// v0.42 — actual billing snapshot from the configured
		// destination's billing API (Splunk for v0.42). Reuses the
		// agents-read scope since it's tied to the Savings page,
		// not a new auth surface.
		v1.GET("/billing/snapshot",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				handlers.NewBillingHandlers(s.billingProvider, s.logger).HandleSnapshot(c)
			})

		// v0.28 Retrospective savings. Two endpoints: one to record
		// an Apply click (UI fires this when operator clicks the
		// recommendation's Apply button), one to fetch the
		// aggregated realized savings + per-outcome breakdown for
		// the Savings dashboard.
		v1.POST("/recommendations/:id/applied",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.recsEngine == nil || s.recsDismissals == nil || s.pricer == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error":   "Retrospective savings tracking is not wired (engine + pricer required)",
						"enabled": false,
					})
					return
				}
				// recsDismissals doubles as the OutcomeStore — both
				// implemented by the application store. We pass the
				// store directly via an interface match.
				store, ok := s.recsDismissals.(handlers.OutcomeStore)
				if !ok {
					c.JSON(http.StatusInternalServerError, gin.H{
						"error": "store does not implement OutcomeStore",
					})
					return
				}
				h := handlers.NewSavingsHandlers(store, s.recsEngine, s.insightsService, s.pricer, s.logger)
				h.HandleApplied(c)
			})
		v1.GET("/savings/realized",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.recsEngine == nil || s.recsDismissals == nil || s.pricer == nil {
					c.JSON(http.StatusOK, gin.H{
						"monthly_realized_usd": 0,
						"enabled":              false,
					})
					return
				}
				store, ok := s.recsDismissals.(handlers.OutcomeStore)
				if !ok {
					c.JSON(http.StatusInternalServerError, gin.H{
						"error": "store does not implement OutcomeStore",
					})
					return
				}
				h := handlers.NewSavingsHandlers(store, s.recsEngine, s.insightsService, s.pricer, s.logger)
				h.HandleRealized(c)
			})

		// v0.29 Cost-spike alerting. Detector runs in the
		// background (started in main.go) and writes events to
		// the application store. These routes are pure reads
		// against that store plus an operator-driven Acknowledge.
		// Tick is exposed for tests + the demo path that needs
		// to provoke a detection without waiting the full minute.
		v1.GET("/alerts/cost-spikes",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.costSpikes == nil {
					c.JSON(http.StatusOK, gin.H{
						"items": []any{}, "count": 0, "status": "open", "enabled": false,
					})
					return
				}
				h := handlers.NewCostSpikesHandlers(s.costSpikes, s.costSpikeDetector)
				h.HandleList(c)
			})
		v1.POST("/alerts/cost-spikes/:id/acknowledge",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.costSpikes == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "cost spikes disabled"})
					return
				}
				h := handlers.NewCostSpikesHandlers(s.costSpikes, s.costSpikeDetector)
				h.HandleAcknowledge(c)
			})
		v1.POST("/alerts/cost-spikes/tick",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.costSpikes == nil || s.costSpikeDetector == nil {
					c.JSON(http.StatusOK, gin.H{"ok": false, "reason": "detector disabled"})
					return
				}
				h := handlers.NewCostSpikesHandlers(s.costSpikes, s.costSpikeDetector)
				h.HandleTick(c)
			})

		// v0.31 Pipeline Health surface — collector self-metrics
		// extracted from the regular OTLP ingest path. All read-only
		// so the natural scope is ScopeAgentsRead. Handlers are
		// constructed inline behind a nil-guard: when no telemetry
		// reader is wired (test_server.go path), the routes 503
		// rather than panicking on the nil service.
		v1.GET("/pipeline-health/fleet",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.pipelineHealth == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error": "pipeline health unavailable (no telemetry reader)",
					})
					return
				}
				handlers.NewPipelineHealthHandlers(s.pipelineHealth, s.logger).HandleFleetSummary(c)
			})
		v1.GET("/pipeline-health/agents/:agentID",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.pipelineHealth == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error": "pipeline health unavailable (no telemetry reader)",
					})
					return
				}
				handlers.NewPipelineHealthHandlers(s.pipelineHealth, s.logger).HandleAgentSnapshot(c)
			})
		v1.GET("/pipeline-health/agents/:agentID/timeseries",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.pipelineHealth == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error": "pipeline health unavailable (no telemetry reader)",
					})
					return
				}
				handlers.NewPipelineHealthHandlers(s.pipelineHealth, s.logger).HandleAgentTimeseries(c)
			})

		// v0.32 Inventory reconciliation — expected vs. actual diff.
		// The list/replace surfaces are designed so a CI/CD pipeline
		// can rotate its target hostlist with a single PUT.
		v1.GET("/inventory/reconciliation",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleReconcile(c)
			})
		v1.GET("/inventory/expected",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleListExpected(c)
			})
		v1.POST("/inventory/expected",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleUpsertExpected(c)
			})
		v1.PUT("/inventory/expected",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleReplaceExpected(c)
			})
		v1.DELETE("/inventory/expected/:hostname",
			middleware.RequireScope(services.ScopeAgentsWrite),
			func(c *gin.Context) {
				if s.inventory == nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inventory unavailable"})
					return
				}
				handlers.NewInventoryHandlers(s.inventory, s.logger).HandleDeleteExpected(c)
			})

		// v0.34 Deploy surface (GitHub Actions integration).
		// All endpoints behind ScopeDeployRead except Trigger +
		// target mutations which need ScopeDeployTrigger.
		deployRead := middleware.RequireScope(services.ScopeDeployRead)
		deployWrite := middleware.RequireScope(services.ScopeDeployTrigger)
		v1.GET("/deploy/targets", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleListTargets(c)
		})
		v1.GET("/deploy/targets/:id", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleGetTarget(c)
		})
		v1.POST("/deploy/targets", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleCreateTarget(c)
		})
		v1.PUT("/deploy/targets/:id", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleUpdateTarget(c)
		})
		v1.DELETE("/deploy/targets/:id", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleDeleteTarget(c)
		})
		v1.POST("/deploy/targets/:id/lint", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleLintConfig(c)
		})
		// v0.34.1: preview the host list parsed from the target's
		// configured inventory.ini. Read-only; the actual auto-population
		// also happens server-side at trigger time.
		v1.GET("/deploy/targets/:id/inventory", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleInventoryPreview(c)
		})
		// v0.35.0: pre-flight validation that exercises every read
		// path without firing a workflow. Operator clicks "Validate"
		// to confirm the target is wired correctly before the first
		// real deploy. Idempotent + cheap.
		v1.POST("/deploy/targets/:id/validate", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleValidate(c)
		})
		// v0.35.0: redeploy with a past run's inputs. Same lint gate
		// applies — if the pinned config has degraded since the last
		// successful deploy, the redeploy still gets blocked.
		v1.POST("/deploy/runs/:id/redeploy", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleRedeploy(c)
		})
		v1.GET("/deploy/runs", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleListRuns(c)
		})
		v1.GET("/deploy/runs/:id", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleGetRun(c)
		})
		v1.POST("/deploy/runs", deployWrite, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleTriggerRun(c)
		})

		// v0.39.0 DORA-style deploy metrics. Computed in-process
		// over the deploy_runs ledger — no new schema. Read-only.
		v1.GET("/deploy/metrics", deployRead, func(c *gin.Context) {
			handlers.NewDeployHandlers(s.deploy, s.logger).HandleMetrics(c)
		})

		// v0.27.1 Quickstart. Pure config-generation; no state.
		// All read-only so ScopeAgentsRead is the natural gate.
		// Handler is constructed inline since it's cheap and the
		// late-bind dance isn't needed (port is always available
		// once SetOpAMPPort runs, which happens in NewServer
		// callers before Start).
		v1.GET("/quickstart/backends",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				port := s.opampPort
				if port == 0 {
					port = 4320
				}
				handlers.NewQuickstartHandlers(port, s.logger).HandleCatalog(c)
			})
		v1.GET("/quickstart/starter-config",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				port := s.opampPort
				if port == 0 {
					port = 4320
				}
				handlers.NewQuickstartHandlers(port, s.logger).HandleStarterConfig(c)
			})
		v1.GET("/quickstart/opamp-snippet",
			middleware.RequireScope(services.ScopeAgentsRead),
			func(c *gin.Context) {
				port := s.opampPort
				if port == 0 {
					port = 4320
				}
				handlers.NewQuickstartHandlers(port, s.logger).HandleOpAMPSnippet(c)
			})
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
