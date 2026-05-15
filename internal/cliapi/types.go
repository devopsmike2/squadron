// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package cliapi

import "time"

// Agent mirrors the agents endpoint response. Only the fields the CLI
// renders or filters on are declared — anything extra in the server's
// response is harmlessly ignored at decode time.
type Agent struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels"`
	Status      string            `json:"status"`
	LastSeen    time.Time         `json:"last_seen"`
	GroupID     *string           `json:"group_id,omitempty"`
	GroupName   *string           `json:"group_name,omitempty"`
	Version     string            `json:"version"`
	DriftStatus string            `json:"drift_status"`
}

// AgentsResponse is the shape of GET /api/v1/agents. The map is keyed
// by agent ID — we flatten it in the command layer for tabular display.
type AgentsResponse struct {
	Agents       map[string]Agent `json:"agents"`
	TotalCount   int              `json:"totalCount"`
	ActiveCount  int              `json:"activeCount"`
	InactiveCount int             `json:"inactiveCount"`
}

// Group mirrors /api/v1/groups items.
type Group struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"created_at"`
}

type GroupsResponse struct {
	Groups []Group `json:"groups"`
}

// Config mirrors /api/v1/configs items.
type Config struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AgentID    *string   `json:"agent_id,omitempty"`
	GroupID    *string   `json:"group_id,omitempty"`
	ConfigHash string    `json:"config_hash"`
	Content    string    `json:"content"`
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
}

type ConfigsResponse struct {
	Configs []Config `json:"configs"`
}

// CreateConfigRequest is the body POST /api/v1/configs accepts when the
// CLI is creating a config from a YAML file.
type CreateConfigRequest struct {
	Name    string  `json:"name"`
	GroupID *string `json:"group_id,omitempty"`
	AgentID *string `json:"agent_id,omitempty"`
	Content string  `json:"content"`
}

// LintFinding mirrors configlint.Finding for the lint subcommand.
type LintFinding struct {
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Line     int    `json:"line,omitempty"`
	Path     string `json:"path,omitempty"`
}

type LintResponse struct {
	Findings []LintFinding `json:"findings"`
}

// Rollout mirrors services.Rollout. CurrentStage indexes into Stages.
type Rollout struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	GroupID          string         `json:"group_id"`
	TargetConfigID   string         `json:"target_config_id"`
	PreviousConfigID string         `json:"previous_config_id,omitempty"`
	Stages           []RolloutStage `json:"stages"`
	AbortCriteria    RolloutAbortCriteria `json:"abort_criteria"`
	NotificationURL  string         `json:"notification_url,omitempty"`
	State            string         `json:"state"`
	CurrentStage     int            `json:"current_stage"`
	StageStartedAt   *time.Time     `json:"stage_started_at,omitempty"`
	AbortReason      string         `json:"abort_reason,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
	CompletedAt      *time.Time     `json:"completed_at,omitempty"`
}

type RolloutStage struct {
	Mode          string            `json:"mode,omitempty"`
	Percentage    int               `json:"percentage,omitempty"`
	LabelSelector map[string]string `json:"label_selector,omitempty"`
	DwellSeconds  int               `json:"dwell_seconds"`
}

type RolloutAbortCriteria struct {
	MaxDriftedAgents           int `json:"max_drifted_agents"`
	MaxErrorLogsPerMinute      int `json:"max_error_logs_per_minute,omitempty"`
	MinDwellSecondsBeforeAbort int `json:"min_dwell_seconds_before_abort,omitempty"`
}

type RolloutsResponse struct {
	Rollouts []Rollout `json:"rollouts"`
}

// RolloutInput is the body POST /api/v1/rollouts accepts.
type RolloutInput struct {
	Name            string               `json:"name"`
	GroupID         string               `json:"group_id"`
	TargetConfigID  string               `json:"target_config_id"`
	Stages          []RolloutStage       `json:"stages"`
	AbortCriteria   RolloutAbortCriteria `json:"abort_criteria"`
	NotificationURL string               `json:"notification_url,omitempty"`
}

// RolloutTemplate mirrors the /rollout-recipes/templates response.
type RolloutTemplate struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Description   string               `json:"description"`
	WhenToUse     string               `json:"when_to_use"`
	DefaultName   string               `json:"default_name"`
	Stages        []RolloutStage       `json:"stages"`
	AbortCriteria RolloutAbortCriteria `json:"abort_criteria"`
}

type RolloutTemplatesResponse struct {
	Templates []RolloutTemplate `json:"templates"`
}

// RolloutPreview mirrors services.RolloutPreview.
type RolloutPreview struct {
	GroupID      string        `json:"group_id"`
	Current      *Config       `json:"current,omitempty"`
	Target       Config        `json:"target"`
	Diff         DiffResult    `json:"diff"`
	LintFindings []LintFinding `json:"lint_findings"`
}

type DiffResult struct {
	Unified   string `json:"unified"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Identical bool   `json:"identical"`
}

// AbortRequest is the body POST /api/v1/rollouts/:id/abort accepts.
type AbortRequest struct {
	Reason string `json:"reason"`
}

// AuditEvent mirrors services.AuditEvent.
type AuditEvent struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`
	Actor      string         `json:"actor"`
	EventType  string         `json:"event_type"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id,omitempty"`
	Action     string         `json:"action"`
	Payload    map[string]any `json:"payload,omitempty"`
}

type AuditResponse struct {
	Events []AuditEvent `json:"events"`
}

// APIToken mirrors services.APIToken. Plaintext is NEVER on this type.
type APIToken struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type TokensResponse struct {
	Tokens []APIToken `json:"tokens"`
}

// CreateTokenResponse is the body POST /api/v1/auth/tokens returns.
// The plaintext is ONLY ever in this response.
type CreateTokenResponse struct {
	Token     APIToken `json:"token"`
	Plaintext string   `json:"plaintext"`
}
