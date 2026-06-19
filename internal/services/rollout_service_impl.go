// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/configdiff"
	"github.com/devopsmike2/squadron/internal/configlint"
	"github.com/devopsmike2/squadron/extension/policy"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// RolloutServiceImpl is the canonical implementation backed by an
// ApplicationStore. It performs validation, snapshots the previous config
// for rollback at Create time, and records audit entries on create + abort.
//
// tracer is optional — when nil (the common case for tests + auth-
// disabled dev instances), every tracer call short-circuits via the
// RolloutTracer interface's nil-receiver-safe contract.
type RolloutServiceImpl struct {
	appStore     applicationstore.ApplicationStore
	agentService AgentService
	auditService AuditService  // optional
	tracer       RolloutTracer // optional
	// groupPolicy is the boundary between the open core (which
	// surfaces require_approval as group metadata) and the
	// Compliance Pack (which enforces it). nil is treated as
	// NoOpProvider — no enforcement. Wired post-construction via
	// SetGroupPolicyProvider so the Compliance Pack build can plug
	// in its own implementation without changing the OSS constructor
	// signature.
	groupPolicy policy.GroupPolicyProvider
	logger      *zap.Logger
}

// SetGroupPolicyProvider wires the Compliance Pack's group policy
// enforcement. Called by main.go (or the Compliance Pack build's
// wire file) after the service is constructed. nil is a valid value
// and disables enforcement (the OSS default).
func (s *RolloutServiceImpl) SetGroupPolicyProvider(p policy.GroupPolicyProvider) {
	s.groupPolicy = p
}

// ApplicationStore returns the underlying application store. Exposed
// so extension wiring (Compliance Pack, alternative builds) can
// construct providers that need to read group / config / agent rows
// without having to be passed the store through a separate channel.
// Read-only by convention: extension code should not mutate state
// through this handle.
func (s *RolloutServiceImpl) ApplicationStore() applicationstore.ApplicationStore {
	return s.appStore
}

// NewRolloutService creates a new rollout service. agentService is used at
// Create time to capture the previous config for rollback; auditService is
// optional but strongly recommended in production so operators can see the
// rollout history.
func NewRolloutService(
	appStore applicationstore.ApplicationStore,
	agentService AgentService,
	audit AuditService,
	logger *zap.Logger,
) RolloutService {
	return &RolloutServiceImpl{
		appStore:     appStore,
		agentService: agentService,
		auditService: audit,
		logger:       logger,
	}
}

// NewRolloutServiceWithTracer is the production constructor used when
// telemetry.enabled is true. Identical to NewRolloutService except for
// the tracer wiring. Keeping it a separate constructor avoids adding a
// nil tracer parameter to every NewRolloutService caller in existing
// tests.
func NewRolloutServiceWithTracer(
	appStore applicationstore.ApplicationStore,
	agentService AgentService,
	audit AuditService,
	tracer RolloutTracer,
	logger *zap.Logger,
) RolloutService {
	return &RolloutServiceImpl{
		appStore:     appStore,
		agentService: agentService,
		auditService: audit,
		tracer:       tracer,
		logger:       logger,
	}
}

// Create validates input and persists a pending rollout. The engine
// goroutine picks it up on its next tick.
func (s *RolloutServiceImpl) Create(ctx context.Context, input RolloutInput) (*Rollout, error) {
	if err := validateRolloutInput(input); err != nil {
		return nil, err
	}

	// Verify the target config exists. Saves the operator from a rollout
	// that's doomed before it starts.
	cfg, err := s.appStore.GetConfig(ctx, input.TargetConfigID)
	if err != nil {
		return nil, fmt.Errorf("failed to verify target config: %w", err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("target config not found: %s", input.TargetConfigID)
	}

	// Snapshot the group's current config for rollback. If the group has
	// no current config, PreviousConfigID stays empty and an abort will
	// just leave the canary on the target config (with a warning logged
	// by the engine).
	var previousID string
	if prev, err := s.appStore.GetLatestConfigForGroup(ctx, input.GroupID); err == nil && prev != nil {
		previousID = prev.ID
	}

	// v0.48 — per-group approval policy. If the target group's
	// policy mandates approval, we force input.RequireApproval=true
	// here so the requester cannot bypass policy by unchecking the
	// form box. This is the actual compliance control: it turns
	// v0.47's honor-system checkbox into per-group enforced policy.
	//
	// v0.52 — the enforcement check moved behind the
	// policy.GroupPolicyProvider interface so the Compliance Pack
	// (private squadron-compliance repo) can own enforcement and
	// the open core stays honor-system. With no provider wired
	// (the OSS default), the requester's value stands.
	//
	// Failing open is preserved at the provider boundary: a
	// provider that can't determine policy returns false rather
	// than blocking the rollout. Operators can recover from "didn't
	// enforce" with an audit; a hard block on a transient lookup
	// failure would prevent legitimate ops work during an incident.
	enforcedByPolicy := false
	if s.groupPolicy != nil && s.groupPolicy.RequiresApproval(ctx, input.GroupID) && !input.RequireApproval {
		input.RequireApproval = true
		enforcedByPolicy = true
		s.logger.Info("forcing rollout into pending_approval per group policy",
			zap.String("group_id", input.GroupID),
			zap.String("requested_by", input.RequestedBy))
	}

	now := time.Now().UTC()
	// v0.47 — RequireApproval gates the initial state. With
	// approval required, the engine refuses to advance the
	// rollout until an approver calls Approve.
	initialState := RolloutStatePending
	if input.RequireApproval {
		initialState = RolloutStatePendingApproval
	}
	// v0.70 — plan steps after the first wait in Queued until the
	// engine promotes them on the predecessor's succeeded transition.
	// Step 0 still respects RequireApproval (the plan approval gate
	// sits there); steps 1..N never carry RequireApproval themselves
	// because the v0.69 design says the plan is approved as a unit.
	if input.PlanID != "" && input.PlanStepIndex > 0 {
		initialState = RolloutStateQueued
	}
	// v0.53 — proposal provenance. Default to operator so existing
	// callers behave unchanged. AI proposers set ProposedBy="ai"
	// plus reasoning + evidence; validation prevents unknown values.
	proposedBy := input.ProposedBy
	if proposedBy == "" {
		proposedBy = RolloutProposedByOperator
	}
	switch proposedBy {
	case RolloutProposedByOperator, RolloutProposedByAI, RolloutProposedBySystem:
		// allowed
	default:
		return nil, fmt.Errorf("invalid proposed_by %q (must be one of operator, ai, system)", proposedBy)
	}
	rollout := &Rollout{
		ID:                uuid.New().String(),
		Name:              input.Name,
		GroupID:           input.GroupID,
		TargetConfigID:    input.TargetConfigID,
		PreviousConfigID:  previousID,
		Stages:            normalizeStages(input.Stages),
		AbortCriteria:     input.AbortCriteria,
		NotificationURL:   input.NotificationURL,
		State:             initialState,
		CurrentStage:      0,
		RequireApproval:   input.RequireApproval,
		RequestedBy:       input.RequestedBy,
		ProposedBy:        proposedBy,
		ProposalReasoning: input.ProposalReasoning,
		EvidenceRefs:      input.EvidenceRefs,
		// v0.69 multi step plan grouping. Round trips to storage; the
		// engine does not yet sequence on these — see
		// docs/multi-step-plans-design.md for the v0.70+ roadmap.
		PlanID:        input.PlanID,
		PlanStepIndex: input.PlanStepIndex,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := s.appStore.CreateRollout(ctx, toStorageRollout(rollout)); err != nil {
		return nil, fmt.Errorf("failed to persist rollout: %w", err)
	}
	s.logger.Info("created rollout",
		zap.String("rollout_id", rollout.ID),
		zap.String("group_id", rollout.GroupID),
		zap.String("target_config_id", rollout.TargetConfigID))

	// Capture the caller's OTel span context so the engine's
	// eventual rollout span can link back to the originating API
	// request. The engine span starts on the engine's next tick —
	// often seconds later — and lives across many subsequent ticks,
	// so a true parent-child relationship to the now-ended API span
	// doesn't fit. Span links are the OTel-blessed primitive for
	// "related but not parent-child". Nil-safe when telemetry is off.
	if s.tracer != nil {
		s.tracer.LinkRolloutToContext(rollout.ID, ctx)
	}

	if s.auditService != nil {
		payload := map[string]any{
			"name":             rollout.Name,
			"group_id":         rollout.GroupID,
			"target_config_id": rollout.TargetConfigID,
			"stage_count":      len(rollout.Stages),
		}
		if rollout.RequireApproval {
			payload["require_approval"] = true
			// v0.48 — surface whether approval was set because
			// the requester checked the box (false) or because
			// the group policy enforced it (true). Auditors
			// need this distinction to prove that policy was
			// actually doing work.
			payload["approval_enforced_by_policy"] = enforcedByPolicy
		}
		// Diff fingerprint — small enough to keep in the audit log so
		// a post-mortem can answer "how big a change was this?" without
		// re-fetching both configs. We record the previous_config_id
		// here too so the diff can be reproduced later via Preview.
		if rollout.PreviousConfigID != "" {
			payload["previous_config_id"] = rollout.PreviousConfigID
		}
		if diff, err := s.diffAtCreate(ctx, rollout.GroupID, rollout.TargetConfigID); err == nil {
			payload["diff_added_lines"] = diff.Added
			payload["diff_removed_lines"] = diff.Removed
			payload["diff_identical"] = diff.Identical
		}
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  "rollout.created",
			TargetType: "rollout",
			TargetID:   rollout.ID,
			Action:     "created",
			Payload:    payload,
		})
	}
	return rollout, nil
}

