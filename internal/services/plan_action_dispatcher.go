// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

// v0.89.14 (#630) — action runner steps in plans, slice 1.
//
// PlanActionDispatcher is the concrete ActionDispatcher the plan
// engine uses to sign + persist action_requests for kind=action
// plan steps. It reuses the existing action_requests table, the
// existing actions.Signer, and the existing audit event types
// (action.dispatched / action.executed / action.failed /
// action.denied) — slice 1 adds the plan-embedded provenance
// (plan_id, plan_step_index, plan_step_origin="plan_embedded")
// to the audit payload but doesn't introduce a new event family.
// Resolution of spec §6.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// planActionOriginPlanEmbedded is the value the plan engine stamps
// onto plan_step_origin in every dispatched / executed / failed /
// denied audit payload for plan-embedded action steps. Standalone
// action requests dispatched via POST /api/v1/actions/dispatch
// stamp "standalone" (or omit the field — slice 1 leaves the
// existing standalone path untouched).
const PlanActionOriginPlanEmbedded = "plan_embedded"

// PlanActionDispatcher signs and persists action_requests for
// kind=action plan steps. Construct via NewPlanActionDispatcher.
// Implements services.ActionDispatcher.
type PlanActionDispatcher struct {
	store    applicationstore.ApplicationStore
	signer   *actions.Signer
	registry *actions.Registry
	audit    AuditService
	logger   *zap.Logger
}

