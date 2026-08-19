// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// DefaultAutomationCooldownSeconds is applied when a rule is created with a
// non-positive cooldown. A restart that fires again within seconds would be a
// storm; five minutes is a conservative floor that still lets a genuinely-stuck
// agent get a second attempt before max-attempts escalates.
const DefaultAutomationCooldownSeconds = 300

// DefaultAutomationMaxAttempts is applied when a rule is created with a
// non-positive max-attempts. After this many actions in one outage the
// evaluator STOPS auto-acting and escalates (ADR 0038): a config-bricking
// crash must not be masked by infinite restarts.
const DefaultAutomationMaxAttempts = 3

// AutomationServiceImpl implements AutomationService over an ApplicationStore.
type AutomationServiceImpl struct {
	appStore applicationstore.ApplicationStore
	logger   *zap.Logger
}

// NewAutomationService creates a new AutomationService.
func NewAutomationService(appStore applicationstore.ApplicationStore, logger *zap.Logger) AutomationService {
	return &AutomationServiceImpl{appStore: appStore, logger: logger}
}

func (s *AutomationServiceImpl) ListAutomations(ctx context.Context) ([]*Automation, error) {
	stored, err := s.appStore.ListAutomations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Automation, len(stored))
	for i, a := range stored {
		out[i] = toServiceAutomation(a)
	}
	return out, nil
}

func (s *AutomationServiceImpl) GetAutomation(ctx context.Context, id string) (*Automation, error) {
	stored, err := s.appStore.GetAutomation(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, nil
	}
	return toServiceAutomation(stored), nil
}