// diffAtCreate computes the (added, removed, identical) summary for the
// audit payload at create time. Failures here are best-effort — they're
// logged but don't fail Create, since a missing diff summary is cosmetic
// and the rollout itself is independent of it.
func (s *RolloutServiceImpl) diffAtCreate(ctx context.Context, groupID, targetConfigID string) (configdiff.Result, error) {
	target, err := s.appStore.GetConfig(ctx, targetConfigID)
	if err != nil || target == nil {
		return configdiff.Result{}, fmt.Errorf("target unreadable")
	}
	currentContent := ""
	if cur, err := s.appStore.GetLatestConfigForGroup(ctx, groupID); err == nil && cur != nil {
		currentContent = cur.Content
	}
	return configdiff.Diff(currentContent, target.Content), nil
}

func (s *RolloutServiceImpl) Get(ctx context.Context, id string) (*Rollout, error) {
	stored, err := s.appStore.GetRollout(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, nil
	}
	return toServiceRollout(stored), nil
}

func (s *RolloutServiceImpl) List(ctx context.Context, filter RolloutFilter) ([]*Rollout, error) {
	stored, err := s.appStore.ListRollouts(ctx, applicationstore.RolloutFilter{
		GroupID: filter.GroupID,
		State:   applicationstore.RolloutState(filter.State),
		Limit:   filter.Limit,
		PlanID:  filter.PlanID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Rollout, len(stored))
	for i, r := range stored {
		out[i] = toServiceRollout(r)
	}
	return out, nil
}

// NextPlanStep looks up the rollout that follows the one at
// currentIndex within the same plan. v0.70. Implemented as a List
// + linear scan because the storage layer doesn't yet have a
// (plan_id, plan_step_index) index — fine for plans of single
// digit step counts (the common case for cost spike fixes) and
// only called once per succeeded transition. If plans ever grow
// long enough to make the scan hot, this method gets a dedicated
// storage query without disturbing the engine's contract.
func (s *RolloutServiceImpl) NextPlanStep(ctx context.Context, planID string, currentIndex int) (*Rollout, error) {
	if planID == "" {
		return nil, nil
	}
	stored, err := s.appStore.ListRollouts(ctx, applicationstore.RolloutFilter{
		// No upper bound on Limit here — a plan with hundreds of
		// steps is pathological, and List caps internally at 1000
		// which is two orders of magnitude beyond reasonable.
		Limit: 1000,
	})
	if err != nil {
		return nil, err
	}
	target := currentIndex + 1
	for _, r := range stored {
		if r == nil {
			continue
		}
		if r.PlanID == planID && r.PlanStepIndex == target {
			return toServiceRollout(r), nil
		}
	}
	return nil, nil
}

// CreatePlan wraps N Create calls under a shared PlanID. v0.73.
// The plan id is generated server side; callers shouldn't pre
// assign one because uniqueness has to be guaranteed.
//
// Validation rules:
//   - At least one step. An empty plan is a misuse of the API.
//   - All steps must share the same GroupID. A plan that crosses
//     groups would have ambiguous approval semantics (which group's
//     RequireApproval policy applies?) and no operator workflow
//     actually wants this today. Service rejects it cleanly so the
//     proposer can't construct one by accident.
//   - The PlanID and PlanStepIndex on the input are ignored. The
//     caller's job is to supply N rollout intents; the service
//     handles the grouping.
//   - RequireApproval is honored on step 0 (the plan's gate) and
//     forced to false on steps 1..N (per design — plan approves as
//     a unit).
//
// Partial failure: if step K's Create fails after K-1 already
// succeeded, the implementation immediately calls
// CancelPlanFollowers with afterIndex=-1 to flip every step in the
// plan to Cancelled. This is a defensive cleanup, not a guaranteed
// rollback — if any of the K-1 already-created steps have already
// started running (the engine ticks between Create calls), the
// cancel only catches Queued ones. The forward step that's in
// Pending or InProgress remains; the operator will see the
// partial plan in the UI and can Abort manually. This is
// documented as a known limitation; v0.74 may tighten this when
// the API surface settles.
func (s *RolloutServiceImpl) CreatePlan(ctx context.Context, steps []RolloutInput) ([]*Rollout, string, error) {
	if len(steps) == 0 {
		return nil, "", fmt.Errorf("plan requires at least one step")
	}
	groupID := strings.TrimSpace(steps[0].GroupID)
	if groupID == "" {
		return nil, "", fmt.Errorf("plan step 0 missing group_id")
	}
	for i, st := range steps[1:] {
		if strings.TrimSpace(st.GroupID) != groupID {
			return nil, "", fmt.Errorf("plan step %d group_id %q does not match step 0 group_id %q",
				i+1, st.GroupID, groupID)
		}
	}

	// v0.78 — pre-flight: walk every step and validate the
	// target_config_id vs inline_config_snippet shape before any
	// storage write fires. Failing fast here is better than
	// materializing step 0's config, then discovering step 1 is
	// ambiguous and having to roll back.
	for i, step := range steps {
		snippet := strings.TrimSpace(step.InlineConfigSnippet)
		target := strings.TrimSpace(step.TargetConfigID)
		switch {
		case snippet != "" && target != "":
			return nil, "", fmt.Errorf("plan step %d sets both inline_config_snippet and target_config_id (ambiguous)", i)
		case snippet == "" && target == "":
			return nil, "", fmt.Errorf("plan step %d sets neither inline_config_snippet nor target_config_id", i)
		}
	}

	planID := uuid.NewString()
	created := make([]*Rollout, 0, len(steps))

	for i, step := range steps {
		// The caller's PlanID + PlanStepIndex are ignored. We assign
		// authoritative values so a misuse of the input shape can't
		// produce inconsistent plans in storage.
		step.PlanID = planID
		step.PlanStepIndex = i
		// Force require_approval=false on steps 1..N. Step 0 keeps
		// whatever the caller set, which is the plan's approval gate.
		if i > 0 {
			step.RequireApproval = false
		}

		// v0.78 — materialize inline config snippet into a real
		// Config row before the rollout is created. Lint check
		// runs first so a malformed snippet never lands a Config.
		// The materialized config carries a name that ties it to
		// the plan + step for forensic clarity.
		if strings.TrimSpace(step.InlineConfigSnippet) != "" {
			if err := s.materializePlanStepConfig(ctx, &step, planID, i, groupID); err != nil {
				// Roll back already-created steps using the v0.71
				// cancellation walk. Same cleanup path as the
				// non-snippet partial-failure case below.
				if len(created) > 0 {
					_, cancelErr := s.CancelPlanFollowers(ctx, planID, -1)
					if cancelErr != nil {
						s.logger.Warn("create plan: cleanup after snippet materialize failure also failed",
							zap.String("plan_id", planID),
							zap.Int("created_so_far", len(created)),
							zap.Error(cancelErr))
					}
				}
				return nil, "", err
			}
		}

		r, err := s.Create(ctx, step)
		if err != nil {
			// Partial failure cleanup. Cancel everything we already
			// created so the storage doesn't keep orphan plan rows.
			// The cancellation walk is best effort — Pending/InProgress
			// rows are out of reach and the operator will need to
			// abort those manually. The audit trail tells the story.
			if len(created) > 0 {
				_, cancelErr := s.CancelPlanFollowers(ctx, planID, -1)
				if cancelErr != nil {
					s.logger.Warn("create plan: cleanup after partial failure also failed",
						zap.String("plan_id", planID),
						zap.Int("created_so_far", len(created)),
						zap.Error(cancelErr))
				}
			}
			return nil, "", fmt.Errorf("plan step %d create failed: %w", i, err)
		}
		created = append(created, r)
	}

	s.logger.Info("plan created",
		zap.String("plan_id", planID),
		zap.String("group_id", groupID),
		zap.Int("step_count", len(created)))

	if s.auditService != nil {
		stepIDs := make([]string, 0, len(created))
		for _, r := range created {
			stepIDs = append(stepIDs, r.ID)
		}
		_ = s.auditService.Record(ctx, AuditEntry{
			// The actor of plan.created is the proposer/requester of
			// step 0 — the same hand that approves the plan when it
			// eventually does. Empty when the caller didn't pass
			// RequestedBy (token less mode).
			Actor:      steps[0].RequestedBy,
			EventType:  "plan.created",
			TargetType: "rollout",
			TargetID:   created[0].ID,
			Action:     "plan_created",
			Payload: map[string]any{
				"plan_id":    planID,
				"group_id":   groupID,
				"step_count": len(created),
				"step_ids":   stepIDs,
				// Proposer attribution from step 0 — when a future AI
				// proposer creates plans, ProposedBy=ai flows through
				// for SIEM and the audit timeline UI explain panel.
				"proposed_by": created[0].ProposedBy,
			},
		})
	}

	return created, planID, nil
}

// GetPlan returns the plan envelope for planID. v0.74.
// Returns (nil, nil) when no rollouts carry that planID, so the
// handler can map to a clean 404. Otherwise builds the envelope
// from the matching rollouts: forward steps in ascending step
// index, rollback steps in ascending negative step index (-1
// first), state derived from the forward steps' lifecycle.
func (s *RolloutServiceImpl) GetPlan(ctx context.Context, planID string) (*Plan, error) {
	if planID == "" {
		return nil, fmt.Errorf("plan id is required")
	}
	stored, err := s.appStore.ListRollouts(ctx, applicationstore.RolloutFilter{
		PlanID: planID,
		Limit:  1000,
	})
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, nil
	}

	// Separate forward + rollback steps. The negative range was
	// introduced in v0.72 for the backwards rollback walk.
	var forward, rollbacks []*Rollout
	for _, r := range stored {
		if r == nil {
			continue
		}
		svc := toServiceRollout(r)
		if r.PlanStepIndex < 0 {
			rollbacks = append(rollbacks, svc)
		} else {
			forward = append(forward, svc)
		}
	}
	// Forward ascending (0, 1, 2 …). Rollbacks ascending (-1
	// numerically lowest absolute value first, then -2, etc. — i.e.
	// the rollback of the highest forward step comes first because
	// that's the order they fire in the engine's backwards walk).
	sort.Slice(forward, func(i, j int) bool {
		return forward[i].PlanStepIndex < forward[j].PlanStepIndex
	})
	sort.Slice(rollbacks, func(i, j int) bool {
		// -1 should come before -2 (i.e. -1 > -2 numerically).
		// Ascending sort on the index puts -3 first, which is the
		// wrong order. Flip via Greater so -1 comes first.
		return rollbacks[i].PlanStepIndex > rollbacks[j].PlanStepIndex
	})

	envelope := &Plan{
		PlanID:        planID,
		StepCount:     len(forward),
		Steps:         forward,
		RollbackSteps: rollbacks,
	}
	if len(forward) > 0 {
		envelope.GroupID = forward[0].GroupID
		envelope.CreatedAt = forward[0].CreatedAt
	}
	// UpdatedAt is the most recent across all steps so a UI
	// showing "last activity" gets the right value without walking.
	envelope.UpdatedAt = envelope.CreatedAt
	for _, r := range forward {
		if r.UpdatedAt.After(envelope.UpdatedAt) {
			envelope.UpdatedAt = r.UpdatedAt
		}
	}
	for _, r := range rollbacks {
		if r.UpdatedAt.After(envelope.UpdatedAt) {
			envelope.UpdatedAt = r.UpdatedAt
		}
	}
	envelope.State = derivePlanState(forward, rollbacks)
	return envelope, nil
}