// NewPlanActionDispatcher wires the dispatcher against the
// process-wide signer + registry. signer must be non-nil — without
// it the engine can't produce a valid request, and the wire layer
// surfaces "signing not configured" the same way the existing
// HandleDispatchAction handler does (503 at request time). registry
// defaults to actions.Default when nil so tests can pass either.
// audit may be nil; when nil, audit emission becomes a no-op.
func NewPlanActionDispatcher(store applicationstore.ApplicationStore, signer *actions.Signer, registry *actions.Registry, audit AuditService, logger *zap.Logger) *PlanActionDispatcher {
	if registry == nil {
		registry = actions.Default
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &PlanActionDispatcher{
		store:    store,
		signer:   signer,
		registry: registry,
		audit:    audit,
		logger:   logger,
	}
}

// Dispatch signs an action_request from the supplied spec and
// persists it with status=pending. Audits a single
// action.dispatched event with plan_id + plan_step_index +
// plan_step_origin="plan_embedded" in the payload so SIEM
// consumers can distinguish plan-embedded dispatch from
// standalone dispatch on event_type alone.
//
// timeout_seconds on the spec overrides the signer's 5-minute
// default — slice 1 wants the plan engine's per-step timeout to
// match the signed envelope's ExpiresAt window so the runner-side
// signature check and the engine-side timeout sweep agree on
// expiry. The action_request's ExpiresAt becomes the single
// source of truth for "this step has run out of time."
func (d *PlanActionDispatcher) Dispatch(ctx context.Context, planID string, stepIndex int, spec ActionStepSpec, actor string) (string, time.Time, error) {
	if d.signer == nil {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: action signer not configured")
	}
	if strings.TrimSpace(spec.RunnerID) == "" {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: runner_id is required")
	}
	if strings.TrimSpace(spec.ActionType) == "" {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: action_type is required")
	}
	// Validate parameters + capability against the registry. The
	// HTTP handler does the same checks (handlers/actions.go
	// HandleDispatchAction) — duplicating them here means a plan-
	// embedded dispatch refuses a bad payload at engine time rather
	// than at runner time. Defense in depth.
	at, ok := d.registry.Get(spec.ActionType)
	if !ok {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: unknown action type %q", spec.ActionType)
	}
	params := spec.Parameters
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	if err := at.ValidateParameters(params); err != nil {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: parameter validation: %w", err)
	}
	reg, err := d.store.GetActionRunnerRegistration(ctx, spec.RunnerID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: runner lookup: %w", err)
	}
	if reg == nil {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: runner %q not registered", spec.RunnerID)
	}
	if reg.RevokedAt != nil {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: runner %q is revoked", spec.RunnerID)
	}
	var caps []actions.Capability
	if reg.CapabilitiesJSON != "" {
		if err := json.Unmarshal([]byte(reg.CapabilitiesJSON), &caps); err != nil {
			return "", time.Time{}, fmt.Errorf("plan action dispatch: runner capabilities corrupt: %w", err)
		}
	}
	if allowed, reason := d.registry.AllowsAction(caps, spec.ActionType, params); !allowed {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: out of runner policy: %s", reason)
	}

	// Sign with an explicit ExpiresAt so the runner's signature
	// envelope and the engine's timeout sweep agree on the
	// timeout the spec set. The signer's Sign method respects a
	// pre-set ExpiresAt (only defaults when it's the zero time).
	reqID := uuid.New().String()
	now := time.Now().UTC()
	timeoutSeconds := spec.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = DefaultActionStepTimeoutSeconds
	}
	expires := now.Add(time.Duration(timeoutSeconds) * time.Second)
	signedReq := &actions.Request{
		RequestID: reqID,
		// ProposalID is left empty for plan-embedded action
		// requests — plan_id + plan_step_index in the audit
		// payload carry the provenance instead. Reusing the
		// ProposalID column for the plan_id would mislead any
		// downstream consumer that thinks ProposalID is an
		// internal/proposer recommendation. Resolution of
		// spec §11 Q3 (reuse table; new fields go in the audit
		// payload, not the row schema).
		RunnerID:  spec.RunnerID,
		Action:    actions.ActionPayload{Type: spec.ActionType, Parameters: params},
		IssuedAt:  now,
		ExpiresAt: expires,
		Phase:     actions.PhaseExecute,
	}
	if _, err := d.signer.Sign(signedReq); err != nil {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: sign: %w", err)
	}
	stored := &types.ActionRequest{
		ID:             reqID,
		RunnerID:       spec.RunnerID,
		ActionType:     spec.ActionType,
		ParametersJSON: string(params),
		Signature:      signedReq.Signature,
		Phase:          string(actions.PhaseExecute),
		Status:         "pending",
		IssuedAt:       signedReq.IssuedAt,
		ExpiresAt:      signedReq.ExpiresAt,
	}
	if err := d.store.CreateActionRequest(ctx, stored); err != nil {
		return "", time.Time{}, fmt.Errorf("plan action dispatch: persist: %w", err)
	}

	if d.audit != nil {
		_ = d.audit.Record(ctx, AuditEntry{
			Actor:      actor,
			EventType:  AuditEventActionDispatched,
			TargetType: AuditTargetActionRequest,
			TargetID:   reqID,
			Action:     "dispatched",
			Payload: map[string]any{
				"runner_id":         spec.RunnerID,
				"action_type":       spec.ActionType,
				"phase":             string(actions.PhaseExecute),
				"issued_at":         signedReq.IssuedAt,
				"expires_at":        signedReq.ExpiresAt,
				"parameters_sha256": sha256HexShort(string(params)),
				// Plan-embedded provenance — the headline addition
				// from spec §6. SIEM consumers fan out on plan_id
				// to correlate the dispatch with the surrounding
				// plan arc; plan_step_origin lets them filter
				// plan-embedded from standalone requests on the
				// event payload without joining tables.
				"plan_id":          planID,
				"plan_step_index":  stepIndex,
				"plan_step_origin": PlanActionOriginPlanEmbedded,
			},
		})
	}
	d.logger.Info("plan action dispatched",
		zap.String("plan_id", planID),
		zap.Int("plan_step_index", stepIndex),
		zap.String("runner_id", spec.RunnerID),
		zap.String("action_type", spec.ActionType),
		zap.String("action_request_id", reqID))
	return reqID, signedReq.ExpiresAt, nil
}

