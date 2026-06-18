// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// RolloutHandlers serves /api/v1/rollouts.
type RolloutHandlers struct {
	rolloutService services.RolloutService
	logger         *zap.Logger
}

func NewRolloutHandlers(rolloutService services.RolloutService, logger *zap.Logger) *RolloutHandlers {
	return &RolloutHandlers{rolloutService: rolloutService, logger: logger}
}

// HandleListRollouts serves GET /api/v1/rollouts.
//
// Optional query params: group_id, state (one of pending/in_progress/
// succeeded/aborted/rolled_back), limit (default 100, max 1000).
func (h *RolloutHandlers) HandleListRollouts(c *gin.Context) {
	filter := services.RolloutFilter{
		GroupID: c.Query("group_id"),
		State:   services.RolloutState(c.Query("state")),
	}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid `limit`"})
			return
		}
		filter.Limit = n
	}
	out, err := h.rolloutService.List(c.Request.Context(), filter)
	if err != nil {
		h.logger.Error("failed to list rollouts", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list rollouts"})
		return
	}
	if out == nil {
		out = []*services.Rollout{}
	}
	c.JSON(http.StatusOK, gin.H{"rollouts": out})
}

// HandleGetRollout serves GET /api/v1/rollouts/:id.
func (h *RolloutHandlers) HandleGetRollout(c *gin.Context) {
	id := c.Param("id")
	r, err := h.rolloutService.Get(c.Request.Context(), id)
	if err != nil {
		h.logger.Error("failed to get rollout", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get rollout"})
		return
	}
	if r == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "rollout not found"})
		return
	}
	c.JSON(http.StatusOK, r)
}

// HandleCreateRollout serves POST /api/v1/rollouts.
//
// Body matches services.RolloutInput. On success returns 201 with the
// created rollout (in 'pending' state — the engine picks it up on its
// next tick).
func (h *RolloutHandlers) HandleCreateRollout(c *gin.Context) {
	var input services.RolloutInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body", "detail": err.Error()})
		return
	}
	// v0.47 — record the requester so the two-person rule has
	// something to compare against at approval time. Falls back to
	// "anonymous" in dev / token-less mode; the approver also has
	// to be non-anonymous for the rule to be meaningful, but we
	// don't enforce that here — leave it to deployment policy.
	input.RequestedBy = actorFromContext(c)
	r, err := h.rolloutService.Create(c.Request.Context(), input)
	if err != nil {
		if isRolloutValidationError(err) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		h.logger.Error("failed to create rollout", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create rollout"})
		return
	}
	c.JSON(http.StatusCreated, r)
}

// HandlePauseRollout serves POST /api/v1/rollouts/:id/pause.
func (h *RolloutHandlers) HandlePauseRollout(c *gin.Context) {
	id := c.Param("id")
	r, err := h.rolloutService.Pause(c.Request.Context(), id)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": msg})
			return
		}
		if strings.Contains(msg, "cannot pause") {
			c.JSON(http.StatusConflict, gin.H{"error": msg})
			return
		}
		h.logger.Error("failed to pause rollout", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to pause rollout"})
		return
	}
	c.JSON(http.StatusOK, r)
}

// HandleResumeRollout serves POST /api/v1/rollouts/:id/resume.
func (h *RolloutHandlers) HandleResumeRollout(c *gin.Context) {
	id := c.Param("id")
	r, err := h.rolloutService.Resume(c.Request.Context(), id)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": msg})
			return
		}
		if strings.Contains(msg, "cannot resume") {
			c.JSON(http.StatusConflict, gin.H{"error": msg})
			return
		}
		h.logger.Error("failed to resume rollout", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resume rollout"})
		return
	}
	c.JSON(http.StatusOK, r)
}

