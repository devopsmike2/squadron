// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package handlers — incidents endpoints. v0.54 Move 3 (engineer
// copilot incident drafter).
//
// Endpoint shape:
//
//   GET    /api/v1/incidents/drafts             list drafts (status filter)
//   GET    /api/v1/incidents/drafts/:id         one draft
//   PATCH  /api/v1/incidents/drafts/:id         edit title + body
//   POST   /api/v1/incidents/drafts/:id/dismiss flip status to dismissed
//   POST   /api/v1/incidents/drafts/:id/publish stamp provider + URL
//
// Auth posture: read endpoint requires incidents:read; mutating
// endpoints require incidents:write. The publish endpoint accepts a
// provider name ("clipboard", "github", "linear", "jira", "generic"),
// but the MVP only implements clipboard (no remote call). Real
// providers slot in here in the next chunk.

package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/incidents"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// IncidentsHandlers owns the /api/v1/incidents/drafts routes. The
// audit service is optional; when present, every dismiss and
// publish lands in the audit timeline. The publisher registry
// drives the publish endpoint; providers with no implementation
// (linear, jira, ...) still publish, but as a stamp only (no remote
// API call).
type IncidentsHandlers struct {
	store      applicationstore.ApplicationStore
	audit      services.AuditService
	publishers incidents.PublisherRegistry
	logger     *zap.Logger
}

// NewIncidentsHandlers constructs the handler. audit may be nil;
// publishers may be nil (in which case only the clipboard provider
// works through the stamp-only fallback).
func NewIncidentsHandlers(store applicationstore.ApplicationStore, audit services.AuditService, publishers incidents.PublisherRegistry, logger *zap.Logger) *IncidentsHandlers {
	if logger == nil {
		logger = zap.NewNop()
	}
	if publishers == nil {
		publishers = incidents.NewPublisherRegistry()
	}
	return &IncidentsHandlers{
		store:      store,
		audit:      audit,
		publishers: publishers,
		logger:     logger,
	}
}

// HandleListDrafts returns recently-drafted tickets. Supports status
// filtering via ?status=draft | published | dismissed. Defaults to
// status=draft so the UI inbox view shows the unhandled items
// without filtering.
func (h *IncidentsHandlers) HandleListDrafts(c *gin.Context) {
	status := c.Query("status")
	if status == "" {
		status = "draft"
	}
	list, err := h.store.ListIncidentDrafts(c.Request.Context(), types.IncidentDraftFilter{
		Status:          status,
		ActionRequestID: c.Query("action_request_id"),
		RolloutID:       c.Query("rollout_id"),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed", "detail": err.Error()})
		return
	}
	if list == nil {
		list = []*types.IncidentDraft{}
	}
	c.JSON(http.StatusOK, gin.H{"drafts": list})
}

// HandleGetDraft returns one draft by ID.
func (h *IncidentsHandlers) HandleGetDraft(c *gin.Context) {
	id := c.Param("id")
	d, err := h.store.GetIncidentDraft(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed", "detail": err.Error()})
		return
	}
	if d == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident draft not found", "id": id})
		return
	}
	c.JSON(http.StatusOK, d)
}

// PatchDraftRequest is the body of PATCH /api/v1/incidents/drafts/:id.
// Operator-driven body edits go through this path; the original
// AI-generated content stays in DraftContentJSON.
type PatchDraftRequest struct {
	Title        *string `json:"title,omitempty"`
	BodyMarkdown *string `json:"body_markdown,omitempty"`
}

// HandlePatchDraft updates title and/or body. Other fields (status,
// provider, external_id) are stamped by dedicated endpoints below
// to keep the state machine explicit.
func (h *IncidentsHandlers) HandlePatchDraft(c *gin.Context) {
	id := c.Param("id")
	var body PatchDraftRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "detail": err.Error()})
		return
	}
	existing, err := h.store.GetIncidentDraft(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed", "detail": err.Error()})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident draft not found", "id": id})
		return
	}
	if existing.Status != "draft" {
		c.JSON(http.StatusConflict, gin.H{"error": "only drafts in status=draft can be edited", "status": existing.Status})
		return
	}
	if body.Title != nil {
		existing.Title = *body.Title
	}
	if body.BodyMarkdown != nil {
		existing.BodyMarkdown = *body.BodyMarkdown
	}
	if err := h.store.UpdateIncidentDraft(c.Request.Context(), existing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, existing)
}

