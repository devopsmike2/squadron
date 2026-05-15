package services

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AgentService defines the interface for agent management operations
type AgentService interface {
	// Agent operations
	CreateAgent(ctx context.Context, agent *Agent) error
	GetAgent(ctx context.Context, id uuid.UUID) (*Agent, error)
	ListAgents(ctx context.Context) ([]*Agent, error)
	UpdateAgentStatus(ctx context.Context, id uuid.UUID, status AgentStatus) error
	UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error
	UpdateAgentEffectiveConfig(ctx context.Context, id uuid.UUID, effectiveConfig string) error
	DeleteAgent(ctx context.Context, id uuid.UUID) error

	// Group operations
	CreateGroup(ctx context.Context, group *Group) error
	GetGroup(ctx context.Context, id string) (*Group, error)
	GetGroupByName(ctx context.Context, name string) (*Group, error)
	ListGroups(ctx context.Context) ([]*Group, error)
	DeleteGroup(ctx context.Context, id string) error

	// Config operations
	CreateConfig(ctx context.Context, config *Config) error
	GetConfig(ctx context.Context, id string) (*Config, error)
	GetLatestConfigForAgent(ctx context.Context, agentID uuid.UUID) (*Config, error)
	GetLatestConfigForGroup(ctx context.Context, groupID string) (*Config, error)
	ListConfigs(ctx context.Context, filter ConfigFilter) ([]*Config, error)

	// StoreConfigForAgent validates and stores configuration for an agent
	// Returns the stored config or error if agent doesn't exist or doesn't support remote config
	StoreConfigForAgent(ctx context.Context, agentID uuid.UUID, content string) (*Config, error)
}

// Agent represents an OpenTelemetry agent
type Agent struct {
	ID              uuid.UUID           `json:"id"`
	Name            string              `json:"name"`
	Labels          map[string]string   `json:"labels"`
	Status          AgentStatus         `json:"status"`
	LastSeen        time.Time           `json:"last_seen"`
	GroupID         *string             `json:"group_id,omitempty"`
	GroupName       *string             `json:"group_name,omitempty"`
	Version         string              `json:"version"`
	Capabilities    []string            `json:"capabilities"`
	EffectiveConfig string              `json:"effective_config,omitempty"`
	ConfigIntent    *ConfigIntent       `json:"config_intent,omitempty"`
	DriftStatus     ConfigDriftStatus   `json:"drift_status"`
	DriftDetails    *ConfigDriftDetails `json:"drift_details,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
}

// ConfigIntentSource represents where a config intent originated
type ConfigIntentSource string

const (
	ConfigIntentSourceAgent ConfigIntentSource = "agent"
	ConfigIntentSourceGroup ConfigIntentSource = "group"
)

// ConfigIntent captures the intended configuration for an agent
type ConfigIntent struct {
	Source     ConfigIntentSource `json:"source"`
	SourceName string             `json:"source_name,omitempty"`
	ConfigID   string             `json:"config_id"`
	Version    int                `json:"version"`
	Hash       string             `json:"hash"`
	UpdatedAt  time.Time          `json:"updated_at"`
	Content    string             `json:"content,omitempty"`
}

// ConfigDriftStatus represents drift evaluation results
type ConfigDriftStatus string

const (
	ConfigDriftStatusUnknown     ConfigDriftStatus = "unknown"
	ConfigDriftStatusSynced      ConfigDriftStatus = "synced"
	ConfigDriftStatusDrifted     ConfigDriftStatus = "drifted"
	ConfigDriftStatusNoIntent    ConfigDriftStatus = "no_intent"
	ConfigDriftStatusNoEffective ConfigDriftStatus = "no_effective"
)

// ConfigDriftDetails contains metadata about the drift evaluation
type ConfigDriftDetails struct {
	IntentHash    string    `json:"intent_hash,omitempty"`
	EffectiveHash string    `json:"effective_hash,omitempty"`
	Diff          string    `json:"diff,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
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
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	AgentCount int               `json:"agent_count"`
	ConfigName string            `json:"config_name,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
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
