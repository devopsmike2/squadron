// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package handlers — actions endpoints. v0.53 Move 2 (action runner).
//
// Endpoint shape:
//
//   POST   /api/v1/runners/register             enroll a new runner
//   GET    /api/v1/runners                      list runners
//   GET    /api/v1/runners/:id                  one runner
//   POST   /api/v1/runners/:id/revoke           revoke a runner
//   GET    /api/v1/runners/:id/pending          runner polls for work
//   POST   /api/v1/actions/dispatch             sign + persist a request
//   GET    /api/v1/actions                      list requests
//   GET    /api/v1/actions/:id                  one request
//   POST   /api/v1/actions/:id/result           runner posts result
//
// Auth posture: read endpoints require actions:read, mutating
// endpoints require actions:write. The runner daemon authenticates
// with a token carrying actions:write (issued at enrollment time
// in production; tests just pass through).
package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// ActionsHandlers owns the /api/v1/runners and /api/v1/actions
// routes. The signer is required for dispatch; the registry knows
// which action types exist and validates parameters.
type ActionsHandlers struct {
	store    applicationstore.ApplicationStore
	signer   *actions.Signer
	registry *actions.Registry
	logger   *zap.Logger
}

// NewActionsHandlers constructs the handler. Signer must be non-nil
// for dispatch to work; registry defaults to actions.Default when
// nil so tests can pass either.
func NewActionsHandlers(store applicationstore.ApplicationStore, signer *actions.Signer, registry *actions.Registry, logger *zap.Logger) *ActionsHandlers {
	if registry == nil {
		registry = actions.Default
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ActionsHandlers{store: store, signer: signer, registry: registry, logger: logger}
}

// ---- runners ---------------------------------------------------------------

// RegisterRunnerRequest is the body of POST /api/v1/runners/register.
type RegisterRunnerRequest struct {
	RunnerID     string               `json:"runner_id" binding:"required"`
	Hostname     string               `json:"hostname" binding:"required"`
	PublicKeyPEM string               `json:"public_key_pem" binding:"required"`
	Capabilities []actions.Capability `json:"capabilities"`
}

// HandleRegisterRunner enrolls a new runner. Idempotent on
// runner_id: a second registration with the same ID updates the
// existing record (capabilities and last_seen_at refresh, all
// other fields overwrite). This matches the runner side, which
// re-registers on every start.
func (h *ActionsHandlers) HandleRegisterRunner(c *gin.Context) {
	var req RegisterRunnerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "detail": err.Error()})
		return
	}
	capsJSON, err := json.Marshal(req.Capabilities)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "capabilities not serializable", "detail": err.Error()})
		return
	}
	now := time.Now().UTC()
	reg := &types.ActionRunnerRegistration{
		RunnerID:         req.RunnerID,
		Hostname:         req.Hostname,
		PublicKeyPEM:     req.PublicKeyPEM,
		CapabilitiesJSON: string(capsJSON),
		RegisteredAt:     now,
		LastSeenAt:       now,
	}
	// Idempotency: try create, fall back to update on conflict.
	existing, err := h.store.GetActionRunnerRegistration(c.Request.Context(), req.RunnerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed", "detail": err.Error()})
		return
	}
	if existing == nil {
		if err := h.store.CreateActionRunnerRegistration(c.Request.Context(), reg); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create failed", "detail": err.Error()})
			return
		}
	} else {
		reg.RegisteredAt = existing.RegisteredAt // preserve original enrollment time
		if err := h.store.UpdateActionRunnerRegistration(c.Request.Context(), reg); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed", "detail": err.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, reg)
}

// HandleListRunners returns every registered runner, newest first.
func (h *ActionsHandlers) HandleListRunners(c *gin.Context) {
	regs, err := h.store.ListActionRunnerRegistrations(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed", "detail": err.Error()})
		return
	}
	if regs == nil {
		regs = []*types.ActionRunnerRegistration{}
	}
	c.JSON(http.StatusOK, gin.H{"runners": regs})
}

