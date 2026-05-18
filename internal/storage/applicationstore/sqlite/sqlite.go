package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// Storage implements the ApplicationStore interface using SQLite
type Storage struct {
	db     *sql.DB
	logger *zap.Logger
}

// NewSQLiteStorage creates a new SQLite storage instance
func NewSQLiteStorage(dbPath string, logger *zap.Logger) (types.ApplicationStore, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Enable WAL mode for better concurrency
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("failed to enable WAL mode: %w", err)
	}

	// Enable foreign keys
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	storage := &Storage{
		db:     db,
		logger: logger,
	}

	// Run migrations
	if err := storage.migrate(); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	logger.Info("SQLite storage initialized", zap.String("path", dbPath))
	return storage, nil
}

// migrate runs database migrations
func (s *Storage) migrate() error {
	// Create tables if they don't exist
	createTables := `
		CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			labels TEXT,
			status TEXT NOT NULL DEFAULT 'offline',
			last_seen DATETIME NOT NULL,
			group_id TEXT,
			group_name TEXT,
			version TEXT,
			capabilities TEXT,
			effective_config TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			labels TEXT,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS configs (
			id TEXT PRIMARY KEY,
			name TEXT,
			agent_id TEXT,
			group_id TEXT,
			config_hash TEXT NOT NULL,
			content TEXT NOT NULL,
			version INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE,
			FOREIGN KEY (group_id) REFERENCES groups(id) ON DELETE CASCADE
		);

		CREATE TABLE IF NOT EXISTS saved_queries (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		description TEXT,
		query TEXT NOT NULL,
		tags TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_saved_queries_updated_at ON saved_queries(updated_at);

	CREATE TABLE IF NOT EXISTS alert_rules (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		description TEXT,
		query TEXT NOT NULL,
		threshold_operator TEXT NOT NULL,
		threshold_value REAL NOT NULL,
		interval_seconds INTEGER NOT NULL,
		severity TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1,
		webhook_url TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_alert_rules_enabled ON alert_rules(enabled);

	CREATE TABLE IF NOT EXISTS audit_events (
		id TEXT PRIMARY KEY,
		timestamp DATETIME NOT NULL,
		actor TEXT NOT NULL,
		event_type TEXT NOT NULL,
		target_type TEXT NOT NULL,
		target_id TEXT,
		action TEXT NOT NULL,
		payload TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_audit_events_timestamp ON audit_events(timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_events_target ON audit_events(target_type, target_id, timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_events_event_type ON audit_events(event_type);

	CREATE TABLE IF NOT EXISTS rollouts (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		group_id TEXT NOT NULL,
		target_config_id TEXT NOT NULL,
		previous_config_id TEXT,
		stages TEXT NOT NULL,                 -- JSON array of {percentage, dwell_seconds}
		abort_criteria TEXT NOT NULL,         -- JSON {max_drifted_agents}
		notification_url TEXT,                -- optional webhook
		state TEXT NOT NULL,
		current_stage INTEGER NOT NULL DEFAULT 0,
		stage_started_at DATETIME,
		abort_reason TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME
	);

	CREATE INDEX IF NOT EXISTS idx_rollouts_state ON rollouts(state);
	CREATE INDEX IF NOT EXISTS idx_rollouts_group ON rollouts(group_id, created_at DESC);

	CREATE TABLE IF NOT EXISTS api_tokens (
		id TEXT PRIMARY KEY,
		label TEXT NOT NULL,
		hash TEXT NOT NULL UNIQUE,            -- sha256 hex digest of the plaintext token
		scopes TEXT NOT NULL DEFAULT '[]',    -- JSON array of permission scopes; '[]' = legacy full-access
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		last_used_at DATETIME,
		revoked_at DATETIME,
		expires_at DATETIME                   -- nullable; nil = never expires
	);

	-- ALTER TABLE for upgrades from pre-v0.10 schemas. SQLite ignores
	-- the second ADD if the column already exists; the IF NOT EXISTS
	-- pattern isn't supported on ALTER, so we guard with a sub-select
	-- against sqlite_master.
	CREATE TABLE IF NOT EXISTS _migrations_done (name TEXT PRIMARY KEY);
	-- (Migration applied below in Go code; defining the marker table here.)

	-- Fast lookup by hash for every authenticated request.
	CREATE INDEX IF NOT EXISTS idx_api_tokens_hash ON api_tokens(hash);

	-- v0.25 recommendation dismissals. PK is the engine's
	-- deterministic recommendation_id hash, so dismissals correlate
	-- across re-evaluations. No FK — recommendations are computed
	-- on-demand, not stored.
	CREATE TABLE IF NOT EXISTS recommendation_dismissals (
		recommendation_id TEXT PRIMARY KEY,
		dismissed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		dismissed_by TEXT NOT NULL DEFAULT 'system',
		reason TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_rec_dismissals_dismissed_at ON recommendation_dismissals(dismissed_at);

	-- v0.28 recommendation outcomes. One row per Apply click;
	-- tracks the realized byte/dollar savings post-apply by polling
	-- the insights surface for the affected attribute's current rate.
	-- Frozen snapshot fields (title, signal, attribute_key, baseline,
	-- est_savings_at_apply) let us describe the outcome even after
	-- the engine stops producing this exact recommendation.
	CREATE TABLE IF NOT EXISTS recommendation_outcomes (
		id TEXT PRIMARY KEY,
		recommendation_id TEXT NOT NULL,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		applied_by TEXT NOT NULL DEFAULT 'system',
		title TEXT NOT NULL,
		category TEXT NOT NULL,
		signal TEXT NOT NULL DEFAULT '',
		attribute_key TEXT NOT NULL DEFAULT '',
		baseline_bytes_per_hour INTEGER NOT NULL DEFAULT 0,
		est_savings_per_month_usd_at_apply REAL NOT NULL DEFAULT 0,
		last_observed_bytes_per_hour INTEGER NOT NULL DEFAULT 0,
		last_observed_at DATETIME,
		realized_savings_per_month_usd REAL NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending'
	);
	CREATE INDEX IF NOT EXISTS idx_rec_outcomes_applied_at ON recommendation_outcomes(applied_at);
	CREATE INDEX IF NOT EXISTS idx_rec_outcomes_status ON recommendation_outcomes(status);

	-- v0.29 cost-spike events. One row per detected anomaly; open
	-- spikes have ended_at IS NULL. AttributionJSON is freeform —
	-- a tiny JSON blob captured at fire time with the top
	-- agents/attributes that drove the spike. Acknowledgement is
	-- operator-only and doesn't close the spike (the detector
	-- closes when the projection drops back below threshold).
	CREATE TABLE IF NOT EXISTS cost_spike_events (
		id TEXT PRIMARY KEY,
		started_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		ended_at DATETIME,
		severity TEXT NOT NULL DEFAULT 'warn',
		signal TEXT NOT NULL DEFAULT '',
		baseline_monthly_usd REAL NOT NULL DEFAULT 0,
		peak_monthly_usd REAL NOT NULL DEFAULT 0,
		peak_pct_above_baseline REAL NOT NULL DEFAULT 0,
		attribution_json TEXT NOT NULL DEFAULT '',
		acknowledged_at DATETIME,
		acknowledged_by TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_cost_spikes_started_at ON cost_spike_events(started_at);
	CREATE INDEX IF NOT EXISTS idx_cost_spikes_open ON cost_spike_events(ended_at) WHERE ended_at IS NULL;

	CREATE INDEX IF NOT EXISTS idx_agents_group_id ON agents(group_id);
		CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);
		CREATE INDEX IF NOT EXISTS idx_configs_agent_id ON configs(agent_id);
		CREATE INDEX IF NOT EXISTS idx_configs_group_id ON configs(group_id);
		CREATE INDEX IF NOT EXISTS idx_configs_config_hash ON configs(config_hash);
	`

	if _, err := s.db.Exec(createTables); err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}

	// Run migrations for schema changes. SQLite doesn't support
	// "ALTER TABLE ... ADD COLUMN IF NOT EXISTS", so we run each
	// migration unconditionally and swallow "duplicate column"
	// errors. This is idempotent across upgrades.
	migrations := []string{
		// v0.1: configs gain a human-readable name.
		`ALTER TABLE configs ADD COLUMN name TEXT`,
		// v0.10: api_tokens gain a scopes column. Existing tokens
		// upgrade with the default '[]' which the service interprets
		// as legacy full-access (so existing operator + automation
		// tokens keep working through the upgrade).
		`ALTER TABLE api_tokens ADD COLUMN scopes TEXT NOT NULL DEFAULT '[]'`,
		// v0.11: api_tokens gain an optional expires_at column.
		// Nullable — pre-v0.11 tokens upgrade with no expiry and
		// stay valid until explicitly revoked.
		`ALTER TABLE api_tokens ADD COLUMN expires_at DATETIME`,
		// v0.5: rollouts gain an optional notification_url for the
		// webhook-notifications feature. Missing here in earlier
		// releases meant any dev DB created before v0.5 stayed
		// non-functional — the rollout engine's List query
		// references this column and fails on every tick. Adding
		// the migration retroactively. NULL is the back-compat
		// no-webhook default.
		`ALTER TABLE rollouts ADD COLUMN notification_url TEXT`,
	}

	for _, migration := range migrations {
		if _, err := s.db.Exec(migration); err != nil {
			// Ignore errors for columns that already exist
			if !isColumnExistsError(err) {
				s.logger.Debug("Migration skipped or failed", zap.Error(err))
			}
		}
	}

	s.logger.Debug("Database migrations completed")
	return nil
}

