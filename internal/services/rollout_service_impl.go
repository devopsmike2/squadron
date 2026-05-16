// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/configdiff"
	"github.com/devopsmike2/squadron/internal/configlint"
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
	auditService AuditService // optional
	tracer       RolloutTracer // optional
	logger       *zap.Logger
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

	now := time.Now().UTC()
	rollout := &Rollout{
		ID:               uuid.New().String(),
		Name:             input.Name,
		GroupID:          input.GroupID,
		TargetConfigID:   input.TargetConfigID,
		PreviousConfigID: previousID,
		Stages:           normalizeStages(input.Stages),
		AbortCriteria:    input.AbortCriteria,
		NotificationURL:  input.NotificationURL,
		State:            RolloutStatePending,
		CurrentStage:     0,
		CreatedAt:        now,
		UpdatedAt:        now,
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
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
		CompletedAt:     r.CompletedAt,
	}
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
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
		CompletedAt:     r.CompletedAt,
	}
}
