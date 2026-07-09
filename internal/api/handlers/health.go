package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/services"
)

// storePinger is the lightweight liveness/readiness primitive backing
// GET /readyz: a cheap SELECT 1-class round-trip, distinct from the deep
// ListAgents scan HandleHealth performs. The application store satisfies it.
type storePinger interface {
	Ping(ctx context.Context) error
}

// HealthHandlers handles health check endpoints
type HealthHandlers struct {
	agentService     services.AgentService
	telemetryService services.TelemetryQueryService
	store            storePinger
	logger           *zap.Logger
}

// NewHealthHandlers creates a new health handlers instance. store is the
// lightweight readiness probe target (GET /readyz); it may be nil, in which
// case /readyz reports unready rather than panicking.
func NewHealthHandlers(agentService services.AgentService, telemetryService services.TelemetryQueryService, store storePinger, logger *zap.Logger) *HealthHandlers {
	return &HealthHandlers{
		agentService:     agentService,
		telemetryService: telemetryService,
		store:            store,
		logger:           logger,
	}
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status    string            `json:"status"`
	Timestamp time.Time         `json:"timestamp"`
	Version   string            `json:"version"`
	Services  map[string]string `json:"services"`
}

// handleHealth handles GET /health
func (h *HealthHandlers) HandleHealth(c *gin.Context) {
	// Check storage health
	sqliteHealthy := h.checkSQLiteHealth(c)
	duckdbHealthy := h.checkDuckDBHealth(c)

	// Determine overall status
	status := "healthy"
	if !sqliteHealthy || !duckdbHealthy {
		status = "unhealthy"
	}

	response := HealthResponse{
		Status:    status,
		Timestamp: time.Now(),
		Version:   "0.1.0",
		Services: map[string]string{
			"sqlite": h.getHealthStatus(sqliteHealthy),
			"duckdb": h.getHealthStatus(duckdbHealthy),
		},
	}

	// Set appropriate HTTP status code
	httpStatus := http.StatusOK
	if status == "unhealthy" {
		httpStatus = http.StatusServiceUnavailable
	}

	c.JSON(httpStatus, response)
}

// HandleLive handles GET /livez — LIVENESS.
//
// Liveness answers one question: is the process up and the HTTP server able to
// route and respond? It MUST NOT touch the store, DuckDB, or any dependency:
// k8s livenessProbe hits this endpoint, and a failure here RESTARTS the pod. A
// slow or unready dependency must never crash-loop an otherwise-live process —
// draining traffic from it is the readiness probe's job (GET /readyz). So this
// unconditionally returns 200 with a fixed body and does zero dependency work.
func (h *HealthHandlers) HandleLive(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// HandleReady handles GET /readyz — READINESS.
//
// Readiness answers: can this process serve traffic right now? It runs a
// LIGHTWEIGHT store probe (a SELECT 1-class Ping), NOT the deep ListAgents scan
// HandleHealth performs. Like /health it probes under a system context: /readyz
// is mounted untenanted on the root router, and under the enterprise build's
// strict tenant scoping a store call on an untenanted ctx is rejected with
// ErrTenantContextRequired (see checkSQLiteHealth). Returns 200 when the store
// answers, 503 when the Ping fails so k8s stops routing to a pod that cannot
// reach its DB.
func (h *HealthHandlers) HandleReady(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unready", "reason": "store not wired"})
		return
	}
	ctx := identity.WithSystemContext(c.Request.Context())
	if err := h.store.Ping(ctx); err != nil {
		if h.logger != nil {
			h.logger.Warn("readiness probe: store ping failed", zap.Error(err))
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unready"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// checkSQLiteHealth checks if SQLite is healthy
func (h *HealthHandlers) checkSQLiteHealth(c *gin.Context) bool {
	// A health probe is a system-wide liveness/readiness check, not a tenant
	// request: /health is mounted on the root router (outside the /api/v1 group)
	// so ResolveTenant never stamps a tenant. Under the enterprise build's strict
	// tenant scoping, any store call on an untenanted context is rejected with
	// ErrTenantContextRequired — which flipped this check to unhealthy and 503'd
	// /health, crash-looping otherwise-healthy pods (liveness/readiness/startup
	// probes all hit /health). Probe under WithSystemContext, the same idiom every
	// background job uses; tenantScope short-circuits system contexts cleanly.
	ctx := identity.WithSystemContext(c.Request.Context())
	_, err := h.agentService.ListAgents(ctx)
	return err == nil
}

// checkDuckDBHealth checks if DuckDB is healthy
func (h *HealthHandlers) checkDuckDBHealth(c *gin.Context) bool {
	// Try to query a simple metric count
	query := services.MetricQuery{
		StartTime: time.Now().Add(-1 * time.Minute),
		EndTime:   time.Now(),
		Limit:     1,
	}
	ctx := identity.WithSystemContext(c.Request.Context())
	_, err := h.telemetryService.QueryMetrics(ctx, query)
	return err == nil
}

// getHealthStatus converts boolean to status string
func (h *HealthHandlers) getHealthStatus(healthy bool) string {
	if healthy {
		return "healthy"
	}
	return "unhealthy"
}
