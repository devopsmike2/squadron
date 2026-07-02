// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package handlers — actions endpoints. v0.53 Move 2 (action runner).
//
// Endpoint shape:
//
//	POST   /api/v1/runners/register             enroll a new runner
//	GET    /api/v1/runners                      list runners
//	GET    /api/v1/runners/:id                  one runner
//	POST   /api/v1/runners/:id/revoke           revoke a runner
//	GET    /api/v1/runners/:id/pending          runner polls for work
//	POST   /api/v1/actions/dispatch             sign + persist a request
//	GET    /api/v1/actions                      list requests
//	GET    /api/v1/actions/:id                  one request
//	POST   /api/v1/actions/:id/result           runner posts result
//
// Auth posture: read endpoints require actions:read, mutating
// endpoints require actions:write. The runner daemon authenticates
// with a token carrying actions:write (issued at enrollment time
// in production; tests just pass through).
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// ActionsHandlers owns the /api/v1/runners and /api/v1/actions
// routes. The signer is required for dispatch; the registry knows
// which action types exist and validates parameters. The audit
// service is optional — when present, every dispatch and every
// result post lands in the audit timeline, and from there fans out
// to any SIEM destination configured in the Enterprise build.
type ActionsHandlers struct {
	store    applicationstore.ApplicationStore
	signer   *actions.Signer
	registry *actions.Registry
	audit    services.AuditService
	logger   *zap.Logger
}

// NewActionsHandlers constructs the handler. Signer must be non-nil
// for dispatch to work; registry defaults to actions.Default when
// nil so tests can pass either. Audit may be nil; when nil, audit
// emission becomes a no-op so unit tests do not have to plumb an
// audit service through every fixture.
func NewActionsHandlers(store applicationstore.ApplicationStore, signer *actions.Signer, registry *actions.Registry, audit services.AuditService, logger *zap.Logger) *ActionsHandlers {
	if registry == nil {
		registry = actions.Default
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ActionsHandlers{store: store, signer: signer, registry: registry, audit: audit, logger: logger}
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
// storeReady guards every handler against a nil store. registerRoutes()
// runs inside NewServer before main.go wires the application store via
// SetActionStoreAndSigner; the route wiring now builds handlers lazily
// (per request) to read a live store, and this is defense-in-depth so a
// nil store degrades to a clean 503 instead of a nil-pointer panic ->
// 500. (v0.89.212)
func (h *ActionsHandlers) storeReady(c *gin.Context) bool {
	if h.store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "actions store not configured",
		})
		return false
	}
	return true
}

