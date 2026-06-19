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
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// v0.66 — Ask context bag extension. Cost spikes + recommendations
// now seed the bag alongside rollouts + audit events. The UI
// already covers spike: and rec: citation kinds defensively in
// AskSquadronDialog (v0.64), so this is a backend only release —
// the chips light up on the next answer that cites them.

// AskCostSpikeLister is the narrow read shape the Ask handler
// needs from the cost spike store. Wider than the type the cost
// spike handler depends on, intentionally: a future spike store
// implementation can satisfy this without exposing the write
// paths. The Server wires this from s.costSpikes when set.
type AskCostSpikeLister interface {
	ListCostSpikeEvents(ctx context.Context, filter storetypes.CostSpikeFilter) ([]*storetypes.CostSpikeEvent, error)
}

// AskRec is the slim shape the Ask handler quotes into the bag.
// Doesn't import internal/recommendations so the handler stays
// decoupled from the engine's heavier types; the wiring layer
// adapts engine output into this shape.
type AskRec struct {
	ID      string
	Title   string
	Detail  string
	AgentID string
}

// AskRecLister returns the slim summaries the bag needs. Server
// adapts recommendations.Engine.Evaluate into this with a default
// window so the handler doesn't have to know about insights.Window.
type AskRecLister interface {
	ListForAsk(ctx context.Context, limit int) ([]AskRec, error)
}

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
//
// v0.66 — costSpikes and recs added. Both are nilable; the bag
// builder skips a source when its lister is nil, so an Ask call
// against a Squadron without cost insights wired still works
// against rollouts + audit events alone.
type AskHandler struct {
	ai             *ai.Service
	rolloutService services.RolloutService
	auditService   services.AuditService
	costSpikes     AskCostSpikeLister
	recs           AskRecLister
	logger         *zap.Logger
}

func NewAskHandler(
	aiSvc *ai.Service,
	rolloutSvc services.RolloutService,
	auditSvc services.AuditService,
	costSpikes AskCostSpikeLister,
	recs AskRecLister,
	logger *zap.Logger,
) *AskHandler {
	return &AskHandler{
		ai:             aiSvc,
		rolloutService: rolloutSvc,
		auditService:   auditSvc,
		costSpikes:     costSpikes,
		recs:           recs,
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

// askRecentSpikes is the cap on cost spike entries pulled into
// the bag. Sized similar to rollouts because each spike is a few
// dense fields that summarize neatly.
const askRecentSpikes = 6

// askRecentRecs caps recommendation entries. The engine returns
// dozens; the bag wants only the top few so the prompt stays
// readable.
const askRecentRecs = 8

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

	// v0.66 — cost spikes. "open" status pulls the active spikes
	// the operator is most likely asking about. Closed spikes would
	// muddy the bag and waste prompt tokens — when an operator
	// wants history they'll narrow the question and we can revisit.
	if h.costSpikes != nil {
		spikes, err := h.costSpikes.ListCostSpikeEvents(ctx, storetypes.CostSpikeFilter{
			Status: "open",
			Limit:  askRecentSpikes,
		})
		if err != nil {
			h.logger.Debug("ask: costSpikes.List failed", zap.Error(err))
		} else {
			hints["spikes_open_count"] = fmt.Sprintf("%d", len(spikes))
			for _, s := range spikes {
				if s == nil {
					continue
				}
				bag["spike:"+s.ID] = summarizeSpike(s)
			}
		}
	}

	// v0.66 — recommendations. Engine.Evaluate normalized through
	// the AskRecLister adapter so the handler doesn't import
	// internal/recommendations directly. Top askRecentRecs entries
	// only; the engine already orders by severity + savings so the
	// prefix is the most relevant subset.
	if h.recs != nil {
		recs, err := h.recs.ListForAsk(ctx, askRecentRecs)
		if err != nil {
			h.logger.Debug("ask: recs.ListForAsk failed", zap.Error(err))
		} else {
			hints["recs_count"] = fmt.Sprintf("%d", len(recs))
			for _, r := range recs {
				if r.ID == "" {
					continue
				}
				bag["rec:"+r.ID] = summarizeRec(r)
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

// summarizeSpike one liners a cost spike for the prompt. Severity
// + dollar delta drives whether the model leads with this row;
// the attribution JSON is intentionally elided because it's bulky
// and the operator can click the chip to see the breakdown.
func summarizeSpike(s *storetypes.CostSpikeEvent) string {
	status := "open"
	if s.EndedAt != nil {
		status = "closed"
	}
	parts := []string{
		"severity=" + s.Severity,
		fmt.Sprintf("baseline=$%.0f/mo", s.BaselineMonthlyUSD),
		fmt.Sprintf("peak=$%.0f/mo", s.PeakMonthlyUSD),
		fmt.Sprintf("over_baseline=%.0f%%", s.PeakPctAboveBaseline),
		"status=" + status,
	}
	if s.Signal != "" {
		parts = append(parts, "signal="+s.Signal)
	}
	if !s.StartedAt.IsZero() {
		parts = append(parts, "started="+s.StartedAt.UTC().Format(time.RFC3339))
	}
	if s.AcknowledgedBy != "" {
		parts = append(parts, "acked_by="+s.AcknowledgedBy)
	}
	return strings.Join(parts, ", ")
}

// summarizeRec one liners a recommendation for the prompt. Title
// is the headline; Detail gets truncated to keep the bag readable.
// The model is told to cite by id, so the value is just authoritative
// summary it can quote.
func summarizeRec(r AskRec) string {
	parts := []string{
		"title=" + r.Title,
	}
	if r.AgentID != "" {
		parts = append(parts, "agent="+r.AgentID)
	}
	if r.Detail != "" {
		d := strings.TrimSpace(r.Detail)
		if len(d) > 160 {
			d = d[:160] + "…"
		}
		// Newlines in the detail would break the one line per bag
		// entry contract. Flatten.
		d = strings.ReplaceAll(d, "\n", " ")
		parts = append(parts, "detail="+d)
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
