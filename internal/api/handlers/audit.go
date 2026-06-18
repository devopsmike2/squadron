// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// AuditHandlers serves the audit log endpoints. Read-only as of v0.50;
// v0.57 added the explain endpoint which mutates exactly one row, the
// requested one, and only to cache the AI explanation. The audit
// service enforces immutability of every other field.
type AuditHandlers struct {
	auditService services.AuditService
	aiService    *ai.Service                          // optional; nil 503s the explain route
	appStore     applicationstore.ApplicationStore    // optional; nil means no context enrichment
	logger       *zap.Logger
}

// NewAuditHandlers constructs the handlers. Pass nil for aiService /
// appStore in tests that do not exercise the explain endpoint.
func NewAuditHandlers(
	auditService services.AuditService,
	aiService *ai.Service,
	appStore applicationstore.ApplicationStore,
	logger *zap.Logger,
) *AuditHandlers {
	return &AuditHandlers{
		auditService: auditService,
		aiService:    aiService,
		appStore:     appStore,
		logger:       logger,
	}
}

// HandleListAuditEvents serves GET /api/v1/audit/events.
//
// Query parameters (all optional):
//   - target_type=agent|group|config|rule
//   - target_id=<uuid|string>
//   - since=<RFC3339 timestamp>
//   - limit=<int, default 100, max 1000>
//
// Returns {events: [...]} sorted newest-first.
func (h *AuditHandlers) HandleListAuditEvents(c *gin.Context) {
	filter := services.AuditEventFilter{
		TargetType: c.Query("target_type"),
		TargetID:   c.Query("target_id"),
	}

	if raw := c.Query("since"); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  "invalid `since` — expected RFC3339",
				"detail": err.Error(),
			})
			return
		}
		filter.Since = ts
	}

	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "invalid `limit` — expected positive integer",
			})
			return
		}
		filter.Limit = n
	}

	events, err := h.auditService.List(c.Request.Context(), filter)
	if err != nil {
		h.logger.Error("failed to list audit events", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list audit events"})
		return
	}
	if events == nil {
		events = []*services.AuditEvent{}
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

// AuditExplainResponse is the JSON body returned by HandleExplainAuditEvent.
type AuditExplainResponse struct {
	Explanation      string    `json:"explanation"`
	Model            string    `json:"model"`
	GeneratedAt      time.Time `json:"generated_at"`
	Cached           bool      `json:"cached"`
	RedactionSummary string    `json:"redaction_summary,omitempty"`
}

// HandleExplainAuditEvent serves POST /api/v1/audit/:id/explain.
//
// Query parameters:
//   - regenerate=1 — bypass the cached explanation and call the LLM
//     even when the row already has one. The new value replaces
//     whatever was cached.
//
// Returns 200 with {explanation, model, generated_at, cached,
// redaction_summary?} on success. 404 if the row does not exist.
// 503 if the AI service is not configured. 502 if the LLM call fails.
func (h *AuditHandlers) HandleExplainAuditEvent(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}

	if h.aiService == nil || !h.aiService.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "AI assist is not configured",
			"enabled": false,
		})
		return
	}

	event, err := h.auditService.Get(c.Request.Context(), id)
	if err != nil {
		h.logger.Error("failed to load audit event for explain",
			zap.String("id", id),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load audit event"})
		return
	}
	if event == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "audit event not found", "id": id})
		return
	}

	regenerate := c.Query("regenerate") == "1" || c.Query("regenerate") == "true"

	// Cache hit short circuits the LLM call entirely. Audit rows are
	// immutable so a cached explanation never goes stale in the data
	// sense; the regenerate flag is the operator's "give me a fresh
	// angle" escape hatch.
	if !regenerate && event.AIExplanation != "" {
		generatedAt := time.Now().UTC()
		if event.AIExplanationGeneratedAt != nil {
			generatedAt = *event.AIExplanationGeneratedAt
		}
		c.JSON(http.StatusOK, AuditExplainResponse{
			Explanation: event.AIExplanation,
			Model:       event.AIExplanationModel,
			GeneratedAt: generatedAt,
			Cached:      true,
		})
		return
	}

	// Context enrichment: try to look up the entity referenced by
	// (target_type, target_id) and pass a few human-readable fields
	// into the prompt so the explanation can use real names instead of
	// raw IDs. The store is optional; a nil appStore just skips this
	// step and the model works from the bare audit row.
	ctxBag := h.buildExplainContext(c, event)

	result, err := h.aiService.ExplainAuditEvent(c.Request.Context(), ai.ExplainAuditEventInput{
		EventID:    event.ID,
		Timestamp:  event.Timestamp,
		Actor:      event.Actor,
		EventType:  event.EventType,
		TargetType: event.TargetType,
		TargetID:   event.TargetID,
		Action:     event.Action,
		Payload:    event.Payload,
		Context:    ctxBag,
	})
	if err != nil {
		h.logger.Warn("explain audit event failed",
			zap.String("id", id),
			zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{
			"error":  "failed to generate explanation",
			"detail": err.Error(),
		})
		return
	}

	now := time.Now().UTC()
	if err := h.auditService.SetExplanation(c.Request.Context(),
		event.ID, result.Explanation, result.Model, now); err != nil {
		// Persistence failure is logged but not fatal; we still
		// return the freshly generated explanation so the operator
		// sees something on their click. The cache miss will repeat
		// the work next time, which is annoying but not broken.
		h.logger.Warn("failed to cache audit explanation",
			zap.String("id", id),
			zap.Error(err))
	}

	c.JSON(http.StatusOK, AuditExplainResponse{
		Explanation:      result.Explanation,
		Model:            result.Model,
		GeneratedAt:      now,
		Cached:           false,
		RedactionSummary: result.RedactionSummary,
	})
}

