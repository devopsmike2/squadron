// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/services"
)

// v0.63 — conversational Ask Squadron surface. The handler walks
// a few read services to build a small context bag, hands it to
// ai.Service.Ask, and returns the answer + citations to the UI.
//
// Deliberately scoped: this slice loads recent rollouts + recent
// audit events. Cost spikes, agents, recommendations are left for
// a follow up release so the citation kinds the UI must know how
// to render stay small. The system prompt knows about all five
// kinds, so adding a source later is a one liner in the handler
// plus a new chip color in the UI.

// AskHandler bundles the AI service with the read services the
// context builder needs. Constructed per request from Server's
// trampoline so a late-wired service nils don't panic.
type AskHandler struct {
	ai             *ai.Service
	rolloutService services.RolloutService
	auditService   services.AuditService
	logger         *zap.Logger
}

func NewAskHandler(
	aiSvc *ai.Service,
	rolloutSvc services.RolloutService,
	auditSvc services.AuditService,
	logger *zap.Logger,
) *AskHandler {
	return &AskHandler{
		ai:             aiSvc,
		rolloutService: rolloutSvc,
		auditService:   auditSvc,
		logger:         logger,
	}
}

// AskRequest is the wire shape. Question is required; everything
// else is freeform color the UI may add later (a focus filter, for
// example) but is unused in this slice.
type AskRequest struct {
	Question string `json:"question"`
}

// askQuestionLimit caps the inbound question. Long prompts are
// almost always copy paste mistakes; a hard cap stops them from
// silently eating an Anthropic context window.
const askQuestionLimit = 500

// askRecentRollouts is how many rollouts we surface in the bag.
// Sized for a one prompt context, not pagination.
const askRecentRollouts = 8

// askRecentAuditEvents is the same cap for audit events.
const askRecentAuditEvents = 15

// HandleAsk — POST /api/v1/ai/ask
//
// Builds the context bag from the rollout + audit services, hands
// it to ai.Service.Ask, returns {answer, citations, model,
// tokens_in, tokens_out}. Scope: ask:read.
func (h *AskHandler) HandleAsk(c *gin.Context) {
	var req AskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := strings.TrimSpace(req.Question)
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "question is required"})
		return
	}
	if len(q) > askQuestionLimit {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("question is too long (max %d chars)", askQuestionLimit),
		})
		return
	}

	ctx := c.Request.Context()
	bag, hints := h.buildBag(ctx)

	result, err := h.ai.Ask(ctx, ai.AskInput{
		Question: q,
		Context:  bag,
		Hints:    hints,
	})
	if err != nil {
		if errors.Is(err, ai.ErrDisabled) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":   "AI assist is not configured (set ANTHROPIC_API_KEY and enable in config)",
				"enabled": false,
			})
			return
		}
		h.logger.Warn("ask failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// buildBag walks the read services and returns the small context
// bag the AI prompt cites from, plus a hints block for color the
// prompt is told NOT to cite from.
//
// Failures from any individual service are swallowed and logged.
// The operator gets a degraded answer ("I don't have rollouts
// loaded") rather than a 500. Two services partial failure is
// better than zero answer.
func (h *AskHandler) buildBag(ctx context.Context) (map[string]string, map[string]string) {
	bag := map[string]string{}
	hints := map[string]string{
		"now": time.Now().UTC().Format(time.RFC3339),
	}

	if h.rolloutService != nil {
		rollouts, err := h.rolloutService.List(ctx, services.RolloutFilter{Limit: askRecentRollouts})
		if err != nil {
			h.logger.Debug("ask: rollouts.List failed", zap.Error(err))
		} else {
			hints["rollouts_recent_count"] = fmt.Sprintf("%d", len(rollouts))
			for _, r := range rollouts {
				if r == nil {
					continue
				}
				bag["rollout:"+r.ID] = summarizeRollout(r)
			}
		}
	}

	if h.auditService != nil {
		events, err := h.auditService.List(ctx, services.AuditEventFilter{Limit: askRecentAuditEvents})
		if err != nil {
			h.logger.Debug("ask: audit.List failed", zap.Error(err))
		} else {
			hints["audit_recent_count"] = fmt.Sprintf("%d", len(events))
			for _, e := range events {
				if e == nil {
					continue
				}
				bag["audit:"+e.ID] = summarizeAuditEvent(e)
			}
		}
	}

	return bag, hints
}

// summarizeRollout produces the one liner the prompt sees. Keep
// concrete: name, state, group, two timestamps. The model is told
// to cite by id (the map key), so the value is just the
// authoritative summary it can quote into the answer.
func summarizeRollout(r *services.Rollout) string {
	parts := []string{
		fmt.Sprintf("name=%s", r.Name),
		fmt.Sprintf("state=%s", r.State),
		fmt.Sprintf("group=%s", r.GroupID),
	}
	if r.RolledBackFromID != "" {
		parts = append(parts, "rollback_of="+r.RolledBackFromID)
	}
	if r.ProposedBy != "" {
		parts = append(parts, "proposed_by="+r.ProposedBy)
	}
	if r.RequireApproval {
		parts = append(parts, "require_approval=true")
	}
	if !r.CreatedAt.IsZero() {
		parts = append(parts, "created="+r.CreatedAt.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, ", ")
}

// summarizeAuditEvent produces the one liner the prompt sees.
// Actor + event type + target id are what matters for citation.
func summarizeAuditEvent(e *services.AuditEvent) string {
	parts := []string{
		"type=" + e.EventType,
		"actor=" + e.Actor,
	}
	if e.TargetType != "" || e.TargetID != "" {
		parts = append(parts, fmt.Sprintf("target=%s/%s", e.TargetType, e.TargetID))
	}
	if e.Action != "" {
		parts = append(parts, "action="+e.Action)
	}
	if !e.Timestamp.IsZero() {
		parts = append(parts, "at="+e.Timestamp.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, ", ")
}