// GetStatus reads the action_request row and returns the runner's
// reported status, the denied_for category (only populated on
// status=denied), and the request's ExpiresAt (used by the
// engine's timeout sweep). Returns an empty status when the row
// is gone (action_requests rows aren't deleted in slice 1, but
// the contract handles the missing-row case to keep the engine's
// tick loop robust to manual cleanup).
//
// When the runner posts a terminal result, the engine emits the
// action.executed / action.failed / action.denied audit event
// with the plan-embedded provenance attached. Slice 1 does this
// emission here, not in handlers/actions.go's HandlePostActionResult,
// because the result handler doesn't know about plans — the
// dispatcher is the single shared spot that does.
func (d *PlanActionDispatcher) GetStatus(ctx context.Context, requestID string) (string, string, time.Time, error) {
	if requestID == "" {
		return "", "", time.Time{}, fmt.Errorf("plan action status: request_id is required")
	}
	row, err := d.store.GetActionRequest(ctx, requestID)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("plan action status: lookup: %w", err)
	}
	if row == nil {
		return "", "", time.Time{}, nil
	}
	return row.Status, row.DeniedFor, row.ExpiresAt, nil
}

// Cancel marks an in-flight action_request as expired so the
// runner's next poll sees a stale row and the engine doesn't
// double-dispatch the same step. Best-effort: errors are
// returned to the caller (the engine logs + continues) so a
// transient storage hiccup during plan abort doesn't strand the
// row but doesn't crash the tick either.
func (d *PlanActionDispatcher) Cancel(ctx context.Context, requestID string, reason string) error {
	if requestID == "" {
		return nil
	}
	row, err := d.store.GetActionRequest(ctx, requestID)
	if err != nil {
		return fmt.Errorf("plan action cancel: lookup: %w", err)
	}
	if row == nil {
		return nil
	}
	if row.Status != "pending" {
		// Already terminal — nothing to do.
		return nil
	}
	now := time.Now().UTC()
	row.Status = "denied"
	if row.DeniedFor == "" {
		row.DeniedFor = reason
	}
	if row.CompletedAt == nil {
		row.CompletedAt = &now
	}
	if err := d.store.UpdateActionRequest(ctx, row); err != nil {
		return fmt.Errorf("plan action cancel: update: %w", err)
	}
	return nil
}

// EmitTerminalAudit lets the runner-result HTTP handler (or any
// other ingest path) record the terminal audit event with plan-
// embedded provenance when the request was plan-embedded. Slice 1
// wires this into handlers/actions.go's HandlePostActionResult
// so the existing runner-poll contract stays intact and the
// plan-embedded fields flow through automatically.
//
// rolloutID is the plan-step rollout whose ActionRequestID is
// requestID; empty rolloutID means "no plan step uses this
// action request" and the helper skips the plan-embedded
// payload fields. The handler resolves rolloutID once per
// result-post via a small lookup on the rollouts table.
func (d *PlanActionDispatcher) EmitTerminalAudit(ctx context.Context, request *types.ActionRequest, planID string, stepIndex int, eventType, action string, actor string) {
	if d.audit == nil || request == nil {
		return
	}
	payload := map[string]any{
		"runner_id":   request.RunnerID,
		"action_type": request.ActionType,
		"phase":       request.Phase,
	}
	if request.DeniedFor != "" {
		payload["denied_for"] = request.DeniedFor
	}
	if request.StartedAt != nil && request.CompletedAt != nil {
		payload["duration_ms"] = request.CompletedAt.Sub(*request.StartedAt).Milliseconds()
	}
	if planID != "" {
		payload["plan_id"] = planID
		payload["plan_step_index"] = stepIndex
		payload["plan_step_origin"] = PlanActionOriginPlanEmbedded
	}
	_ = d.audit.Record(ctx, AuditEntry{
		Actor:      actor,
		EventType:  eventType,
		TargetType: AuditTargetActionRequest,
		TargetID:   request.ID,
		Action:     action,
		Payload:    payload,
	})
}

// sha256HexShort is a small helper for the parameters_sha256
// audit field. Mirrors the same helper handlers/actions.go's
// sha256Hex defines without importing handlers (would invert the
// dependency arrow).
func sha256HexShort(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}
