// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/csv"
	"encoding/json"
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
	aiService    *ai.Service                       // optional; nil 503s the explain route
	appStore     applicationstore.ApplicationStore // optional; nil means no context enrichment
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
//   - event_type=<dotted name, e.g. discovery.aws.connection_created>
//   - target_type=agent|group|config|rule
//   - target_id=<uuid|string>
//   - since=<RFC3339 timestamp>
//   - limit=<int, default 100, max 1000>
//   - format=csv|json — when set, the response is an evidence EXPORT: it
//     downloads as an attachment and the export itself is self-audited
//     (audit.exported). When absent (default), the response is the plain JSON
//     list the /audit page polls — unchanged, not self-audited.
//
// The export is tenant-scoped to the caller's tenant (the audit service's M2
// predicate already applies it), so a self-hoster can pull their own SOC 2 / ISO
// evidence with the operational audit:read scope (ADR 0020: single-tenant export
// is OSS breadth; cross-tenant / tamper-evident export is the enterprise wedge).
//
// Returns {events: [...]} sorted newest-first (default), or a CSV/JSON download.
func (h *AuditHandlers) HandleListAuditEvents(c *gin.Context) {
	format := strings.ToLower(strings.TrimSpace(c.Query("format")))
	switch format {
	case "", "json", "csv":
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid `format` — expected csv or json",
		})
		return
	}

	filter := services.AuditEventFilter{
		EventType:  c.Query("event_type"),
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

	// Default (no format): the plain list the /audit page polls. Not an export,
	// so no attachment and no self-audit (the poll would flood the log).
	if format == "" {
		c.JSON(http.StatusOK, gin.H{"events": events})
		return
	}

	// Export path: self-audit the export (audit-of-audit; compliance evidence
	// integrity — ADR 0020 D2 canonical shape), best-effort, then download.
	h.recordExport(c, format, filter, len(events))
	if format == "csv" {
		h.writeAuditCSV(c, events)
		return
	}
	// format == "json": same payload as the list, but as a download.
	c.Header("Content-Disposition", `attachment; filename="audit-export-`+time.Now().UTC().Format("20060102T150405Z")+`.json"`)
	c.JSON(http.StatusOK, gin.H{"events": events})
}

// auditCSVHeader is the fixed column set for the CSV export. The freeform
// event-type-specific Payload is emitted as a single JSON-encoded column so the
// header stays stable regardless of payload shape (no per-event-type schema).
var auditCSVHeader = []string{"id", "timestamp", "actor", "event_type", "target_type", "target_id", "action", "payload"}

// writeAuditCSV streams the events as a CSV attachment. Rows are written in the
// service's newest-first order; the payload map is compact-JSON-encoded.
func (h *AuditHandlers) writeAuditCSV(c *gin.Context, events []*services.AuditEvent) {
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="audit-export-`+time.Now().UTC().Format("20060102T150405Z")+`.csv"`)
	c.Status(http.StatusOK)

	w := csv.NewWriter(c.Writer)
	if err := w.Write(auditCSVHeader); err != nil {
		h.logger.Error("audit csv export: header write failed", zap.Error(err))
		return
	}
	for _, e := range events {
		payload := ""
		if len(e.Payload) > 0 {
			if b, err := json.Marshal(e.Payload); err == nil {
				payload = string(b)
			}
		}
		rec := []string{
			e.ID,
			e.Timestamp.UTC().Format(time.RFC3339),
			e.Actor,
			e.EventType,
			e.TargetType,
			e.TargetID,
			e.Action,
			payload,
		}
		if err := w.Write(rec); err != nil {
			h.logger.Error("audit csv export: row write failed", zap.Error(err))
			return
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		h.logger.Error("audit csv export: flush failed", zap.Error(err))
	}
}

// recordExport self-audits an audit export (best-effort). The actor is
// auto-attributed from the request context by the audit service, so the trail
// shows WHO exported WHAT filter and HOW MANY rows — the compliance
// evidence-integrity requirement (ADR 0020).
func (h *AuditHandlers) recordExport(c *gin.Context, format string, filter services.AuditEventFilter, count int) {
	payload := map[string]any{"format": format, "count": count}
	if filter.EventType != "" {
		payload["event_type"] = filter.EventType
	}
	if filter.TargetType != "" {
		payload["target_type"] = filter.TargetType
	}
	if filter.TargetID != "" {
		payload["target_id"] = filter.TargetID
	}
	if !filter.Since.IsZero() {
		payload["since"] = filter.Since.UTC().Format(time.RFC3339)
	}
	if filter.Limit > 0 {
		payload["limit"] = filter.Limit
	}
	if err := h.auditService.Record(c.Request.Context(), services.AuditEntry{
		EventType:  "audit.exported",
		TargetType: "audit_log",
		Action:     "exported",
		Payload:    payload,
	}); err != nil {
		h.logger.Warn("audit export self-audit failed", zap.Error(err))
	}
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
	case "cost_spike":
		// v0.59 — proposal.created / proposal.declined / proposal.skipped
		// all target the cost spike. Resolve the spike to give the
		// model real numbers it can name in the narrative (severity,
		// signal, percent above baseline) instead of just an ID. The
		// attribution top_agents / top_attributes are interesting too
		// when the bridge skipped — they explain why the proposer was
		// never called.
		s, err := h.appStore.GetCostSpikeEvent(ctx, event.TargetID)
		if err == nil && s != nil {
			ctxBag["cost_spike.severity"] = s.Severity
			if s.Signal != "" {
				ctxBag["cost_spike.signal"] = s.Signal
			}
			ctxBag["cost_spike.pct_above_baseline"] = fmt.Sprintf("%.0f%%", s.PeakPctAboveBaseline)
			ctxBag["cost_spike.baseline_usd"] = fmt.Sprintf("$%.0f", s.BaselineMonthlyUSD)
			ctxBag["cost_spike.peak_usd"] = fmt.Sprintf("$%.0f", s.PeakMonthlyUSD)
		}
	}
	return ctxBag
}
