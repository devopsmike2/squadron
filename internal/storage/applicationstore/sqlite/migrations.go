package sqlite

const SchemaVersion = 10

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

// ExcludeFromLearningSchema bumps the database to schema v5. Adds:
//
//   - rollouts.exclude_from_learning (INTEGER NOT NULL DEFAULT 0):
//     per-rollout opt-out flag for the proposer's prior-verdicts
//     few-shot loop. Default 0 (included) so post-upgrade behavior
//     matches the v0.89.17 design — operators flip individual rows
//     to 1 to suppress that rollout's reasoning + notes from future
//     proposals without disabling the whole group's learning loop.
//   - idx_ai_verdicts_exclude: a regular companion index over
//     (group_id, proposed_by, exclude_from_learning,
//     COALESCE(approved_at, rejected_at) DESC). The v4 partial
//     idx_ai_verdicts does not cover the exclude_from_learning=0
//     predicate; this regular index keeps ListAIVerdictsForGroup
//     off a table scan on deployments with many excluded AI
//     rollouts. Slim because excluded rows are expected to be a
//     small minority of the AI-rollout population.
//
// See docs/proposals/531-proposer-learns-from-accepted-rejected.md
// §10 Q3 (slice 2).
const ExcludeFromLearningSchema = `
ALTER TABLE rollouts ADD COLUMN exclude_from_learning INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_ai_verdicts_exclude ON rollouts(
	group_id,
	proposed_by,
	exclude_from_learning,
	COALESCE(approved_at, rejected_at) DESC
) WHERE proposed_by='ai' AND (approved_at IS NOT NULL OR rejected_at IS NOT NULL);

INSERT OR IGNORE INTO schema_version (version) VALUES (5);
`

// WebhookDeliveryDedupeSchema bumps the database to schema v6.
// Adds the webhook_delivery_dedupe table that the v0.89.30 (#649)
// GitHub webhook receiver consults to reject captured-and-replayed
// deliveries before they reach the audit-emit path.
//
//   - delivery_id (TEXT PRIMARY KEY) is the X-GitHub-Delivery UUID
//     GitHub stamps on every webhook delivery. Same UUID across
//     redeliveries of the same payload, so the PK guarantees the
//     row inserts exactly once per legitimate delivery.
//   - received_at (DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP) is
//     the timestamp the receiver first saw the delivery. The
//     idx_webhook_delivery_dedupe_received_at index keeps the GC
//     loop's `received_at < ?` sweep off a full-table scan.
//   - event_type (TEXT NOT NULL) is the X-GitHub-Event header value.
//     Captured at insert time so the audit payload's
//     original_received_at sidecar carries it without a second
//     header parse on replay.
//
// Threat closed: a compromised TLS terminator or intermediary proxy
// captures a legitimate signed delivery and replays it later. HMAC
// verification still passes (the body + secret produce the same
// signature) but the delivery_id row collision makes the receiver
// short-circuit to a replayed-audit + 200 ignored response. See
// docs/webhook-listener.md §"Slice 2 roadmap" for the design.
const WebhookDeliveryDedupeSchema = `
CREATE TABLE IF NOT EXISTS webhook_delivery_dedupe (
  delivery_id TEXT PRIMARY KEY,
  received_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  event_type TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_dedupe_received_at ON webhook_delivery_dedupe(received_at);
INSERT OR IGNORE INTO schema_version (version) VALUES (6);
`

// DiscoveryVerdictScopeIndexSchema bumps the database to schema v7.
// v0.89.36 (#655 Stream 53, #531 slice 2 chunk 3) — the discovery
// proposer's ListDiscoveryVerdicts query unions
// recommendation.pr_merged AND recommendation.pr_closed_not_merged
// audit rows. The v6 idx_audit_pr_merged_scope partial index was
// scoped to pr_merged only; we drop it and recreate it as a wider
// partial index covering both event types so the new SQL stays a
// single ranged scan.
//
// Forward-only migration: the drop+create is safe because the index
// is partial and small. Existing rows for pr_merged remain indexed
// under the new name; the new pr_closed_not_merged rows join the
// same index.
//
// See docs/proposals/531-proposer-learning-slice2.md §5.2.
const DiscoveryVerdictScopeIndexSchema = `
DROP INDEX IF EXISTS idx_audit_pr_merged_scope;
CREATE INDEX IF NOT EXISTS idx_audit_recommendation_verdict_scope
    ON audit_events(event_type, timestamp DESC)
    WHERE event_type IN ('recommendation.pr_merged',
                         'recommendation.pr_closed_not_merged');
INSERT OR IGNORE INTO schema_version (version) VALUES (7);
`