// derivePlanState collapses the forward + rollback steps into a
// single status word for the envelope. v0.74. The mapping is
// best effort — the per step rollout.* + plan.* audit events are
// the canonical sources for SIEM. UI uses this for the headline
// badge on the plan card.
func derivePlanState(forward, rollbacks []*Rollout) string {
	if len(rollbacks) > 0 {
		return "rolled_back"
	}
	// Step 0 carries the approval gate, so its state shapes the
	// plan's headline before any forward progress.
	if len(forward) > 0 {
		switch forward[0].State {
		case RolloutStateRejected:
			return "rejected"
		case RolloutStatePendingApproval:
			return "pending_approval"
		}
	}
	// Walk forward steps in order. Any cancelled / aborted /
	// in_progress wins precedence over the all-succeeded path.
	allSucceeded := true
	for _, r := range forward {
		switch r.State {
		case RolloutStateAborted:
			return "aborted"
		case RolloutStateCancelled:
			return "cancelled"
		case RolloutStateInProgress, RolloutStatePending, RolloutStatePaused, RolloutStateQueued:
			allSucceeded = false
		case RolloutStateSucceeded:
			// keep walking
		default:
			allSucceeded = false
		}
	}
	if allSucceeded && len(forward) > 0 {
		return "succeeded"
	}
	return "in_progress"
}

