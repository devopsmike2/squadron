package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/api/middleware"
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

	// auditService, when non-nil, receives an agent.group_reassigned event
	// when an operator changes an agent's group via HandleUpdateAgentGroup.
	// Optional — a nil recorder means "no audit emission" so existing tests
	// stay compiling. Mirrors the DiscoveryHandlers.WithAuditService idiom.
	auditService services.AuditService
}

// WithAuditService wires the audit recorder used by HandleUpdateAgentGroup.
// Optional — a nil recorder is treated as "no audit emission". Fluent so the
// server can chain it onto the constructor.
func (h *AgentHandlers) WithAuditService(a services.AuditService) *AgentHandlers {
	h.auditService = a
	return h
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

// UpdateAgentGroupRequest represents the request to update agent group.
// A null/absent or empty group_id clears the agent's group assignment;
// a non-empty value must reference an existing group (validated against
// the store in the handler rather than by a binding tag, so a "" clear
// isn't rejected as a malformed UUID).
type UpdateAgentGroupRequest struct {
	GroupID *string `json:"group_id"`
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
//     key=value pairs (case-insensitive)
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

// handleUpdateAgentGroup handles PATCH /api/v1/agents/:id/group.
//
// Re-points an existing agent at a different group (or clears its
// assignment). Persists through AgentService.UpdateAgentRegistration so
// the stored GroupID/GroupName — which rollout canary scoping reads
// back — stays in sync with the operator's choice. Before v0.89.347
// this was a hard 501 stub even though the Fleet UI's group dropdown
// called it.
func (h *AgentHandlers) HandleUpdateAgentGroup(c *gin.Context) {
	agentID := c.Param("id")
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
		return
	}

	var req UpdateAgentGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	agent, err := h.agentService.GetAgent(c.Request.Context(), agentUUID)
	if err != nil {
		h.logger.Error("Failed to get agent for group update",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch agent"})
		return
	}
	if agent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent not found"})
		return
	}

	// Snapshot the pre-change group BEFORE the normalize block mutates the
	// agent in place, so the audit row can report the from→to transition.
	fromGroupID := derefOrEmpty(agent.GroupID)
	fromGroupName := derefOrEmpty(agent.GroupName)

	// Normalize: treat null and "" identically as "clear assignment".
	if req.GroupID == nil || *req.GroupID == "" {
		agent.GroupID = nil
		agent.GroupName = nil
	} else {
		group, err := h.agentService.GetGroup(c.Request.Context(), *req.GroupID)
		if err != nil {
			h.logger.Error("Failed to look up group for agent update",
				zap.String("group_id", *req.GroupID), zap.Error(err))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up group"})
			return
		}
		if group == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Group not found"})
			return
		}
		gid := group.ID
		gname := group.Name
		agent.GroupID = &gid
		agent.GroupName = &gname
	}

	if err := h.agentService.UpdateAgentRegistration(c.Request.Context(), agent); err != nil {
		h.logger.Error("Failed to update agent group",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update agent group"})
		return
	}

	// Operator-action audit: record the group reassignment on the timeline,
	// but only when the group actually changed — a no-op re-assignment to the
	// same group (the Fleet dropdown re-selecting the current value) emits
	// nothing, mirroring the exclusion/rollback-flag transition-only posture.
	// Best-effort: a nil recorder or a Record error never fails the request.
	toGroupID := derefOrEmpty(agent.GroupID)
	toGroupName := derefOrEmpty(agent.GroupName)
	if h.auditService != nil && toGroupID != fromGroupID {
		actor := middleware.ActorFromGin(c).String()
		if actor == "" {
			actor = services.AuditActorSystem
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      actor,
			EventType:  services.AuditEventAgentGroupReassigned,
			TargetType: services.AuditTargetAgent,
			TargetID:   agentID,
			Action:     "group_reassigned",
			Payload: map[string]any{
				"agent_id":        agentID,
				"from_group_id":   fromGroupID,
				"from_group_name": fromGroupName,
				"to_group_id":     toGroupID,
				"to_group_name":   toGroupName,
			},
		})
	}

	c.JSON(http.StatusOK, agent)
}

// derefOrEmpty returns the pointed-to string or "" for a nil pointer. Used to
// render optional group id/name fields for comparison + audit payloads.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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