// isColumnExistsError checks if the error is due to a column already existing
func isColumnExistsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// SQLite phrases the error as "duplicate column name: <col>" or
	// "column <col> already exists" depending on driver version.
	// Match the prefix rather than enumerating every column name.
	return strings.HasPrefix(msg, "duplicate column name") ||
		strings.Contains(msg, "already exists")
}

// Agent management
func (s *Storage) CreateAgent(ctx context.Context, agent *types.Agent) error {
	labelsJSON, _ := json.Marshal(agent.Labels)
	capabilitiesJSON, _ := json.Marshal(agent.Capabilities)

	query := `
		INSERT INTO agents (id, name, labels, status, last_seen, group_id, group_name, version, capabilities, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.ExecContext(ctx, query,
		agent.ID.String(),
		agent.Name,
		string(labelsJSON),
		string(agent.Status),
		agent.LastSeen,
		agent.GroupID,
		agent.GroupName,
		agent.Version,
		string(capabilitiesJSON),
		agent.CreatedAt,
		agent.UpdatedAt,
	)

	if err != nil {
		return fmt.Errorf("failed to create agent: %w", err)
	}

	s.logger.Debug("Created agent", zap.String("agent_id", agent.ID.String()))
	return nil
}

func (s *Storage) GetAgent(ctx context.Context, id uuid.UUID) (*types.Agent, error) {
	query := `
		SELECT id, name, labels, status, last_seen, group_id, group_name, version, capabilities, effective_config, created_at, updated_at
		FROM agents WHERE id = ?
	`

	var agent types.Agent
	var labelsJSON, capabilitiesJSON string
	var agentIDStr string
	var effectiveConfig sql.NullString

	err := s.db.QueryRowContext(ctx, query, id.String()).Scan(
		&agentIDStr,
		&agent.Name,
		&labelsJSON,
		&agent.Status,
		&agent.LastSeen,
		&agent.GroupID,
		&agent.GroupName,
		&agent.Version,
		&capabilitiesJSON,
		&effectiveConfig,
		&agent.CreatedAt,
		&agent.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get agent: %w", err)
	}

	agent.ID = id
	_ = json.Unmarshal([]byte(labelsJSON), &agent.Labels)
	_ = json.Unmarshal([]byte(capabilitiesJSON), &agent.Capabilities)
	if effectiveConfig.Valid {
		agent.EffectiveConfig = effectiveConfig.String
	}

	return &agent, nil
}

func (s *Storage) ListAgents(ctx context.Context) ([]*types.Agent, error) {
	query := `
		SELECT id, name, labels, status, last_seen, group_id, group_name, version, capabilities, effective_config, created_at, updated_at
		FROM agents ORDER BY created_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list agents: %w", err)
	}
	defer rows.Close()

	var agents []*types.Agent
	for rows.Next() {
		var agent types.Agent
		var labelsJSON, capabilitiesJSON string
		var agentIDStr string
		var effectiveConfig sql.NullString

		err := rows.Scan(
			&agentIDStr,
			&agent.Name,
			&labelsJSON,
			&agent.Status,
			&agent.LastSeen,
			&agent.GroupID,
			&agent.GroupName,
			&agent.Version,
			&capabilitiesJSON,
			&effectiveConfig,
			&agent.CreatedAt,
			&agent.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan agent: %w", err)
		}

		agent.ID, _ = uuid.Parse(agentIDStr)
		_ = json.Unmarshal([]byte(labelsJSON), &agent.Labels)
		_ = json.Unmarshal([]byte(capabilitiesJSON), &agent.Capabilities)
		if effectiveConfig.Valid {
			agent.EffectiveConfig = effectiveConfig.String
		}

		agents = append(agents, &agent)
	}

	return agents, nil
}