// materializePlanStepConfig lints the step's inline snippet, then
// creates a Config row in the step's group and updates the step's
// TargetConfigID to point at it. v0.78.
//
// Lint policy: error-severity findings reject the snippet. Warnings
// and infos pass through — they're surfaced via the rollout's
// own pre-flight lint at apply time. This is the same posture
// HandleCreateRollout takes today for snippets pasted by operators.
//
// After this function returns nil, step.TargetConfigID is the new
// config's id and step.InlineConfigSnippet is cleared so the
// subsequent Create call doesn't loop or re-materialize.
func (s *RolloutServiceImpl) materializePlanStepConfig(ctx context.Context, step *RolloutInput, planID string, stepIndex int, groupID string) error {
	// Preserve the operator's exact bytes — they may have
	// intentionally formatted the snippet with leading/trailing
	// whitespace. Only the empty check trims.
	if strings.TrimSpace(step.InlineConfigSnippet) == "" {
		return nil
	}
	snippet := step.InlineConfigSnippet

	findings := configlint.Lint(snippet)
	for _, f := range findings {
		if f.Severity == configlint.SeverityError {
			return fmt.Errorf("plan step %d snippet failed lint (%s): %s", stepIndex, f.Rule, f.Message)
		}
	}

	if s.agentService == nil {
		// Belt and suspenders: in tests where the agent service is
		// nil but the caller wired CreatePlan with snippet steps,
		// surface a clean error instead of panicking inside the
		// service.
		return fmt.Errorf("plan step %d: agent service not wired; cannot materialize inline config", stepIndex)
	}

	hash := sha256.Sum256([]byte(snippet))
	configID := uuid.NewString()
	gid := groupID
	// Config name encodes the plan and step for forensic clarity.
	// When operators see this name on the Configs page or in an
	// audit row, they can trace it back to the originating plan
	// without spelunking through audit events.
	name := fmt.Sprintf("ai-plan-%s-step-%d", planID[:8], stepIndex)

	cfg := &Config{
		ID:         configID,
		Name:       name,
		GroupID:    &gid,
		ConfigHash: hex.EncodeToString(hash[:]),
		Content:    snippet,
		Version:    1,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.agentService.CreateConfig(ctx, cfg); err != nil {
		return fmt.Errorf("plan step %d: materialize config: %w", stepIndex, err)
	}

	// Audit the materialization so SIEM consumers see config.created
	// events linked to the plan id. This is in addition to the
	// per step rollout.created and plan.created events that fire
	// downstream from Create + CreatePlan respectively.
	if s.auditService != nil {
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  "config.created",
			TargetType: "config",
			TargetID:   configID,
			Action:     "created",
			Payload: map[string]any{
				"plan_id":         planID,
				"plan_step_index": stepIndex,
				"group_id":        groupID,
				"source":          "plan_inline_snippet",
			},
		})
	}

	step.TargetConfigID = configID
	step.InlineConfigSnippet = "" // clear so Create doesn't re-process
	return nil
}

// RollBackPlanPredecessors walks steps 0..failedIndex-1 in planID,
// finds every step in succeeded state, and creates a rollback
// rollout for each using the reserved negative PlanStepIndex range
// (-1 for the rollback of the highest succeeded forward step, -2
// for the next, etc.). v0.72.
//
// The new rollback rollouts:
//   - share the failed plan's PlanID so the audit timeline groups
//     the full forward + backward arc under one query
//   - each carry RolledBackFromID pointing at the forward step
//     they undo (the v0.60 link field)
//   - run in parallel by design — each is independent of the
//     others, just an emergency undo of one step's config push
//   - inherit the v0.61 group-level RequireApprovalForRollback
//     policy via the existing RollBack code path; an
//     approval-strict group still gates plan rollbacks
//
// Returns the rollback rollouts in creation order (highest forward
// step's rollback first, step 0's last). Empty slice means there
// were no succeeded forward steps to roll back — e.g. step 0 itself
// aborted, no work to do.
func (s *RolloutServiceImpl) RollBackPlanPredecessors(ctx context.Context, planID string, failedIndex int, operator string) ([]*Rollout, error) {
	if planID == "" {
		return nil, nil
	}
	stored, err := s.appStore.ListRollouts(ctx, applicationstore.RolloutFilter{Limit: 1000})
	if err != nil {
		return nil, err
	}
	// Collect succeeded forward steps in descending index order so
	// the highest-index rollback fires first. This is the order an
	// operator would naturally walk if undoing manually.
	type stepRef struct {
		id    string
		index int
	}
	var succeeded []stepRef
	for _, r := range stored {
		if r == nil {
			continue
		}
		if r.PlanID != planID {
			continue
		}
		if r.PlanStepIndex < 0 || r.PlanStepIndex >= failedIndex {
			// Skip rollback steps (negative index), the failed
			// step itself, and any forward step at or after the
			// failed index (those got cancelled in v0.71's walk).
			continue
		}
		if r.State != applicationstore.RolloutStateSucceeded {
			// Only succeeded steps need rolling back. Anything
			// else (failed/cancelled/queued/paused) either has no
			// effect to undo or is already terminal in a non
			// succeeded way.
			continue
		}
		succeeded = append(succeeded, stepRef{id: r.ID, index: r.PlanStepIndex})
	}
	// Descending by forward index — so the highest succeeded step's
	// rollback gets PlanStepIndex -1, the next -2, etc.
	sort.Slice(succeeded, func(i, j int) bool {
		return succeeded[i].index > succeeded[j].index
	})

	out := []*Rollout{}
	for i, step := range succeeded {
		rollback, err := s.RollBack(ctx, step.id, operator)
		if err != nil {
			// Partial failure mode: log + carry on so the rest of
			// the chain at least gets attempted. The caller (engine
			// triggerAbort) will see which steps got rollback
			// rollouts and which didn't via the returned slice.
			s.logger.Warn("plan rollback: failed to create rollback for step",
				zap.String("plan_id", planID),
				zap.Int("forward_step", step.index),
				zap.String("forward_step_id", step.id),
				zap.Error(err))
			continue
		}
		// Stamp the reserved negative PlanStepIndex. RollBack used
		// the standard Create path which left PlanID empty (since
		// the RolloutInput it constructed didn't pass PlanID). We
		// attach both fields here and persist via UpdateRollout so
		// the rollback rollout joins the plan in storage.
		rollback.PlanID = planID
		rollback.PlanStepIndex = -(i + 1)
		if err := s.appStore.UpdateRollout(ctx, toStorageRollout(rollback)); err != nil {
			s.logger.Warn("plan rollback: failed to attach plan grouping to rollback rollout",
				zap.String("plan_id", planID),
				zap.String("rollback_id", rollback.ID),
				zap.Error(err))
			continue
		}
		out = append(out, rollback)
	}
	return out, nil
}

