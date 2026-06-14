package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/configs"
	"github.com/devopsmike2/squadron/internal/services"
)

// AgentCommander defines the interface for sending commands to agents.
//
// SendConfigToAgentWithContext is the trace-aware variant — the handler
// passes its per-push span context so the OpAMP CustomMessage carries
// the W3C TraceContext to the agent (see internal/opamp/traceparent.go).
// SendConfigToAgent stays on the interface for back-compat with the
// non-traced group push and to keep existing test mocks compiling.
type AgentCommander interface {
	SendConfigToAgent(agentId uuid.UUID, configContent string) error
	SendConfigToAgentWithContext(ctx context.Context, agentId uuid.UUID, configContent string) error
	RestartAgent(agentId uuid.UUID) error
	RestartAgentsInGroup(groupId string) ([]uuid.UUID, []error)
	SendConfigToAgentsInGroup(groupId string, configContent string) ([]uuid.UUID, []error)
}

// AgentHandlers handles agent-related API endpoints
type AgentHandlers struct {
	agentService  services.AgentService
	commander     AgentCommander
	configsTracer *configs.Tracer // optional; nil disables config-push spans
	logger        *zap.Logger
}

// NewAgentHandlers creates a new agent handlers instance. configsTracer
// is optional — when nil, push tracing is disabled (matches the test
// path; production wires the real tracer via NewAgentHandlersWithTracer).
func NewAgentHandlers(agentService services.AgentService, commander AgentCommander, logger *zap.Logger) *AgentHandlers {
	return &AgentHandlers{
		agentService: agentService,
		commander:    commander,
		logger:       logger,
	}
}

// NewAgentHandlersWithTracer is the production constructor used when
// telemetry.enabled is true. Mirrors v0.12's NewAuditServiceWithSelfTelemetry
// pattern — separate constructor avoids adding a nil tracer parameter
// to every existing test caller.
func NewAgentHandlersWithTracer(agentService services.AgentService, commander AgentCommander, tracer *configs.Tracer, logger *zap.Logger) *AgentHandlers {
	return &AgentHandlers{
		agentService:  agentService,
		commander:     commander,
		configsTracer: tracer,
		logger:        logger,
	}
}

// GetAgentsRequest represents the request for getting agents
type GetAgentsRequest struct {
	// No filters supported in current interface
}

// GetAgentsResponse is the paginated response for GET /api/v1/agents.
//
// v0.23 added `Items` + the pagination envelope (`Total`, `Offset`,
// `Limit`) so the UI can fetch incrementally and not blow up at
// fleet sizes >1000. The legacy fields (`Agents`, `TotalCount`,
// `ActiveCount`, `InactiveCount`) stay in the response untouched so
// older callers — squadronctl pre-v0.18, dashboards built against
// v0.22 — keep working. We'll remove the legacy block in a future
// major bump after deprecation noise.
//
// `Items` is always sorted by agent ID ascending so successive page
// requests with the same filter give a stable order; the legacy
// `Agents` map continues to be order-undefined (it's a JSON object).
type GetAgentsResponse struct {
	// New (v0.23+).
	Items  []*services.Agent `json:"items"`
	Total  int               `json:"total"`
	Offset int               `json:"offset"`
	Limit  int               `json:"limit"`

	// Legacy (pre-v0.23). Same agents as Items, just keyed by ID.
	// totalCount mirrors Total; activeCount/inactiveCount are fleet
	// counters useful for the dashboard's old single-shot fetch.
	Agents        map[string]*services.Agent `json:"agents"`
	TotalCount    int                        `json:"totalCount"`
	ActiveCount   int                        `json:"activeCount"`
	InactiveCount int                        `json:"inactiveCount"`
}

// Pagination tunables. defaultLimit balances the cost of a single
// scroll-position fetch vs the overhead of round-trips; maxLimit is
// a defense-in-depth against a misconfigured client asking for the
// full fleet in one shot. Both can be revisited once we have
// real-world numbers.
const (
	defaultAgentsLimit = 100
	maxAgentsLimit     = 500
)

// validStatusFilters is the set of accepted ?status= values.
// Mirrors services.AgentStatus. "any" / empty is treated as no
// filter.
var validStatusFilters = map[string]services.AgentStatus{
	"online":  services.AgentStatusOnline,
	"offline": services.AgentStatusOffline,
	"error":   services.AgentStatusError,
}

// GetAgentStatsResponse represents agent statistics
type GetAgentStatsResponse struct {
	TotalAgents   int `json:"totalAgents"`
	OnlineAgents  int `json:"onlineAgents"`
	OfflineAgents int `json:"offlineAgents"`
	ErrorAgents   int `json:"errorAgents"`
	GroupsCount   int `json:"groupsCount"`
}