// HandleGetRunner returns one runner by ID.
func (h *ActionsHandlers) HandleGetRunner(c *gin.Context) {
	id := c.Param("id")
	reg, err := h.store.GetActionRunnerRegistration(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed", "detail": err.Error()})
		return
	}
	if reg == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "runner not found", "runner_id": id})
		return
	}
	c.JSON(http.StatusOK, reg)
}

// HandleRevokeRunner marks the runner as revoked. The row stays in
// the table for audit history; dispatch refuses to send to revoked
// runners.
func (h *ActionsHandlers) HandleRevokeRunner(c *gin.Context) {
	id := c.Param("id")
	if err := h.store.RevokeActionRunnerRegistration(c.Request.Context(), id, time.Now().UTC()); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "runner not found or revoke failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runner_id": id, "revoked_at": time.Now().UTC()})
}

// HandleRunnerPending returns the pending action requests addressed
// to the supplied runner. The runner daemon calls this in a loop;
// long-polling is a future improvement. v0.54 work will add a
// since_id parameter so the runner can resume cleanly after a
// restart without re-pulling already-handled requests.
func (h *ActionsHandlers) HandleRunnerPending(c *gin.Context) {
	id := c.Param("id")
	list, err := h.store.ListActionRequests(c.Request.Context(), types.ActionRequestFilter{
		RunnerID: id,
		Status:   "pending",
		Limit:    50,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed", "detail": err.Error()})
		return
	}
	if list == nil {
		list = []*types.ActionRequest{}
	}
	// Bump last_seen_at as a side effect of the poll so the UI
	// can show "runner X seen 30s ago". Best effort; ignore errors.
	if reg, err := h.store.GetActionRunnerRegistration(c.Request.Context(), id); err == nil && reg != nil {
		reg.LastSeenAt = time.Now().UTC()
		_ = h.store.UpdateActionRunnerRegistration(c.Request.Context(), reg)
	}
	c.JSON(http.StatusOK, gin.H{"requests": list})
}

// ---- actions ---------------------------------------------------------------

// DispatchActionRequest is the body of POST /api/v1/actions/dispatch.
// The proposer (or an operator with actions:write) supplies the
// shape; Squadron signs the request, validates against the runner's
// capabilities, persists, and returns the signed Request for
// runner consumption.
type DispatchActionRequest struct {
	ProposalID string          `json:"proposal_id,omitempty"`
	RunnerID   string          `json:"runner_id" binding:"required"`
	ActionType string          `json:"action_type" binding:"required"`
	Parameters json.RawMessage `json:"parameters" binding:"required"`
	Phase      string          `json:"phase" binding:"required"` // dry_run | execute
}

// HandleDispatchAction signs the request and persists it with
// status=pending. Returns the persisted ActionRequest including the
// signed signature; the runner polls and consumes it on the next
// HandleRunnerPending call.
func (h *ActionsHandlers) HandleDispatchAction(c *gin.Context) {
	var req DispatchActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "detail": err.Error()})
		return
	}
	if h.signer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "action signing not configured (set SQUADRON_ACTION_SIGNING_KEY)"})
		return
	}

	// Look up the runner and decode its capability list.
	reg, err := h.store.GetActionRunnerRegistration(c.Request.Context(), req.RunnerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "runner lookup failed", "detail": err.Error()})
		return
	}
	if reg == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "runner not registered", "runner_id": req.RunnerID})
		return
	}
	if reg.RevokedAt != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "runner revoked", "revoked_at": reg.RevokedAt})
		return
	}
	var caps []actions.Capability
	if reg.CapabilitiesJSON != "" {
		if err := json.Unmarshal([]byte(reg.CapabilitiesJSON), &caps); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "runner capabilities corrupt", "detail": err.Error()})
			return
		}
	}

	// Validate parameters against the action type schema.
	at, ok := h.registry.Get(req.ActionType)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown action type %q", req.ActionType)})
		return
	}
	if err := at.ValidateParameters(req.Parameters); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "parameter validation failed", "detail": err.Error()})
		return
	}

	// Capability check against the runner.
	allowed, reason := h.registry.AllowsAction(caps, req.ActionType, req.Parameters)
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "out of runner policy", "detail": reason})
		return
	}

	// Sign the request.
	phase := actions.Phase(req.Phase)
	if phase != actions.PhaseDryRun && phase != actions.PhaseExecute {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phase must be one of dry_run, execute"})
		return
	}
	reqID := uuid.New().String()
	signedReq := &actions.Request{
		RequestID:  reqID,
		ProposalID: req.ProposalID,
		RunnerID:   req.RunnerID,
		Action: actions.ActionPayload{
			Type:       req.ActionType,
			Parameters: req.Parameters,
		},
		Phase: phase,
	}
	if _, err := h.signer.Sign(signedReq); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sign failed", "detail": err.Error()})
		return
	}

	// Persist as pending.
	stored := &types.ActionRequest{
		ID:             reqID,
		ProposalID:     req.ProposalID,
		RunnerID:       req.RunnerID,
		ActionType:     req.ActionType,
		ParametersJSON: string(req.Parameters),
		Signature:      signedReq.Signature,
		Phase:          string(phase),
		Status:         "pending",
		IssuedAt:       signedReq.IssuedAt,
		ExpiresAt:      signedReq.ExpiresAt,
	}
	if err := h.store.CreateActionRequest(c.Request.Context(), stored); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"request": stored, "signed": signedReq})
}