// CancelPlanFollowers transitions every queued step in planID with
// index > afterIndex to Cancelled. v0.71. The cancellation walk
// runs synchronously because the engine's recovery work shouldn't
// race against a tick that picks up an "unfinished" queued step
// while the walk is in flight.
//
// Returns the list of cancelled rollouts (in storage order) so
// the caller can fan out per step audit events. Empty list means
// the failed step had no queued followers (it was the last in the
// plan), which is a valid case and not an error.
func (s *RolloutServiceImpl) CancelPlanFollowers(ctx context.Context, planID string, afterIndex int) ([]*Rollout, error) {
	if planID == "" {
		return nil, nil
	}
	stored, err := s.appStore.ListRollouts(ctx, applicationstore.RolloutFilter{Limit: 1000})
	if err != nil {
		return nil, err
	}
	cancelled := []*Rollout{}
	now := time.Now().UTC()
	for _, r := range stored {
		if r == nil {
			continue
		}
		if r.PlanID != planID || r.PlanStepIndex <= afterIndex {
			continue
		}
		// Only queued steps get cancelled. A step that already ran
		// (succeeded, failed, etc.) stays in its current state —
		// the cancellation walk is about preventing future work,
		// not rewriting history.
		if r.State != applicationstore.RolloutState(RolloutStateQueued) {
			continue
		}
		r.State = applicationstore.RolloutState(RolloutStateCancelled)
		r.UpdatedAt = now
		r.CompletedAt = &now
		if err := s.appStore.UpdateRollout(ctx, r); err != nil {
			return nil, fmt.Errorf("cancel plan follower %s: %w", r.ID, err)
		}
		cancelled = append(cancelled, toServiceRollout(r))
	}
	return cancelled, nil
}

// Abort flips the rollout to the aborted state. The engine performs the
// actual rollback work on its next tick. Operator-supplied reason lands
// in AbortReason for the audit trail.
func (s *RolloutServiceImpl) Abort(ctx context.Context, id, reason string) (*Rollout, error) {
	stored, err := s.appStore.GetRollout(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("rollout not found: %s", id)
	}
	if stored.State == applicationstore.RolloutStateSucceeded ||
		stored.State == applicationstore.RolloutStateRolledBack {
		return toServiceRollout(stored), fmt.Errorf("rollout is already in terminal state %q", stored.State)
	}
	if reason == "" {
		reason = "aborted by operator"
	}
	stored.State = applicationstore.RolloutStateAborted
	stored.AbortReason = reason
	if err := s.appStore.UpdateRollout(ctx, stored); err != nil {
		return nil, fmt.Errorf("failed to persist abort: %w", err)
	}
	s.logger.Info("rollout aborted",
		zap.String("rollout_id", id),
		zap.String("reason", reason))

	if s.auditService != nil {
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  "rollout.aborted",
			TargetType: "rollout",
			TargetID:   id,
			Action:     "aborted",
			Payload:    map[string]any{"reason": reason},
		})
	}
	// Operator-initiated abort also lands on the rollout's parent
	// span. The engine's triggerAbort fires the same event for
	// auto-aborts; we cover the manual path here so a trace tells
	// the full story regardless of who pulled the lever.
	if s.tracer != nil {
		s.tracer.RecordEvent(id, "aborted", reason)
	}
	return toServiceRollout(stored), nil
}

// RollBack creates a new rollout that targets the source rollout's
// PreviousConfigID (the config the group was on before the source
// ran). The new rollout flows through Create normally, so the same
// approval workflow, audit pipeline, and engine loop handle it —
// it is not a special-case path, just a one-click way to construct
// the right RolloutInput.
//
// The source rollout must be in a terminal state. Operators who
// want to stop an in-progress rollout reach for Abort instead.
// PreviousConfigID must be non-empty; brand-new groups whose first
// rollout succeeded have nowhere to roll back to.
//
// The new rollout's RolledBackFromID points at the source so the UI
// and the audit timeline can show the chain. Stages are a single
// 100% push with zero dwell because the caller is asking to undo a
// known-bad change as fast as possible; an operator who wants
// staged rollback can use Create directly.
//
// Added in v0.60.
func (s *RolloutServiceImpl) RollBack(ctx context.Context, id, operator string) (*Rollout, error) {
	source, err := s.appStore.GetRollout(ctx, id)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("rollout not found: %s", id)
	}
	switch source.State {
	case applicationstore.RolloutStateSucceeded,
		applicationstore.RolloutStateAborted,
		applicationstore.RolloutStateRolledBack:
		// Terminal states are the only ones rollback is allowed
		// against. The succeeded case is the common one (rollout
		// completed; metrics regressed; undo). The aborted case
		// lets an operator unwind a partial canary. The
		// rolled_back case is allowed because chained rollbacks
		// can happen during a long incident.
	default:
		return nil, fmt.Errorf("rollout is not in a terminal state (state=%q)", source.State)
	}
	if source.PreviousConfigID == "" {
		return nil, fmt.Errorf("rollout has no previous config to roll back to (likely the group's first rollout)")
	}
	// Build the input. Single 100% stage with zero dwell: this is
	// an emergency undo, not a fresh push. Abort criteria stay
	// permissive so an operator's rollback does not itself abort
	// on the first hiccup.
	name := "Rollback of: " + source.Name
	if len(name) > 200 {
		name = name[:200]
	}

	// v0.61 — require approval on the rollback when either of:
	//  - the source rollout required approval (carry forward), OR
	//  - the group has require_approval_for_rollback set, which
	//    treats rollback as a more dangerous operation regardless
	//    of how the source landed.
	requireApproval := source.RequireApproval
	if g, err := s.appStore.GetGroup(ctx, source.GroupID); err == nil && g != nil && g.RequireApprovalForRollback {
		requireApproval = true
	}

	input := RolloutInput{
		Name:           name,
		GroupID:        source.GroupID,
		TargetConfigID: source.PreviousConfigID,
		Stages: []RolloutStage{
			{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
		AbortCriteria: RolloutAbortCriteria{
			MaxDriftedAgents:           0,
			MaxErrorLogsPerMinute:      0,
			MinDwellSecondsBeforeAbort: 0,
		},
		RequireApproval: requireApproval,
		RequestedBy:     operator,
		ProposedBy:      RolloutProposedByOperator,
	}

	newRollout, err := s.Create(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("failed to create rollback rollout: %w", err)
	}

	// Stamp the link field. Create() does not know about
	// RolledBackFromID; we attach it here and persist via
	// UpdateRollout so the field lands on the row.
	newRollout.RolledBackFromID = source.ID
	if err := s.appStore.UpdateRollout(ctx, toStorageRollout(newRollout)); err != nil {
		return nil, fmt.Errorf("failed to persist rollback link: %w", err)
	}

	s.logger.Info("rollback created",
		zap.String("rollback_rollout_id", newRollout.ID),
		zap.String("source_rollout_id", source.ID),
		zap.String("operator", operator),
		zap.String("target_config_id", newRollout.TargetConfigID))

	if s.auditService != nil {
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  "rollout.rollback_requested",
			TargetType: "rollout",
			TargetID:   source.ID,
			Action:     "rollback_requested",
			Payload: map[string]any{
				"rollback_rollout_id": newRollout.ID,
				"target_config_id":    newRollout.TargetConfigID,
				"requested_by":        operator,
				"source_state":        string(source.State),
			},
		})
	}
	return newRollout, nil
}

