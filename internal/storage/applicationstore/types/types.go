package types

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ApplicationStore interface for managing application data
type ApplicationStore interface {
	CreateAgent(ctx context.Context, agent *Agent) error
	GetAgent(ctx context.Context, id uuid.UUID) (*Agent, error)
	ListAgents(ctx context.Context) ([]*Agent, error)
	UpdateAgentStatus(ctx context.Context, id uuid.UUID, status AgentStatus) error
	UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error
	UpdateAgentEffectiveConfig(ctx context.Context, id uuid.UUID, effectiveConfig string) error
	DeleteAgent(ctx context.Context, id uuid.UUID) error

	// Group management
	CreateGroup(ctx context.Context, group *Group) error
	GetGroup(ctx context.Context, id string) (*Group, error)
	ListGroups(ctx context.Context) ([]*Group, error)
	DeleteGroup(ctx context.Context, id string) error

	// Config management
	CreateConfig(ctx context.Context, config *Config) error
	GetConfig(ctx context.Context, id string) (*Config, error)
	GetLatestConfigForAgent(ctx context.Context, agentID uuid.UUID) (*Config, error)
	GetLatestConfigForGroup(ctx context.Context, groupID string) (*Config, error)
	ListConfigs(ctx context.Context, filter ConfigFilter) ([]*Config, error)
	ListSavedQueries(ctx context.Context) ([]*SavedQuery, error)
	GetSavedQuery(ctx context.Context, id string) (*SavedQuery, error)
	CreateSavedQuery(ctx context.Context, query *SavedQuery) error
	UpdateSavedQuery(ctx context.Context, query *SavedQuery) error
	DeleteSavedQuery(ctx context.Context, id string) error

	// Alert rule management
	CreateAlertRule(ctx context.Context, rule *AlertRule) error
	GetAlertRule(ctx context.Context, id string) (*AlertRule, error)
	ListAlertRules(ctx context.Context) ([]*AlertRule, error)
	UpdateAlertRule(ctx context.Context, rule *AlertRule) error
	DeleteAlertRule(ctx context.Context, id string) error

	// Audit log
	CreateAuditEvent(ctx context.Context, event *AuditEvent) error
	ListAuditEvents(ctx context.Context, filter AuditEventFilter) ([]*AuditEvent, error)

	// Rollouts (safe staged config rollouts)
	CreateRollout(ctx context.Context, rollout *Rollout) error
	GetRollout(ctx context.Context, id string) (*Rollout, error)
	ListRollouts(ctx context.Context, filter RolloutFilter) ([]*Rollout, error)
	UpdateRollout(ctx context.Context, rollout *Rollout) error
}

// RolloutState is the lifecycle position of a Rollout.
type RolloutState string

const (
	RolloutStatePending     RolloutState = "pending"      // created but engine hasn't picked it up yet
	RolloutStateInProgress  RolloutState = "in_progress"  // actively advancing through stages
	RolloutStatePaused      RolloutState = "paused"       // operator paused — engine no-ops, no advance, no auto-abort
	RolloutStateSucceeded   RolloutState = "succeeded"    // final stage completed cleanly
	RolloutStateAborted     RolloutState = "aborted"      // operator clicked Abort or criteria fired; rollback in progress
	RolloutStateRolledBack  RolloutState = "rolled_back"  // previous config restored; terminal
)

// RolloutStageMode controls how the engine picks the canary set for a
// stage. "percent" is the original behavior — take the first N% of the
// group's agents in deterministic id order. "label" uses a key=value
// equality match against agent labels, letting operators name specific
// agents (e.g. host.name=canary-1) or whole sub-environments
// (e.g. deployment.environment=staging) as the canary.
type RolloutStageMode string

const (
	RolloutStageModePercent RolloutStageMode = "percent"
	RolloutStageModeLabel   RolloutStageMode = "label"
)

// RolloutStage is one promotion step. The Mode field decides which other
// fields are honored:
//   - "percent": Percentage (1-100). Cumulative — stage[N] targets that
//     many percent of the group's agents (so [10, 50, 100] means 10%
//     first, then expand to 50%, then 100%).
//   - "label": LabelSelector. AND-semantics over key=value equality on
//     agent labels. The matched set is the canary for this stage.
//     Stages within a label-mode rollout don't have a "cumulative"
//     constraint — the operator is responsible for ordering them in a
//     sensible superset progression.
//
// v1 requires every stage in a rollout to share the same mode. Mixed-mode
// rollouts return a validation error.
type RolloutStage struct {
	Mode          RolloutStageMode  `json:"mode"`                     // "percent" or "label"
	Percentage    int               `json:"percentage,omitempty"`     // for percent mode; 1-100
	LabelSelector map[string]string `json:"label_selector,omitempty"` // for label mode
	DwellSeconds  int               `json:"dwell_seconds"`            // pause at this stage before auto-advancing
}

// RolloutAbortCriteria are the conditions under which the engine auto-aborts
// a rollout and rolls back to PreviousConfigID. Conservative defaults are
// recommended — a rolled-back-by-mistake rollout is recoverable, a let-it-
// burn rollout often isn't.
type RolloutAbortCriteria struct {
	// MaxDriftedAgents: if more than this many canary agents end up in
	// drift state during a dwell, abort. 0 means any drift aborts.
	MaxDriftedAgents int `json:"max_drifted_agents"`

	// MaxErrorLogsPerMinute: if the canary agents collectively produce
	// more than this many ERROR/FATAL log records per minute (averaged
	// over the dwell window so far), abort. 0 disables the check.
	MaxErrorLogsPerMinute int `json:"max_error_logs_per_minute,omitempty"`

	// MinDwellSecondsBeforeAbort: how long after a stage starts the
	// engine waits before applying error-rate criteria. Gives newly-
	// pushed agents time to flush startup noise. Default 30s.
	MinDwellSecondsBeforeAbort int `json:"min_dwell_seconds_before_abort,omitempty"`
}

