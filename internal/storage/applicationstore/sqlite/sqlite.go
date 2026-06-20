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
			discovery_source TEXT NOT NULL DEFAULT 'opamp',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			labels TEXT,
			-- v0.48: when 1, every rollout to this group is forced
			-- into pending_approval at create time, regardless of
			-- what the requester sets on the rollout input.
			require_approval INTEGER NOT NULL DEFAULT 0,
			-- v0.49: JSON-serialized []changewindow.Window. Empty
			-- array means no blackouts; otherwise the engine
			-- refuses to advance rollouts to this group while
			-- any window is active.
			change_windows TEXT NOT NULL DEFAULT '[]',
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

	-- Expected agents (v0.32 inventory reconciliation). One row per
	-- (hostname) tracked by some CI/CD pipeline. Hostname is the
	-- natural key — CI knows hostnames, not OpAMP-discovered UUIDs.
	-- labels_json is the standard map[string]string serialization.
	CREATE TABLE IF NOT EXISTS expected_agents (
		hostname TEXT PRIMARY KEY,
		labels_json TEXT NOT NULL DEFAULT '{}',
		source TEXT NOT NULL DEFAULT '',
		expected_since DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		notes TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_expected_agents_source ON expected_agents(source);

	-- Deploy targets (v0.34 GitHub Actions integration). encrypted_credential
	-- holds the PAT in nonce(24)||ciphertext form; the deploy package
	-- never persists plaintext. default_inputs is JSON map[string]string.
	CREATE TABLE IF NOT EXISTS deploy_targets (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		provider TEXT NOT NULL DEFAULT 'github',
		github_owner TEXT NOT NULL DEFAULT '',
		github_repo TEXT NOT NULL DEFAULT '',
		github_workflow TEXT NOT NULL DEFAULT '',
		github_branch TEXT NOT NULL DEFAULT 'main',
		encrypted_credential BLOB,
		default_inputs_json TEXT NOT NULL DEFAULT '{}',
		config_id TEXT NOT NULL DEFAULT '',
		inventory_path TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	-- Deploy runs. github_run_id starts at 0 (we resolve it after the
	-- first poll because workflow_dispatch returns 204 with no id).
	-- expected_hosts_json is the set we register into expected_agents
	-- on success — closes the v0.32 inventory loop.
	CREATE TABLE IF NOT EXISTS deploy_runs (
		id TEXT PRIMARY KEY,
		target_id TEXT NOT NULL,
		requested_by TEXT NOT NULL DEFAULT '',
		requested_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		inputs_json TEXT NOT NULL DEFAULT '{}',
		github_run_id INTEGER NOT NULL DEFAULT 0,
		github_run_url TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'queued',
		conclusion TEXT NOT NULL DEFAULT '',
		completed_at DATETIME,
		expected_hosts_json TEXT NOT NULL DEFAULT '[]',
		verification_state TEXT NOT NULL DEFAULT '',
		verified_at DATETIME,
		notes TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_deploy_runs_target_time ON deploy_runs(target_id, requested_at);
	CREATE INDEX IF NOT EXISTS idx_deploy_runs_status ON deploy_runs(status);

	CREATE INDEX IF NOT EXISTS idx_agents_group_id ON agents(group_id);
		CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);
		CREATE INDEX IF NOT EXISTS idx_configs_agent_id ON configs(agent_id);
		CREATE INDEX IF NOT EXISTS idx_configs_group_id ON configs(group_id);
		CREATE INDEX IF NOT EXISTS idx_configs_config_hash ON configs(config_hash);

		-- SIEM destinations (v0.50). secret holds nonce(24)||ciphertext;
		-- the siem package owns encryption. event_type_prefixes_json is
		-- a JSON array of string prefixes; empty/null means forward all.
		CREATE TABLE IF NOT EXISTS siem_destinations (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			url TEXT NOT NULL,
			secret BLOB,
			enabled INTEGER NOT NULL DEFAULT 1,
			event_type_prefixes_json TEXT NOT NULL DEFAULT '[]',
			last_event_sent_at DATETIME,
			last_error TEXT,
			last_error_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
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
		// v0.34.1: deploy_targets gain an optional inventory_path
		// that points at an Ansible inventory file inside the
		// target repo. When set, Squadron auto-derives the
		// expected-host list from the file at trigger time. Empty
		// is the back-compat default (manual host entry).
		`ALTER TABLE deploy_targets ADD COLUMN inventory_path TEXT NOT NULL DEFAULT ''`,
		// v0.36.0: agents gain a discovery_source column to
		// distinguish OpAMP-managed agents from telemetry-only
		// agents discovered via the OTLP receiver. "opamp" is the
		// back-compat default — every pre-v0.36 agent was OpAMP.
		`ALTER TABLE agents ADD COLUMN discovery_source TEXT NOT NULL DEFAULT 'opamp'`,
		// v0.47.0: rollouts gain approval-workflow columns. When
		// require_approval = 1 the engine refuses to advance until
		// approved_by is set (which the two-person rule enforces
		// can't equal requested_by). All defaults are NULL / 0 so
		// pre-v0.47 rollouts still work — no approval required for
		// rollouts created on older Squadron versions.
		`ALTER TABLE rollouts ADD COLUMN require_approval INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE rollouts ADD COLUMN requested_by TEXT`,
		`ALTER TABLE rollouts ADD COLUMN approved_by TEXT`,
		`ALTER TABLE rollouts ADD COLUMN approved_at DATETIME`,
		`ALTER TABLE rollouts ADD COLUMN rejected_by TEXT`,
		`ALTER TABLE rollouts ADD COLUMN rejected_at DATETIME`,
		`ALTER TABLE rollouts ADD COLUMN approval_notes TEXT`,
		// v0.49.0: rollouts get last_blackout_reason / at columns so
		// the engine can record why advancement was skipped (active
		// change window) and the UI can render a badge. Both
		// nullable — empty when not in a blackout.
		`ALTER TABLE rollouts ADD COLUMN last_blackout_reason TEXT`,
		`ALTER TABLE rollouts ADD COLUMN last_blackout_at DATETIME`,
		// v0.48.0: groups gain a require_approval policy column.
		// When set to 1, every rollout created against the group
		// is forced into pending_approval regardless of what the
		// requester set on the rollout input — this is the
		// compliance control that turns v0.47's checkbox into
		// enforced policy. Default 0 so existing groups carry
		// forward unchanged.
		`ALTER TABLE groups ADD COLUMN require_approval INTEGER NOT NULL DEFAULT 0`,
		// v0.49.0: groups gain a change_windows column for recurring
		// blackout periods (peak demand, storm response, quarterly
		// freezes). Stored as a JSON-serialized []changewindow.Window
		// since the operator manages the list as one unit and we
		// never need to query "which groups have a window active
		// right now" — always the other direction.
		`ALTER TABLE groups ADD COLUMN change_windows TEXT NOT NULL DEFAULT '[]'`,
		// v0.51.0: agents gain a tombstone column. DeleteAgent flips
		// it to a timestamp instead of removing the row; ListAgents
		// hides tombstoned rows but the audit trail (agent.created,
		// agent.config_pushed, agent.decommissioned) still resolves
		// by ID. This is the CIP-007-6 R4.3 / R4.4 evidence path.
		`ALTER TABLE agents ADD COLUMN deleted_at DATETIME`,
		// v0.53: rollouts gain proposal provenance columns. Every
		// rollout is conceptually a proposal; these capture who or
		// what originated it. Default proposed_by to 'operator' so
		// existing rows carry forward with the right semantics.
		// proposal_reasoning is the natural-language justification
		// (used by AI-originated proposals). evidence_refs is a
		// JSON array of RolloutEvidenceRef objects pointing at the
		// alerts, metrics, configlint findings, or recommendations
		// that informed the proposal. See Squadron Move 1 in
		// docs/roadmap-post-v0.52.md.
		`ALTER TABLE rollouts ADD COLUMN proposed_by TEXT NOT NULL DEFAULT 'operator'`,
		`ALTER TABLE rollouts ADD COLUMN proposal_reasoning TEXT`,
		`ALTER TABLE rollouts ADD COLUMN evidence_refs TEXT`,
		// v0.53 Move 2: action runner tables. Two tables, both
		// indexed for the access patterns the UI and dispatch loop
		// actually use. Runners are addressed by runner_id (the
		// fingerprint Squadron pins at enrollment). Action requests
		// are listed by proposal_id (the UI shows every request for
		// a given proposal) and by status (the dispatch loop sweeps
		// pending requests).
		`CREATE TABLE IF NOT EXISTS action_runner_registrations (
			runner_id TEXT PRIMARY KEY,
			hostname TEXT NOT NULL,
			public_key_pem TEXT NOT NULL,
			capabilities_json TEXT NOT NULL,
			registered_at DATETIME NOT NULL,
			last_seen_at DATETIME NOT NULL,
			revoked_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS action_requests (
			id TEXT PRIMARY KEY,
			proposal_id TEXT,
			runner_id TEXT NOT NULL,
			action_type TEXT NOT NULL,
			parameters_json TEXT NOT NULL,
			signature TEXT NOT NULL,
			phase TEXT NOT NULL,
			status TEXT NOT NULL,
			denied_for TEXT,
			dry_run_output_json TEXT,
			execution_output_json TEXT,
			issued_at DATETIME NOT NULL,
			expires_at DATETIME NOT NULL,
			started_at DATETIME,
			completed_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_action_requests_proposal ON action_requests(proposal_id)`,
		`CREATE INDEX IF NOT EXISTS idx_action_requests_runner ON action_requests(runner_id)`,
		`CREATE INDEX IF NOT EXISTS idx_action_requests_status ON action_requests(status)`,

		// SQ-3 incident drafts. One draft per action by default;
		// look-ups by action_request_id are the bridge dedup path,
		// look-ups by status power the UI inbox view. The body and
		// the structured draft JSON live in the same row so the
		// edit-and-publish UI does not have to JOIN.
		`CREATE TABLE IF NOT EXISTS incident_drafts (
			id TEXT PRIMARY KEY,
			action_request_id TEXT,
			rollout_id TEXT,
			status TEXT NOT NULL,
			title TEXT NOT NULL,
			body_markdown TEXT NOT NULL,
			draft_content_json TEXT,
			provider TEXT,
			external_id TEXT,
			external_url TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_incident_drafts_action ON incident_drafts(action_request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_incident_drafts_rollout ON incident_drafts(rollout_id)`,
		`CREATE INDEX IF NOT EXISTS idx_incident_drafts_status ON incident_drafts(status)`,

		// v0.57 — cached AI explanation surface on audit_events. Three
		// nullable columns adjacent to the row so a List query gets the
		// explanation in one read without a JOIN. We do not index any of
		// these because the access pattern is always "explain by id"
		// after the operator clicks a specific row.
		`ALTER TABLE audit_events ADD COLUMN ai_explanation TEXT`,
		`ALTER TABLE audit_events ADD COLUMN ai_explanation_model TEXT`,
		`ALTER TABLE audit_events ADD COLUMN ai_explanation_generated_at DATETIME`,

		// v0.60 — operator initiated rollback chain. When this rollout
		// was created by clicking "Roll back" on a previous rollout,
		// the column carries the source rollout's ID. NULL on every
		// existing row and every fresh rollout from operator / AI
		// proposer flows; only the rollback handler sets it.
		`ALTER TABLE rollouts ADD COLUMN rolled_back_from_id TEXT`,

		// v0.61 — per group policy: force approval on rollback
		// rollouts independently of require_approval. Default 0 so
		// existing groups carry forward unchanged.
		`ALTER TABLE groups ADD COLUMN require_approval_for_rollback INTEGER NOT NULL DEFAULT 0`,

		// v0.69 — multi step plans. PlanID groups rollouts that
		// belong to one approved plan; PlanStepIndex orders them.
		// Both NULL/0 on every existing row preserves backwards
		// compatibility — a standalone rollout has empty PlanID
		// and the engine treats it exactly as before.
		`ALTER TABLE rollouts ADD COLUMN plan_id TEXT`,
		`ALTER TABLE rollouts ADD COLUMN plan_step_index INTEGER NOT NULL DEFAULT 0`,
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

	source := agent.DiscoverySource
	if source == "" {
		source = "opamp"
	}
	query := `
		INSERT INTO agents (id, name, labels, status, last_seen, group_id, group_name, version, capabilities, discovery_source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		source,
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
	// v0.51 — tombstoned rows are excluded from the operational
	// view. Audit events keyed by ID still resolve via the
	// audit_events table.
	query := `
		SELECT id, name, labels, status, last_seen, group_id, group_name, version, capabilities, effective_config, discovery_source, created_at, updated_at
		FROM agents WHERE id = ? AND deleted_at IS NULL
	`

	var agent types.Agent
	var labelsJSON, capabilitiesJSON string
	var agentIDStr string
	var effectiveConfig sql.NullString
	var discoverySource sql.NullString

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
		&discoverySource,
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
	if discoverySource.Valid {
		agent.DiscoverySource = discoverySource.String
	}

	return &agent, nil
}

func (s *Storage) ListAgents(ctx context.Context) ([]*types.Agent, error) {
	// v0.51 — exclude tombstoned (soft-deleted) agents by default.
	// The audit trail for the agent persists indefinitely; the
	// operational view shows only live agents.
	query := `
		SELECT id, name, labels, status, last_seen, group_id, group_name, version, capabilities, effective_config, discovery_source, created_at, updated_at
		FROM agents WHERE deleted_at IS NULL ORDER BY created_at DESC
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
		var discoverySource sql.NullString

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
			&discoverySource,
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
		if discoverySource.Valid {
			agent.DiscoverySource = discoverySource.String
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
	// v0.51 — soft delete. UPDATE the tombstone column instead of
	// DELETE so the row remains for audit history (CIP-007-6 R4.3).
	// The agent.decommissioned audit event carries the operator's
	// identity and timing; the row carries the tombstone marker.
	query := `UPDATE agents SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, query, now, now, id.String())
	if err != nil {
		return fmt.Errorf("failed to delete agent: %w", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}
	return nil
}

// hardDeleteAgentLegacy kept around as a method that we are no longer
// wiring through the public interface. If a future operator wants
// real deletion for storage hygiene, expose this as a separate
// purge call gated by an admin scope.
func (s *Storage) hardDeleteAgentLegacy(ctx context.Context, id uuid.UUID) error {
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
	cw := group.ChangeWindowsJSON
	if cw == "" {
		cw = "[]"
	}

	query := `
		INSERT INTO groups (id, name, labels, require_approval, require_approval_for_rollback, change_windows, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.ExecContext(ctx, query,
		group.ID,
		group.Name,
		string(labelsJSON),
		boolToInt(group.RequireApproval),
		boolToInt(group.RequireApprovalForRollback),
		cw,
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
	query := `SELECT id, name, labels, require_approval, require_approval_for_rollback, change_windows, created_at, updated_at FROM groups WHERE id = ?`

	var group types.Group
	var labelsJSON string
	var requireApproval int
	var requireApprovalForRollback int

	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&group.ID,
		&group.Name,
		&labelsJSON,
		&requireApproval,
		&requireApprovalForRollback,
		&group.ChangeWindowsJSON,
		&group.CreatedAt,
		&group.UpdatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get group: %w", err)
	}

	group.RequireApproval = requireApproval != 0
	group.RequireApprovalForRollback = requireApprovalForRollback != 0
	_ = json.Unmarshal([]byte(labelsJSON), &group.Labels)
	return &group, nil
}

func (s *Storage) ListGroups(ctx context.Context) ([]*types.Group, error) {
	query := `SELECT id, name, labels, require_approval, require_approval_for_rollback, change_windows, created_at, updated_at FROM groups ORDER BY created_at DESC`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list groups: %w", err)
	}
	defer rows.Close()

	var groups []*types.Group
	for rows.Next() {
		var group types.Group
		var labelsJSON string
		var requireApproval int
		var requireApprovalForRollback int

		err := rows.Scan(
			&group.ID,
			&group.Name,
			&labelsJSON,
			&requireApproval,
			&requireApprovalForRollback,
			&group.ChangeWindowsJSON,
			&group.CreatedAt,
			&group.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan group: %w", err)
		}

		group.RequireApproval = requireApproval != 0
		group.RequireApprovalForRollback = requireApprovalForRollback != 0
		_ = json.Unmarshal([]byte(labelsJSON), &group.Labels)
		groups = append(groups, &group)
	}

	return groups, nil
}

// UpdateGroup writes mutable fields. ID and CreatedAt are immutable;
// the caller (service layer) is expected to bump UpdatedAt before
// calling. Added in v0.48 to support the approval-policy toggle;
// v0.49 extended to round-trip change_windows.
func (s *Storage) UpdateGroup(ctx context.Context, group *types.Group) error {
	labelsJSON, _ := json.Marshal(group.Labels)
	cw := group.ChangeWindowsJSON
	if cw == "" {
		cw = "[]"
	}
	query := `
		UPDATE groups
		SET name = ?, labels = ?, require_approval = ?, require_approval_for_rollback = ?, change_windows = ?, updated_at = ?
		WHERE id = ?
	`
	result, err := s.db.ExecContext(ctx, query,
		group.Name,
		string(labelsJSON),
		boolToInt(group.RequireApproval),
		boolToInt(group.RequireApprovalForRollback),
		cw,
		group.UpdatedAt,
		group.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update group: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("group not found: %s", group.ID)
	}
	return nil
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
	q := "SELECT id, timestamp, actor, event_type, target_type, target_id, action, payload, created_at, ai_explanation, ai_explanation_model, ai_explanation_generated_at FROM audit_events WHERE 1=1"
	var args []any
	if filter.EventType != "" {
		q += " AND event_type = ?"
		args = append(args, filter.EventType)
	}
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
		var aiExplanation, aiModel sql.NullString
		var aiGeneratedAt sql.NullTime
		if err := rows.Scan(
			&e.ID, &e.Timestamp, &e.Actor, &e.EventType, &e.TargetType,
			&targetID, &e.Action, &payload, &e.CreatedAt,
			&aiExplanation, &aiModel, &aiGeneratedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan audit event: %w", err)
		}
		if targetID.Valid {
			e.TargetID = targetID.String
		}
		if payload.Valid && payload.String != "" {
			_ = json.Unmarshal([]byte(payload.String), &e.Payload)
		}
		if aiExplanation.Valid {
			e.AIExplanation = aiExplanation.String
		}
		if aiModel.Valid {
			e.AIExplanationModel = aiModel.String
		}
		if aiGeneratedAt.Valid {
			t := aiGeneratedAt.Time
			e.AIExplanationGeneratedAt = &t
		}
		out = append(out, e)
	}
	return out, nil
}

// GetAuditEvent fetches one audit row by ID. Returns (nil, nil) when no
// row matches so the caller can render a 404 distinct from a 500.
func (s *Storage) GetAuditEvent(ctx context.Context, id string) (*types.AuditEvent, error) {
	q := "SELECT id, timestamp, actor, event_type, target_type, target_id, action, payload, created_at, ai_explanation, ai_explanation_model, ai_explanation_generated_at FROM audit_events WHERE id = ?"
	row := s.db.QueryRowContext(ctx, q, id)

	e := &types.AuditEvent{}
	var targetID sql.NullString
	var payload sql.NullString
	var aiExplanation, aiModel sql.NullString
	var aiGeneratedAt sql.NullTime
	err := row.Scan(
		&e.ID, &e.Timestamp, &e.Actor, &e.EventType, &e.TargetType,
		&targetID, &e.Action, &payload, &e.CreatedAt,
		&aiExplanation, &aiModel, &aiGeneratedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get audit event: %w", err)
	}
	if targetID.Valid {
		e.TargetID = targetID.String
	}
	if payload.Valid && payload.String != "" {
		_ = json.Unmarshal([]byte(payload.String), &e.Payload)
	}
	if aiExplanation.Valid {
		e.AIExplanation = aiExplanation.String
	}
	if aiModel.Valid {
		e.AIExplanationModel = aiModel.String
	}
	if aiGeneratedAt.Valid {
		t := aiGeneratedAt.Time
		e.AIExplanationGeneratedAt = &t
	}
	return e, nil
}

// UpdateAuditEventExplanation writes a cached AI explanation onto the
// row. The row stays otherwise immutable; this is the one mutation the
// audit log allows. Returns an error if no row matches the supplied id.
func (s *Storage) UpdateAuditEventExplanation(ctx context.Context, id, explanation, model string, generatedAt time.Time) error {
	stmt := `UPDATE audit_events SET ai_explanation = ?, ai_explanation_model = ?, ai_explanation_generated_at = ? WHERE id = ?`
	res, err := s.db.ExecContext(ctx, stmt, explanation, model, generatedAt, id)
	if err != nil {
		return fmt.Errorf("failed to update audit explanation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to check rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("audit event %q not found", id)
	}
	return nil
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
	// v0.53 — proposal provenance. Default proposed_by to "operator"
	// at the storage layer so old callers that don't set it carry
	// the right semantics. Marshal evidence refs to JSON; the empty
	// slice marshals to "null" via Go's json package which we coerce
	// to the explicit empty string so the column stays NULL.
	proposedBy := r.ProposedBy
	if proposedBy == "" {
		proposedBy = types.RolloutProposedByOperator
	}
	evidenceJSON := ""
	if len(r.EvidenceRefs) > 0 {
		buf, err := json.Marshal(r.EvidenceRefs)
		if err != nil {
			return fmt.Errorf("failed to marshal rollout evidence refs: %w", err)
		}
		evidenceJSON = string(buf)
	}
	stmt := `
		INSERT INTO rollouts (id, name, group_id, target_config_id, previous_config_id, stages, abort_criteria, notification_url, state, current_stage, stage_started_at, abort_reason, created_at, updated_at, completed_at, require_approval, requested_by, approved_by, approved_at, rejected_by, rejected_at, approval_notes, last_blackout_reason, last_blackout_at, proposed_by, proposal_reasoning, evidence_refs, rolled_back_from_id, plan_id, plan_step_index)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	_, err = s.db.ExecContext(ctx, stmt,
		r.ID, r.Name, r.GroupID, r.TargetConfigID,
		nullableString(r.PreviousConfigID),
		string(stagesJSON), string(criteriaJSON),
		nullableString(r.NotificationURL),
		string(r.State), r.CurrentStage,
		r.StageStartedAt, nullableString(r.AbortReason),
		r.CreatedAt, r.UpdatedAt, r.CompletedAt,
		// v0.47 approval columns.
		boolToInt(r.RequireApproval),
		nullableString(r.RequestedBy),
		nullableString(r.ApprovedBy),
		r.ApprovedAt,
		nullableString(r.RejectedBy),
		r.RejectedAt,
		nullableString(r.ApprovalNotes),
		// v0.49 blackout columns. Empty at create — the engine
		// only ever sets them later.
		nullableString(r.LastBlackoutReason),
		r.LastBlackoutAt,
		// v0.53 proposal provenance columns.
		proposedBy,
		nullableString(r.ProposalReasoning),
		nullableString(evidenceJSON),
		// v0.60 rollback chain.
		nullableString(r.RolledBackFromID),
		// v0.69 plan grouping.
		nullableString(r.PlanID),
		r.PlanStepIndex,
	)
	if err != nil {
		return fmt.Errorf("failed to create rollout: %w", err)
	}
	return nil
}

// boolToInt is reused from alerts CRUD above — sqlite stores bools
// as ints, so the same helper handles rollouts.require_approval.

func (s *Storage) GetRollout(ctx context.Context, id string) (*types.Rollout, error) {
	stmt := `SELECT id, name, group_id, target_config_id, previous_config_id, stages, abort_criteria, notification_url, state, current_stage, stage_started_at, abort_reason, created_at, updated_at, completed_at, require_approval, requested_by, approved_by, approved_at, rejected_by, rejected_at, approval_notes, last_blackout_reason, last_blackout_at, proposed_by, proposal_reasoning, evidence_refs, rolled_back_from_id, plan_id, plan_step_index FROM rollouts WHERE id = ?`
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

	q := "SELECT id, name, group_id, target_config_id, previous_config_id, stages, abort_criteria, notification_url, state, current_stage, stage_started_at, abort_reason, created_at, updated_at, completed_at, require_approval, requested_by, approved_by, approved_at, rejected_by, rejected_at, approval_notes, last_blackout_reason, last_blackout_at, proposed_by, proposal_reasoning, evidence_refs, rolled_back_from_id, plan_id, plan_step_index FROM rollouts WHERE 1=1"
	var args []any
	if filter.GroupID != "" {
		q += " AND group_id = ?"
		args = append(args, filter.GroupID)
	}
	if filter.State != "" {
		q += " AND state = ?"
		args = append(args, string(filter.State))
	}
	// v0.74 — narrow to one plan id.
	if filter.PlanID != "" {
		q += " AND plan_id = ?"
		args = append(args, filter.PlanID)
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
	// v0.53 — same evidence/proposed_by encoding as CreateRollout.
	proposedBy := r.ProposedBy
	if proposedBy == "" {
		proposedBy = types.RolloutProposedByOperator
	}
	evidenceJSON := ""
	if len(r.EvidenceRefs) > 0 {
		buf, mErr := json.Marshal(r.EvidenceRefs)
		if mErr != nil {
			return fmt.Errorf("failed to marshal rollout evidence refs: %w", mErr)
		}
		evidenceJSON = string(buf)
	}
	stmt := `
		UPDATE rollouts
		SET name = ?, group_id = ?, target_config_id = ?, previous_config_id = ?,
		    stages = ?, abort_criteria = ?, notification_url = ?,
		    state = ?, current_stage = ?,
		    stage_started_at = ?, abort_reason = ?, updated_at = ?, completed_at = ?,
		    require_approval = ?, requested_by = ?, approved_by = ?, approved_at = ?,
		    rejected_by = ?, rejected_at = ?, approval_notes = ?,
		    last_blackout_reason = ?, last_blackout_at = ?,
		    proposed_by = ?, proposal_reasoning = ?, evidence_refs = ?,
		    rolled_back_from_id = ?, plan_id = ?, plan_step_index = ?
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
		boolToInt(r.RequireApproval),
		nullableString(r.RequestedBy),
		nullableString(r.ApprovedBy),
		r.ApprovedAt,
		nullableString(r.RejectedBy),
		r.RejectedAt,
		nullableString(r.ApprovalNotes),
		// v0.49 blackout columns.
		nullableString(r.LastBlackoutReason),
		r.LastBlackoutAt,
		// v0.53 proposal provenance columns.
		proposedBy,
		nullableString(r.ProposalReasoning),
		nullableString(evidenceJSON),
		// v0.60 rollback chain.
		nullableString(r.RolledBackFromID),
		// v0.69 plan grouping.
		nullableString(r.PlanID),
		r.PlanStepIndex,
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
		// v0.47 approval columns. Nullable / int because the
		// migration leaves them empty for pre-v0.47 rollouts.
		requireApprovalInt int
		requestedBy        sql.NullString
		approvedBy         sql.NullString
		approvedAt         sql.NullTime
		rejectedBy         sql.NullString
		rejectedAt         sql.NullTime
		approvalNotes      sql.NullString
		// v0.49 blackout columns.
		lastBlackoutReason sql.NullString
		lastBlackoutAt     sql.NullTime
		// v0.53 proposal provenance.
		proposedBy        sql.NullString
		proposalReasoning sql.NullString
		evidenceRefsJSON  sql.NullString
		// v0.60 rollback chain.
		rolledBackFromID sql.NullString
		// v0.69 plan grouping.
		planID        sql.NullString
		planStepIndex int
	)
	if err := sc.Scan(
		&r.ID, &r.Name, &r.GroupID, &r.TargetConfigID,
		&previousConfigID, &stagesJSON, &criteriaJSON, &notificationURL,
		&stateStr, &r.CurrentStage,
		&stageStartedAt, &abortReason,
		&r.CreatedAt, &r.UpdatedAt, &completedAt,
		&requireApprovalInt, &requestedBy, &approvedBy, &approvedAt,
		&rejectedBy, &rejectedAt, &approvalNotes,
		&lastBlackoutReason, &lastBlackoutAt,
		&proposedBy, &proposalReasoning, &evidenceRefsJSON,
		&rolledBackFromID,
		&planID, &planStepIndex,
	); err != nil {
		return nil, err
	}
	if rolledBackFromID.Valid {
		r.RolledBackFromID = rolledBackFromID.String
	}
	if planID.Valid {
		r.PlanID = planID.String
	}
	r.PlanStepIndex = planStepIndex
	// v0.53 — proposal provenance decoding.
	if proposedBy.Valid && proposedBy.String != "" {
		r.ProposedBy = proposedBy.String
	} else {
		// Pre-v0.53 rows fall back to operator semantics.
		r.ProposedBy = types.RolloutProposedByOperator
	}
	if proposalReasoning.Valid {
		r.ProposalReasoning = proposalReasoning.String
	}
	if evidenceRefsJSON.Valid && evidenceRefsJSON.String != "" {
		if err := json.Unmarshal([]byte(evidenceRefsJSON.String), &r.EvidenceRefs); err != nil {
			return nil, fmt.Errorf("failed to unmarshal evidence refs: %w", err)
		}
	}
	if lastBlackoutReason.Valid {
		r.LastBlackoutReason = lastBlackoutReason.String
	}
	if lastBlackoutAt.Valid {
		t := lastBlackoutAt.Time
		r.LastBlackoutAt = &t
	}
	if previousConfigID.Valid {
		r.PreviousConfigID = previousConfigID.String
	}
	if notificationURL.Valid {
		r.NotificationURL = notificationURL.String
	}
	r.RequireApproval = requireApprovalInt != 0
	if requestedBy.Valid {
		r.RequestedBy = requestedBy.String
	}
	if approvedBy.Valid {
		r.ApprovedBy = approvedBy.String
	}
	if approvedAt.Valid {
		t := approvedAt.Time
		r.ApprovedAt = &t
	}
	if rejectedBy.Valid {
		r.RejectedBy = rejectedBy.String
	}
	if rejectedAt.Valid {
		t := rejectedAt.Time
		r.RejectedAt = &t
	}
	if approvalNotes.Valid {
		r.ApprovalNotes = approvalNotes.String
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

// ===================================================================
// v0.32 expected agents (inventory reconciliation)
// ===================================================================

// UpsertExpectedAgent inserts or updates a single expected-agent
// row. Used for one-off additions from squadronctl or the UI; the
// bulk-rotate path used by CI is ReplaceExpectedAgentsForSource.
func (s *Storage) UpsertExpectedAgent(ctx context.Context, e *types.ExpectedAgent) error {
	if e == nil || e.Hostname == "" {
		return fmt.Errorf("hostname required")
	}
	if e.ExpectedSince.IsZero() {
		e.ExpectedSince = time.Now().UTC()
	}
	e.UpdatedAt = time.Now().UTC()
	labelsJSON, _ := json.Marshal(e.Labels)
	stmt := `INSERT INTO expected_agents (hostname, labels_json, source, expected_since, updated_at, notes)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(hostname) DO UPDATE SET
			labels_json = excluded.labels_json,
			source = excluded.source,
			updated_at = excluded.updated_at,
			notes = excluded.notes`
	if _, err := s.db.ExecContext(ctx, stmt,
		e.Hostname, string(labelsJSON), e.Source,
		e.ExpectedSince.UTC(), e.UpdatedAt.UTC(), e.Notes); err != nil {
		return fmt.Errorf("failed to upsert expected agent: %w", err)
	}
	return nil
}

// DeleteExpectedAgent removes a single hostname from the inventory.
// Used when CI decommissions a host and wants to stop flagging it
// as missing.
func (s *Storage) DeleteExpectedAgent(ctx context.Context, hostname string) error {
	if hostname == "" {
		return fmt.Errorf("hostname required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM expected_agents WHERE hostname = ?`, hostname); err != nil {
		return fmt.Errorf("failed to delete expected agent: %w", err)
	}
	return nil
}

// ListExpectedAgents returns every expected entry, optionally
// filtered to one source pipeline. An empty source returns all.
func (s *Storage) ListExpectedAgents(ctx context.Context, source string) ([]*types.ExpectedAgent, error) {
	q := `SELECT hostname, labels_json, source, expected_since, updated_at, notes FROM expected_agents`
	args := []interface{}{}
	if source != "" {
		q += ` WHERE source = ?`
		args = append(args, source)
	}
	q += ` ORDER BY hostname`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list expected agents: %w", err)
	}
	defer rows.Close()
	out := []*types.ExpectedAgent{}
	for rows.Next() {
		e := &types.ExpectedAgent{}
		var labelsJSON string
		if err := rows.Scan(&e.Hostname, &labelsJSON, &e.Source,
			&e.ExpectedSince, &e.UpdatedAt, &e.Notes); err != nil {
			return nil, fmt.Errorf("scan expected agent: %w", err)
		}
		if labelsJSON != "" {
			_ = json.Unmarshal([]byte(labelsJSON), &e.Labels)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReplaceExpectedAgentsForSource is the atomic bulk-rotate used by
// CI: drop everything tagged with the given source, then re-insert
// the new list. Wrapped in a transaction so a partial failure leaves
// the previous inventory intact.
func (s *Storage) ReplaceExpectedAgentsForSource(ctx context.Context, source string, entries []*types.ExpectedAgent) error {
	if source == "" {
		return fmt.Errorf("source required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM expected_agents WHERE source = ?`, source); err != nil {
		return fmt.Errorf("delete by source: %w", err)
	}

	prepared, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO expected_agents
			(hostname, labels_json, source, expected_since, updated_at, notes)
			VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer func() { _ = prepared.Close() }()

	now := time.Now().UTC()
	for _, e := range entries {
		if e == nil || e.Hostname == "" {
			continue
		}
		labelsJSON, _ := json.Marshal(e.Labels)
		expected := e.ExpectedSince
		if expected.IsZero() {
			expected = now
		}
		if _, err := prepared.ExecContext(ctx,
			e.Hostname, string(labelsJSON), source,
			expected.UTC(), now, e.Notes); err != nil {
			return fmt.Errorf("insert %s: %w", e.Hostname, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ===================================================================
// v0.34 deploy targets + runs (GitHub Actions integration)
// ===================================================================

func (s *Storage) CreateDeployTarget(ctx context.Context, t *types.DeployTarget) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("id required")
	}
	if t.Provider == "" {
		t.Provider = "github"
	}
	if t.GitHubBranch == "" {
		t.GitHubBranch = "main"
	}
	if t.DefaultInputs == nil {
		t.DefaultInputs = map[string]string{}
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	inputsJSON, _ := json.Marshal(t.DefaultInputs)
	stmt := `INSERT INTO deploy_targets (
		id, name, provider, github_owner, github_repo, github_workflow,
		github_branch, encrypted_credential, default_inputs_json,
		config_id, inventory_path, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, stmt,
		t.ID, t.Name, t.Provider, t.GitHubOwner, t.GitHubRepo,
		t.GitHubWorkflow, t.GitHubBranch, t.EncryptedCredential,
		string(inputsJSON), t.ConfigID, t.InventoryPath,
		t.CreatedAt.UTC(), t.UpdatedAt.UTC(),
	); err != nil {
		return fmt.Errorf("failed to create deploy target: %w", err)
	}
	return nil
}

func (s *Storage) UpdateDeployTarget(ctx context.Context, t *types.DeployTarget) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("id required")
	}
	t.UpdatedAt = time.Now().UTC()
	inputsJSON, _ := json.Marshal(t.DefaultInputs)
	// Only update the credential when it's been re-supplied. The
	// "leave the existing credential alone" path is critical so the
	// UI can render edit forms without round-tripping the secret.
	if len(t.EncryptedCredential) > 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE deploy_targets SET
			name = ?, provider = ?, github_owner = ?, github_repo = ?, github_workflow = ?,
			github_branch = ?, encrypted_credential = ?, default_inputs_json = ?,
			config_id = ?, inventory_path = ?, updated_at = ? WHERE id = ?`,
			t.Name, t.Provider, t.GitHubOwner, t.GitHubRepo, t.GitHubWorkflow,
			t.GitHubBranch, t.EncryptedCredential, string(inputsJSON), t.ConfigID,
			t.InventoryPath, t.UpdatedAt.UTC(), t.ID)
		if err != nil {
			return fmt.Errorf("update deploy target (with credential): %w", err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE deploy_targets SET
		name = ?, provider = ?, github_owner = ?, github_repo = ?, github_workflow = ?,
		github_branch = ?, default_inputs_json = ?,
		config_id = ?, inventory_path = ?, updated_at = ? WHERE id = ?`,
		t.Name, t.Provider, t.GitHubOwner, t.GitHubRepo, t.GitHubWorkflow,
		t.GitHubBranch, string(inputsJSON), t.ConfigID,
		t.InventoryPath, t.UpdatedAt.UTC(), t.ID)
	if err != nil {
		return fmt.Errorf("update deploy target: %w", err)
	}
	return nil
}

func (s *Storage) GetDeployTarget(ctx context.Context, id string) (*types.DeployTarget, error) {
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT
		id, name, provider, github_owner, github_repo, github_workflow,
		github_branch, encrypted_credential, default_inputs_json,
		config_id, inventory_path, created_at, updated_at
		FROM deploy_targets WHERE id = ?`, id)
	t := &types.DeployTarget{}
	var inputsJSON string
	var cred []byte
	if err := row.Scan(&t.ID, &t.Name, &t.Provider, &t.GitHubOwner, &t.GitHubRepo,
		&t.GitHubWorkflow, &t.GitHubBranch, &cred, &inputsJSON,
		&t.ConfigID, &t.InventoryPath, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get deploy target: %w", err)
	}
	t.EncryptedCredential = cred
	t.HasCredential = len(cred) > 0
	if inputsJSON != "" {
		_ = json.Unmarshal([]byte(inputsJSON), &t.DefaultInputs)
	}
	return t, nil
}

func (s *Storage) ListDeployTargets(ctx context.Context) ([]*types.DeployTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, name, provider, github_owner, github_repo, github_workflow,
		github_branch, encrypted_credential, default_inputs_json,
		config_id, inventory_path, created_at, updated_at
		FROM deploy_targets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list deploy targets: %w", err)
	}
	defer rows.Close()
	out := []*types.DeployTarget{}
	for rows.Next() {
		t := &types.DeployTarget{}
		var inputsJSON string
		var cred []byte
		if err := rows.Scan(&t.ID, &t.Name, &t.Provider, &t.GitHubOwner, &t.GitHubRepo,
			&t.GitHubWorkflow, &t.GitHubBranch, &cred, &inputsJSON,
			&t.ConfigID, &t.InventoryPath, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan deploy target: %w", err)
		}
		t.HasCredential = len(cred) > 0
		// Strip the credential bytes from list responses; the caller
		// asks for it explicitly via GetDeployTarget when needed for
		// a dispatch.
		t.EncryptedCredential = nil
		if inputsJSON != "" {
			_ = json.Unmarshal([]byte(inputsJSON), &t.DefaultInputs)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Storage) DeleteDeployTarget(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM deploy_targets WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete deploy target: %w", err)
	}
	return nil
}

func (s *Storage) CreateDeployRun(ctx context.Context, r *types.DeployRun) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("id required")
	}
	if r.Status == "" {
		r.Status = "queued"
	}
	if r.RequestedAt.IsZero() {
		r.RequestedAt = time.Now().UTC()
	}
	if r.Inputs == nil {
		r.Inputs = map[string]string{}
	}
	if r.ExpectedHosts == nil {
		r.ExpectedHosts = []string{}
	}
	inputsJSON, _ := json.Marshal(r.Inputs)
	hostsJSON, _ := json.Marshal(r.ExpectedHosts)
	stmt := `INSERT INTO deploy_runs (
		id, target_id, requested_by, requested_at, inputs_json,
		github_run_id, github_run_url, status, conclusion, completed_at,
		expected_hosts_json, verification_state, verified_at, notes
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, stmt,
		r.ID, r.TargetID, r.RequestedBy, r.RequestedAt.UTC(), string(inputsJSON),
		r.GitHubRunID, r.GitHubRunURL, r.Status, r.Conclusion, nullableTime(r.CompletedAt),
		string(hostsJSON), r.VerificationState, nullableTime(r.VerifiedAt), r.Notes,
	)
	if err != nil {
		return fmt.Errorf("create deploy run: %w", err)
	}
	return nil
}

func (s *Storage) UpdateDeployRun(ctx context.Context, r *types.DeployRun) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("id required")
	}
	if r.Inputs == nil {
		r.Inputs = map[string]string{}
	}
	if r.ExpectedHosts == nil {
		r.ExpectedHosts = []string{}
	}
	inputsJSON, _ := json.Marshal(r.Inputs)
	hostsJSON, _ := json.Marshal(r.ExpectedHosts)
	_, err := s.db.ExecContext(ctx, `UPDATE deploy_runs SET
		inputs_json = ?, github_run_id = ?, github_run_url = ?, status = ?,
		conclusion = ?, completed_at = ?, expected_hosts_json = ?,
		verification_state = ?, verified_at = ?, notes = ?
		WHERE id = ?`,
		string(inputsJSON), r.GitHubRunID, r.GitHubRunURL, r.Status,
		r.Conclusion, nullableTime(r.CompletedAt), string(hostsJSON),
		r.VerificationState, nullableTime(r.VerifiedAt), r.Notes, r.ID)
	if err != nil {
		return fmt.Errorf("update deploy run: %w", err)
	}
	return nil
}

func (s *Storage) GetDeployRun(ctx context.Context, id string) (*types.DeployRun, error) {
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT
		id, target_id, requested_by, requested_at, inputs_json,
		github_run_id, github_run_url, status, conclusion, completed_at,
		expected_hosts_json, verification_state, verified_at, notes
		FROM deploy_runs WHERE id = ?`, id)
	r := &types.DeployRun{}
	var inputsJSON, hostsJSON string
	var completedAt, verifiedAt sql.NullTime
	if err := row.Scan(&r.ID, &r.TargetID, &r.RequestedBy, &r.RequestedAt, &inputsJSON,
		&r.GitHubRunID, &r.GitHubRunURL, &r.Status, &r.Conclusion, &completedAt,
		&hostsJSON, &r.VerificationState, &verifiedAt, &r.Notes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get deploy run: %w", err)
	}
	if completedAt.Valid {
		t := completedAt.Time
		r.CompletedAt = &t
	}
	if verifiedAt.Valid {
		t := verifiedAt.Time
		r.VerifiedAt = &t
	}
	if inputsJSON != "" {
		_ = json.Unmarshal([]byte(inputsJSON), &r.Inputs)
	}
	if hostsJSON != "" {
		_ = json.Unmarshal([]byte(hostsJSON), &r.ExpectedHosts)
	}
	return r, nil
}

func (s *Storage) ListDeployRuns(ctx context.Context, filter types.DeployRunFilter) ([]*types.DeployRun, error) {
	q := `SELECT
		id, target_id, requested_by, requested_at, inputs_json,
		github_run_id, github_run_url, status, conclusion, completed_at,
		expected_hosts_json, verification_state, verified_at, notes
		FROM deploy_runs WHERE 1=1`
	args := []interface{}{}
	if filter.TargetID != "" {
		q += " AND target_id = ?"
		args = append(args, filter.TargetID)
	}
	if filter.Status != "" {
		q += " AND status = ?"
		args = append(args, filter.Status)
	}
	q += " ORDER BY requested_at DESC"
	if filter.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list deploy runs: %w", err)
	}
	defer rows.Close()
	out := []*types.DeployRun{}
	for rows.Next() {
		r := &types.DeployRun{}
		var inputsJSON, hostsJSON string
		var completedAt, verifiedAt sql.NullTime
		if err := rows.Scan(&r.ID, &r.TargetID, &r.RequestedBy, &r.RequestedAt, &inputsJSON,
			&r.GitHubRunID, &r.GitHubRunURL, &r.Status, &r.Conclusion, &completedAt,
			&hostsJSON, &r.VerificationState, &verifiedAt, &r.Notes); err != nil {
			return nil, fmt.Errorf("scan deploy run: %w", err)
		}
		if completedAt.Valid {
			t := completedAt.Time
			r.CompletedAt = &t
		}
		if verifiedAt.Valid {
			t := verifiedAt.Time
			r.VerifiedAt = &t
		}
		if inputsJSON != "" {
			_ = json.Unmarshal([]byte(inputsJSON), &r.Inputs)
		}
		if hostsJSON != "" {
			_ = json.Unmarshal([]byte(hostsJSON), &r.ExpectedHosts)
		}
		out = append(out, r)
	}
	return out, rows.Err()
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

// --- SIEM destinations (v0.50) ----------------------------------------

func (s *Storage) CreateSiemDestination(ctx context.Context, d *types.SiemDestination) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = d.CreatedAt
	}
	prefixes := d.EventTypePrefixesJSON
	if prefixes == "" {
		prefixes = "[]"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO siem_destinations (
			id, name, type, url, secret, enabled, event_type_prefixes_json,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		d.ID, d.Name, d.Type, d.URL, d.Secret,
		boolToInt(d.Enabled), prefixes,
		d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create siem destination: %w", err)
	}
	return nil
}

func (s *Storage) GetSiemDestination(ctx context.Context, id string) (*types.SiemDestination, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, type, url, secret, enabled, event_type_prefixes_json,
		       last_event_sent_at, last_error, last_error_at, created_at, updated_at
		FROM siem_destinations WHERE id = ?
	`, id)
	d, err := scanSiemDestination(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return d, err
}

func (s *Storage) ListSiemDestinations(ctx context.Context) ([]*types.SiemDestination, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, type, url, secret, enabled, event_type_prefixes_json,
		       last_event_sent_at, last_error, last_error_at, created_at, updated_at
		FROM siem_destinations ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to list siem destinations: %w", err)
	}
	defer rows.Close()
	var out []*types.SiemDestination
	for rows.Next() {
		d, err := scanSiemDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Storage) UpdateSiemDestination(ctx context.Context, d *types.SiemDestination) error {
	d.UpdatedAt = time.Now().UTC()
	prefixes := d.EventTypePrefixesJSON
	if prefixes == "" {
		prefixes = "[]"
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE siem_destinations
		SET name = ?, type = ?, url = ?, secret = ?, enabled = ?,
		    event_type_prefixes_json = ?, updated_at = ?
		WHERE id = ?
	`,
		d.Name, d.Type, d.URL, d.Secret, boolToInt(d.Enabled),
		prefixes, d.UpdatedAt, d.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update siem destination: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("siem destination not found: %s", d.ID)
	}
	return nil
}

func (s *Storage) DeleteSiemDestination(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM siem_destinations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete siem destination: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("siem destination not found: %s", id)
	}
	return nil
}

// UpdateSiemDestinationStatus narrow-updates only the dispatcher-
// owned status columns. Separate from UpdateSiemDestination so the
// dispatcher's writes don't race with an operator editing the URL
// or secret at the same moment.
func (s *Storage) UpdateSiemDestinationStatus(ctx context.Context, id string, sentAt *time.Time, errMsg string, errAt *time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE siem_destinations
		SET last_event_sent_at = ?, last_error = ?, last_error_at = ?
		WHERE id = ?
	`, sentAt, nullableString(errMsg), errAt, id)
	if err != nil {
		return fmt.Errorf("failed to update siem destination status: %w", err)
	}
	return nil
}

func scanSiemDestination(sc scanner) (*types.SiemDestination, error) {
	d := &types.SiemDestination{}
	var (
		enabledInt int
		prefixes   sql.NullString
		sentAt     sql.NullTime
		lastErr    sql.NullString
		errAt      sql.NullTime
	)
	if err := sc.Scan(
		&d.ID, &d.Name, &d.Type, &d.URL, &d.Secret,
		&enabledInt, &prefixes,
		&sentAt, &lastErr, &errAt, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return nil, err
	}
	d.Enabled = enabledInt != 0
	if prefixes.Valid {
		d.EventTypePrefixesJSON = prefixes.String
	}
	if sentAt.Valid {
		t := sentAt.Time
		d.LastEventSentAt = &t
	}
	if lastErr.Valid {
		d.LastError = lastErr.String
	}
	if errAt.Valid {
		t := errAt.Time
		d.LastErrorAt = &t
	}
	return d, nil
}