// Pause flips an in-progress rollout to paused. The engine will leave it
// alone — no stage advance, no auto-abort — until Resume is called.
// Pause is a no-op on already-paused rollouts and an error on terminal
// states.
func (s *RolloutServiceImpl) Pause(ctx context.Context, id string) (*Rollout, error) {
	stored, err := s.appStore.GetRollout(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("rollout not found: %s", id)
	}
	if stored.State == applicationstore.RolloutStatePaused {
		return toServiceRollout(stored), nil
	}
	if stored.State != applicationstore.RolloutStateInProgress && stored.State != applicationstore.RolloutStatePending {
		return toServiceRollout(stored), fmt.Errorf("cannot pause rollout in state %q", stored.State)
	}
	stored.State = applicationstore.RolloutStatePaused
	if err := s.appStore.UpdateRollout(ctx, stored); err != nil {
		return nil, fmt.Errorf("failed to persist pause: %w", err)
	}
	s.logger.Info("rollout paused", zap.String("rollout_id", id))
	if s.auditService != nil {
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor: AuditActorSystem, EventType: "rollout.paused",
			TargetType: "rollout", TargetID: id, Action: "paused",
		})
	}
	// Weave the transition into the rollout's parent OTel span as a
	// named event. Operators searching a rollout trace for "why did
	// this stall?" see paused/resumed on the parent timeline
	// alongside stage_applied + aborted + rollback_started. Nil-safe.
	if s.tracer != nil {
		s.tracer.RecordEvent(id, "paused", "")
	}
	return toServiceRollout(stored), nil
}

// Resume flips a paused rollout back to in_progress. Resets the stage
// dwell clock so the operator gets a fresh window of dwell + abort
// criteria evaluation — the safer default than picking up mid-dwell with
// stale criteria state.
func (s *RolloutServiceImpl) Resume(ctx context.Context, id string) (*Rollout, error) {
	stored, err := s.appStore.GetRollout(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("rollout not found: %s", id)
	}
	if stored.State != applicationstore.RolloutStatePaused {
		return toServiceRollout(stored), fmt.Errorf("cannot resume rollout in state %q", stored.State)
	}
	stored.State = applicationstore.RolloutStateInProgress
	now := time.Now().UTC()
	stored.StageStartedAt = &now
	if err := s.appStore.UpdateRollout(ctx, stored); err != nil {
		return nil, fmt.Errorf("failed to persist resume: %w", err)
	}
	s.logger.Info("rollout resumed", zap.String("rollout_id", id))
	if s.auditService != nil {
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor: AuditActorSystem, EventType: "rollout.resumed",
			TargetType: "rollout", TargetID: id, Action: "resumed",
		})
	}
	if s.tracer != nil {
		s.tracer.RecordEvent(id, "resumed", "")
	}
	return toServiceRollout(stored), nil
}

// Persist writes a rollout back to the store after the engine mutates it
// (state transitions, current_stage bumps, etc.). Wraps the storage call
// so the engine doesn't take an application-store dependency directly.
func (s *RolloutServiceImpl) Persist(ctx context.Context, rollout *Rollout) error {
	return s.appStore.UpdateRollout(ctx, toStorageRollout(rollout))
}

// Approve transitions a rollout from pending_approval to pending so the
// engine can pick it up on the next tick. v0.47 — two-person rule:
// the approver must not equal the rollout's RequestedBy. We compare
// case-insensitively because tokens / SSO can emit the same actor in
// either case depending on the issuer.
func (s *RolloutServiceImpl) Approve(ctx context.Context, id, approver, notes string) (*Rollout, error) {
	stored, err := s.appStore.GetRollout(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("rollout not found: %s", id)
	}
	if stored.State != applicationstore.RolloutStatePendingApproval {
		return toServiceRollout(stored), fmt.Errorf("cannot approve rollout in state %q", stored.State)
	}
	if strings.EqualFold(strings.TrimSpace(approver), strings.TrimSpace(stored.RequestedBy)) {
		return toServiceRollout(stored), fmt.Errorf("two-person rule: requester %q cannot approve their own rollout", stored.RequestedBy)
	}

	now := time.Now().UTC()
	stored.State = applicationstore.RolloutStatePending
	stored.ApprovedBy = approver
	stored.ApprovedAt = &now
	stored.ApprovalNotes = notes
	if err := s.appStore.UpdateRollout(ctx, stored); err != nil {
		return nil, fmt.Errorf("failed to persist approval: %w", err)
	}
	s.logger.Info("rollout approved",
		zap.String("rollout_id", id),
		zap.String("approver", approver))
	if s.auditService != nil {
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor:      approver,
			EventType:  "rollout.approved",
			TargetType: "rollout",
			TargetID:   id,
			Action:     "approved",
			Payload:    map[string]any{"notes": notes},
		})
	}
	if s.tracer != nil {
		s.tracer.RecordEvent(id, "approved", approver)
	}
	return toServiceRollout(stored), nil
}