// ClearAgentConfigResponse is returned by HandleClearAgentConfig.
//
// Cleared reports whether an agent-scoped config actually existed and was
// removed (the delete is idempotent, so a clear on an agent with none succeeds
// with cleared=false). FellBackTo is "group" when the agent belongs to a group
// that has a config — now the resolved desired config — or "none" when no group
// config applies. GroupID is set when FellBackTo is "group". Pushed reports
// whether that resolved group config was delivered to the (connected) agent in
// this call; the reconcile loop is the durability backstop when it was not.
type ClearAgentConfigResponse struct {
	Cleared    bool   `json:"cleared"`
	FellBackTo string `json:"fell_back_to"`
	GroupID    string `json:"group_id,omitempty"`
	Pushed     bool   `json:"pushed"`
	Message    string `json:"message"`
}

// HandleClearAgentConfig handles DELETE /api/v1/agents/:id/config.
//
// Clears an agent's OWN agent-scoped config so config resolution falls back to
// the agent's GROUP config. Config resolution is agent-specific-wins
// (internal/opamp/config_resolver.go resolveStoredConfig: agent-scoped config,
// else group config, else the connect-path default/adopt-on-supervise). A
// supervised agent that was auto-seeded an agent-scoped config on first
// supervise (adopt-on-first-supervise, ADR 0039) therefore ignores any group
// config assigned later. This endpoint is the operator's way to drop that
// agent-scoped config so the agent resumes tracking its group's config.
//
// It deletes ONLY the agent-scoped config(s) — DeleteConfigsForAgent filters on
// agent_id, so a group-scoped config (agent_id NULL) is never touched, and no
// other agent is affected. Scoped agents:write, mirroring HandleUpdateAgentGroup.
//
// After clearing, if the agent is in a group that has a config, that group
// config is the newly-resolved desired config; the handler delivers it promptly
// via the same direct push HandleSendConfigToAgent uses, so a supervised agent
// gets the group config now rather than on the next ~30s reconcile tick. A
// delivery failure is non-fatal (the config is already cleared and the reconcile
// loop re-delivers).
//
// Edge case — no group config: if the agent is in no group, or its group has no
// config, clearing leaves the agent with NO resolved stored config. For a
// supervised agent that still advertises accepts_remote_config and reports an
// effective config, the next OpAMP connect can re-fire adopt-on-first-supervise
// and re-seed a fresh agent-scoped config. The intended use is "fall back to an
// EXISTING group config"; the UI disables the action when there is no group.
func (h *AgentHandlers) HandleClearAgentConfig(c *gin.Context) {
	agentID := c.Param("id")
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
		return
	}

	agent, err := h.agentService.GetAgent(c.Request.Context(), agentUUID)
	if err != nil {
		h.logger.Error("Failed to get agent for clear-config",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch agent"})
		return
	}
	if agent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent not found"})
		return
	}

	// Was an agent-scoped config actually present? Reported back so the operator
	// sees "nothing to clear" distinctly from "cleared"; the delete below is
	// idempotent regardless of this.
	existing, err := h.agentService.GetLatestConfigForAgent(c.Request.Context(), agentUUID)
	if err != nil {
		h.logger.Error("Failed to look up agent-scoped config for clear-config",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to look up agent config"})
		return
	}
	hadAgentConfig := existing != nil

	// Delete ONLY the agent-scoped config(s). Never touches a group config.
	if err := h.agentService.DeleteConfigsForAgent(c.Request.Context(), agentUUID); err != nil {
		h.logger.Error("Failed to clear agent-scoped config",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to clear agent config"})
		return
	}

	resp := ClearAgentConfigResponse{
		Cleared:    hadAgentConfig,
		FellBackTo: "none",
	}

	// Resolve the newly-current desired config. With the agent-scoped row gone,
	// resolution falls back to the group config (resolveStoredConfig precedence).
	// When the agent is in a group that has a config, deliver it promptly —
	// mirror the direct-push path so a supervised agent gets the group config now.
	if agent.GroupID != nil && *agent.GroupID != "" {
		groupCfg, gerr := h.agentService.GetLatestConfigForGroup(c.Request.Context(), *agent.GroupID)
		if gerr != nil {
			h.logger.Warn("clear-config: failed to resolve group config after clear",
				zap.String("agent_id", agentID), zap.String("group_id", *agent.GroupID), zap.Error(gerr))
		} else if groupCfg != nil {
			resp.FellBackTo = "group"
			resp.GroupID = *agent.GroupID

			push := h.configsTracer.BeginPush(c.Request.Context(), agentUUID.String(), groupCfg.ID, *agent.GroupID, configs.SourceDirect)
			if perr := h.commander.SendConfigToAgentWithContext(push.Context(), agentUUID, groupCfg.Content); perr != nil {
				push.RecordNack(perr.Error())
				push.End()
				h.logger.Warn("clear-config: group config delivery failed; reconcile loop is the backstop",
					zap.String("agent_id", agentID), zap.String("config_id", groupCfg.ID), zap.Error(perr))
			} else {
				push.RecordAck()
				push.End()
				resp.Pushed = true
			}
		}
	}

	// Operator-action audit (best-effort; a nil recorder or Record error never
	// fails the request). Mirrors HandleUpdateAgentGroup.
	if h.auditService != nil {
		actor := middleware.ActorFromGin(c).String()
		if actor == "" {
			actor = services.AuditActorSystem
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      actor,
			EventType:  services.AuditEventAgentConfigCleared,
			TargetType: services.AuditTargetAgent,
			TargetID:   agentID,
			Action:     "config_cleared",
			Payload: map[string]any{
				"agent_id":         agentID,
				"had_agent_config": hadAgentConfig,
				"fell_back_to":     resp.FellBackTo,
				"group_id":         resp.GroupID,
			},
		})
	}

	switch {
	case resp.Pushed:
		resp.Message = "Cleared the agent config. The group config is now in effect and was delivered to the agent."
	case resp.FellBackTo == "group":
		resp.Message = "Cleared the agent config. The group config is now in effect and will be delivered on the next reconcile."
	default:
		resp.Message = "Cleared the agent config. The agent is in no group with a config, so no config is currently assigned."
	}

	h.logger.Info("Cleared agent-scoped config",
		zap.String("agent_id", agentID),
		zap.Bool("had_agent_config", hadAgentConfig),
		zap.String("fell_back_to", resp.FellBackTo),
		zap.Bool("pushed", resp.Pushed))

	c.JSON(http.StatusOK, resp)
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

// HandleDismissDuplicate is POST /api/v1/agents/:id/dismiss-duplicate.
//
// Backlog #5 (Southern log-fan-out incident). When Squadron flags a
// telemetry-only agent as a suspected duplicate of an OpAMP-managed agent on
// the same host, the operator has two outs: Decommission (the phantom is real
// junk — reuse DELETE /agents/:id) or Dismiss (this is a legitimate separate
// agent that just happens to share the host.name — stop flagging it). This is
// the Dismiss path: it records the decision as a reserved agent label the
// detector honors (services.LabelDuplicateDismissed), so the badge clears and
// stays cleared across restarts. It does NOT delete, merge, or move any
// telemetry — it is purely "I looked, it's fine".
//
// The label rides on the agent's existing registration fields, persisted via
// UpdateAgentRegistration. That is safe precisely because telemetry-only agents
// never re-report an AgentDescription over OpAMP, so nothing overwrites their
// labels after this write.
func (h *AgentHandlers) HandleDismissDuplicate(c *gin.Context) {
	agentID := c.Param("id")
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
		return
	}

	agent, err := h.agentService.GetAgent(c.Request.Context(), agentUUID)
	if err != nil {
		h.logger.Error("Failed to get agent for dismiss-duplicate",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch agent"})
		return
	}
	if agent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent not found"})
		return
	}

	// Idempotent: set the reserved dismiss label and persist. Re-dismissing an
	// already-dismissed agent is a harmless no-op write.
	if agent.Labels == nil {
		agent.Labels = map[string]string{}
	}
	agent.Labels[services.LabelDuplicateDismissed] = "true"

	if err := h.agentService.UpdateAgentRegistration(c.Request.Context(), agent); err != nil {
		h.logger.Error("Failed to persist duplicate dismissal",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to dismiss duplicate"})
		return
	}

	// Operator-action audit (best-effort; a nil recorder or Record error never
	// fails the request).
	if h.auditService != nil {
		actor := middleware.ActorFromGin(c).String()
		if actor == "" {
			actor = services.AuditActorSystem
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      actor,
			EventType:  services.AuditEventAgentDuplicateDismissed,
			TargetType: services.AuditTargetAgent,
			TargetID:   agentID,
			Action:     "duplicate_dismissed",
			Payload: map[string]any{
				"agent_id":  agentID,
				"host_name": agent.Labels["host.name"],
			},
		})
	}

	// Re-read so the response reflects the cleared flag (the detector now skips
	// this agent, so SuspectedDuplicateOf comes back nil).
	updated, err := h.agentService.GetAgent(c.Request.Context(), agentUUID)
	if err != nil || updated == nil {
		c.JSON(http.StatusOK, agent)
		return
	}
	c.JSON(http.StatusOK, updated)
}