// IaCRecommendationVerdictsSchema bumps the database to schema v8.
// v0.89.37 (#656 Stream 54, #531 slice 2 chunk 4) — introduces the
// iac_recommendation_verdicts table that carries the operator-set
// exclusion flag for discovery recommendations the operator has
// explicitly suppressed without ever opening a PR ("Don't propose
// this again" affordance on the Recommendations tab).
//
// recommendation_id is the deterministic ID the discovery proposer
// assigns when it emits a recommendation; the row is created lazily
// on the first exclude click and updated in place on subsequent
// toggles. resource_id is nullable — a NULL row means the operator
// excluded the entire kind at that scope; a populated row scopes
// the exclusion to a single resource (the §11 Q4 distinction the
// prompt renderer surfaces with different instruction text).
//
// excluded_at + excluded_by are populated only while
// exclude_from_learning=1; they're cleared on a transition to 0 so
// the row's audit story stays unambiguous. The store layer enforces
// this; the SQL CHECK constraint is not strict because SQLite
// CHECK constraints can't reference DEFAULT-stamped values without
// significant ceremony, and the application layer is the only
// writer.
//
// idx_iac_rec_verdicts_scope is the discovery proposer's bridge
// lookup index: the bridge sweeps every (connection_id, account_id,
// region) tuple for excluded rows whenever it assembles verdicts.
// Including exclude_from_learning in the index keeps the partial
// scan to just the rows that contribute signal.
//
// See docs/proposals/531-proposer-learning-slice2.md §4.2 and §5.2.
const IaCRecommendationVerdictsSchema = `
CREATE TABLE IF NOT EXISTS iac_recommendation_verdicts (
    recommendation_id      TEXT PRIMARY KEY,
    connection_id          TEXT NOT NULL,
    account_id             TEXT NOT NULL,
    region                 TEXT NOT NULL,
    recommendation_kind    TEXT NOT NULL,
    resource_id            TEXT,
    exclude_from_learning  INTEGER NOT NULL DEFAULT 0,
    excluded_at            TIMESTAMP,
    excluded_by            TEXT,
    created_at             TIMESTAMP NOT NULL,
    updated_at             TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_iac_rec_verdicts_scope
    ON iac_recommendation_verdicts(connection_id, account_id, region, exclude_from_learning);
INSERT OR IGNORE INTO schema_version (version) VALUES (8);
`