// Reject terminates a rollout that was waiting for approval. Same
// two-person rule as Approve. Terminal state — the requester has to
// clone the rollout to retry.
func (s *RolloutServiceImpl) Reject(ctx context.Context, id, rejecter, notes string) (*Rollout, error) {
	stored, err := s.appStore.GetRollout(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("rollout not found: %s", id)
	}
	if stored.State != applicationstore.RolloutStatePendingApproval {
		return toServiceRollout(stored), fmt.Errorf("cannot reject rollout in state %q", stored.State)
	}
	if strings.EqualFold(strings.TrimSpace(rejecter), strings.TrimSpace(stored.RequestedBy)) {
		return toServiceRollout(stored), fmt.Errorf("two-person rule: requester %q cannot reject their own rollout", stored.RequestedBy)
	}

	now := time.Now().UTC()
	stored.State = applicationstore.RolloutStateRejected
	stored.RejectedBy = rejecter
	stored.RejectedAt = &now
	stored.ApprovalNotes = notes
	stored.CompletedAt = &now
	if err := s.appStore.UpdateRollout(ctx, stored); err != nil {
		return nil, fmt.Errorf("failed to persist rejection: %w", err)
	}
	s.logger.Info("rollout rejected",
		zap.String("rollout_id", id),
		zap.String("rejecter", rejecter))
	if s.auditService != nil {
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor:      rejecter,
			EventType:  "rollout.rejected",
			TargetType: "rollout",
			TargetID:   id,
			Action:     "rejected",
			Payload:    map[string]any{"notes": notes},
		})
	}
	if s.tracer != nil {
		s.tracer.RecordEvent(id, "rejected", rejecter)
	}

	// v0.71 — if this rejected rollout is a plan step, cancel
	// every queued follower. By design only step 0 carries the
	// approval gate, so this branch fires precisely when an
	// operator rejects a multi step plan at the approval stage.
	// Standalone rollouts (empty PlanID) and steps 1..N (never
	// carry RequireApproval per the design doc) skip this branch.
	if stored.PlanID != "" {
		cancelled, cancelErr := s.CancelPlanFollowers(ctx, stored.PlanID, stored.PlanStepIndex)
		if cancelErr != nil {
			// Don't fail the rejection — the rollout itself was
			// already rejected and persisted. Log + audit the
			// degraded state and move on. The orphan queued steps
			// will eventually be visible in the UI and a follow on
			// release can add a manual cleanup path.
			s.logger.Warn("plan reject: failed to cancel followers",
				zap.String("plan_id", stored.PlanID),
				zap.Error(cancelErr))
		} else if s.auditService != nil {
			cancelledIDs := make([]string, 0, len(cancelled))
			for _, c := range cancelled {
				cancelledIDs = append(cancelledIDs, c.ID)
				_ = s.auditService.Record(ctx, AuditEntry{
					Actor:      rejecter,
					EventType:  "plan.step_cancelled",
					TargetType: "rollout",
					TargetID:   c.ID,
					Action:     "plan_step_cancelled",
					Payload: map[string]any{
						"plan_id":         c.PlanID,
						"plan_step_index": c.PlanStepIndex,
						"reason":          "plan_rejected_at_approval",
					},
				})
			}
			_ = s.auditService.Record(ctx, AuditEntry{
				Actor:      rejecter,
				EventType:  "plan.rejected",
				TargetType: "rollout",
				TargetID:   id,
				Action:     "plan_rejected",
				Payload: map[string]any{
					"plan_id":         stored.PlanID,
					"rejected_step":   stored.PlanStepIndex,
					"notes":           notes,
					"cancelled_count": len(cancelled),
					"cancelled_ids":   cancelledIDs,
				},
			})
		}
	}

	return toServiceRollout(stored), nil
}

// Preview returns a side-by-side view of what creating a rollout against
// (groupID, targetConfigID) would do: the group's current effective
// config, the target, a line-level diff, and lint findings against the
// target. The UI displays this in the create form so operators see what
// they're about to ship before clicking Start.
//
// A missing target config is a user-facing 404 (the caller picked a bad
// id). A missing current config is benign — new groups have no
// baseline, the diff just shows the entire target as additions.
//
// This is the same logic the engine would run if you called Create —
// kept aligned so the preview the operator sees is what actually ships.
func (s *RolloutServiceImpl) Preview(ctx context.Context, groupID, targetConfigID string) (*RolloutPreview, error) {
	if groupID == "" {
		return nil, fmt.Errorf("group_id is required")
	}
	if targetConfigID == "" {
		return nil, fmt.Errorf("target_config_id is required")
	}

	target, err := s.appStore.GetConfig(ctx, targetConfigID)
	if err != nil {
		return nil, fmt.Errorf("failed to load target config: %w", err)
	}
	if target == nil {
		return nil, fmt.Errorf("target config not found: %s", targetConfigID)
	}

	// Current may legitimately not exist for a brand-new group; we
	// don't propagate the storage error here unless it's a real
	// failure (a not-found is just *Config==nil with no error).
	var current *applicationstore.Config
	if c, err := s.appStore.GetLatestConfigForGroup(ctx, groupID); err == nil {
		current = c
	} else {
		s.logger.Warn("rollout preview: failed to load current group config; treating as none",
			zap.String("group_id", groupID), zap.Error(err))
	}

	preview := &RolloutPreview{
		GroupID: groupID,
		Target:  toServiceConfig(target),
	}
	currentContent := ""
	if current != nil {
		preview.Current = toServiceConfig(current)
		currentContent = current.Content
	}
	preview.Diff = configdiff.Diff(currentContent, target.Content)
	findings := configlint.Lint(target.Content)
	if findings == nil {
		findings = []configlint.Finding{}
	}
	preview.LintFindings = findings
	return preview, nil
}

// toServiceConfig is a tiny wrapper to avoid importing applicationstore
// types into other call sites. Service-layer Config has the same field
// shape as the storage one for this purpose; keep them in sync.
func toServiceConfig(c *applicationstore.Config) *Config {
	if c == nil {
		return nil
	}
	return &Config{
		ID:         c.ID,
		Name:       c.Name,
		AgentID:    c.AgentID,
		GroupID:    c.GroupID,
		ConfigHash: c.ConfigHash,
		Content:    c.Content,
		Version:    c.Version,
		CreatedAt:  c.CreatedAt,
	}
}

// validateRolloutInput rejects rollouts the engine can't safely run.
//
// Stages must all share one mode: either every stage is percent or every
// stage is label. Mixed-mode rollouts return a validation error in v1 —
// the canary-set monotonicity guarantee that percent mode relies on
// doesn't generalize cleanly to label mode, so we keep them separate
// until operators ask for the combination.
func validateRolloutInput(in RolloutInput) error {
	if in.Name == "" {
		return fmt.Errorf("name is required")
	}
	if in.GroupID == "" {
		return fmt.Errorf("group_id is required")
	}
	if in.TargetConfigID == "" {
		return fmt.Errorf("target_config_id is required")
	}
	if len(in.Stages) == 0 {
		return fmt.Errorf("at least one stage is required")
	}

	// Default empty mode to percent for backward compatibility with
	// callers that haven't been updated yet. Treat a missing mode as
	// the historical "percentage-only" stage shape.
	mode := in.Stages[0].Mode
	if mode == "" {
		mode = RolloutStageModePercent
	}
	if mode != RolloutStageModePercent && mode != RolloutStageModeLabel {
		return fmt.Errorf("stage 0: invalid mode %q (must be percent or label)", mode)
	}

	for i, st := range in.Stages {
		stMode := st.Mode
		if stMode == "" {
			stMode = RolloutStageModePercent
		}
		if stMode != mode {
			return fmt.Errorf("stage %d: mixed stage modes are not supported (rollout uses %q, got %q)", i, mode, stMode)
		}
		if st.DwellSeconds < 0 {
			return fmt.Errorf("stage %d: dwell_seconds must be >= 0", i)
		}
		switch stMode {
		case RolloutStageModePercent:
			if st.Percentage <= 0 || st.Percentage > 100 {
				return fmt.Errorf("stage %d: percentage must be in (0, 100]", i)
			}
			if i > 0 && st.Percentage < in.Stages[i-1].Percentage {
				return fmt.Errorf("stage %d: percentage %d must be >= previous stage's %d", i, st.Percentage, in.Stages[i-1].Percentage)
			}
		case RolloutStageModeLabel:
			if len(st.LabelSelector) == 0 {
				return fmt.Errorf("stage %d: label mode requires a non-empty label_selector", i)
			}
			for k, v := range st.LabelSelector {
				if k == "" {
					return fmt.Errorf("stage %d: label_selector keys must be non-empty", i)
				}
				if v == "" {
					return fmt.Errorf("stage %d: label_selector value for %q must be non-empty", i, k)
				}
			}
		}
	}

	// Percent mode demands a final stage reaching 100 — operators
	// sometimes write [10, 50] and wonder why the rollout never finishes.
	// Label mode has no equivalent constraint; the final stage is
	// whatever the operator decided is "everyone we care about".
	if mode == RolloutStageModePercent {
		if in.Stages[len(in.Stages)-1].Percentage != 100 {
			return fmt.Errorf("final stage must have percentage = 100 to complete the rollout")
		}
	}

	if in.AbortCriteria.MaxDriftedAgents < 0 {
		return fmt.Errorf("abort_criteria.max_drifted_agents must be >= 0")
	}
	if in.AbortCriteria.MaxErrorLogsPerMinute < 0 {
		return fmt.Errorf("abort_criteria.max_error_logs_per_minute must be >= 0")
	}
	if in.AbortCriteria.MinDwellSecondsBeforeAbort < 0 {
		return fmt.Errorf("abort_criteria.min_dwell_seconds_before_abort must be >= 0")
	}
	return nil
}

