package sqlite

const SchemaVersion = 4

// InitialSchema creates the initial SQLite database schema
const InitialSchema = `
-- Schema version tracking
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER PRIMARY KEY,
	applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Agents table
CREATE TABLE IF NOT EXISTS agents (
	id TEXT PRIMARY KEY,
	instance_id_str TEXT NOT NULL,
	name TEXT,
	group_id TEXT,
	group_name TEXT,
	version TEXT,
	status TEXT NOT NULL DEFAULT 'offline',
	last_seen DATETIME,
	started_at DATETIME NOT NULL,
	effective_config TEXT,
	custom_config TEXT,
	remote_config_status TEXT,
	health_status TEXT,
	error_message TEXT,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	FOREIGN KEY (group_id) REFERENCES groups(id)
);

-- Groups table
CREATE TABLE IF NOT EXISTS groups (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	description TEXT,
	config TEXT,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Configs table (config history for agents and groups)
CREATE TABLE IF NOT EXISTS configs (
	id TEXT PRIMARY KEY,
	agent_id TEXT,
	group_id TEXT,
	config_type TEXT NOT NULL, -- 'agent' or 'group'
	config_body TEXT NOT NULL,
	version INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	created_by TEXT,
	FOREIGN KEY (agent_id) REFERENCES agents(id),
	FOREIGN KEY (group_id) REFERENCES groups(id),
	CHECK (
		(config_type = 'agent' AND agent_id IS NOT NULL AND group_id IS NULL) OR
		(config_type = 'group' AND group_id IS NOT NULL AND agent_id IS NULL)
	)
);

-- Agent capabilities table
CREATE TABLE IF NOT EXISTS agent_capabilities (
	agent_id TEXT NOT NULL,
	capability TEXT NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (agent_id, capability),
	FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- Agent attributes table
CREATE TABLE IF NOT EXISTS agent_attributes (
	agent_id TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (agent_id, key),
	FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

-- Indexes for performance
CREATE INDEX IF NOT EXISTS idx_agents_group_id ON agents(group_id);
CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);
CREATE INDEX IF NOT EXISTS idx_agents_last_seen ON agents(last_seen);
CREATE INDEX IF NOT EXISTS idx_configs_agent_id ON configs(agent_id);
CREATE INDEX IF NOT EXISTS idx_configs_group_id ON configs(group_id);
CREATE INDEX IF NOT EXISTS idx_configs_created_at ON configs(created_at DESC);

-- Triggers to update updated_at timestamps
CREATE TRIGGER IF NOT EXISTS update_agents_timestamp
AFTER UPDATE ON agents
BEGIN
	UPDATE agents SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS update_groups_timestamp
AFTER UPDATE ON groups
BEGIN
	UPDATE groups SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS update_agent_attributes_timestamp
AFTER UPDATE ON agent_attributes
BEGIN
	UPDATE agent_attributes SET updated_at = CURRENT_TIMESTAMP
	WHERE agent_id = NEW.agent_id AND key = NEW.key;
END;

-- Insert initial schema version
INSERT OR IGNORE INTO schema_version (version) VALUES (1);
`

// AlertRulesSchema adds the alert_rules table for the phase 3b alerting feature.
const AlertRulesSchema = `
-- Alert rules table
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
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_alert_rules_enabled ON alert_rules(enabled);

CREATE TRIGGER IF NOT EXISTS update_alert_rules_timestamp
AFTER UPDATE ON alert_rules
BEGIN
	UPDATE alert_rules SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

INSERT OR IGNORE INTO schema_version (version) VALUES (2);
`

// RecommendationDismissalsSchema adds the v0.25 dismissals table.
// The recommendation_id is the engine's deterministic hash (SHA-1
// truncated to 16 hex chars) — same fleet shape produces the same
// id across re-evaluations, so dismissals stick. No FK constraint
// because recommendations are computed on-demand, not stored.
const RecommendationDismissalsSchema = `
CREATE TABLE IF NOT EXISTS recommendation_dismissals (
	recommendation_id TEXT PRIMARY KEY,
	dismissed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	dismissed_by TEXT NOT NULL DEFAULT 'system',
	reason TEXT
);

CREATE INDEX IF NOT EXISTS idx_rec_dismissals_dismissed_at ON recommendation_dismissals(dismissed_at);

INSERT OR IGNORE INTO schema_version (version) VALUES (3);
`

// AIVerdictsSchema bumps the database to schema v4. Adds:
//
//   - groups.learn_from_verdicts (INTEGER NOT NULL DEFAULT 1): per-
//     group opt-out for the proposer's prior-verdicts few-shot
//     loop. Default 1 (opt-in) so post-upgrade behavior matches the
//     v0.89.17 design.
//   - idx_ai_verdicts: a partial index on rollouts(group_id,
//     proposed_by, COALESCE(approved_at, rejected_at) DESC) over
//     just AI-originated rollouts that have a terminal verdict. The
//     proposer bridge sweeps this on every cost-spike proposal;
//     the partial predicate keeps the index lean on storage
//     (operator-originated rollouts and pending AI proposals never
//     contribute index rows).
//
// See docs/proposals/531-proposer-learns-from-accepted-rejected.md §4.
const AIVerdictsSchema = `
ALTER TABLE groups ADD COLUMN learn_from_verdicts INTEGER NOT NULL DEFAULT 1;

CREATE INDEX IF NOT EXISTS idx_ai_verdicts ON rollouts(
	group_id,
	proposed_by,
	COALESCE(approved_at, rejected_at) DESC
) WHERE proposed_by='ai' AND (approved_at IS NOT NULL OR rejected_at IS NOT NULL);

INSERT OR IGNORE INTO schema_version (version) VALUES (4);
`

// Migrations is a list of all schema migrations
var Migrations = []string{
	InitialSchema,
	AlertRulesSchema,
	RecommendationDismissalsSchema,
	AIVerdictsSchema,
}