// AdoptConfigRequest is the (optional) body for POST
// /api/v1/agents/:id/adopt-config. Name overrides the default
// "<agent>-adopted" template name; everything else is derived from the
// agent's reported effective config.
type AdoptConfigRequest struct {
	Name string `json:"name,omitempty"`
}

// AdoptConfigResponse is returned by HandleAdoptConfig. Config is the
// newly created managed template. Source records WHERE the adopted
// content came from: "agent_config" or "group_config" when the agent
// resolves to a managed (delivered/intent) config whose templated content
// preserves ${ENV}, or "effective_config" when the agent is unmanaged and
// we fall back to its reported effective config. Redacted flags that the
// adopted content still contained literal redaction markers (OpAMP redacts
// secret values in a supervisor's reported effective config); Note carries
// the operator-facing explanation — including, on the effective-config
// fallback, that ${ENV} references could NOT be preserved because the
// reported effective config is post-resolution. Warnings surfaces the same
// non-fatal structural hints (missing recommended sections, etc.) the
// /configs/validate endpoint returns, so the operator sees them before
// assigning.
type AdoptConfigResponse struct {
	Config   *services.Config `json:"config"`
	Source   string           `json:"source,omitempty"`
	Redacted bool             `json:"redacted"`
	Note     string           `json:"note,omitempty"`
	Warnings []string         `json:"warnings,omitempty"`
}