// HandlePreviewRollout serves GET /api/v1/rollout-preview.
//
// Query params:
//   - group_id (required): the target group whose current effective
//     config will be used as the diff baseline.
//   - target_config_id (required): the config the operator is
//     considering rolling out.
//
// Returns {current, target, diff, lint_findings}. current may be null
// if the group has no current effective config (a brand-new group).
//
// Read-only; safe to call repeatedly from the create form as the
// operator types the target config id (caller should debounce).
func (h *RolloutHandlers) HandlePreviewRollout(c *gin.Context) {
	groupID := c.Query("group_id")
	targetConfigID := c.Query("target_config_id")
	if groupID == "" || targetConfigID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "group_id and target_config_id query params are required",
		})
		return
	}
	preview, err := h.rolloutService.Preview(c.Request.Context(), groupID, targetConfigID)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": msg})
			return
		}
		if strings.Contains(msg, "is required") {
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
			return
		}
		h.logger.Error("failed to build rollout preview",
			zap.String("group_id", groupID),
			zap.String("target_config_id", targetConfigID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build preview"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

// HandleListRolloutTemplates serves GET /api/v1/rollout-recipes/templates.
//
// Returns the curated template gallery. Templates are bigger than
// abort-criteria recipes — each one bundles stages + criteria + a
// default name so an operator who picks one only needs to fill in the
// target group and config before clicking Start.
//
// Same cache properties as the recipe cookbook: server-of-record,
// changes only on Squadron upgrade, fine to cache for the page's
// lifetime.
func (h *RolloutHandlers) HandleListRolloutTemplates(c *gin.Context) {
	templates := services.RolloutTemplates()
	c.JSON(http.StatusOK, gin.H{"templates": templates})
}

// HandleListAbortCriteriaRecipes serves GET /api/v1/rollouts/abort-criteria/recipes.
//
// Returns the curated cookbook of pre-tuned RolloutAbortCriteria values
// that operators can pick from in the create form. The list is
// server-of-record (built into the binary, not stored) and returned in
// a stable order — see services.AbortCriteriaRecipes for the order and
// the rationale behind each recipe.
//
// This endpoint is intentionally cache-friendly: the response only
// changes when Squadron itself is upgraded. UI may cache it for the
// lifetime of the page.
func (h *RolloutHandlers) HandleListAbortCriteriaRecipes(c *gin.Context) {
	recipes := services.AbortCriteriaRecipes()
	c.JSON(http.StatusOK, gin.H{"recipes": recipes})
}

// HandleAbortRollout serves POST /api/v1/rollouts/:id/abort.
//
// Optional body: {reason: string}. If reason is empty, "aborted by
// operator" is used. Returns the updated rollout (state=aborted; the
// engine will perform the actual rollback on its next tick).
func (h *RolloutHandlers) HandleAbortRollout(c *gin.Context) {
	id := c.Param("id")
	var body struct {
		Reason string `json:"reason"`
	}
	// Body is optional — tolerate empty.
	_ = c.ShouldBindJSON(&body)

	r, err := h.rolloutService.Abort(c.Request.Context(), id, body.Reason)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": msg})
			return
		}
		if strings.Contains(msg, "terminal state") {
			c.JSON(http.StatusConflict, gin.H{"error": msg})
			return
		}
		h.logger.Error("failed to abort rollout", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to abort rollout"})
		return
	}
	c.JSON(http.StatusOK, r)
}

// HandleRollBackRollout serves POST /api/v1/rollouts/:id/rollback.
//
// The source rollout (the one being rolled back from) must be in a
// terminal state. The handler creates a new rollout that targets the
// source's previous_config_id and returns it. The new rollout flows
// through the normal Create pipeline, so it goes through approval if
// the source did and emits the usual rollout.created plus a new
// rollout.rollback_requested audit pair so the timeline shows the
// chain.
//
// Added in v0.60.0.
func (h *RolloutHandlers) HandleRollBackRollout(c *gin.Context) {
	id := c.Param("id")
	// The operator is read from the auth context the same way Approve
	// and Reject read theirs. Falls back to "operator" so dev/no-auth
	// mode still records something useful in the audit payload.
	operator := "operator"
	if actor, ok := c.Get("auth_actor"); ok {
		if s, ok := actor.(string); ok && s != "" {
			operator = s
		}
	}

	r, err := h.rolloutService.RollBack(c.Request.Context(), id, operator)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": msg})
			return
		}
		if strings.Contains(msg, "terminal state") ||
			strings.Contains(msg, "no previous config") {
			c.JSON(http.StatusConflict, gin.H{"error": msg})
			return
		}
		h.logger.Error("failed to roll back rollout", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to roll back rollout"})
		return
	}
	c.JSON(http.StatusOK, r)
}

// HandleApproveRollout serves POST /api/v1/rollouts/:id/approve.
//
// Body (optional): {notes: string}. The approver actor is taken from
// the gin auth context — never from the body — so the two-person rule
// is enforced against the authenticated identity, not a self-asserted
// value.
//
// Returns 200 with the updated rollout (state=pending — the engine
// picks it up on its next tick), 404 if the rollout doesn't exist,
// 409 if the rollout is in a state that can't be approved (already
// approved, rejected, in_progress, etc.) or the approver is the same
// actor that requested the rollout (two-person rule violation).
//
// Added in v0.47.0.
func (h *RolloutHandlers) HandleApproveRollout(c *gin.Context) {
	id := c.Param("id")
	var body struct {
		Notes string `json:"notes"`
	}
	_ = c.ShouldBindJSON(&body)
	approver := actorFromContext(c)

	r, err := h.rolloutService.Approve(c.Request.Context(), id, approver, body.Notes)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": msg})
			return
		}
		if strings.Contains(msg, "cannot approve") ||
			strings.Contains(msg, "two-person rule") {
			c.JSON(http.StatusConflict, gin.H{"error": msg})
			return
		}
		h.logger.Error("failed to approve rollout", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to approve rollout"})
		return
	}
	c.JSON(http.StatusOK, r)
}

// HandleRejectRollout serves POST /api/v1/rollouts/:id/reject.
//
// Body (optional): {notes: string}. The rejecter actor is taken from
// the gin auth context. Two-person rule applies — a requester cannot
// reject their own rollout, which forces a real approval cycle even
// for a cancel-this gesture (the requester should abort instead if
// they have any state to roll back, or just let the rollout sit until
// someone else rejects it).
//
// Returns 200 with the rolled-out rollout (state=rejected, terminal),
// 404 if the rollout doesn't exist, 409 if the state can't be rejected
// or the rejecter is the requester.
//
// Added in v0.47.0.
func (h *RolloutHandlers) HandleRejectRollout(c *gin.Context) {
	id := c.Param("id")
	var body struct {
		Notes string `json:"notes"`
	}
	_ = c.ShouldBindJSON(&body)
	rejecter := actorFromContext(c)

	r, err := h.rolloutService.Reject(c.Request.Context(), id, rejecter, body.Notes)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			c.JSON(http.StatusNotFound, gin.H{"error": msg})
			return
		}
		if strings.Contains(msg, "cannot reject") ||
			strings.Contains(msg, "two-person rule") {
			c.JSON(http.StatusConflict, gin.H{"error": msg})
			return
		}
		h.logger.Error("failed to reject rollout", zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reject rollout"})
		return
	}
	c.JSON(http.StatusOK, r)
}

// isRolloutValidationError reports whether an error from the service is a
// user-input problem (vs. an internal failure).
func isRolloutValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is required") ||
		strings.Contains(msg, "must be") ||
		strings.Contains(msg, "must reach") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "stage")
}