// UpdateAgentGroupRequest represents the request to update agent group
type UpdateAgentGroupRequest struct {
	GroupID *string `json:"group_id" binding:"omitempty,uuid"`
}

// validDriftFilters is the set of drift_status query values the endpoint
// accepts. Mirrors services.ConfigDriftStatus.
var validDriftFilters = map[string]services.ConfigDriftStatus{
	"synced":       services.ConfigDriftStatusSynced,
	"drifted":      services.ConfigDriftStatusDrifted,
	"no_intent":    services.ConfigDriftStatusNoIntent,
	"no_effective": services.ConfigDriftStatusNoEffective,
	"unknown":      services.ConfigDriftStatusUnknown,
}

// HandleGetAgents handles GET /api/v1/agents.
//
// Query parameters (all optional, all compose):
//   - drift_status = synced | drifted | no_intent | no_effective | unknown
//   - status       = online | offline | error
//   - group_id     = UUID — agents with this exact group_id
//   - q            = free-text — substring match against name + label
//                    key=value pairs (case-insensitive)
//   - offset       = integer >= 0, default 0
//   - limit        = integer 1..500, default 100
//
// Filtering happens BEFORE pagination — `total` in the response is
// the post-filter, pre-pagination count, so the UI can render an
// accurate "Showing N of M" line and decide whether to fetch more
// pages.
//
// Items are sorted by agent ID ascending. That ordering is stable
// across calls so a client can paginate without worrying about a
// page "shuffling" between requests. The legacy `agents` map field
// is also populated for back-compat but the JSON object key order
// is undefined per the spec — clients that need stable order
// should read `items`.
//
// activeCount / inactiveCount mirror the legacy semantics: they
// count online vs everything-else in the FILTERED result, not the
// raw fleet. Use /api/v1/agents/stats for fleet-wide totals.
func (h *AgentHandlers) HandleGetAgents(c *gin.Context) {
	agents, err := h.agentService.ListAgents(c.Request.Context())
	if err != nil {
		h.logger.Error("Failed to get agents", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch agents"})
		return
	}

	// ----- Filter -----
	// Each filter narrows the working slice in place. Order is
	// deliberate: validate-and-reject fast (400 for bad inputs)
	// before doing any allocation work.

	driftFilter, driftSet := services.ConfigDriftStatus(""), false
	if raw := c.Query("drift_status"); raw != "" {
		want, ok := validDriftFilters[raw]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "invalid drift_status",
				"allowed": []string{"synced", "drifted", "no_intent", "no_effective", "unknown"},
			})
			return
		}
		driftFilter, driftSet = want, true
	}

	statusFilter, statusSet := services.AgentStatus(""), false
	if raw := c.Query("status"); raw != "" {
		want, ok := validStatusFilters[raw]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "invalid status",
				"allowed": []string{"online", "offline", "error"},
			})
			return
		}
		statusFilter, statusSet = want, true
	}

	groupFilter := c.Query("group_id")
	// Free-text search needles lowercased once up front; per-agent
	// match converts on the fly so we avoid copying agent strings.
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))

	filtered := make([]*services.Agent, 0, len(agents))
	for _, a := range agents {
		if driftSet && a.DriftStatus != driftFilter {
			continue
		}
		if statusSet && a.Status != statusFilter {
			continue
		}
		if groupFilter != "" {
			if a.GroupID == nil || *a.GroupID != groupFilter {
				continue
			}
		}
		if q != "" && !agentMatchesSearch(a, q) {
			continue
		}
		filtered = append(filtered, a)
	}

	// ----- Sort -----
	// Stable order by UUID string so the same filter set produces
	// the same page across calls. UUID strings have no semantic
	// meaning to operators but the stability matters for
	// pagination correctness.
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].ID.String() < filtered[j].ID.String()
	})

	total := len(filtered)
	activeCount := 0
	for _, a := range filtered {
		if a.Status == services.AgentStatusOnline {
			activeCount++
		}
	}

	// ----- Paginate -----
	offset, limit, err := parsePagination(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	page := pageSlice(filtered, offset, limit)

	// Build the legacy agents map from the same paged slice so
	// pre-v0.23 clients see a consistent view. Note that the legacy
	// map shape doesn't expose Total separately from len(Agents),
	// so old clients that paginate by counting will need to switch
	// to items+total. We document the deprecation in the response
	// struct comment.
	agentsMap := make(map[string]*services.Agent, len(page))
	for _, a := range page {
		agentsMap[a.ID.String()] = a
	}

	c.JSON(http.StatusOK, GetAgentsResponse{
		Items:  page,
		Total:  total,
		Offset: offset,
		Limit:  limit,

		Agents:        agentsMap,
		TotalCount:    total,
		ActiveCount:   activeCount,
		InactiveCount: total - activeCount,
	})
}