// buildExplainContext resolves the target referenced by the audit row
// into a small bag of human-readable strings the model can use in its
// narrative. The bag is intentionally flat (key/value strings) so the
// prompt doesn't have to encode nested structure; the LLM uses them as
// hints, not as structured data.
//
// The lookup is best-effort: if any step fails (store error, target
// not found, target type unknown) we return whatever we have. The
// model gets less context, the explanation is still produced.
func (h *AuditHandlers) buildExplainContext(c *gin.Context, event *services.AuditEvent) map[string]string {
	ctxBag := make(map[string]string)
	if h.appStore == nil || event.TargetID == "" {
		return ctxBag
	}
	ctx := c.Request.Context()

	switch event.TargetType {
	case services.AuditTargetGroup:
		g, err := h.appStore.GetGroup(ctx, event.TargetID)
		if err == nil && g != nil {
			ctxBag["group.name"] = g.Name
		}
	case services.AuditTargetAgent:
		// Agent IDs are stored as uuid.UUID; an audit row addressed at
		// a malformed UUID falls through the parse and we skip the
		// lookup (no context, but the explanation still runs).
		if agentID, err := uuid.Parse(event.TargetID); err == nil {
			a, err := h.appStore.GetAgent(ctx, agentID)
			if err == nil && a != nil {
				ctxBag["agent.name"] = a.Name
				ctxBag["agent.status"] = string(a.Status)
				if a.GroupID != nil && *a.GroupID != "" {
					ctxBag["agent.group_id"] = *a.GroupID
				}
				if a.GroupName != nil && *a.GroupName != "" {
					ctxBag["agent.group_name"] = *a.GroupName
				}
			}
		}
	case "rollout":
		r, err := h.appStore.GetRollout(ctx, event.TargetID)
		if err == nil && r != nil {
			ctxBag["rollout.name"] = r.Name
			ctxBag["rollout.state"] = string(r.State)
			ctxBag["rollout.group_id"] = r.GroupID
			ctxBag["rollout.stage_index"] = fmt.Sprintf("%d of %d",
				r.CurrentStage+1, len(r.Stages))
			if r.ProposedBy != "" {
				ctxBag["rollout.proposed_by"] = r.ProposedBy
			}
		}
	case services.AuditTargetActionRequest:
		req, err := h.appStore.GetActionRequest(ctx, event.TargetID)
		if err == nil && req != nil {
			ctxBag["action.type"] = req.ActionType
			ctxBag["action.phase"] = req.Phase
			ctxBag["action.status"] = req.Status
			ctxBag["action.runner_id"] = req.RunnerID
			if req.DeniedFor != "" {
				ctxBag["action.denied_for"] = req.DeniedFor
			}
		}
	case services.AuditTargetIncidentDraft:
		d, err := h.appStore.GetIncidentDraft(ctx, event.TargetID)
		if err == nil && d != nil {
			ctxBag["incident.title"] = d.Title
			ctxBag["incident.status"] = d.Status
			if d.Provider != "" {
				ctxBag["incident.provider"] = d.Provider
			}
		}
	}
	return ctxBag
}
