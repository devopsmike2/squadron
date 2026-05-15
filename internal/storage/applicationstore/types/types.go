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