// agentMatchesSearch reports whether the agent matches a
// lowercased substring across its name + id + label "k=v" pairs.
// Operators paste partial label values (e.g. "host.arch=arm") and
// expect those to filter; matching the encoded "key=value" form
// keeps that intuitive.
func agentMatchesSearch(a *services.Agent, q string) bool {
	if strings.Contains(strings.ToLower(a.Name), q) {
		return true
	}
	if strings.Contains(strings.ToLower(a.ID.String()), q) {
		return true
	}
	for k, v := range a.Labels {
		if strings.Contains(strings.ToLower(k+"="+v), q) {
			return true
		}
	}
	return false
}

// parsePagination resolves the offset/limit query params with
// sensible defaults + a hard cap on limit. Returns 400-friendly
// errors for malformed inputs so clients don't get a silent
// fallback to the default.
func parsePagination(c *gin.Context) (offset, limit int, err error) {
	if raw := c.Query("offset"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			return 0, 0, fmt.Errorf("offset must be a non-negative integer")
		}
		offset = n
	}
	limit = defaultAgentsLimit
	if raw := c.Query("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n <= 0 {
			return 0, 0, fmt.Errorf("limit must be a positive integer")
		}
		limit = n
	}
	if limit > maxAgentsLimit {
		limit = maxAgentsLimit
	}
	return offset, limit, nil
}

// pageSlice returns agents[offset:offset+limit] guarded against
// out-of-range offset / limit (returns empty slice rather than a
// panic). Pre-sized for the actual page so we don't keep a
// reference to the underlying full slice longer than necessary.
func pageSlice(agents []*services.Agent, offset, limit int) []*services.Agent {
	if offset >= len(agents) {
		return []*services.Agent{}
	}
	end := offset + limit
	if end > len(agents) {
		end = len(agents)
	}
	page := make([]*services.Agent, end-offset)
	copy(page, agents[offset:end])
	return page
}

// handleGetAgent handles GET /api/v1/agents/:id
func (h *AgentHandlers) HandleGetAgent(c *gin.Context) {
	agentID := c.Param("id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Agent ID is required"})
		return
	}

	// Parse UUID
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
		return
	}

	// Get agent from service
	agent, err := h.agentService.GetAgent(c.Request.Context(), agentUUID)
	if err != nil {
		h.logger.Error("Failed to get agent", zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch agent"})
		return
	}

	if agent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent not found"})
		return
	}

	c.JSON(http.StatusOK, agent)
}

// handleUpdateAgentGroup handles PATCH /api/v1/agents/:id/group
func (h *AgentHandlers) HandleUpdateAgentGroup(c *gin.Context) {
	// Not implemented in current interface
	c.JSON(http.StatusNotImplemented, gin.H{"error": "Agent group update not implemented"})
}

// handleGetAgentStats handles GET /api/v1/agents/stats
func (h *AgentHandlers) HandleGetAgentStats(c *gin.Context) {
	// Get all agents
	agents, err := h.agentService.ListAgents(c.Request.Context())
	if err != nil {
		h.logger.Error("Failed to get agents for stats", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch agent statistics"})
		return
	}

	// Count agents by status
	stats := GetAgentStatsResponse{
		TotalAgents: len(agents),
	}

	for _, agent := range agents {
		switch agent.Status {
		case services.AgentStatusOnline:
			stats.OnlineAgents++
		case services.AgentStatusOffline:
			stats.OfflineAgents++
		case services.AgentStatusError:
			stats.ErrorAgents++
		}
	}

	// Get groups count
	groups, err := h.agentService.ListGroups(c.Request.Context())
	if err != nil {
		h.logger.Error("Failed to get groups for stats", zap.Error(err))
		// Don't fail the request, just set groups count to 0
		stats.GroupsCount = 0
	} else {
		stats.GroupsCount = len(groups)
	}

	c.JSON(http.StatusOK, stats)
}

// SendConfigRequest represents the request to send config to an agent
type SendConfigRequest struct {
	Content string `json:"content" binding:"required"`
}

// SendConfigResponse represents the response after sending config to an agent
type SendConfigResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	ConfigID string `json:"config_id,omitempty"`
}