// HandleDismissDraft flips status to dismissed. The row stays for
// audit history; future tooling can purge dismissed drafts older
// than N days if storage pressure shows up.
func (h *IncidentsHandlers) HandleDismissDraft(c *gin.Context) {
	id := c.Param("id")
	existing, err := h.store.GetIncidentDraft(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed", "detail": err.Error()})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident draft not found", "id": id})
		return
	}
	if existing.Status == "dismissed" {
		c.JSON(http.StatusOK, existing) // idempotent
		return
	}
	existing.Status = "dismissed"
	if err := h.store.UpdateIncidentDraft(c.Request.Context(), existing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed", "detail": err.Error()})
		return
	}
	if h.audit != nil {
		_ = h.audit.Record(c.Request.Context(), services.AuditEntry{
			EventType:  "incident.dismissed",
			TargetType: services.AuditTargetIncidentDraft,
			TargetID:   existing.ID,
			Action:     "dismissed",
			Payload: map[string]any{
				"action_request_id": existing.ActionRequestID,
				"rollout_id":        existing.RolloutID,
				"title":             existing.Title,
			},
		})
	}
	c.JSON(http.StatusOK, existing)
}

// PublishDraftRequest is the body of POST
// /api/v1/incidents/drafts/:id/publish. The operator picks a
// provider in the UI; for the MVP the only provider that actually
// does work is "clipboard" (no remote call, just stamps the draft
// for audit).
type PublishDraftRequest struct {
	Provider string `json:"provider" binding:"required"`
	// ExternalID and ExternalURL are accepted from the body so the
	// operator can record the result of a manual paste-into-Linear
	// today, before the real provider plug-ins land. For
	// auto-publishing providers (next chunk), the handler will
	// compute these from the provider's response.
	ExternalID  string `json:"external_id,omitempty"`
	ExternalURL string `json:"external_url,omitempty"`
}

// HandlePublishDraft stamps provider + optional external_id/url
// onto the draft and flips status to published. The clipboard
// provider just records the operator's intent; integrated providers
// will land in a follow-up chunk that calls out to Linear / Jira /
// GitHub.
func (h *IncidentsHandlers) HandlePublishDraft(c *gin.Context) {
	id := c.Param("id")
	var body PublishDraftRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "detail": err.Error()})
		return
	}
	switch body.Provider {
	case "clipboard", "github", "linear", "jira", "generic":
		// allowed
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown provider", "provider": body.Provider})
		return
	}
	existing, err := h.store.GetIncidentDraft(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed", "detail": err.Error()})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "incident draft not found", "id": id})
		return
	}
	if existing.Status == "dismissed" {
		c.JSON(http.StatusConflict, gin.H{"error": "dismissed drafts cannot be published"})
		return
	}

	// If we have a registered Publisher for this provider, call it.
	// Otherwise fall back to stamping with the operator supplied
	// external_id / external_url. The fallback preserves the audit
	// trail when the operator manually filed the ticket in Linear
	// (say) and is now telling Squadron where it landed.
	externalID := body.ExternalID
	externalURL := body.ExternalURL
	if pub := h.publishers.Lookup(body.Provider); pub != nil {
		if id, url, err := pub.Publish(c.Request.Context(), existing); err != nil {
			h.logger.Warn("incidents: publisher failed",
				zap.String("provider", body.Provider),
				zap.String("draft_id", existing.ID),
				zap.Error(err),
			)
			c.JSON(http.StatusBadGateway, gin.H{
				"error":  "publisher failed",
				"detail": err.Error(),
			})
			return
		} else {
			if id != "" {
				externalID = id
			}
			if url != "" {
				externalURL = url
			}
		}
	}

	existing.Status = "published"
	existing.Provider = body.Provider
	existing.ExternalID = externalID
	existing.ExternalURL = externalURL
	if err := h.store.UpdateIncidentDraft(c.Request.Context(), existing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed", "detail": err.Error()})
		return
	}
	if h.audit != nil {
		_ = h.audit.Record(c.Request.Context(), services.AuditEntry{
			EventType:  "incident.published",
			TargetType: services.AuditTargetIncidentDraft,
			TargetID:   existing.ID,
			Action:     "published",
			Payload: map[string]any{
				"action_request_id": existing.ActionRequestID,
				"rollout_id":        existing.RolloutID,
				"provider":          body.Provider,
				"external_id":       existing.ExternalID,
				"external_url":      existing.ExternalURL,
				"published_at":      time.Now().UTC(),
			},
		})
	}
	c.JSON(http.StatusOK, existing)
}
