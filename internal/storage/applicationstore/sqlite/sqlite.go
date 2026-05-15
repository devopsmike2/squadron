package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

	CREATE INDEX IF NOT EXISTS idx_agents_group_id ON agents(group_id);
		CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);
		CREATE INDEX IF NOT EXISTS idx_configs_agent_id ON configs(agent_id);
		CREATE INDEX IF NOT EXISTS idx_configs_group_id ON configs(group_id);
		CREATE INDEX IF NOT EXISTS idx_configs_config_hash ON configs(config_hash);
	`

	if _, err := s.db.Exec(createTables); err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}

	// Run migrations for schema changes
	migrations := []string{
		// Add name column to configs table if it doesn't exist
		`ALTER TABLE configs ADD COLUMN name TEXT`,
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
	return err != nil && (err.Error() == "duplicate column name: name" ||
		err.Error() == "column name already exists")
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

// Close closes the database connection
func (s *Storage) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("failed to close database: %w", err)
	}
	s.logger.Info("SQLite storage closed")
	return nil
}