func (h *ActionsHandlers) HandleRegisterRunner(c *gin.Context) {
	if !h.storeReady(c) {
		return
	}
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
	if !h.storeReady(c) {
		return
	}
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
	if !h.storeReady(c) {
		return
	}
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
	if !h.storeReady(c) {
		return
	}
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
	if !h.storeReady(c) {
		return
	}
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
	if !h.storeReady(c) {
		return
	}
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

	// SQ-2.7 — drop an audit event the moment Squadron signs and
	// persists a request. The actor is filled in by audit_service_impl
	// from the AuthActor on the request context (bearer middleware
	// puts it there), so we leave Actor empty here. The payload
	// carries enough of the request shape that an auditor reviewing
	// the timeline can answer: which proposal, which runner, what
	// action, dry run or execute, when does the signature expire.
	// We omit the raw signature and full parameters — the signature
	// is in the persisted ActionRequest row, and parameters can be
	// large; we summarize with type plus a short fingerprint.
	if h.audit != nil {
		_ = h.audit.Record(c.Request.Context(), services.AuditEntry{
			EventType:  services.AuditEventActionDispatched,
			TargetType: services.AuditTargetActionRequest,
			TargetID:   reqID,
			Action:     "dispatched",
			Payload: map[string]any{
				"runner_id":         req.RunnerID,
				"proposal_id":       req.ProposalID,
				"action_type":       req.ActionType,
				"phase":             string(phase),
				"issued_at":         signedReq.IssuedAt,
				"expires_at":        signedReq.ExpiresAt,
				"parameters_sha256": sha256Hex(string(req.Parameters)),
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"request": stored, "signed": signedReq})
}

// HandleListActions returns recent action requests with optional
// filtering by proposal, runner, and status.
func (h *ActionsHandlers) HandleListActions(c *gin.Context) {
	if !h.storeReady(c) {
		return
	}
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
	if !h.storeReady(c) {
		return
	}
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
	if !h.storeReady(c) {
		return
	}
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

	// SQ-2.7 — emit the lifecycle terminator event. We split on the
	// result status so the audit timeline (and any SIEM destination
	// fanning it out) gets three discrete event types: success path,
	// failure path, denial path. Splitting beats one "completed"
	// event with a buried status field, because alerting and search
	// query patterns work cleanly on event_type alone.
	//
	// v0.89.14 (#630) — when the request was dispatched as a plan-
	// embedded action step (the engine attached request.ID to a
	// rollout row with StepKind="action"), enrich the payload with
	// plan_id + plan_step_index + plan_step_origin="plan_embedded".
	// Standalone requests carry "standalone" so SIEM consumers can
	// filter on plan_step_origin without joining tables. The
	// rollout lookup is a single in-memory scan via ListRollouts;
	// SQLite has plan_id indexed but the scan helper below works
	// against the same filter so the contract stays stable.
	if h.audit != nil {
		eventType, action := actionResultEventType(body.Status)
		payload := map[string]any{
			"runner_id":   existing.RunnerID,
			"proposal_id": existing.ProposalID,
			"action_type": existing.ActionType,
			"phase":       existing.Phase,
		}
		if body.DeniedFor != "" {
			payload["denied_for"] = body.DeniedFor
		}
		if existing.StartedAt != nil && existing.CompletedAt != nil {
			payload["duration_ms"] = existing.CompletedAt.Sub(*existing.StartedAt).Milliseconds()
		}
		// Plan-embedded provenance — find the rollout step that
		// owns this action_request (if any) and attach plan_id +
		// plan_step_index. Empty planID means standalone; the
		// origin tag flips accordingly.
		planID, stepIndex := lookupPlanForActionRequest(c.Request.Context(), h.store, existing.ID)
		if planID != "" {
			payload["plan_id"] = planID
			payload["plan_step_index"] = stepIndex
			payload["plan_step_origin"] = services.PlanActionOriginPlanEmbedded
		} else {
			payload["plan_step_origin"] = "standalone"
		}
		_ = h.audit.Record(c.Request.Context(), services.AuditEntry{
			// The runner posts this result asynchronously — there's no
			// operator on the request context (unlike action.dispatched), so
			// stamp the system actor explicitly rather than leaving Actor empty
			// (which violates the AuditEntry contract's actor enum).
			Actor:      services.AuditActorSystem,
			EventType:  eventType,
			TargetType: services.AuditTargetActionRequest,
			TargetID:   existing.ID,
			Action:     action,
			Payload:    payload,
		})
	}

	c.JSON(http.StatusOK, existing)
}

// lookupPlanForActionRequest finds the plan-step rollout (if any)
// whose ActionRequestID matches actionRequestID. Returns the plan
// id and step index when a matching kind=action rollout exists;
// empty planID otherwise. v0.89.14 (#630). Used by the result
// handler to attach plan-embedded provenance to action.* audit
// events.
//
// Implementation: filter ListRollouts on State + scan for the
// ActionRequestID. The scan is bounded by the number of
// in-progress action steps which is small in practice (rarely
// more than a handful at once). When discovery shows this becomes
// hot the storage layer can grow a dedicated lookup index; for
// slice 1 the linear scan is correct and cheap.
func lookupPlanForActionRequest(ctx context.Context, store applicationstore.ApplicationStore, actionRequestID string) (string, int) {
	if actionRequestID == "" {
		return "", 0
	}
	rows, err := store.ListRollouts(ctx, types.RolloutFilter{Limit: 1000})
	if err != nil {
		return "", 0
	}
	for _, r := range rows {
		if r == nil {
			continue
		}
		if r.ActionRequestID == actionRequestID && r.PlanID != "" {
			return r.PlanID, r.PlanStepIndex
		}
	}
	return "", 0
}

// actionResultEventType maps the runner-reported status onto the
// canonical audit event type and verb. Kept as a small helper so
// the dispatch handler and the tests reference the same mapping.
func actionResultEventType(status string) (eventType, action string) {
	switch status {
	case "success":
		return services.AuditEventActionExecuted, "executed"
	case "denied":
		return services.AuditEventActionDenied, "denied"
	default:
		// "failure" or anything unexpected — treat as failure so the
		// event still gets written rather than silently dropped.
		return services.AuditEventActionFailed, "failed"
	}
}