// HandleSendConfigToAgent handles POST /api/v1/agents/:id/config
// Orchestrates config storage (via AgentService) and delivery (via ConfigSender)
func (h *AgentHandlers) HandleSendConfigToAgent(c *gin.Context) {
	// 1. Parse agent ID from URL
	agentID := c.Param("id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Agent ID is required"})
		return
	}

	// Parse UUID
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
		return
	}

	// 2. Parse config content from request body
	var req SendConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Invalid request body: %v", err)})
		return
	}

	if req.Content == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Config content is required"})
		return
	}

	// 3. Store config in database (validates agent and capability)
	config, err := h.agentService.StoreConfigForAgent(c.Request.Context(), agentUUID, req.Content)
	if err != nil {
		h.logger.Error("Failed to store config",
			zap.String("agent_id", agentID),
			zap.Error(err))

		// Map service errors to appropriate HTTP status codes
		statusCode := http.StatusInternalServerError
		message := err.Error()

		if err.Error() == "agent not found" {
			statusCode = http.StatusNotFound
		} else if err.Error() == "agent does not support remote config" {
			statusCode = http.StatusBadRequest
		}

		c.JSON(statusCode, SendConfigResponse{
			Success: false,
			Message: message,
		})
		return
	}

	// 4. Send config to agent via OpAMP. Wrap in a config.push span
	// so the operator sees this direct manual push alongside the
	// rollout-driven pushes in their trace tool.
	push := h.configsTracer.BeginPush(c.Request.Context(), agentUUID.String(), config.ID, "", configs.SourceDirect)
	if err := h.commander.SendConfigToAgentWithContext(push.Context(), agentUUID, req.Content); err != nil {
		push.RecordNack(err.Error())
		push.End()
		h.logger.Error("Failed to send config to agent",
			zap.String("agent_id", agentID),
			zap.String("config_id", config.ID),
			zap.Error(err))

		// Config was stored but delivery failed
		c.JSON(http.StatusAccepted, SendConfigResponse{
			Success:  false,
			Message:  fmt.Sprintf("Config stored but delivery failed: %v", err),
			ConfigID: config.ID,
		})
		return
	}
	push.RecordAck()
	push.End()

	// 5. Return success response
	h.logger.Info("Configuration sent to agent successfully",
		zap.String("agent_id", agentID),
		zap.String("config_id", config.ID))

	c.JSON(http.StatusOK, SendConfigResponse{
		Success:  true,
		Message:  "Configuration sent to agent successfully",
		ConfigID: config.ID,
	})
}

// HandleDecommissionAgent is DELETE /api/v1/agents/:id. v0.35
// affordance for cleaning up agents that have been retired from
// the fleet — without this, an offline Windows host that's been
// physically decommissioned sits forever in the agents table as
// "offline" and clutters the inventory reconciliation view.
//
// The agent record is hard-deleted; the audit log retains the
// decommission event for trail. Telemetry rows in the
// metrics_*/logs/traces tables are unaffected (they carry an
// agent_id but are not foreign-keyed). The next OpAMP heartbeat
// from the same UUID would re-create the agent — which is what we
// want if the host wasn't actually retired.
func (h *AgentHandlers) HandleDecommissionAgent(c *gin.Context) {
	agentID := c.Param("id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Agent ID is required"})
		return
	}
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
		return
	}
	if err := h.agentService.DeleteAgent(c.Request.Context(), agentUUID); err != nil {
		h.logger.Error("decommission agent failed", zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decommission agent"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "agent_id": agentID})
}

// RestartAgentResponse represents the response after restarting an agent
type RestartAgentResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// HandleRestartAgent handles POST /api/v1/agents/:id/restart
func (h *AgentHandlers) HandleRestartAgent(c *gin.Context) {
	// 1. Parse agent ID from URL
	agentID := c.Param("id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Agent ID is required"})
		return
	}

	// Parse UUID
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
		return
	}

	// 2. Send restart command to agent via OpAMP
	if err := h.commander.RestartAgent(agentUUID); err != nil {
		h.logger.Error("Failed to restart agent",
			zap.String("agent_id", agentID),
			zap.Error(err))

		// Map errors to appropriate HTTP status codes
		statusCode := http.StatusInternalServerError
		message := err.Error()

		if err.Error() == "agent not found" {
			statusCode = http.StatusNotFound
		} else if err.Error() == "agent does not support restart command" {
			statusCode = http.StatusBadRequest
		}

		c.JSON(statusCode, RestartAgentResponse{
			Success: false,
			Message: message,
		})
		return
	}

	// 3. Return success response
	h.logger.Info("Restart command sent to agent successfully",
		zap.String("agent_id", agentID))

	c.JSON(http.StatusOK, RestartAgentResponse{
		Success: true,
		Message: "Restart command sent to agent successfully",
	})
}