// CheckRunStateSchema bumps the database to schema v9.
// v0.89.42 (#662 Stream 60, slice 1 chunk 1 of the GitHub Checks
// API back-signal arc) — adds 5 optional columns to the existing
// iac_recommendation_verdicts table so the durable check-run state
// for a recommendation lives on the same row as its operator-set
// exclusion + verdict-learning history.
//
// Column-by-column rationale (see design doc §6.1 + §11 Q3):
//
//   - check_run_id (INTEGER, nullable): the int64 GitHub assigns on
//     the create POST. NULL when the row was created by the chunk-4
//     exclusion path before any PR was opened, or while a future
//     reconciliation job has not yet stamped the late-arriving id.
//   - check_run_head_sha (TEXT, nullable): the commit SHA the check
//     run was created against. §7.2 of the design doc names "force-
//     pushed head SHA" — slice 1 stays pinned to the original SHA
//     even when GitHub's HEAD moves.
//   - check_run_status (TEXT, nullable): "queued" | "in_progress" |
//     "completed" per the Checks API. NULL = "no check run on this
//     row yet" — distinct from "" which would be a write the
//     application layer never produces.
//   - check_run_conclusion (TEXT, nullable): "success" | "failure" |
//     "neutral" per the Checks API. NULL while status is
//     "in_progress"; populated when status transitions to
//     "completed". Conclusion-without-completed is invalid per the
//     GitHub API and the application layer enforces the pairing.
//   - check_run_updated_at (TIMESTAMP, nullable): the timestamp of
//     the last successful create / patch on the row. Used by a
//     future slice-2 reconciliation job that compares stored state
//     against the audit log on startup.
//
// All 5 columns are nullable so rows the chunk-4 exclusion handler
// has been writing since v0.89.37 keep round-tripping unchanged.
// The shape lets the chunk-2 bridge integration upsert
// check_run_id + head_sha + status on PR open, and the chunk-3
// webhook handler patch status + conclusion on PR merge / close,
// without ever needing to mutate the chunk-4 exclude_from_learning
// column.
//
// See docs/proposals/checks-api-back-signal.md §6.1, §7, §10
// contract item 3, and §11 open question 3.
const CheckRunStateSchema = `
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_owner TEXT;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_repo TEXT;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_id INTEGER;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_head_sha TEXT;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_status TEXT;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_conclusion TEXT;
ALTER TABLE iac_recommendation_verdicts ADD COLUMN check_run_updated_at TIMESTAMP;
INSERT OR IGNORE INTO schema_version (version) VALUES (9);
`

// TraceResourceSeenSchema bumps the database to schema v10.
// v0.89.74 (#705 Stream 103, slice 1 chunk 1 of the Trace integration
// arc) — adds the trace_resource_seen table that the new
// internal/traceindex package flushes to every 30s.
//
// The table stores one row per resource the OTLP receiver has seen
// emit spans recently, keyed by the §3 fallback-chain resource_key.
// span_count_24h is the rolling counter the chunk-3 Discovery
// dashboard's TRACE COVERAGE panel reads. attributes_json carries
// the latest resource-attribute snapshot for the diagnostic UI; per
// §12 of the design doc the field stores RESOURCE attributes only
// (host.id, cloud.provider, k8s.cluster.name, etc.), explicitly NOT
// span attributes — the threat model pins the no-span-content
// guarantee on this column.
//
// idx_trace_resource_seen_provider_scope backs the per-provider
// coverage rollup (the Discovery dashboard's per-card breakdown) and
// the per-scope inventory join (chunk 4's last_seen_at column on
// DiscoveryAWS / GCP / Azure / OCI). idx_trace_resource_seen_last_seen
// backs the LRU eviction sweep — when the row count exceeds
// SQUADRON_TRACEINDEX_MAX_ROWS the storage layer DELETEs the oldest
// last_seen_at rows until the count drops to the cap.
//
// See docs/proposals/trace-integration-slice1.md §4.
const TraceResourceSeenSchema = `
CREATE TABLE IF NOT EXISTS trace_resource_seen (
    resource_key             TEXT PRIMARY KEY,
    provider                 TEXT NOT NULL,
    scope_id                 TEXT,
    resource_id_hint         TEXT,
    service_name             TEXT,
    first_seen_at            TIMESTAMP NOT NULL,
    last_seen_at             TIMESTAMP NOT NULL,
    span_count_24h           INTEGER NOT NULL,
    root_span_count_24h      INTEGER NOT NULL,
    attributes_json          TEXT,
    match_confidence         TEXT NOT NULL,
    updated_at               TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trace_resource_seen_provider_scope
    ON trace_resource_seen(provider, scope_id);
CREATE INDEX IF NOT EXISTS idx_trace_resource_seen_last_seen
    ON trace_resource_seen(last_seen_at);
INSERT OR IGNORE INTO schema_version (version) VALUES (10);
`

// Migrations is a list of all schema migrations
var Migrations = []string{
	InitialSchema,
	AlertRulesSchema,
	RecommendationDismissalsSchema,
	AIVerdictsSchema,
	ExcludeFromLearningSchema,
	WebhookDeliveryDedupeSchema,
	DiscoveryVerdictScopeIndexSchema,
	IaCRecommendationVerdictsSchema,
	CheckRunStateSchema,
	TraceResourceSeenSchema,
}