func (s *AutomationServiceImpl) CreateAutomation(ctx context.Context, input AutomationInput) (*Automation, error) {
	input = applyAutomationDefaults(input)
	if err := validateAutomationInput(input); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	a := &Automation{
		ID:              uuid.New().String(),
		Name:            input.Name,
		Enabled:         input.Enabled,
		AgentID:         normalizePtr(input.AgentID),
		GroupID:         normalizePtr(input.GroupID),
		Trigger:         input.Trigger,
		Action:          input.Action,
		CooldownSeconds: input.CooldownSeconds,
		MaxAttempts:     input.MaxAttempts,
		DryRun:          input.DryRun,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.appStore.CreateAutomation(ctx, toStorageAutomation(a)); err != nil {
		return nil, fmt.Errorf("failed to create automation: %w", err)
	}
	s.logger.Info("created automation", zap.String("automation_id", a.ID), zap.String("name", a.Name))
	return a, nil
}

func (s *AutomationServiceImpl) UpdateAutomation(ctx context.Context, id string, input AutomationInput) (*Automation, error) {
	input = applyAutomationDefaults(input)
	if err := validateAutomationInput(input); err != nil {
		return nil, err
	}

	existing, err := s.appStore.GetAutomation(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("automation not found: %s", id)
	}

	now := time.Now().UTC()
	updated := &Automation{
		ID:              id,
		Name:            input.Name,
		Enabled:         input.Enabled,
		AgentID:         normalizePtr(input.AgentID),
		GroupID:         normalizePtr(input.GroupID),
		Trigger:         input.Trigger,
		Action:          input.Action,
		CooldownSeconds: input.CooldownSeconds,
		MaxAttempts:     input.MaxAttempts,
		DryRun:          input.DryRun,
		CreatedAt:       existing.CreatedAt,
		UpdatedAt:       now,
	}
	if err := s.appStore.UpdateAutomation(ctx, toStorageAutomation(updated)); err != nil {
		return nil, fmt.Errorf("failed to update automation: %w", err)
	}
	s.logger.Info("updated automation", zap.String("automation_id", id))
	return updated, nil
}

func (s *AutomationServiceImpl) SetAutomationEnabled(ctx context.Context, id string, enabled bool) (*Automation, error) {
	existing, err := s.appStore.GetAutomation(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("automation not found: %s", id)
	}
	existing.Enabled = enabled
	existing.UpdatedAt = time.Now().UTC()
	if err := s.appStore.UpdateAutomation(ctx, existing); err != nil {
		return nil, fmt.Errorf("failed to set automation enabled: %w", err)
	}
	s.logger.Info("set automation enabled", zap.String("automation_id", id), zap.Bool("enabled", enabled))
	return toServiceAutomation(existing), nil
}

func (s *AutomationServiceImpl) DeleteAutomation(ctx context.Context, id string) error {
	if err := s.appStore.DeleteAutomation(ctx, id); err != nil {
		return err
	}
	s.logger.Info("deleted automation", zap.String("automation_id", id))
	return nil
}

// applyAutomationDefaults fills non-positive guardrail fields with the
// conservative defaults so a rule created via a minimal API body is still
// storm-safe.
func applyAutomationDefaults(input AutomationInput) AutomationInput {
	if input.CooldownSeconds <= 0 {
		input.CooldownSeconds = DefaultAutomationCooldownSeconds
	}
	if input.MaxAttempts <= 0 {
		input.MaxAttempts = DefaultAutomationMaxAttempts
	}
	return input
}

// validateAutomationInput rejects rules the evaluator can't safely run. The
// load-bearing check is the agent XOR group target: a rule scoped to neither
// (or both) has no well-defined blast radius.
func validateAutomationInput(input AutomationInput) error {
	if strings.TrimSpace(input.Name) == "" {
		return fmt.Errorf("name is required")
	}
	hasAgent := normalizePtr(input.AgentID) != nil
	hasGroup := normalizePtr(input.GroupID) != nil
	switch {
	case hasAgent && hasGroup:
		return fmt.Errorf("invalid target: set exactly one of agent_id or group_id, not both")
	case !hasAgent && !hasGroup:
		return fmt.Errorf("invalid target: set exactly one of agent_id or group_id")
	}
	switch input.Trigger {
	case AutomationTriggerAgentUnhealthy, AutomationTriggerAgentSilent:
	default:
		return fmt.Errorf("invalid trigger %q", input.Trigger)
	}
	switch input.Action {
	case AutomationActionSupervisorRestart:
	default:
		return fmt.Errorf("invalid action %q", input.Action)
	}
	if input.CooldownSeconds <= 0 {
		return fmt.Errorf("cooldown_seconds must be positive")
	}
	if input.MaxAttempts <= 0 {
		return fmt.Errorf("max_attempts must be positive")
	}
	return nil
}

// normalizePtr treats a nil pointer and a pointer to an empty/whitespace string
// as the same "unset" value, so a JSON body sending "agent_id": "" reads as no
// agent target rather than a target with an empty id.
func normalizePtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

func toStorageAutomation(a *Automation) *applicationstore.Automation {
	return &applicationstore.Automation{
		ID:              a.ID,
		Name:            a.Name,
		Enabled:         a.Enabled,
		AgentID:         a.AgentID,
		GroupID:         a.GroupID,
		Trigger:         applicationstore.AutomationTrigger(a.Trigger),
		Action:          applicationstore.AutomationAction(a.Action),
		CooldownSeconds: a.CooldownSeconds,
		MaxAttempts:     a.MaxAttempts,
		DryRun:          a.DryRun,
		CreatedAt:       a.CreatedAt,
		UpdatedAt:       a.UpdatedAt,
	}
}

func toServiceAutomation(a *applicationstore.Automation) *Automation {
	return &Automation{
		ID:              a.ID,
		Name:            a.Name,
		Enabled:         a.Enabled,
		AgentID:         a.AgentID,
		GroupID:         a.GroupID,
		Trigger:         AutomationTrigger(a.Trigger),
		Action:          AutomationAction(a.Action),
		CooldownSeconds: a.CooldownSeconds,
		MaxAttempts:     a.MaxAttempts,
		DryRun:          a.DryRun,
		CreatedAt:       a.CreatedAt,
		UpdatedAt:       a.UpdatedAt,
	}
}