func (s *Storage) UpdateAgentStatus(ctx context.Context, id uuid.UUID, status types.AgentStatus) error {
	query := `UPDATE agents SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`

	result, err := s.db.ExecContext(ctx, query, string(status), id.String())
	if err != nil {
		return fmt.Errorf("failed to update agent status: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}

	s.logger.Debug("Updated agent status", zap.String("agent_id", id.String()), zap.String("status", string(status)))
	return nil
}

func (s *Storage) UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error {
	query := `UPDATE agents SET last_seen = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`

	result, err := s.db.ExecContext(ctx, query, lastSeen, id.String())
	if err != nil {
		return fmt.Errorf("failed to update agent last seen: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}

	return nil
}

func (s *Storage) UpdateAgentEffectiveConfig(ctx context.Context, id uuid.UUID, effectiveConfig string) error {
	query := `UPDATE agents SET effective_config = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`

	result, err := s.db.ExecContext(ctx, query, effectiveConfig, id.String())
	if err != nil {
		return fmt.Errorf("failed to update agent effective config: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}

	s.logger.Debug("Updated agent effective config", zap.String("agent_id", id.String()))
	return nil
}

func (s *Storage) DeleteAgent(ctx context.Context, id uuid.UUID) error {
	query := `DELETE FROM agents WHERE id = ?`

	result, err := s.db.ExecContext(ctx, query, id.String())
	if err != nil {
		return fmt.Errorf("failed to delete agent: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}

	s.logger.Debug("Deleted agent", zap.String("agent_id", id.String()))
	return nil
}

// Group management
func (s *Storage) CreateGroup(ctx context.Context, group *types.Group) error {
	labelsJSON, _ := json.Marshal(group.Labels)

	query := `
		INSERT INTO groups (id, name, labels, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`

	_, err := s.db.ExecContext(ctx, query,
		group.ID,
		group.Name,
		string(labelsJSON),
		group.CreatedAt,
		group.UpdatedAt,
	)

	if err != nil {
		return fmt.Errorf("failed to create group: %w", err)
	}

	s.logger.Debug("Created group", zap.String("group_id", group.ID))
	return nil
}

func (s *Storage) GetGroup(ctx context.Context, id string) (*types.Group, error) {
	query := `SELECT id, name, labels, created_at, updated_at FROM groups WHERE id = ?`

	var group types.Group
	var labelsJSON string

	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&group.ID,
		&group.Name,
		&labelsJSON,
		&group.CreatedAt,
		&group.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get group: %w", err)
	}

	_ = json.Unmarshal([]byte(labelsJSON), &group.Labels)
	return &group, nil
}

func (s *Storage) ListGroups(ctx context.Context) ([]*types.Group, error) {
	query := `SELECT id, name, labels, created_at, updated_at FROM groups ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups: %w", err)
	}
	defer rows.Close()

	var groups []*types.Group
	for rows.Next() {
		var group types.Group
		var labelsJSON string

		err := rows.Scan(
			&group.ID,
			&group.Name,
			&labelsJSON,
			&group.CreatedAt,
			&group.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}

		_ = json.Unmarshal([]byte(labelsJSON), &group.Labels)
		groups = append(groups, &group)
	}

	return groups, nil
}

func (s *Storage) DeleteGroup(ctx context.Context, id string) error {
	query := `DELETE FROM groups WHERE id = ?`

	result, err := s.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("failed to delete group: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("group not found: %s", id)
	}

	s.logger.Debug("Deleted group", zap.String("group_id", id))
	return nil
}

// Config management
func (s *Storage) CreateConfig(ctx context.Context, config *types.Config) error {
	query := `
		INSERT INTO configs (id, name, agent_id, group_id, config_hash, content, version, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.ExecContext(ctx, query,
		config.ID,
		config.Name,
		config.AgentID,
		config.GroupID,
		config.ConfigHash,
		config.Content,
		config.Version,
		config.CreatedAt,
	)

	if err != nil {
		return fmt.Errorf("failed to create config: %w", err)
	}

	s.logger.Debug("Created config", zap.String("config_id", config.ID))
	return nil
}

func (s *Storage) GetConfig(ctx context.Context, id string) (*types.Config, error) {
	query := `SELECT id, name, agent_id, group_id, config_hash, content, version, created_at FROM configs WHERE id = ?`

	var config types.Config
	var agentIDStr, groupIDStr sql.NullString
	var nameStr sql.NullString

	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&config.ID,
		&nameStr,
		&agentIDStr,
		&groupIDStr,
		&config.ConfigHash,
		&config.Content,
		&config.Version,
		&config.CreatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	if nameStr.Valid {
		config.Name = nameStr.String
	}
	if agentIDStr.Valid {
		agentID, _ := uuid.Parse(agentIDStr.String)
		config.AgentID = &agentID
	}
	if groupIDStr.Valid {
		config.GroupID = &groupIDStr.String
	}

	return &config, nil
}

func (s *Storage) GetLatestConfigForAgent(ctx context.Context, agentID uuid.UUID) (*types.Config, error) {
	query := `
		SELECT id, name, agent_id, group_id, config_hash, content, version, created_at
		FROM configs
		WHERE agent_id = ?
		ORDER BY version DESC, created_at DESC
		LIMIT 1
	`

	var config types.Config
	var agentIDStr, groupIDStr sql.NullString
	var nameStr sql.NullString

	err := s.db.QueryRowContext(ctx, query, agentID.String()).Scan(
		&config.ID,
		&nameStr,
		&agentIDStr,
		&groupIDStr,
		&config.ConfigHash,
		&config.Content,
		&config.Version,
		&config.CreatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get latest config for agent: %w", err)
	}

	if nameStr.Valid {
		config.Name = nameStr.String
	}
	if agentIDStr.Valid {
		agentID, _ := uuid.Parse(agentIDStr.String)
		config.AgentID = &agentID
	}
	if groupIDStr.Valid {
		config.GroupID = &groupIDStr.String
	}

	return &config, nil
}

func (s *Storage) GetLatestConfigForGroup(ctx context.Context, groupID string) (*types.Config, error) {
	query := `
		SELECT id, name, agent_id, group_id, config_hash, content, version, created_at
		FROM configs
		WHERE group_id = ?
		ORDER BY version DESC, created_at DESC
		LIMIT 1
	`

	var config types.Config
	var agentIDStr, groupIDStr sql.NullString
	var nameStr sql.NullString

	err := s.db.QueryRowContext(ctx, query, groupID).Scan(
		&config.ID,
		&nameStr,
		&agentIDStr,
		&groupIDStr,
		&config.ConfigHash,
		&config.Content,
		&config.Version,
		&config.CreatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get latest config for group: %w", err)
	}

	if nameStr.Valid {
		config.Name = nameStr.String
	}
	if agentIDStr.Valid {
		agentID, _ := uuid.Parse(agentIDStr.String)
		config.AgentID = &agentID
	}
	if groupIDStr.Valid {
		config.GroupID = &groupIDStr.String
	}

	return &config, nil
}

func (s *Storage) ListConfigs(ctx context.Context, filter types.ConfigFilter) ([]*types.Config, error) {
	query := `SELECT id, name, agent_id, group_id, config_hash, content, version, created_at FROM configs WHERE 1=1`
	args := []interface{}{}

	if filter.AgentID != nil {
		query += ` AND agent_id = ?`
		args = append(args, filter.AgentID.String())
	}

	if filter.GroupID != nil {
		query += ` AND group_id = ?`
		args = append(args, *filter.GroupID)
	}

	query += ` ORDER BY created_at DESC`

	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list configs: %w", err)
	}
	defer rows.Close()

	var configs []*types.Config
	for rows.Next() {
		var config types.Config
		var agentIDStr, groupIDStr sql.NullString
		var nameStr sql.NullString

		err := rows.Scan(
			&config.ID,
			&nameStr,
			&agentIDStr,
			&groupIDStr,
			&config.ConfigHash,
			&config.Content,
			&config.Version,
			&config.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan config: %w", err)
		}

		if nameStr.Valid {
			config.Name = nameStr.String
		}
		if agentIDStr.Valid {
			agentID, _ := uuid.Parse(agentIDStr.String)
			config.AgentID = &agentID
		}
		if groupIDStr.Valid {
			config.GroupID = &groupIDStr.String
		}

		configs = append(configs, &config)
	}

	return configs, nil
}

// Saved query management
func (s *Storage) CreateSavedQuery(ctx context.Context, query *types.SavedQuery) error {
	tagsJSON, _ := json.Marshal(query.Tags)
	stmt := `
		INSERT INTO saved_queries (id, name, description, query, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, stmt,
		query.ID,
		query.Name,
		query.Description,
		query.Query,
		string(tagsJSON),
		query.CreatedAt,
		query.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create saved query: %w", err)
	}
	s.logger.Debug("Created saved query", zap.String("saved_query_id", query.ID))
	return nil
}


func (s *Storage) GetSavedQuery(ctx context.Context, id string) (*types.SavedQuery, error) {
	stmt := `SELECT id, name, description, query, tags, created_at, updated_at FROM saved_queries WHERE id = ?`
	var sq types.SavedQuery
	var tagsJSON sql.NullString
	if err := s.db.QueryRowContext(ctx, stmt, id).Scan(&sq.ID, &sq.Name, &sq.Description, &sq.Query, &tagsJSON, &sq.CreatedAt, &sq.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get saved query: %w", err)
	}
	if tagsJSON.Valid {
		_ = json.Unmarshal([]byte(tagsJSON.String), &sq.Tags)
	}
	return &sq, nil
}

func (s *Storage) ListSavedQueries(ctx context.Context) ([]*types.SavedQuery, error) {
	stmt := `
		SELECT id, name, description, query, tags, created_at, updated_at
		FROM saved_queries
		ORDER BY updated_at DESC
	`
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("failed to list saved queries: %w", err)
	}
	defer rows.Close()

	var queries []*types.SavedQuery
	for rows.Next() {
		var sq types.SavedQuery
		var tagsJSON sql.NullString
		if err := rows.Scan(&sq.ID, &sq.Name, &sq.Description, &sq.Query, &tagsJSON, &sq.CreatedAt, &sq.UpdatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan saved query: %w", err)
		}
		if tagsJSON.Valid {
			_ = json.Unmarshal([]byte(tagsJSON.String), &sq.Tags)
		}
		queries = append(queries, &sq)
	}

	return queries, nil
}

func (s *Storage) UpdateSavedQuery(ctx context.Context, query *types.SavedQuery) error {
	tagsJSON, _ := json.Marshal(query.Tags)
	stmt := `
		UPDATE saved_queries
		SET name = ?, description = ?, query = ?, tags = ?, updated_at = ?
		WHERE id = ?
	`
	result, err := s.db.ExecContext(ctx, stmt,
		query.Name,
		query.Description,
		query.Query,
		string(tagsJSON),
		query.UpdatedAt,
		query.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update saved query: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("saved query not found: %s", query.ID)
	}
	s.logger.Debug("Updated saved query", zap.String("saved_query_id", query.ID))
	return nil
}

func (s *Storage) DeleteSavedQuery(ctx context.Context, id string) error {
	stmt := `DELETE FROM saved_queries WHERE id = ?`
	result, err := s.db.ExecContext(ctx, stmt, id)
	if err != nil {
		return fmt.Errorf("failed to delete saved query: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("saved query not found: %s", id)
	}
	s.logger.Debug("Deleted saved query", zap.String("saved_query_id", id))
	return nil
}


// Alert rule management

func (s *Storage) CreateAlertRule(ctx context.Context, rule *types.AlertRule) error {
	stmt := `
		INSERT INTO alert_rules (id, name, description, query, threshold_operator, threshold_value, interval_seconds, severity, enabled, webhook_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err := s.db.ExecContext(ctx, stmt,
		rule.ID,
		rule.Name,
		rule.Description,
		rule.Query,
		string(rule.ThresholdOperator),
		rule.ThresholdValue,
		rule.IntervalSeconds,
		string(rule.Severity),
		boolToInt(rule.Enabled),
		rule.WebhookURL,
		rule.CreatedAt,
		rule.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create alert rule: %w", err)
	}
	s.logger.Debug("Created alert rule", zap.String("rule_id", rule.ID), zap.String("name", rule.Name))
	return nil
}

func (s *Storage) GetAlertRule(ctx context.Context, id string) (*types.AlertRule, error) {
	stmt := `SELECT id, name, description, query, threshold_operator, threshold_value, interval_seconds, severity, enabled, webhook_url, created_at, updated_at FROM alert_rules WHERE id = ?`
	rule := &types.AlertRule{}
	var op, severity string
	var enabledInt int
	if err := s.db.QueryRowContext(ctx, stmt, id).Scan(
		&rule.ID, &rule.Name, &rule.Description, &rule.Query,
		&op, &rule.ThresholdValue, &rule.IntervalSeconds, &severity,
		&enabledInt, &rule.WebhookURL, &rule.CreatedAt, &rule.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get alert rule: %w", err)
	}
	rule.ThresholdOperator = types.ThresholdOperator(op)
	rule.Severity = types.AlertSeverity(severity)
	rule.Enabled = enabledInt != 0
	return rule, nil
}

func (s *Storage) ListAlertRules(ctx context.Context) ([]*types.AlertRule, error) {
	stmt := `SELECT id, name, description, query, threshold_operator, threshold_value, interval_seconds, severity, enabled, webhook_url, created_at, updated_at FROM alert_rules ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("failed to list alert rules: %w", err)
	}
	defer rows.Close()

	var rules []*types.AlertRule
	for rows.Next() {
		rule := &types.AlertRule{}
		var op, severity string
		var enabledInt int
		if err := rows.Scan(
			&rule.ID, &rule.Name, &rule.Description, &rule.Query,
			&op, &rule.ThresholdValue, &rule.IntervalSeconds, &severity,
			&enabledInt, &rule.WebhookURL, &rule.CreatedAt, &rule.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan alert rule: %w", err)
		}
		rule.ThresholdOperator = types.ThresholdOperator(op)
		rule.Severity = types.AlertSeverity(severity)
		rule.Enabled = enabledInt != 0
		rules = append(rules, rule)
	}
	return rules, nil
}

func (s *Storage) UpdateAlertRule(ctx context.Context, rule *types.AlertRule) error {
	stmt := `
		UPDATE alert_rules
		SET name = ?, description = ?, query = ?, threshold_operator = ?, threshold_value = ?,
		    interval_seconds = ?, severity = ?, enabled = ?, webhook_url = ?, updated_at = ?
		WHERE id = ?
	`
	result, err := s.db.ExecContext(ctx, stmt,
		rule.Name, rule.Description, rule.Query,
		string(rule.ThresholdOperator), rule.ThresholdValue,
		rule.IntervalSeconds, string(rule.Severity),
		boolToInt(rule.Enabled), rule.WebhookURL,
		rule.UpdatedAt, rule.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update alert rule: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("alert rule not found: %s", rule.ID)
	}
	s.logger.Debug("Updated alert rule", zap.String("rule_id", rule.ID))
	return nil
}

func (s *Storage) DeleteAlertRule(ctx context.Context, id string) error {
	stmt := `DELETE FROM alert_rules WHERE id = ?`
	result, err := s.db.ExecContext(ctx, stmt, id)
	if err != nil {
		return fmt.Errorf("failed to delete alert rule: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("alert rule not found: %s", id)
	}
	s.logger.Debug("Deleted alert rule", zap.String("rule_id", id))
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Audit log

const defaultAuditLimit = 100
const maxAuditLimit = 1000

func (s *Storage) CreateAuditEvent(ctx context.Context, e *types.AuditEvent) error {
	var payloadJSON []byte
	if e.Payload != nil {
		var err error
		payloadJSON, err = json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("failed to marshal audit payload: %w", err)
		}
	}
	stmt := `
		INSERT INTO audit_events (id, timestamp, actor, event_type, target_type, target_id, action, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = e.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, stmt,
		e.ID,
		e.Timestamp,
		e.Actor,
		e.EventType,
		e.TargetType,
		nullableString(e.TargetID),
		e.Action,
		string(payloadJSON),
		e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create audit event: %w", err)
	}
	return nil
}

func (s *Storage) ListAuditEvents(ctx context.Context, filter types.AuditEventFilter) ([]*types.AuditEvent, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}

	// Build a parameterized query. We always add a LIMIT clause; the rest
	// of the clauses are appended conditionally.
	q := "SELECT id, timestamp, actor, event_type, target_type, target_id, action, payload, created_at FROM audit_events WHERE 1=1"
	var args []any
	if filter.TargetType != "" {
		q += " AND target_type = ?"
		args = append(args, filter.TargetType)
	}
	if filter.TargetID != "" {
		q += " AND target_id = ?"
		args = append(args, filter.TargetID)
	}
	if !filter.Since.IsZero() {
		q += " AND timestamp >= ?"
		args = append(args, filter.Since)
	}
	q += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list audit events: %w", err)
	}
	defer rows.Close()

	var out []*types.AuditEvent
	for rows.Next() {
		e := &types.AuditEvent{}
		var targetID sql.NullString
		var payload sql.NullString
		if err := rows.Scan(
			&e.ID, &e.Timestamp, &e.Actor, &e.EventType, &e.TargetType,
			&targetID, &e.Action, &payload, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan audit event: %w", err)
		}
		if targetID.Valid {
			e.TargetID = targetID.String
		}
		if payload.Valid && payload.String != "" {
			_ = json.Unmarshal([]byte(payload.String), &e.Payload)
		}
		out = append(out, e)
	}
	return out, nil
}

// nullableString returns a sql.NullString that's invalid for the empty
// string. SQLite would otherwise persist "" which makes filtering harder.
func nullableString(s string) any {
	if s == "" {
		return sql.NullString{Valid: false}
	}
	return s
}

// Rollout management

func (s *Storage) CreateRollout(ctx context.Context, r *types.Rollout) error {
	stagesJSON, err := json.Marshal(r.Stages)
	if err != nil {
		return fmt.Errorf("failed to marshal rollout stages: %w", err)
	}
	criteriaJSON, err := json.Marshal(r.AbortCriteria)
	if err != nil {
		return fmt.Errorf("failed to marshal rollout abort criteria: %w", err)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	stmt := `
		INSERT INTO rollouts (id, name, group_id, target_config_id, previous_config_id, stages, abort_criteria, notification_url, state, current_stage, stage_started_at, abort_reason, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = s.db.ExecContext(ctx, stmt,
		r.ID, r.Name, r.GroupID, r.TargetConfigID,
		nullableString(r.PreviousConfigID),
		string(stagesJSON), string(criteriaJSON),
		nullableString(r.NotificationURL),
		string(r.State), r.CurrentStage,
		r.StageStartedAt, nullableString(r.AbortReason),
		r.CreatedAt, r.UpdatedAt, r.CompletedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create rollout: %w", err)
	}
	return nil
}

func (s *Storage) GetRollout(ctx context.Context, id string) (*types.Rollout, error) {
	stmt := `SELECT id, name, group_id, target_config_id, previous_config_id, stages, abort_criteria, notification_url, state, current_stage, stage_started_at, abort_reason, created_at, updated_at, completed_at FROM rollouts WHERE id = ?`
	r, err := s.scanRollout(s.db.QueryRowContext(ctx, stmt, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

func (s *Storage) ListRollouts(ctx context.Context, filter types.RolloutFilter) ([]*types.Rollout, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	q := "SELECT id, name, group_id, target_config_id, previous_config_id, stages, abort_criteria, notification_url, state, current_stage, stage_started_at, abort_reason, created_at, updated_at, completed_at FROM rollouts WHERE 1=1"
	var args []any
	if filter.GroupID != "" {
		q += " AND group_id = ?"
		args = append(args, filter.GroupID)
	}
	if filter.State != "" {
		q += " AND state = ?"
		args = append(args, string(filter.State))
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list rollouts: %w", err)
	}
	defer rows.Close()

	var out []*types.Rollout
	for rows.Next() {
		r, err := s.scanRollout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Storage) UpdateRollout(ctx context.Context, r *types.Rollout) error {
	stagesJSON, err := json.Marshal(r.Stages)
	if err != nil {
		return fmt.Errorf("failed to marshal rollout stages: %w", err)
	}
	criteriaJSON, err := json.Marshal(r.AbortCriteria)
	if err != nil {
		return fmt.Errorf("failed to marshal rollout abort criteria: %w", err)
	}
	r.UpdatedAt = time.Now().UTC()
	stmt := `
		UPDATE rollouts
		SET name = ?, group_id = ?, target_config_id = ?, previous_config_id = ?,
		    stages = ?, abort_criteria = ?, notification_url = ?,
		    state = ?, current_stage = ?,
		    stage_started_at = ?, abort_reason = ?, updated_at = ?, completed_at = ?
		WHERE id = ?
	`
	res, err := s.db.ExecContext(ctx, stmt,
		r.Name, r.GroupID, r.TargetConfigID,
		nullableString(r.PreviousConfigID),
		string(stagesJSON), string(criteriaJSON),
		nullableString(r.NotificationURL),
		string(r.State), r.CurrentStage,
		r.StageStartedAt, nullableString(r.AbortReason),
		r.UpdatedAt, r.CompletedAt,
		r.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update rollout: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("rollout not found: %s", r.ID)
	}
	return nil
}

// scanner is the minimal interface satisfied by both *sql.Row and *sql.Rows
// so scanRollout can serve both code paths.
type scanner interface {
	Scan(dest ...any) error
}

func (s *Storage) scanRollout(sc scanner) (*types.Rollout, error) {
	r := &types.Rollout{}
	var (
		previousConfigID sql.NullString
		stagesJSON       string
		criteriaJSON     string
		notificationURL  sql.NullString
		stateStr         string
		stageStartedAt   sql.NullTime
		abortReason      sql.NullString
		completedAt      sql.NullTime
	)
	if err := sc.Scan(
		&r.ID, &r.Name, &r.GroupID, &r.TargetConfigID,
		&previousConfigID, &stagesJSON, &criteriaJSON, &notificationURL,
		&stateStr, &r.CurrentStage,
		&stageStartedAt, &abortReason,
		&r.CreatedAt, &r.UpdatedAt, &completedAt,
	); err != nil {
		return nil, err
	}
	if previousConfigID.Valid {
		r.PreviousConfigID = previousConfigID.String
	}
	if notificationURL.Valid {
		r.NotificationURL = notificationURL.String
	}
	if err := json.Unmarshal([]byte(stagesJSON), &r.Stages); err != nil {
		return nil, fmt.Errorf("failed to unmarshal rollout stages: %w", err)
	}
	if err := json.Unmarshal([]byte(criteriaJSON), &r.AbortCriteria); err != nil {
		return nil, fmt.Errorf("failed to unmarshal abort criteria: %w", err)
	}
	r.State = types.RolloutState(stateStr)
	if stageStartedAt.Valid {
		r.StageStartedAt = &stageStartedAt.Time
	}
	if abortReason.Valid {
		r.AbortReason = abortReason.String
	}
	if completedAt.Valid {
		r.CompletedAt = &completedAt.Time
	}
	return r, nil
}

// API token management
//
// Plaintext token values are never persisted. The middleware hashes the
// incoming bearer with sha256 and looks up the row by hash; if a row
// exists and RevokedAt is null, the request is authenticated.

func (s *Storage) CreateAPIToken(ctx context.Context, t *types.APIToken) error {
	scopesJSON, err := marshalScopes(t.Scopes)
	if err != nil {
		return err
	}
	stmt := `
		INSERT INTO api_tokens (id, label, hash, scopes, created_at, last_used_at, revoked_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`
	if _, err := s.db.ExecContext(ctx, stmt,
		t.ID, t.Label, t.Hash, scopesJSON, t.CreatedAt, t.LastUsedAt, t.RevokedAt, t.ExpiresAt); err != nil {
		return fmt.Errorf("failed to create api token: %w", err)
	}
	return nil
}

func (s *Storage) GetAPITokenByHash(ctx context.Context, hash string) (*types.APIToken, error) {
	stmt := `SELECT id, label, hash, scopes, created_at, last_used_at, revoked_at, expires_at FROM api_tokens WHERE hash = ?`
	t := &types.APIToken{}
	var (
		scopesJSON                       string
		lastUsedAt, revokedAt, expiresAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, stmt, hash).Scan(
		&t.ID, &t.Label, &t.Hash, &scopesJSON, &t.CreatedAt, &lastUsedAt, &revokedAt, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get api token: %w", err)
	}
	t.Scopes = unmarshalScopes(scopesJSON)
	if lastUsedAt.Valid {
		t.LastUsedAt = &lastUsedAt.Time
	}
	if revokedAt.Valid {
		t.RevokedAt = &revokedAt.Time
	}
	if expiresAt.Valid {
		t.ExpiresAt = &expiresAt.Time
	}
	return t, nil
}

// ListAPITokens returns every issued token, revoked or not, newest first.
// Revoked tokens stay in the list so the UI can show a full history and
// audit consumers can still resolve old token IDs to labels.
func (s *Storage) ListAPITokens(ctx context.Context) ([]*types.APIToken, error) {
	stmt := `
		SELECT id, label, hash, scopes, created_at, last_used_at, revoked_at, expires_at
		FROM api_tokens
		ORDER BY created_at DESC
	`
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("failed to list api tokens: %w", err)
	}
	defer rows.Close()
	var out []*types.APIToken
	for rows.Next() {
		t := &types.APIToken{}
		var (
			scopesJSON                       string
			lastUsedAt, revokedAt, expiresAt sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.Label, &t.Hash, &scopesJSON, &t.CreatedAt, &lastUsedAt, &revokedAt, &expiresAt); err != nil {
			return nil, err
		}
		t.Scopes = unmarshalScopes(scopesJSON)
		if lastUsedAt.Valid {
			t.LastUsedAt = &lastUsedAt.Time
		}
		if revokedAt.Valid {
			t.RevokedAt = &revokedAt.Time
		}
		if expiresAt.Valid {
			t.ExpiresAt = &expiresAt.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// marshalScopes serializes the scope list to the JSON-array form
// stored in the scopes column. Nil and empty both become '[]' so the
// SELECT path can rely on a non-null body.
func marshalScopes(s []string) (string, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal scopes: %w", err)
	}
	return string(b), nil
}

// unmarshalScopes returns the scope list for a stored token. Empty
// arrays decode as nil so callers can distinguish "no scopes recorded"
// from "scopes recorded but empty" — both currently mean legacy
// full-access for the service layer, but keeping nil keeps the
// JSON-marshaled response shape stable when the column is missing.
func unmarshalScopes(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// UpdateAPITokenLastUsed touches the last_used_at column. Best-effort:
// the middleware fires this asynchronously and doesn't fail requests on
// error. Concurrent-update races resolve as "newest write wins", which
// is fine since the column is only used for display.
func (s *Storage) UpdateAPITokenLastUsed(ctx context.Context, id string, at time.Time) error {
	stmt := `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`
	_, err := s.db.ExecContext(ctx, stmt, at, id)
	if err != nil {
		return fmt.Errorf("failed to update api token last_used_at: %w", err)
	}
	return nil
}

// RevokeAPIToken sets revoked_at if it's null. Already-revoked tokens
// keep their original revoked_at — there's no point re-stamping a
// revocation that already happened.
func (s *Storage) RevokeAPIToken(ctx context.Context, id string, at time.Time) error {
	stmt := `UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`
	res, err := s.db.ExecContext(ctx, stmt, at, id)
	if err != nil {
		return fmt.Errorf("failed to revoke api token: %w", err)
	}
	// We don't return an error if no row was affected — calling revoke
	// on an already-revoked or missing token is idempotent from the
	// service's perspective. The service layer enforces the "not found"
	// distinction by doing a List/Get before the revoke if needed.
	_ = res
	return nil
}

// ----------------------------------------------------------------
// Recommendation dismissals (v0.25)
// ----------------------------------------------------------------
//
// The dismissals table is a tiny lookup the recommendations engine
// consults at the tail end of every Evaluate to filter out advice
// the operator has explicitly hidden. Inserts use ON CONFLICT
// REPLACE so a second dismissal (perhaps with a new reason) just
// updates the row; restore is a plain DELETE.

// DismissRecommendation inserts or updates a dismissal row.
// Repeat dismissals just refresh dismissed_at + reason.
func (s *Storage) DismissRecommendation(ctx context.Context, d *types.RecommendationDismissal) error {
	if d == nil || d.RecommendationID == "" {
		return fmt.Errorf("recommendation_id required")
	}
	when := d.DismissedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}
	by := d.DismissedBy
	if by == "" {
		by = "system"
	}
	stmt := `INSERT INTO recommendation_dismissals (recommendation_id, dismissed_at, dismissed_by, reason)
	         VALUES (?, ?, ?, ?)
	         ON CONFLICT(recommendation_id) DO UPDATE SET
	             dismissed_at = excluded.dismissed_at,
	             dismissed_by = excluded.dismissed_by,
	             reason       = excluded.reason`
	if _, err := s.db.ExecContext(ctx, stmt, d.RecommendationID, when, by, d.Reason); err != nil {
		return fmt.Errorf("failed to dismiss recommendation: %w", err)
	}
	return nil
}

// RestoreRecommendation removes the dismissal row. Idempotent —
// restoring an already-restored (or never-dismissed) ID is a no-op.
func (s *Storage) RestoreRecommendation(ctx context.Context, recommendationID string) error {
	if recommendationID == "" {
		return fmt.Errorf("recommendation_id required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM recommendation_dismissals WHERE recommendation_id = ?`,
		recommendationID); err != nil {
		return fmt.Errorf("failed to restore recommendation: %w", err)
	}
	return nil
}

// IsRecommendationDismissed is the hot path — the engine calls it
// once per generated recommendation. Tiny indexed PK lookup, so
// no caching needed at this layer.
func (s *Storage) IsRecommendationDismissed(ctx context.Context, recommendationID string) (bool, error) {
	if recommendationID == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recommendation_dismissals WHERE recommendation_id = ?`,
		recommendationID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("failed to check dismissal: %w", err)
	}
	return n > 0, nil
}

// ListRecommendationDismissals returns the full set, newest first.
// Cheap because operators only ever accumulate dozens of these in
// practice — not paginated for v0.25.
func (s *Storage) ListRecommendationDismissals(ctx context.Context) ([]*types.RecommendationDismissal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT recommendation_id, dismissed_at, dismissed_by, COALESCE(reason, '')
		 FROM recommendation_dismissals
		 ORDER BY dismissed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("failed to list dismissals: %w", err)
	}
	defer rows.Close()
	out := []*types.RecommendationDismissal{}
	for rows.Next() {
		d := &types.RecommendationDismissal{}
		if err := rows.Scan(&d.RecommendationID, &d.DismissedAt, &d.DismissedBy, &d.Reason); err != nil {
			return nil, fmt.Errorf("failed to scan dismissal row: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ----------------------------------------------------------------
// Recommendation outcomes (v0.28)
// ----------------------------------------------------------------

// CreateRecommendationOutcome inserts a new row. Generated on every
// Apply click. ID is supplied by the caller (typically a uuid) so
// the caller can immediately reference the row in the response.
func (s *Storage) CreateRecommendationOutcome(ctx context.Context, o *types.RecommendationOutcome) error {
	if o == nil || o.ID == "" || o.RecommendationID == "" {
		return fmt.Errorf("id + recommendation_id required")
	}
	if o.AppliedAt.IsZero() {
		o.AppliedAt = time.Now().UTC()
	}
	if o.Status == "" {
		o.Status = "pending"
	}
	if o.AppliedBy == "" {
		o.AppliedBy = "system"
	}
	stmt := `INSERT INTO recommendation_outcomes
		(id, recommendation_id, applied_at, applied_by, title, category,
		 signal, attribute_key, baseline_bytes_per_hour,
		 est_savings_per_month_usd_at_apply, last_observed_bytes_per_hour,
		 last_observed_at, realized_savings_per_month_usd, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	var lastObs interface{}
	if !o.LastObservedAt.IsZero() {
		lastObs = o.LastObservedAt
	}
	_, err := s.db.ExecContext(ctx, stmt,
		o.ID, o.RecommendationID, o.AppliedAt, o.AppliedBy, o.Title, o.Category,
		o.Signal, o.AttributeKey, o.BaselineBytesPerHour,
		o.EstSavingsPerMonthUSDAtApply, o.LastObservedBytesPerHour,
		lastObs, o.RealizedSavingsPerMonthUSD, o.Status)
	if err != nil {
		return fmt.Errorf("failed to create recommendation outcome: %w", err)
	}
	return nil
}

// UpdateRecommendationOutcome refreshes the running observation
// fields. The periodic computation reads each outcome, queries the
// insights surface for the current byte rate of the affected
// attribute, and writes back. Only the observation columns are
// updated (status, last_observed_*, realized_savings); the frozen
// snapshot fields stay immutable.
func (s *Storage) UpdateRecommendationOutcome(ctx context.Context, o *types.RecommendationOutcome) error {
	if o == nil || o.ID == "" {
		return fmt.Errorf("id required")
	}
	stmt := `UPDATE recommendation_outcomes SET
		last_observed_bytes_per_hour = ?,
		last_observed_at = ?,
		realized_savings_per_month_usd = ?,
		status = ?
		WHERE id = ?`
	_, err := s.db.ExecContext(ctx, stmt,
		o.LastObservedBytesPerHour, o.LastObservedAt,
		o.RealizedSavingsPerMonthUSD, o.Status, o.ID)
	if err != nil {
		return fmt.Errorf("failed to update outcome: %w", err)
	}
	return nil
}

// ListRecommendationOutcomes returns every recorded outcome,
// newest applies first. Small table in practice; no pagination
// concern at the v0.28 scale.
func (s *Storage) ListRecommendationOutcomes(ctx context.Context) ([]*types.RecommendationOutcome, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, recommendation_id, applied_at, applied_by, title, category,
		        signal, attribute_key, baseline_bytes_per_hour,
		        est_savings_per_month_usd_at_apply, last_observed_bytes_per_hour,
		        last_observed_at, realized_savings_per_month_usd, status
		 FROM recommendation_outcomes
		 ORDER BY applied_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("failed to list outcomes: %w", err)
	}
	defer rows.Close()
	out := []*types.RecommendationOutcome{}
	for rows.Next() {
		o := &types.RecommendationOutcome{}
		// last_observed_at is nullable; the mattn/sqlite driver
		// hands it back as a *time.Time-friendly nullable when we
		// use sql.NullTime. Earlier I tried COALESCE to applied_at
		// to dodge the null, but SQLite's loose typing returned a
		// string from COALESCE that the driver couldn't decode.
		var lastObs sql.NullTime
		if err := rows.Scan(&o.ID, &o.RecommendationID, &o.AppliedAt, &o.AppliedBy,
			&o.Title, &o.Category, &o.Signal, &o.AttributeKey,
			&o.BaselineBytesPerHour, &o.EstSavingsPerMonthUSDAtApply,
			&o.LastObservedBytesPerHour, &lastObs,
			&o.RealizedSavingsPerMonthUSD, &o.Status); err != nil {
			return nil, fmt.Errorf("failed to scan outcome row: %w", err)
		}
		if lastObs.Valid {
			o.LastObservedAt = lastObs.Time
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ===================================================================
// v0.29 cost-spike events
// ===================================================================

// CreateCostSpikeEvent inserts a new spike. The detector calls this
// only when crossing from baseline-normal to over-threshold;
// in-progress severity escalations update the existing row instead.
func (s *Storage) CreateCostSpikeEvent(ctx context.Context, e *types.CostSpikeEvent) error {
	if e == nil || e.ID == "" {
		return fmt.Errorf("id required")
	}
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now().UTC()
	}
	if e.Severity == "" {
		e.Severity = "warn"
	}
	stmt := `INSERT INTO cost_spike_events
		(id, started_at, ended_at, severity, signal,
		 baseline_monthly_usd, peak_monthly_usd, peak_pct_above_baseline,
		 attribution_json, acknowledged_at, acknowledged_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, stmt,
		e.ID, e.StartedAt.UTC(), nullableTime(e.EndedAt),
		e.Severity, e.Signal, e.BaselineMonthlyUSD, e.PeakMonthlyUSD,
		e.PeakPctAboveBaseline, e.AttributionJSON,
		nullableTime(e.AcknowledgedAt), e.AcknowledgedBy)
	if err != nil {
		return fmt.Errorf("failed to create cost spike event: %w", err)
	}
	return nil
}

// UpdateCostSpikeEvent overwrites the mutable fields of an existing
// spike. Used to bump the peak, close the spike (set EndedAt), or
// record an acknowledgement.
func (s *Storage) UpdateCostSpikeEvent(ctx context.Context, e *types.CostSpikeEvent) error {
	if e == nil || e.ID == "" {
		return fmt.Errorf("id required")
	}
	stmt := `UPDATE cost_spike_events SET
		ended_at = ?, severity = ?, signal = ?,
		baseline_monthly_usd = ?, peak_monthly_usd = ?,
		peak_pct_above_baseline = ?, attribution_json = ?,
		acknowledged_at = ?, acknowledged_by = ?
		WHERE id = ?`
	_, err := s.db.ExecContext(ctx, stmt,
		nullableTime(e.EndedAt), e.Severity, e.Signal,
		e.BaselineMonthlyUSD, e.PeakMonthlyUSD, e.PeakPctAboveBaseline,
		e.AttributionJSON, nullableTime(e.AcknowledgedAt),
		e.AcknowledgedBy, e.ID)
	if err != nil {
		return fmt.Errorf("failed to update cost spike event: %w", err)
	}
	return nil
}

// GetCostSpikeEvent returns one spike by id, or nil if not found.
func (s *Storage) GetCostSpikeEvent(ctx context.Context, id string) (*types.CostSpikeEvent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, started_at, ended_at, severity, signal,
		       baseline_monthly_usd, peak_monthly_usd, peak_pct_above_baseline,
		       attribution_json, acknowledged_at, acknowledged_by
		FROM cost_spike_events WHERE id = ?`, id)
	e := &types.CostSpikeEvent{}
	var endedAt, ackAt sql.NullTime
	if err := row.Scan(&e.ID, &e.StartedAt, &endedAt, &e.Severity, &e.Signal,
		&e.BaselineMonthlyUSD, &e.PeakMonthlyUSD, &e.PeakPctAboveBaseline,
		&e.AttributionJSON, &ackAt, &e.AcknowledgedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get cost spike event: %w", err)
	}
	if endedAt.Valid {
		e.EndedAt = &endedAt.Time
	}
	if ackAt.Valid {
		e.AcknowledgedAt = &ackAt.Time
	}
	return e, nil
}

// ListCostSpikeEvents returns events newest-first. Filter.Status
// scopes to open/closed/all (default all).
func (s *Storage) ListCostSpikeEvents(ctx context.Context, filter types.CostSpikeFilter) ([]*types.CostSpikeEvent, error) {
	q := `SELECT id, started_at, ended_at, severity, signal,
		       baseline_monthly_usd, peak_monthly_usd, peak_pct_above_baseline,
		       attribution_json, acknowledged_at, acknowledged_by
		FROM cost_spike_events`
	switch filter.Status {
	case "open":
		q += " WHERE ended_at IS NULL"
	case "closed":
		q += " WHERE ended_at IS NOT NULL"
	}
	q += " ORDER BY started_at DESC"
	if filter.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("failed to list cost spike events: %w", err)
	}
	defer rows.Close()
	var out []*types.CostSpikeEvent
	for rows.Next() {
		e := &types.CostSpikeEvent{}
		var endedAt, ackAt sql.NullTime
		if err := rows.Scan(&e.ID, &e.StartedAt, &endedAt, &e.Severity, &e.Signal,
			&e.BaselineMonthlyUSD, &e.PeakMonthlyUSD, &e.PeakPctAboveBaseline,
			&e.AttributionJSON, &ackAt, &e.AcknowledgedBy); err != nil {
			return nil, fmt.Errorf("failed to scan cost spike event: %w", err)
		}
		if endedAt.Valid {
			e.EndedAt = &endedAt.Time
		}
		if ackAt.Valid {
			e.AcknowledgedAt = &ackAt.Time
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestOpenCostSpike returns the newest spike with no ended_at,
// or nil if none. The detector uses it to decide append-vs-create.
func (s *Storage) LatestOpenCostSpike(ctx context.Context) (*types.CostSpikeEvent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, started_at, ended_at, severity, signal,
		       baseline_monthly_usd, peak_monthly_usd, peak_pct_above_baseline,
		       attribution_json, acknowledged_at, acknowledged_by
		FROM cost_spike_events
		WHERE ended_at IS NULL
		ORDER BY started_at DESC LIMIT 1`)
	e := &types.CostSpikeEvent{}
	var endedAt, ackAt sql.NullTime
	if err := row.Scan(&e.ID, &e.StartedAt, &endedAt, &e.Severity, &e.Signal,
		&e.BaselineMonthlyUSD, &e.PeakMonthlyUSD, &e.PeakPctAboveBaseline,
		&e.AttributionJSON, &ackAt, &e.AcknowledgedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query latest open cost spike: %w", err)
	}
	if endedAt.Valid {
		e.EndedAt = &endedAt.Time
	}
	if ackAt.Valid {
		e.AcknowledgedAt = &ackAt.Time
	}
	return e, nil
}

// nullableTime converts a *time.Time to sql.NullTime for INSERT/UPDATE
// of nullable DATETIME columns.
func nullableTime(t *time.Time) sql.NullTime {
	if t == nil || t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t.UTC(), Valid: true}
}

// Close closes the database connection
func (s *Storage) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("failed to close database: %w", err)
	}
	s.logger.Info("SQLite storage closed")
	return nil
}
