// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// RolloutServiceImpl is the canonical implementation backed by an
// ApplicationStore. It performs validation, snapshots the previous config
// for rollback at Create time, and records audit entries on create + abort.
type RolloutServiceImpl struct {
	appStore     applicationstore.ApplicationStore
	agentService AgentService
	auditService AuditService // optional
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
		Stages:           input.Stages,
		AbortCriteria:    input.AbortCriteria,
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

	if s.auditService != nil {
		_ = s.auditService.Record(ctx, AuditEntry{
			Actor:      AuditActorSystem,
			EventType:  "rollout.created",
			TargetType: "rollout",
			TargetID:   rollout.ID,
			Action:     "created",
			Payload: map[string]any{
				"name":             rollout.Name,
				"group_id":         rollout.GroupID,
				"target_config_id": rollout.TargetConfigID,
				"stage_count":      len(rollout.Stages),
			},
		})
	}
	return rollout, nil
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
	return toServiceRollout(stored), nil
}

// Persist writes a rollout back to the store after the engine mutates it
// (state transitions, current_stage bumps, etc.). Wraps the storage call
// so the engine doesn't take an application-store dependency directly.
func (s *RolloutServiceImpl) Persist(ctx context.Context, rollout *Rollout) error {
	return s.appStore.UpdateRollout(ctx, toStorageRollout(rollout))
}

// validateRolloutInput rejects rollouts the engine can't safely run.
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
	prev := 0
	for i, st := range in.Stages {
		if st.Percentage <= 0 || st.Percentage > 100 {
			return fmt.Errorf("stage %d: percentage must be in (0, 100]", i)
		}
		if st.Percentage < prev {
			return fmt.Errorf("stage %d: percentage %d must be >= previous stage's %d", i, st.Percentage, prev)
		}
		if st.DwellSeconds < 0 {
			return fmt.Errorf("stage %d: dwell_seconds must be >= 0", i)
		}
		prev = st.Percentage
	}
	// Final stage must reach 100 — operators sometimes write [10, 50] and
	// wonder why the rollout never finishes. Catch it.
	if in.Stages[len(in.Stages)-1].Percentage != 100 {
		return fmt.Errorf("final stage must have percentage = 100 to complete the rollout")
	}
	if in.AbortCriteria.MaxDriftedAgents < 0 {
		return fmt.Errorf("abort_criteria.max_drifted_agents must be >= 0")
	}
	return nil
}

func toStorageRollout(r *Rollout) *applicationstore.Rollout {
	stages := make([]applicationstore.RolloutStage, len(r.Stages))
	for i, st := range r.Stages {
		stages[i] = applicationstore.RolloutStage{
			Percentage:   st.Percentage,
			DwellSeconds: st.DwellSeconds,
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
			MaxDriftedAgents: r.AbortCriteria.MaxDriftedAgents,
		},
		State:          applicationstore.RolloutState(r.State),
		CurrentStage:   r.CurrentStage,
		StageStartedAt: r.StageStartedAt,
		AbortReason:    r.AbortReason,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		CompletedAt:    r.CompletedAt,
	}
}

func toServiceRollout(r *applicationstore.Rollout) *Rollout {
	stages := make([]RolloutStage, len(r.Stages))
	for i, st := range r.Stages {
		stages[i] = RolloutStage{
			Percentage:   st.Percentage,
			DwellSeconds: st.DwellSeconds,
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
			MaxDriftedAgents: r.AbortCriteria.MaxDriftedAgents,
		},
		State:          RolloutState(r.State),
		CurrentStage:   r.CurrentStage,
		StageStartedAt: r.StageStartedAt,
		AbortReason:    r.AbortReason,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		CompletedAt:    r.CompletedAt,
	}
}