// HandleAdoptConfig handles POST /api/v1/agents/:id/adopt-config.
//
// "Adopt an agent's config as a managed template": capture a brownfield
// agent's config as a standalone managed Config entity so an operator can
// assign and manage it going forward — the clean bridge from report-only /
// supervised to managed without re-authoring the config by hand.
//
// Source precedence (this is the load-bearing fix — see the field finding
// in the adopt knowledge note): prefer the agent's DELIVERED/INTENT managed
// config over its reported effective config.
//   - If the agent resolves to a managed config — agent-scoped intent first,
//     then its group's config (the same agent → group precedence the OpAMP
//     resolver uses) — adopt FROM that config's STORED, TEMPLATED content.
//     That content is what Squadron pushes, so it RETAINS ${ENV} references
//     and carries no [REDACTED] secrets. Adopting a group-config agent thus
//     produces a standalone, unassigned copy of the group's templated config.
//   - Only when the agent is genuinely UNMANAGED (no agent-scoped intent and
//     no group config) do we fall back to the reported effective config —
//     today's behavior. Under a supervisor that reported config is
//     POST-resolution: ${ENV} refs are already substituted to literals and
//     secrets are redacted. So the fallback flags Redacted when markers are
//     present AND its Note states plainly that ${ENV} could NOT be preserved
//     (adopted from the resolved effective config).
//
// In all cases:
//   - The content is VALIDATED structurally before creation (same YAML check
//     the update/validate paths use); an unparseable config is rejected
//     rather than stored, so Squadron never captures a template it could
//     later push to brick an agent.
//   - The new Config is created UNASSIGNED (no agent_id, no group_id) and is
//     NOT pushed anywhere. Assignment happens later via the normal flow. The
//     source agent and any existing managed config are not modified.
//
// This is the OPERATOR-INITIATED adopt endpoint. It is distinct from the
// SUPERVISE-time auto-seed (ADR 0039, internal/opamp tryAdoptEffectiveConfig),
// which seeds an unmanaged agent's FIRST config from its reported effective
// config on connect. That path is untouched here.
//
// Per-host parameterization (filelog paths, service.instance.id, hostnames)
// is deliberately out of scope for this slice — see the backlog follow-ups.
func (h *AgentHandlers) HandleAdoptConfig(c *gin.Context) {
	agentID := c.Param("id")
	agentUUID, err := uuid.Parse(agentID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid agent ID format"})
		return
	}

	// Body is optional — an absent/empty body just means "use defaults".
	var req AdoptConfigRequest
	if c.Request.Body != nil {
		// Ignore bind errors on an empty body; only a malformed non-empty
		// JSON body is a client error worth surfacing.
		if bindErr := c.ShouldBindJSON(&req); bindErr != nil && bindErr.Error() != "EOF" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body", "details": bindErr.Error()})
			return
		}
	}

	agent, err := h.agentService.GetAgent(c.Request.Context(), agentUUID)
	if err != nil {
		h.logger.Error("Failed to get agent for adopt-config",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch agent"})
		return
	}
	if agent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Agent not found"})
		return
	}

	// Prefer the DELIVERED/INTENT managed config over the reported effective
	// config. A managed config's STORED content is the templated source
	// Squadron pushes, so it RETAINS ${ENV} references and is not redacted;
	// a supervisor's reported effective config is post-resolution (env refs
	// substituted, secrets redacted). Resolve agent-scoped intent first, then
	// the group config — the same agent → group precedence the OpAMP resolver
	// (internal/opamp/config_resolver.go) applies.
	var (
		content       string
		source        string
		fromEffective bool
	)
	if managed, err := h.agentService.GetLatestConfigForAgent(c.Request.Context(), agentUUID); err == nil && managed != nil {
		content = managed.Content
		source = "agent_config"
	} else if agent.GroupID != nil && strings.TrimSpace(*agent.GroupID) != "" {
		if groupCfg, err := h.agentService.GetLatestConfigForGroup(c.Request.Context(), *agent.GroupID); err == nil && groupCfg != nil {
			content = groupCfg.Content
			source = "group_config"
		}
	}

	// Fall back to the reported effective config only when the agent is
	// genuinely UNMANAGED (no agent-scoped intent, no group config).
	if strings.TrimSpace(content) == "" {
		content = agent.EffectiveConfig
		source = "effective_config"
		fromEffective = true
	}

	if strings.TrimSpace(content) == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Agent has no managed config assigned and has not reported an effective config yet. Assign a config, or register the agent report-only over OpAMP (reports_effective_config) and let it check in, then try again.",
		})
		return
	}

	// Validate structurally before we store anything. Reuse the same YAML
	// check the config update/validate endpoints use so an unparseable
	// config is rejected here rather than captured as a template that could
	// later be pushed to an agent.
	if err := validateYAMLConfig(content); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Adopted config is not valid YAML and cannot be adopted",
			"details": err.Error(),
		})
		return
	}

	// Non-fatal structural hints (missing recommended sections, no
	// pipelines) — surfaced, not blocking. validateOTelConfig only errors
	// on a YAML parse failure, which we already ruled out above.
	warnings, _ := validateOTelConfig(content)

	name := strings.TrimSpace(req.Name)
	if name == "" {
		base := strings.TrimSpace(agent.Name)
		if base == "" {
			base = agentUUID.String()
		}
		name = base + "-adopted"
	}

	config := &services.Config{
		ID:         uuid.New().String(),
		Name:       name,
		AgentID:    nil, // unassigned managed template
		GroupID:    nil,
		ConfigHash: hashConfig(content),
		Content:    content,
		Version:    1,
		CreatedAt:  time.Now(),
	}

	if err := h.agentService.CreateConfig(c.Request.Context(), config); err != nil {
		h.logger.Error("Failed to create adopted config",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create config"})
		return
	}

	resp := AdoptConfigResponse{
		Config:   config,
		Source:   source,
		Warnings: warnings,
	}
	if fromEffective {
		// Fallback path: adopted from the agent's reported effective config
		// because it has no managed (delivered/intent) config. Under a
		// supervisor that reported config is post-resolution — ${ENV} refs are
		// already substituted to literals and secrets are redacted — so state
		// plainly that env references could NOT be preserved.
		resp.Note = "Adopted from the agent's reported effective config because it has no managed (assigned or group) config. If this agent runs under a supervisor, the reported effective config is already resolved: ${ENV} references are substituted to literal values and cannot be preserved, and any secrets are redacted. Review the config and re-introduce ${ENV} references before assigning."
		if configLooksRedacted(content) {
			resp.Redacted = true
			resp.Note = "The reported effective config appears to contain redacted secret values, and it was adopted from the resolved effective config (this agent has no managed config), so ${ENV} references could NOT be preserved — they are already substituted to literal values. Replace any literal redaction markers and re-introduce the appropriate ${ENV} references before assigning this config to an agent."
		}
	} else {
		// Adopted from a managed (delivered/intent) config: its stored,
		// templated content preserves ${ENV} references and is not redacted.
		resp.Note = "Adopted from the agent's managed " + source + "; its templated content preserves ${ENV} references."
	}

	h.logger.Info("Adopted agent config as managed template",
		zap.String("agent_id", agentID),
		zap.String("config_id", config.ID),
		zap.String("config_name", name),
		zap.String("source", source),
		zap.Bool("redacted", resp.Redacted))

	c.JSON(http.StatusCreated, resp)
}

// configLooksRedacted reports whether the config text carries a literal
// redaction marker of the kind OpAMP inserts when it strips a secret
// value from the effective config. It is intentionally conservative — a
// false positive only adds an advisory note, it never blocks adoption.
// ${ENV} references are NOT redaction markers (they are the desired,
// preserved form) so they are not matched here.
func configLooksRedacted(content string) bool {
	lower := strings.ToLower(content)
	for _, marker := range []string{"<redacted>", "[redacted]", "${redacted}", "\"redacted\"", "'redacted'"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
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