// copyStringMap returns a defensive copy so callers can't mutate stored
// state through their reference. Returns nil for nil input so empty stays
// empty.
func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Also: any caller that built a stage without specifying Mode is treated
// as percent mode at create time so old clients don't break. The Create
// path normalizes the stored value before persisting.
func normalizeStages(stages []RolloutStage) []RolloutStage {
	out := make([]RolloutStage, len(stages))
	for i, st := range stages {
		if st.Mode == "" {
			st.Mode = RolloutStageModePercent
		}
		out[i] = st
	}
	return out
}

func toStorageRollout(r *Rollout) *applicationstore.Rollout {
	stages := make([]applicationstore.RolloutStage, len(r.Stages))
	for i, st := range r.Stages {
		stages[i] = applicationstore.RolloutStage{
			Mode:          applicationstore.RolloutStageMode(st.Mode),
			Percentage:    st.Percentage,
			LabelSelector: copyStringMap(st.LabelSelector),
			DwellSeconds:  st.DwellSeconds,
		}
	}
	return &applicationstore.Rollout{
		ID:               r.ID,
		Name:             r.Name,
		GroupID:          r.GroupID,
		TargetConfigID:   r.TargetConfigID,
		PreviousConfigID: r.PreviousConfigID,
		Stages:           stages,
		AbortCriteria: applicationstore.RolloutAbortCriteria{
			MaxDriftedAgents:           r.AbortCriteria.MaxDriftedAgents,
			MaxErrorLogsPerMinute:      r.AbortCriteria.MaxErrorLogsPerMinute,
			MinDwellSecondsBeforeAbort: r.AbortCriteria.MinDwellSecondsBeforeAbort,
		},
		NotificationURL: r.NotificationURL,
		State:           applicationstore.RolloutState(r.State),
		CurrentStage:    r.CurrentStage,
		StageStartedAt:  r.StageStartedAt,
		AbortReason:     r.AbortReason,
		// v0.47 approval fields.
		RequireApproval: r.RequireApproval,
		RequestedBy:     r.RequestedBy,
		ApprovedBy:      r.ApprovedBy,
		ApprovedAt:      r.ApprovedAt,
		RejectedBy:      r.RejectedBy,
		RejectedAt:      r.RejectedAt,
		ApprovalNotes:   r.ApprovalNotes,
		// v0.49 blackout fields.
		LastBlackoutReason: r.LastBlackoutReason,
		LastBlackoutAt:     r.LastBlackoutAt,
		// v0.53 proposal provenance.
		ProposedBy:        r.ProposedBy,
		ProposalReasoning: r.ProposalReasoning,
		EvidenceRefs:      toStorageEvidenceRefs(r.EvidenceRefs),
		// v0.60 rollback chain.
		RolledBackFromID: r.RolledBackFromID,
		// v0.69 plan grouping.
		PlanID:        r.PlanID,
		PlanStepIndex: r.PlanStepIndex,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		CompletedAt:   r.CompletedAt,
	}
}

// toStorageEvidenceRefs lifts the service-layer evidence type into
// its applicationstore counterpart. Shape is identical; the
// conversion keeps the service package from leaking the storage
// type to handlers.
func toStorageEvidenceRefs(in []EvidenceRef) []applicationstore.RolloutEvidenceRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]applicationstore.RolloutEvidenceRef, len(in))
	for i, e := range in {
		out[i] = applicationstore.RolloutEvidenceRef{
			Kind:        e.Kind,
			ID:          e.ID,
			URL:         e.URL,
			Description: e.Description,
		}
	}
	return out
}

// toServiceEvidenceRefs is the inverse of toStorageEvidenceRefs.
func toServiceEvidenceRefs(in []applicationstore.RolloutEvidenceRef) []EvidenceRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]EvidenceRef, len(in))
	for i, e := range in {
		out[i] = EvidenceRef{
			Kind:        e.Kind,
			ID:          e.ID,
			URL:         e.URL,
			Description: e.Description,
		}
	}
	return out
}

func toServiceRollout(r *applicationstore.Rollout) *Rollout {
	stages := make([]RolloutStage, len(r.Stages))
	for i, st := range r.Stages {
		stages[i] = RolloutStage{
			Mode:          RolloutStageMode(st.Mode),
			Percentage:    st.Percentage,
			LabelSelector: copyStringMap(st.LabelSelector),
			DwellSeconds:  st.DwellSeconds,
		}
	}
	return &Rollout{
		ID:               r.ID,
		Name:             r.Name,
		GroupID:          r.GroupID,
		TargetConfigID:   r.TargetConfigID,
		PreviousConfigID: r.PreviousConfigID,
		Stages:           stages,
		AbortCriteria: RolloutAbortCriteria{
			MaxDriftedAgents:           r.AbortCriteria.MaxDriftedAgents,
			MaxErrorLogsPerMinute:      r.AbortCriteria.MaxErrorLogsPerMinute,
			MinDwellSecondsBeforeAbort: r.AbortCriteria.MinDwellSecondsBeforeAbort,
		},
		NotificationURL: r.NotificationURL,
		State:           RolloutState(r.State),
		CurrentStage:    r.CurrentStage,
		StageStartedAt:  r.StageStartedAt,
		AbortReason:     r.AbortReason,
		// v0.47 approval fields.
		RequireApproval: r.RequireApproval,
		RequestedBy:     r.RequestedBy,
		ApprovedBy:      r.ApprovedBy,
		ApprovedAt:      r.ApprovedAt,
		RejectedBy:      r.RejectedBy,
		RejectedAt:      r.RejectedAt,
		ApprovalNotes:   r.ApprovalNotes,
		// v0.49 blackout fields.
		LastBlackoutReason: r.LastBlackoutReason,
		LastBlackoutAt:     r.LastBlackoutAt,
		// v0.53 proposal provenance.
		ProposedBy:        r.ProposedBy,
		ProposalReasoning: r.ProposalReasoning,
		EvidenceRefs:      toServiceEvidenceRefs(r.EvidenceRefs),
		// v0.60 rollback chain.
		RolledBackFromID: r.RolledBackFromID,
		// v0.69 plan grouping.
		PlanID:        r.PlanID,
		PlanStepIndex: r.PlanStepIndex,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		CompletedAt:   r.CompletedAt,
	}
}