// HandleListActions returns recent action requests with optional
// filtering by proposal, runner, and status.
func (h *ActionsHandlers) HandleListActions(c *gin.Context) {
	filter := types.ActionRequestFilter{
		ProposalID: c.Query("proposal_id"),
		RunnerID:   c.Query("runner_id"),
		Status:     c.Query("status"),
	}
	list, err := h.store.ListActionRequests(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed", "detail": err.Error()})
		return
	}
	if list == nil {
		list = []*types.ActionRequest{}
	}
	c.JSON(http.StatusOK, gin.H{"requests": list})
}

// HandleGetAction returns one request by ID.
func (h *ActionsHandlers) HandleGetAction(c *gin.Context) {
	id := c.Param("id")
	r, err := h.store.GetActionRequest(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed", "detail": err.Error()})
		return
	}
	if r == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "action request not found", "id": id})
		return
	}
	c.JSON(http.StatusOK, r)
}

// PostResultRequest is the body of POST /api/v1/actions/:id/result.
// The runner reports the outcome of the phase it just ran.
type PostResultRequest struct {
	Status              string `json:"status" binding:"required"`
	DeniedFor           string `json:"denied_for,omitempty"`
	DryRunOutputJSON    string `json:"dry_run_output_json,omitempty"`
	ExecutionOutputJSON string `json:"execution_output_json,omitempty"`
}

// HandlePostActionResult records the runner's result. Status must
// be one of success / failure / denied. Started_at + completed_at
// are stamped server-side from the existing row to avoid trusting
// the runner's clock.
func (h *ActionsHandlers) HandlePostActionResult(c *gin.Context) {
	id := c.Param("id")
	var body PostResultRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "detail": err.Error()})
		return
	}
	switch body.Status {
	case "success", "failure", "denied":
		// allowed
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be one of success, failure, denied"})
		return
	}
	existing, err := h.store.GetActionRequest(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed", "detail": err.Error()})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "action request not found", "id": id})
		return
	}
	now := time.Now().UTC()
	existing.Status = body.Status
	existing.DeniedFor = body.DeniedFor
	if body.DryRunOutputJSON != "" {
		existing.DryRunOutputJSON = body.DryRunOutputJSON
	}
	if body.ExecutionOutputJSON != "" {
		existing.ExecutionOutputJSON = body.ExecutionOutputJSON
	}
	if existing.StartedAt == nil {
		t := now
		existing.StartedAt = &t
	}
	existing.CompletedAt = &now
	if err := h.store.UpdateActionRequest(c.Request.Context(), existing); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, existing)
}