// Rollout is one safe staged config rollout against a group.
type Rollout struct {
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	GroupID          string               `json:"group_id"`
	TargetConfigID   string               `json:"target_config_id"`
	PreviousConfigID string               `json:"previous_config_id,omitempty"` // captured at create time for rollback
	Stages           []RolloutStage       `json:"stages"`
	AbortCriteria    RolloutAbortCriteria `json:"abort_criteria"`
	NotificationURL  string               `json:"notification_url,omitempty"` // optional webhook for state transitions

	State          RolloutState `json:"state"`
	CurrentStage   int          `json:"current_stage"`              // index into Stages
	StageStartedAt *time.Time   `json:"stage_started_at,omitempty"` // when CurrentStage began dwelling
	AbortReason    string       `json:"abort_reason,omitempty"`     // populated when State transitions to aborted

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"` // set on terminal state
}

// RolloutFilter narrows ListRollouts. Empty filter returns all.
type RolloutFilter struct {
	GroupID string
	State   RolloutState
	Limit   int
}

// AuditEvent is one entry in the audit log. Every state change in Squadron
// — config push, drift transition, rule edit, agent registration — is
// recorded as an AuditEvent so operators have an answerable history when
// something goes wrong.
//
// The Payload is intentionally freeform so publishers can attach event-
// specific metadata (before/after, diff, value at firing, etc.) without
// forcing a schema migration every time we add a new event type.
type AuditEvent struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`            // when the event happened
	Actor      string         `json:"actor"`                // "system" | "operator:<email>" | "agent:<id>" | "opamp"
	EventType  string         `json:"event_type"`           // dotted name, e.g. "config.applied"
	TargetType string         `json:"target_type"`          // "agent" | "group" | "config" | "rule"
	TargetID   string         `json:"target_id,omitempty"`  // affected entity id; may be empty for fleet-wide events
	Action     string         `json:"action"`               // "created" | "updated" | "deleted" | "applied" | "drift" | ...
	Payload    map[string]any `json:"payload,omitempty"`    // freeform JSON metadata
	CreatedAt  time.Time      `json:"created_at"`           // when the row was inserted
}

// AuditEventFilter narrows a ListAuditEvents query.
//
// All fields are optional. An empty filter returns the most recent events
// across the whole fleet (subject to Limit).
type AuditEventFilter struct {
	TargetType string
	TargetID   string
	Since      time.Time // events with Timestamp >= Since; zero value disables the filter
	Limit      int       // default 100 if zero; capped at 1000 by the storage layer
}

// AlertSeverity is the severity level attached to a firing alert.
type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
)

// ThresholdOperator is the comparison used between the query result and the
// threshold value. Mirrors the common Prometheus expression operators.
type ThresholdOperator string

const (
	ThresholdGreater        ThresholdOperator = ">"
	ThresholdGreaterOrEqual ThresholdOperator = ">="
	ThresholdLess           ThresholdOperator = "<"
	ThresholdLessOrEqual    ThresholdOperator = "<="
	ThresholdEqual          ThresholdOperator = "=="
	ThresholdNotEqual       ThresholdOperator = "!="
)

// AlertRule defines a periodically-evaluated Squadron QL query and what to do
// when its scalar result satisfies the threshold.
//
// Example: name "high drift rate", query "fleet_drift_status_drifted",
// operator ">", threshold 5, interval 60s, severity warning,
// webhook https://hooks.example.com/squadron.
type AlertRule struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	Query             string            `json:"query"`
	ThresholdOperator ThresholdOperator `json:"threshold_operator"`
	ThresholdValue    float64           `json:"threshold_value"`
	IntervalSeconds   int               `json:"interval_seconds"`
	Severity          AlertSeverity     `json:"severity"`
	Enabled           bool              `json:"enabled"`
	WebhookURL        string            `json:"webhook_url,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// Agent represents an OpenTelemetry agent
type Agent struct {
	ID              uuid.UUID         `json:"id"`
	Name            string            `json:"name"`
	Labels          map[string]string `json:"labels"`
	Status          AgentStatus       `json:"status"`
	LastSeen        time.Time         `json:"last_seen"`
	GroupID         *string           `json:"group_id,omitempty"`
	GroupName       *string           `json:"group_name,omitempty"`
	Version         string            `json:"version"`
	Capabilities    []string          `json:"capabilities"`
	EffectiveConfig string            `json:"effective_config,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// AgentStatus represents the status of an agent
type AgentStatus string

const (
	AgentStatusOnline  AgentStatus = "online"
	AgentStatusOffline AgentStatus = "offline"
	AgentStatusError   AgentStatus = "error"
)

// Group represents a group of agents
type Group struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Config represents an agent configuration
type Config struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	AgentID    *uuid.UUID `json:"agent_id,omitempty"`
	GroupID    *string    `json:"group_id,omitempty"`
	ConfigHash string     `json:"config_hash"`
	Content    string     `json:"content"`
	Version    int        `json:"version"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ConfigFilter represents filters for listing configs
type ConfigFilter struct {
	AgentID *uuid.UUID
	GroupID *string
	Limit   int
}

// SavedQuery represents a saved Squadron QL query
type SavedQuery struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Query       string   `json:"query"`
	Tags        []string `json:"tags"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}
