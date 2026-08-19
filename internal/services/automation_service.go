// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"
)

// AutomationService manages Automations (ADR 0038, first slice) —
// condition-triggered auto-remediation rules: trigger → action → target
// (an agent XOR a group), under guardrails. This is the CRUD surface; the
// leader-only evaluator (internal/automations) consumes the ENABLED rules.
type AutomationService interface {
	ListAutomations(ctx context.Context) ([]*Automation, error)
	GetAutomation(ctx context.Context, id string) (*Automation, error)
	CreateAutomation(ctx context.Context, input AutomationInput) (*Automation, error)
	UpdateAutomation(ctx context.Context, id string, input AutomationInput) (*Automation, error)
	// SetAutomationEnabled flips the enabled flag without touching any other
	// field — the instant enable/disable guardrail (ADR 0038).
	SetAutomationEnabled(ctx context.Context, id string, enabled bool) (*Automation, error)
	DeleteAutomation(ctx context.Context, id string) error
}

// AutomationTrigger is the condition an automation reacts to.
type AutomationTrigger string

const (
	AutomationTriggerAgentUnhealthy AutomationTrigger = "agent_unhealthy"
	AutomationTriggerAgentSilent    AutomationTrigger = "agent_silent"
)

// AutomationAction is what an automation does when its trigger holds.
type AutomationAction string

const (
	AutomationActionSupervisorRestart AutomationAction = "supervisor_restart"
)

// Automation is one auto-remediation rule as surfaced by the API. Exactly one
// of AgentID / GroupID is set.
type Automation struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Enabled bool    `json:"enabled"`
	AgentID *string `json:"agent_id,omitempty"`
	GroupID *string `json:"group_id,omitempty"`

	Trigger AutomationTrigger `json:"trigger"`
	Action  AutomationAction  `json:"action"`

	CooldownSeconds int  `json:"cooldown_seconds"`
	MaxAttempts     int  `json:"max_attempts"`
	DryRun          bool `json:"dry_run"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AutomationInput captures the user-editable fields for an automation. Exactly
// one of AgentID / GroupID must be set (validated at the service boundary).
type AutomationInput struct {
	Name            string            `json:"name"`
	Enabled         bool              `json:"enabled"`
	AgentID         *string           `json:"agent_id"`
	GroupID         *string           `json:"group_id"`
	Trigger         AutomationTrigger `json:"trigger"`
	Action          AutomationAction  `json:"action"`
	CooldownSeconds int               `json:"cooldown_seconds"`
	MaxAttempts     int               `json:"max_attempts"`
	DryRun          bool              `json:"dry_run"`
}
