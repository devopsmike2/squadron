// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package postgres is the opt-in Postgres/Aurora application-store backend
// (ADR 0033). It is the HA / org-scale alternative to the default embedded
// SQLite store, selected via storage.app.type: postgres + storage.app.dsn.
//
// It is built in tested slices. Entity coverage grows until Storage satisfies
// the full types.ApplicationStore interface, at which point it is wired into
// the store factory and added to AllStorageTypes. Until then it is deliberately
// NOT selectable in a running deployment, so a half-implemented backend can
// never be chosen by config. Tests run against a real Postgres in CI (a
// `services: postgres` container) and skip locally unless TEST_POSTGRES_DSN is
// set.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	// pgx stdlib driver, registered under the name "pgx".
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Storage is the Postgres-backed application store.
type Storage struct {
	db *sql.DB
}

// Compile-time proof that the Postgres backend satisfies the FULL
// types.ApplicationStore interface (ADR 0033 slice 6). This assertion is the
// completeness gate: if any interface method is unimplemented the package fails
// to build. It does NOT wire the backend into the store factory / AllStorageTypes
// — that (plus the SQLite→Postgres migration guide) is a separate next slice.
var _ types.ApplicationStore = (*Storage)(nil)

// Open connects to Postgres via the pgx stdlib driver, verifies the connection,
// and applies the schema. dsn is a standard Postgres connection string, e.g.
// "postgres://user:pass@host:5432/squadron?sslmode=require".
func Open(ctx context.Context, dsn string) (*Storage, error) {
	if dsn == "" {
		return nil, errors.New("postgres: dsn is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	s := &Storage{db: db}
	if err := s.initSchema(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: init schema: %w", err)
	}
	return s, nil
}

// Close releases the connection pool.
func (s *Storage) Close() error { return s.db.Close() }

// Ping is the lightweight readiness primitive (backs GET /readyz).
func (s *Storage) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// schemaSQL is the Postgres-dialect schema. Tables are added as their entity
// slices land (ADR 0033). CREATE TABLE IF NOT EXISTS keeps init idempotent.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS groups (
    id                            TEXT PRIMARY KEY,
    name                          TEXT NOT NULL,
    labels                        JSONB NOT NULL DEFAULT '{}'::jsonb,
    require_approval              BOOLEAN NOT NULL DEFAULT FALSE,
    require_approval_for_rollback BOOLEAN NOT NULL DEFAULT FALSE,
    change_windows                TEXT NOT NULL DEFAULT '',
    learn_from_verdicts           BOOLEAN NOT NULL DEFAULT TRUE,
    tenant_id                     TEXT NOT NULL DEFAULT '',
    created_at                    TIMESTAMPTZ NOT NULL,
    updated_at                    TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    labels           JSONB NOT NULL DEFAULT '{}'::jsonb,
    status           TEXT NOT NULL DEFAULT 'offline',
    last_seen        TIMESTAMPTZ,
    group_id         TEXT,
    group_name       TEXT,
    version          TEXT NOT NULL DEFAULT '',
    capabilities     JSONB NOT NULL DEFAULT '[]'::jsonb,
    effective_config TEXT NOT NULL DEFAULT '',
    delivered_config_hash TEXT NOT NULL DEFAULT '',
    discovery_source TEXT NOT NULL DEFAULT 'opamp',
    deleted_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agents_group_id ON agents(group_id);
CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);
-- ADR 0040: agents gain a delivered_config_hash column (the confignorm hash of
-- the config the agent has confirmed applied — the DELIVERED signal supervised-
-- agent drift detection keys on). Added as an idempotent ALTER so Postgres
-- databases created before 0040 upgrade in place; fresh databases already carry
-- it from the CREATE TABLE above.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS delivered_config_hash TEXT NOT NULL DEFAULT '';

-- configs carries agent config-intent (agent_id set) OR group config-assignment
-- (group_id set). Referential integrity for a deleted group (don't dangle member
-- agents' group_id, don't orphan group configs) is enforced in APPLICATION code,
-- transactionally, in DeleteGroup — NOT via DB foreign keys. Deliberate:
--   * No in-place FK ALTER on an existing PVC (validating a new FK against live
--     data can fail on pre-existing dangling rows), and adding FKs only to the
--     fresh schema would silently diverge fresh vs migrated deployments.
--   * The store intentionally supports inserting a config keyed to an agent/group
--     id without a co-located parent row (e.g. isolated config CRUD); a configs->
--     agents/groups FK would reject those inserts.
--   * An agents.group_id -> groups FK is a non-starter regardless: the write path
--     stores '' (not NULL) for an ungrouped agent, which an FK would reject.
-- The transactional DeleteGroup cleanup delivers the SET-NULL fallback safely on
-- existing and fresh deployments alike, which is the load-bearing guarantee.
CREATE TABLE IF NOT EXISTS configs (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL DEFAULT '',
    agent_id    TEXT,
    group_id    TEXT,
    config_hash TEXT NOT NULL DEFAULT '',
    content     TEXT NOT NULL DEFAULT '',
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL,
    -- Soft-archive tombstone: configs are versioned and immutable (hard delete
    -- stays a 501), so archiving hides a superseded/residue config from the
    -- default listing without destroying the row. NULL = active. Added as an
    -- idempotent ALTER below so Postgres DBs created before this upgrade in place.
    archived_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_configs_agent_id ON configs(agent_id);
CREATE INDEX IF NOT EXISTS idx_configs_group_id ON configs(group_id);
CREATE INDEX IF NOT EXISTS idx_configs_created_at ON configs(created_at DESC);
ALTER TABLE configs ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS rollouts (
    id                    TEXT PRIMARY KEY,
    name                  TEXT NOT NULL,
    group_id              TEXT NOT NULL,
    target_config_id      TEXT NOT NULL,
    previous_config_id    TEXT,
    -- JSONB so structured fields round-trip naturally. abort_criteria carries
    -- the ADR 0034 SLOBurn criterion (types.RolloutSLOBurnCriterion) inline.
    stages                JSONB NOT NULL,
    abort_criteria        JSONB NOT NULL,
    notification_url      TEXT,
    state                 TEXT NOT NULL,
    current_stage         INTEGER NOT NULL DEFAULT 0,
    stage_started_at      TIMESTAMPTZ,
    abort_reason          TEXT,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL,
    completed_at          TIMESTAMPTZ,
    -- v0.47 approval workflow columns.
    require_approval      BOOLEAN NOT NULL DEFAULT FALSE,
    requested_by          TEXT,
    approved_by           TEXT,
    approved_at           TIMESTAMPTZ,
    rejected_by           TEXT,
    rejected_at           TIMESTAMPTZ,
    approval_notes        TEXT,
    -- ADR 0029 — N-of-M approvals threshold. DEFAULT 1 reproduces the v0.47
    -- two-person behavior (one distinct approver flips pending_approval → pending).
    required_approvals    INTEGER NOT NULL DEFAULT 1,
    -- v0.49 blackout columns.
    last_blackout_reason  TEXT,
    last_blackout_at      TIMESTAMPTZ,
    -- v0.53 proposal provenance.
    proposed_by           TEXT NOT NULL DEFAULT 'operator',
    proposal_reasoning    TEXT,
    evidence_refs         JSONB,
    -- v0.60 rollback chain.
    rolled_back_from_id   TEXT,
    -- v0.69 plan grouping.
    plan_id               TEXT,
    plan_step_index       INTEGER NOT NULL DEFAULT 0,
    -- v0.89.14 action steps.
    step_kind             TEXT,
    action_request_id     TEXT,
    -- v0.89.26 (#642) per-rollout exclude-from-learning flag.
    exclude_from_learning BOOLEAN NOT NULL DEFAULT FALSE,
    -- Optimistic-concurrency counter, bumped on every UpdateRollout.
    version               INTEGER NOT NULL DEFAULT 1,
    -- v0.90 (ADR 0007) cumulative pushed-agent set.
    pushed_agent_ids      JSONB
);
CREATE INDEX IF NOT EXISTS idx_rollouts_state ON rollouts(state);
CREATE INDEX IF NOT EXISTS idx_rollouts_group ON rollouts(group_id, created_at DESC);

-- ADR 0029 — N-of-M rollout approvals append-log. One row per distinct
-- approver; the composite PK enforces distinctness so COUNT(*) is the
-- distinct-approver count.
CREATE TABLE IF NOT EXISTS rollout_approvals (
    rollout_id        TEXT NOT NULL,
    approver          TEXT NOT NULL,
    approver_token_id TEXT,
    notes             TEXT,
    approved_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (rollout_id, approver)
);

-- ADR 0033, slice 4 — saved queries. CRUD-only entity mirroring the sqlite
-- backend. tags is JSONB (structured, never hashed). As with agents/rollouts,
-- OSS tenant scoping is intentionally omitted (types.SavedQuery carries no
-- tenant field); OSS is single-tenant. description NOT NULL DEFAULT '' so an
-- empty Go string round-trips to "" (not NULL), matching the sqlite roundtrip.
CREATE TABLE IF NOT EXISTS saved_queries (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    query       TEXT NOT NULL,
    tags        JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_saved_queries_updated_at ON saved_queries(updated_at);

-- ADR 0033, slice 4 — alert rules. CRUD-only entity mirroring the sqlite
-- backend. enabled is a native BOOLEAN (sqlite stored 0/1); threshold_value is
-- DOUBLE PRECISION (float64). The sqlite backend enforces a per-tenant
-- UNIQUE(tenant_id, name); the OSS single-tenant port keeps the equivalent
-- UNIQUE(name). As with saved queries, OSS tenant scoping is omitted
-- (types.AlertRule carries no tenant field). description / webhook_url are
-- NOT NULL DEFAULT '' so empty Go strings round-trip to "".
CREATE TABLE IF NOT EXISTS alert_rules (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    query              TEXT NOT NULL,
    threshold_operator TEXT NOT NULL,
    threshold_value    DOUBLE PRECISION NOT NULL,
    interval_seconds   INTEGER NOT NULL,
    severity           TEXT NOT NULL,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    webhook_url        TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL,
    UNIQUE (name)
);
CREATE INDEX IF NOT EXISTS idx_alert_rules_enabled ON alert_rules(enabled);

-- ADR 0038, first slice — automations (condition-triggered auto-remediation).
-- CRUD-only entity mirroring the sqlite backend. enabled / dry_run are native
-- BOOLEAN (sqlite stored 0/1); agent_id / group_id are nullable (exactly one
-- set per row, enforced at the service layer, not the schema). As with alert
-- rules, OSS tenant scoping is omitted (single-tenant); the sqlite backend's
-- tenant predicate is a no-op for OSS so behavior is identical.
CREATE TABLE IF NOT EXISTS automations (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    agent_id         TEXT,
    group_id         TEXT,
    trigger_type     TEXT NOT NULL,
    action_type      TEXT NOT NULL,
    cooldown_seconds INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL DEFAULT 0,
    dry_run          BOOLEAN NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_automations_enabled ON automations(enabled);

-- ADR 0027 / ADR 0033 slice 4 — per-tenant tamper-evident audit hash-chain.
-- HIGH-STAKES: the chain hash (internal/audit/chain) covers the immutable
-- content columns + tenant_id + seq, chained via prev_hash. This port MUST
-- persist/return exactly what that shared package consumes so append + verify
-- behave BYTE-IDENTICALLY to the sqlite backend.
--
--   * seq        — per-tenant monotonic ordering key the chain walk relies on
--                  (BIGINT = int64). Nullable to mirror sqlite's post-hoc
--                  ALTER (pre-chain rows carry NULL; the walk skips seq NULL).
--   * prev_hash  — links to the prior row's row_hash.
--   * row_hash   — the stored SHA-256 chain hash, re-derived on verify.
--   * payload    — stored as TEXT, NOT JSONB, ON PURPOSE: the chain hashes the
--                  RAW payload string byte-for-byte. JSONB would re-serialize
--                  it (key reorder / whitespace normalization) and the read-back
--                  would no longer match what was hashed, silently breaking the
--                  chain. TEXT preserves the exact bytes the append path hashed.
CREATE TABLE IF NOT EXISTS audit_events (
    id                          TEXT PRIMARY KEY,
    timestamp                   TIMESTAMPTZ NOT NULL,
    actor                       TEXT NOT NULL,
    event_type                  TEXT NOT NULL,
    target_type                 TEXT NOT NULL,
    target_id                   TEXT,
    payload                     TEXT,
    tenant_id                   TEXT NOT NULL DEFAULT 'default',
    created_at                  TIMESTAMPTZ NOT NULL,
    action                      TEXT NOT NULL,
    seq                         BIGINT,
    prev_hash                   TEXT,
    row_hash                    TEXT,
    ai_explanation              TEXT,
    ai_explanation_model        TEXT,
    ai_explanation_generated_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_audit_events_timestamp ON audit_events(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_target ON audit_events(target_type, target_id, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_event_type ON audit_events(event_type);
CREATE INDEX IF NOT EXISTS idx_audit_events_actor ON audit_events(actor, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant ON audit_events(tenant_id);
-- The ordering key the chain walk seeks: (tenant_id, seq).
CREATE INDEX IF NOT EXISTS idx_audit_events_chain ON audit_events(tenant_id, seq);

-- ADR 0027 slice 2 — retention/chain reconciliation checkpoints. Each prune
-- records the pruned head (checkpoint_seq + checkpoint_row_hash) so
-- VerifyAuditChain can positively anchor the first surviving row. sealed_sig is
-- nullable + unused in OSS (enterprise fills it later). Keyed by
-- (tenant_id, checkpoint_seq), mirroring sqlite exactly.
CREATE TABLE IF NOT EXISTS audit_chain_checkpoints (
    tenant_id           TEXT        NOT NULL,
    checkpoint_seq      BIGINT      NOT NULL,
    checkpoint_row_hash TEXT        NOT NULL,
    rows_pruned         BIGINT      NOT NULL,
    kind                TEXT        NOT NULL DEFAULT 'retention-cut',
    created_at          TIMESTAMPTZ NOT NULL,
    sealed_sig          TEXT,
    PRIMARY KEY (tenant_id, checkpoint_seq)
);
CREATE INDEX IF NOT EXISTS idx_audit_chain_checkpoints_tenant ON audit_chain_checkpoints(tenant_id, checkpoint_seq DESC);

-- ADR 0033, slice 5 (OPERATIONS cluster) — action runner registrations
-- (v0.53 Move 2). CRUD + revoke, semantics mirror the sqlite backend.
-- public_key_pem / capabilities_json are persisted VERBATIM as TEXT: they are
-- opaque blobs the crypto/capability logic in internal/actions parses at use
-- time; the store never reserializes them. OSS is single-tenant so runner_id
-- alone is the PK (the sqlite composite (tenant_id, runner_id) collapses to this
-- when every row is DefaultTenant). revoked_at is nullable — a revoked runner
-- stays for audit history but is excluded from dispatch.
CREATE TABLE IF NOT EXISTS action_runner_registrations (
    runner_id         TEXT PRIMARY KEY,
    hostname          TEXT NOT NULL,
    public_key_pem    TEXT NOT NULL,
    capabilities_json TEXT NOT NULL,
    registered_at     TIMESTAMPTZ NOT NULL,
    last_seen_at      TIMESTAMPTZ NOT NULL,
    revoked_at        TIMESTAMPTZ
);

-- ADR 0033, slice 5 — signed action requests (v0.53 Move 2). parameters_json and
-- signature are persisted VERBATIM as TEXT: the runner-side crypto verifies the
-- exact bytes, so (like the audit payload) JSONB is WRONG here — it would reorder
-- keys / normalize whitespace and break signature verification. Two rows exist
-- per approved action (phase=dry_run + phase=execute). Nullable optionals mirror
-- the sqlite NULL columns.
CREATE TABLE IF NOT EXISTS action_requests (
    id                    TEXT PRIMARY KEY,
    proposal_id           TEXT,
    runner_id             TEXT NOT NULL,
    action_type           TEXT NOT NULL,
    parameters_json       TEXT NOT NULL,
    signature             TEXT NOT NULL,
    phase                 TEXT NOT NULL,
    status                TEXT NOT NULL,
    denied_for            TEXT,
    dry_run_output_json   TEXT,
    execution_output_json TEXT,
    issued_at             TIMESTAMPTZ NOT NULL,
    expires_at            TIMESTAMPTZ NOT NULL,
    started_at            TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_action_requests_proposal ON action_requests(proposal_id);
CREATE INDEX IF NOT EXISTS idx_action_requests_runner ON action_requests(runner_id);
CREATE INDEX IF NOT EXISTS idx_action_requests_status ON action_requests(status);

-- ADR 0033, slice 5 — deploy targets (v0.34 GitHub Actions integration).
-- encrypted_credential is nonce(24)||ciphertext stored as BYTEA (the deploy
-- package never persists plaintext); default_inputs is JSONB (structured
-- map[string]string, never hashed). tenant_id IS persisted + surfaced here
-- because types.DeployTarget carries a TenantID field that background bridges
-- read (e.g. the GHA walker stamping expected_agents); OSS single-tenant defaults
-- it to 'default', mirroring the sqlite system-context insert.
CREATE TABLE IF NOT EXISTS deploy_targets (
    id                   TEXT PRIMARY KEY,
    name                 TEXT NOT NULL,
    provider             TEXT NOT NULL DEFAULT 'github',
    github_owner         TEXT NOT NULL DEFAULT '',
    github_repo          TEXT NOT NULL DEFAULT '',
    github_workflow      TEXT NOT NULL DEFAULT '',
    github_branch        TEXT NOT NULL DEFAULT 'main',
    encrypted_credential BYTEA,
    default_inputs       JSONB NOT NULL DEFAULT '{}'::jsonb,
    config_id            TEXT NOT NULL DEFAULT '',
    inventory_path       TEXT NOT NULL DEFAULT '',
    tenant_id            TEXT NOT NULL DEFAULT 'default',
    created_at           TIMESTAMPTZ NOT NULL,
    updated_at           TIMESTAMPTZ NOT NULL
);

-- ADR 0033, slice 5 — deploy runs (one workflow_dispatch firing). inputs and
-- expected_hosts are JSONB (structured). github_run_id is BIGINT (int64), starts
-- at 0 until resolved after the first poll (workflow_dispatch returns 204 with no
-- id). expected_hosts is the set registered into expected_agents on success,
-- closing the v0.32 inventory loop. No tenant_id column: types.DeployRun carries
-- no tenant field (matches the agents/configs ports).
CREATE TABLE IF NOT EXISTS deploy_runs (
    id                 TEXT PRIMARY KEY,
    target_id          TEXT NOT NULL,
    requested_by       TEXT NOT NULL DEFAULT '',
    requested_at       TIMESTAMPTZ NOT NULL,
    inputs             JSONB NOT NULL DEFAULT '{}'::jsonb,
    github_run_id      BIGINT NOT NULL DEFAULT 0,
    github_run_url     TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'queued',
    conclusion         TEXT NOT NULL DEFAULT '',
    completed_at       TIMESTAMPTZ,
    expected_hosts     JSONB NOT NULL DEFAULT '[]'::jsonb,
    verification_state TEXT NOT NULL DEFAULT '',
    verified_at        TIMESTAMPTZ,
    notes              TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_deploy_runs_target_time ON deploy_runs(target_id, requested_at);
CREATE INDEX IF NOT EXISTS idx_deploy_runs_status ON deploy_runs(status);

-- ADR 0033, slice 5 — expected agents (v0.32 inventory reconciliation). hostname
-- is the natural key. labels is JSONB. No tenant_id column: types.ExpectedAgent
-- carries no tenant field; the sqlite composite (tenant_id, hostname) PK collapses
-- to hostname alone in OSS single-tenant.
CREATE TABLE IF NOT EXISTS expected_agents (
    hostname       TEXT PRIMARY KEY,
    labels         JSONB NOT NULL DEFAULT '{}'::jsonb,
    source         TEXT NOT NULL DEFAULT '',
    expected_since TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    notes          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_expected_agents_source ON expected_agents(source);

-- ============================================================================
-- ADR 0033, slice 6 — remaining entity ports. Semantics mirror the sqlite
-- backend EXACTLY; see the per-entity Go files for the method-level contracts.
-- OSS is single-tenant: as with the earlier ports, tenant scoping is omitted for
-- entities whose types carry no tenant field (a later consolidated pass adds the
-- columns). api_tokens is the one exception — types.APIToken carries TenantID,
-- read back by Get/List — so it persists tenant_id (default 'default') without a
-- scoping predicate, mirroring the deploy_targets port. The discovery-verdict
-- reads run against the already-tenant-scoped audit_events table.
-- ============================================================================

-- API tokens (bearer auth). hash is the sha256 hex digest (UNIQUE). scopes is a
-- JSON-array string persisted VERBATIM as TEXT ('[]' = legacy full-access) so the
-- empty-array sentinel round-trips exactly as the sqlite backend stores it; the
-- service layer parses it. connection_id is omitted (enterprise-only; never read
-- or written by any ApplicationStore method in OSS).
CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    label        TEXT NOT NULL,
    hash         TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL DEFAULT '[]',
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    tenant_id    TEXT NOT NULL DEFAULT 'default'
);

-- Recommendation dismissals (v0.25). recommendation_id is the engine's
-- deterministic hash; DismissRecommendation upserts on it, RestoreRecommendation
-- is a plain DELETE. reason is stored as TEXT (empty string, not NULL, matching
-- the sqlite insert of d.Reason); the list COALESCEs it defensively.
CREATE TABLE IF NOT EXISTS recommendation_dismissals (
    recommendation_id TEXT PRIMARY KEY,
    dismissed_at      TIMESTAMPTZ NOT NULL,
    dismissed_by      TEXT NOT NULL DEFAULT 'system',
    reason            TEXT
);
CREATE INDEX IF NOT EXISTS idx_rec_dismissals_dismissed_at ON recommendation_dismissals(dismissed_at);

-- Recommendation outcomes (v0.28 retrospective savings). Frozen snapshot columns
-- + running observation columns; only the observation columns mutate on update.
-- byte-rate columns are BIGINT (int64); USD columns are DOUBLE PRECISION.
CREATE TABLE IF NOT EXISTS recommendation_outcomes (
    id                                 TEXT PRIMARY KEY,
    recommendation_id                  TEXT NOT NULL,
    applied_at                         TIMESTAMPTZ NOT NULL,
    applied_by                         TEXT NOT NULL DEFAULT 'system',
    title                              TEXT NOT NULL,
    category                           TEXT NOT NULL,
    signal                             TEXT NOT NULL DEFAULT '',
    attribute_key                      TEXT NOT NULL DEFAULT '',
    baseline_bytes_per_hour            BIGINT NOT NULL DEFAULT 0,
    est_savings_per_month_usd_at_apply DOUBLE PRECISION NOT NULL DEFAULT 0,
    last_observed_bytes_per_hour       BIGINT NOT NULL DEFAULT 0,
    last_observed_at                   TIMESTAMPTZ,
    realized_savings_per_month_usd     DOUBLE PRECISION NOT NULL DEFAULT 0,
    status                             TEXT NOT NULL DEFAULT 'pending'
);
CREATE INDEX IF NOT EXISTS idx_rec_outcomes_applied_at ON recommendation_outcomes(applied_at);
CREATE INDEX IF NOT EXISTS idx_rec_outcomes_status ON recommendation_outcomes(status);

-- Cost-spike events (v0.29). One row per detected anomaly; open spikes have
-- ended_at IS NULL. attribution_json is a freeform TEXT blob captured at fire
-- time. USD/pct columns are DOUBLE PRECISION.
CREATE TABLE IF NOT EXISTS cost_spike_events (
    id                      TEXT PRIMARY KEY,
    started_at              TIMESTAMPTZ NOT NULL,
    ended_at                TIMESTAMPTZ,
    severity                TEXT NOT NULL DEFAULT 'warn',
    signal                  TEXT NOT NULL DEFAULT '',
    baseline_monthly_usd    DOUBLE PRECISION NOT NULL DEFAULT 0,
    peak_monthly_usd        DOUBLE PRECISION NOT NULL DEFAULT 0,
    peak_pct_above_baseline DOUBLE PRECISION NOT NULL DEFAULT 0,
    attribution_json        TEXT NOT NULL DEFAULT '',
    acknowledged_at         TIMESTAMPTZ,
    acknowledged_by         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_cost_spikes_started_at ON cost_spike_events(started_at);
CREATE INDEX IF NOT EXISTS idx_cost_spikes_open ON cost_spike_events(ended_at) WHERE ended_at IS NULL;

-- Webhook delivery dedupe (v0.89.30, #649). delivery_id is the X-GitHub-Delivery
-- UUID (PK); received_at defaults to now() so the DB stamps the first-seen time,
-- which RecordWebhookDelivery reads back on both the fresh and replay paths. This
-- table is GLOBAL (no tenant column) — mirrors the sqlite schema.
CREATE TABLE IF NOT EXISTS webhook_delivery_dedupe (
    delivery_id TEXT PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_webhook_delivery_dedupe_received_at ON webhook_delivery_dedupe(received_at);

-- IaC recommendation verdicts (v0.89.37 chunk 4 + v0.89.42 chunk 1). Carries the
-- operator-set exclusion flag AND the durable GitHub Checks-API check-run state
-- on ONE row keyed by recommendation_id. exclude_from_learning is a native
-- BOOLEAN (sqlite stored 0/1); resource_id + the check_run_* columns are nullable
-- (NULL = "no check run on this row yet", distinct from an empty-string write).
-- check_run_id is BIGINT (the int64 GitHub assigns).
CREATE TABLE IF NOT EXISTS iac_recommendation_verdicts (
    recommendation_id     TEXT PRIMARY KEY,
    connection_id         TEXT NOT NULL,
    account_id            TEXT NOT NULL,
    region                TEXT NOT NULL,
    recommendation_kind   TEXT NOT NULL,
    resource_id           TEXT,
    exclude_from_learning BOOLEAN NOT NULL DEFAULT FALSE,
    excluded_at           TIMESTAMPTZ,
    excluded_by           TEXT,
    check_run_owner       TEXT,
    check_run_repo        TEXT,
    check_run_id          BIGINT,
    check_run_head_sha    TEXT,
    check_run_status      TEXT,
    check_run_conclusion  TEXT,
    check_run_updated_at  TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_iac_rec_verdicts_scope
    ON iac_recommendation_verdicts(connection_id, account_id, region, exclude_from_learning);

-- Incident drafts (SQ-3). One draft per action by default; the bridge dedups on
-- action_request_id. Optional string fields are nullable so they round-trip to ""
-- via nullString on write + COALESCE-free NullString scan on read.
CREATE TABLE IF NOT EXISTS incident_drafts (
    id                 TEXT PRIMARY KEY,
    action_request_id  TEXT,
    rollout_id         TEXT,
    status             TEXT NOT NULL,
    title              TEXT NOT NULL,
    body_markdown      TEXT NOT NULL,
    draft_content_json TEXT,
    provider           TEXT,
    external_id        TEXT,
    external_url       TEXT,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_incident_drafts_action ON incident_drafts(action_request_id);
CREATE INDEX IF NOT EXISTS idx_incident_drafts_rollout ON incident_drafts(rollout_id);
CREATE INDEX IF NOT EXISTS idx_incident_drafts_status ON incident_drafts(status);

-- Discovery scans (v0.89.250). One row per completed scan. regions + summary are
-- structured → JSONB; result_json is the full marshaled inventory, an opaque
-- string returned VERBATIM (TEXT), omitted from list responses. partial is a
-- native BOOLEAN.
CREATE TABLE IF NOT EXISTS discovery_scans (
    scan_id        TEXT PRIMARY KEY,
    provider       TEXT NOT NULL,
    scope_id       TEXT NOT NULL,
    regions        JSONB NOT NULL DEFAULT '[]'::jsonb,
    started_at     TIMESTAMPTZ NOT NULL,
    completed_at   TIMESTAMPTZ NOT NULL,
    partial        BOOLEAN NOT NULL DEFAULT FALSE,
    partial_reason TEXT,
    summary        JSONB NOT NULL DEFAULT '{}'::jsonb,
    result_json    TEXT,
    created_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_discovery_scans_scope ON discovery_scans(provider, scope_id, started_at DESC);

-- Trace resource index (v0.89.74). One row per resource the OTLP receiver has
-- seen emit spans recently, keyed by the fallback-chain resource_key. span_count
-- columns are BIGINT rolling counters accumulated across the flush cadence via the
-- upsert; attributes_json carries RESOURCE attributes only (per the design doc's
-- no-span-content guarantee), stored VERBATIM as TEXT. Nullable optionals
-- (scope_id, resource_id_hint, service_name, attributes_json) round-trip via
-- COALESCE on read.
CREATE TABLE IF NOT EXISTS trace_resource_seen (
    resource_key        TEXT PRIMARY KEY,
    provider            TEXT NOT NULL,
    scope_id            TEXT,
    resource_id_hint    TEXT,
    service_name        TEXT,
    first_seen_at       TIMESTAMPTZ NOT NULL,
    last_seen_at        TIMESTAMPTZ NOT NULL,
    span_count_24h      BIGINT NOT NULL,
    root_span_count_24h BIGINT NOT NULL,
    attributes_json     TEXT,
    match_confidence    TEXT NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trace_resource_seen_provider_scope ON trace_resource_seen(provider, scope_id);
CREATE INDEX IF NOT EXISTS idx_trace_resource_seen_last_seen ON trace_resource_seen(last_seen_at);

-- SIEM destinations (v0.50 audit export). secret is the encrypted-at-rest
-- credential (BYTEA), never returned in plaintext to the API layer. enabled is a
-- native BOOLEAN. Operational status columns (last_event_sent_at / last_error /
-- last_error_at) are written by the narrow UpdateSiemDestinationStatus path.
CREATE TABLE IF NOT EXISTS siem_destinations (
    id                       TEXT PRIMARY KEY,
    name                     TEXT NOT NULL,
    type                     TEXT NOT NULL,
    url                      TEXT NOT NULL,
    secret                   BYTEA,
    enabled                  BOOLEAN NOT NULL DEFAULT TRUE,
    event_type_prefixes_json TEXT NOT NULL DEFAULT '[]',
    last_event_sent_at       TIMESTAMPTZ,
    last_error               TEXT,
    last_error_at            TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL,
    updated_at               TIMESTAMPTZ NOT NULL
);

-- Connection registry (HA S3b, ADR 0035). One row per agent recording which
-- Squadron instance currently owns that agent's OpAMP WebSocket. Semantics
-- mirror the sqlite backend EXACTLY. This table is SYSTEM-SCOPED: instance
-- identity is orthogonal to tenant, so it carries NO tenant_id column (unlike
-- the per-tenant tables) and the store methods apply no tenant predicate.
-- agent_id is the Squadron fleet id and the PRIMARY KEY — one owner per agent,
-- last-writer-wins on reconnect. idx_connection_registry_heartbeat backs the
-- ReclaimStale "last_heartbeat_at < $1" sweep.
CREATE TABLE IF NOT EXISTS connection_registry (
    agent_id          TEXT PRIMARY KEY,
    instance_id       TEXT NOT NULL,
    connected_at      TIMESTAMPTZ NOT NULL,
    last_heartbeat_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_connection_registry_heartbeat ON connection_registry(last_heartbeat_at);
`

// schemaInitLockKey is the fixed 64-bit key for the Postgres transaction-scoped
// advisory lock that serializes schema initialization (see initSchema).
//
// Concurrent Open() calls against the SAME database race in the system catalog:
// `CREATE TABLE IF NOT EXISTS` is NOT concurrency-safe, so when two processes
// create the same not-yet-existing table at once the loser fails with
// `duplicate key value violates unique constraint "pg_type_typname_nsp_index"`
// (SQLSTATE 23505) on the table's companion pg_type row. This flakes CI when the
// `postgres` and `applicationstore` test packages open the shared TEST_POSTGRES_DSN
// database concurrently. Every process initializing this schema takes the lock
// under the same key, so exactly one runs the DDL at a time.
//
// The value is an arbitrary but stable application-chosen constant: the FNV-1a
// 64-bit hash of "squadron.schema.init", reinterpreted as a signed int64
// (pg_advisory_xact_lock takes a bigint).
const schemaInitLockKey int64 = -1673074391011295357

// initSchema applies schemaSQL. It runs inside a single transaction that first
// takes a transaction-scoped advisory lock (pg_advisory_xact_lock) so concurrent
// initializations serialize instead of racing the catalog; the lock auto-releases
// on COMMIT/ROLLBACK. Postgres DDL is transactional, and every statement in
// schemaSQL is idempotent (IF NOT EXISTS), so the second (blocked) runner sees a
// fully-created schema and its DDL is a clean no-op. The normal single-process
// path is unchanged apart from the added serialization.
func (s *Storage) initSchema(ctx context.Context) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema init tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaInitLockKey); err != nil {
		return fmt.Errorf("acquire schema init advisory lock: %w", err)
	}
	if _, err = tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit schema init: %w", err)
	}
	return nil
}

// ============================================================================
// Groups (ADR 0033, slice 1). Semantics mirror the sqlite backend: a missing
// GetGroup returns (nil, nil); Update/Delete on a missing row returns a
// "group not found" error.
// ============================================================================

const groupColumns = `id, name, labels, require_approval, require_approval_for_rollback, change_windows, learn_from_verdicts, tenant_id, created_at, updated_at`

func (s *Storage) CreateGroup(ctx context.Context, g *types.Group) error {
	labels, err := json.Marshal(g.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO groups (`+groupColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		g.ID, g.Name, labels, g.RequireApproval, g.RequireApprovalForRollback,
		g.ChangeWindowsJSON, g.LearnFromVerdicts, g.TenantID, g.CreatedAt, g.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create group: %w", err)
	}
	return nil
}

func (s *Storage) GetGroup(ctx context.Context, id string) (*types.Group, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+groupColumns+` FROM groups WHERE id = $1`, id)
	g, err := scanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

func (s *Storage) ListGroups(ctx context.Context) ([]*types.Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+groupColumns+` FROM groups ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	var out []*types.Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Storage) UpdateGroup(ctx context.Context, g *types.Group) error {
	labels, err := json.Marshal(g.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE groups SET name=$2, labels=$3, require_approval=$4,
			require_approval_for_rollback=$5, change_windows=$6,
			learn_from_verdicts=$7, updated_at=$8
		WHERE id=$1`,
		g.ID, g.Name, labels, g.RequireApproval, g.RequireApprovalForRollback,
		g.ChangeWindowsJSON, g.LearnFromVerdicts, g.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("group not found: %s", g.ID)
	}
	return nil
}

// DeleteGroup removes a group and cleans up references in one transaction so a
// deleted group can never brick or dangle an agent (configs FK ON DELETE
// hardening). Member agents' group is nulled (fall back to no-group resolution,
// NOT deleted) and the group's group-scoped configs are removed. This runs in
// application code — the Postgres agents/configs tables carry no FK on the live
// pilot PVC (adding one in place is unsafe; see the schema note), so the cleanup
// is done here to be correct on existing and fresh deployments alike.
func (s *Storage) DeleteGroup(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete group tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE agents SET group_id=NULL, group_name=NULL, updated_at=now() WHERE group_id=$1`, id); err != nil {
		return fmt.Errorf("clear agent group membership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM configs WHERE group_id=$1`, id); err != nil {
		return fmt.Errorf("delete group configs: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("group not found: %s", id)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete group: %w", err)
	}
	return nil
}

// scanner unifies *sql.Row and *sql.Rows for scanGroup.
type scanner interface{ Scan(dest ...any) error }

func scanGroup(sc scanner) (*types.Group, error) {
	var g types.Group
	var labels []byte
	if err := sc.Scan(&g.ID, &g.Name, &labels, &g.RequireApproval,
		&g.RequireApprovalForRollback, &g.ChangeWindowsJSON, &g.LearnFromVerdicts,
		&g.TenantID, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, err
	}
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &g.Labels); err != nil {
			return nil, fmt.Errorf("unmarshal labels: %w", err)
		}
	}
	return &g, nil
}

// ============================================================================
// Agents (ADR 0033, slice 2). Semantics mirror the sqlite backend exactly:
//   - a missing GetAgent returns (nil, nil);
//   - GetAgent / ListAgents / UpdateAgentRegistration exclude soft-deleted
//     (tombstoned) rows via `deleted_at IS NULL`; ListAgents orders by
//     created_at DESC;
//   - UpdateAgentStatus / UpdateAgentLastSeen / UpdateAgentEffectiveConfig do
//     NOT filter on deleted_at (they can touch a tombstoned row), matching the
//     sqlite queries; each returns "agent not found" when no row matches;
//   - DeleteAgent is a SOFT delete (stamps deleted_at) — a second delete of the
//     same id returns "agent not found";
//   - CreateAgent defaults an empty DiscoverySource to "opamp".
//
// Tenant scoping is intentionally omitted here, as it is in the slice-1 groups
// port: types.Agent carries no tenant field, the OSS path is single-tenant, and
// tenant_id is not part of the observable ApplicationStore contract in OSS. A
// later slice can add the column + scoping alongside the interface/struct work
// that threads a tenant through.
// ============================================================================

const agentColumns = `id, name, labels, status, last_seen, group_id, group_name, version, capabilities, effective_config, delivered_config_hash, discovery_source, created_at, updated_at`

func (s *Storage) CreateAgent(ctx context.Context, agent *types.Agent) error {
	labels, err := json.Marshal(agent.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	capabilities, err := json.Marshal(agent.Capabilities)
	if err != nil {
		return fmt.Errorf("marshal capabilities: %w", err)
	}
	source := agent.DiscoverySource
	if source == "" {
		source = "opamp"
	}
	// Revive-on-reconnect — mirrors the sqlite store. DeleteAgent soft-deletes
	// (tombstones) an agent, keeping the PRIMARY-KEY row for the CIP-007-6 R4.3
	// audit trail; a plain INSERT then collided with that row on every OpAMP
	// re-registration and stranded the agent. This upsert REVIVES a tombstoned
	// row (clears deleted_at, refreshes mutable fields, keeps the same id) but
	// leaves a LIVE row untouched (the ON CONFLICT ... WHERE guard skips it, so
	// RowsAffected==0) and reports it as an already-exists error. created_at is
	// left untouched on revive to retain the original registration time.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (id, name, labels, status, last_seen, group_id, group_name, version, capabilities, discovery_source, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			labels = excluded.labels,
			status = excluded.status,
			last_seen = excluded.last_seen,
			group_id = excluded.group_id,
			group_name = excluded.group_name,
			version = excluded.version,
			capabilities = excluded.capabilities,
			discovery_source = excluded.discovery_source,
			updated_at = excluded.updated_at,
			deleted_at = NULL
		WHERE agents.deleted_at IS NOT NULL`,
		agent.ID.String(), agent.Name, labels, string(agent.Status), agent.LastSeen,
		agent.GroupID, agent.GroupName, agent.Version, capabilities, source,
		agent.CreatedAt, agent.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("create agent: agent already exists: %s", agent.ID.String())
	}
	return nil
}

func (s *Storage) GetAgent(ctx context.Context, id uuid.UUID) (*types.Agent, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE id = $1 AND deleted_at IS NULL`, id.String())
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (s *Storage) ListAgents(ctx context.Context) ([]*types.Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentColumns+` FROM agents WHERE deleted_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()
	var out []*types.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Storage) UpdateAgentStatus(ctx context.Context, id uuid.UUID, status types.AgentStatus) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET status=$2, updated_at=now() WHERE id=$1`, id.String(), string(status))
	if err != nil {
		return fmt.Errorf("update agent status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}
	return nil
}

func (s *Storage) UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET last_seen=$2, updated_at=now() WHERE id=$1`, id.String(), lastSeen)
	if err != nil {
		return fmt.Errorf("update agent last seen: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}
	return nil
}

func (s *Storage) UpdateAgentEffectiveConfig(ctx context.Context, id uuid.UUID, effectiveConfig string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET effective_config=$2, updated_at=now() WHERE id=$1`, id.String(), effectiveConfig)
	if err != nil {
		return fmt.Errorf("update agent effective config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}
	return nil
}

// UpdateAgentDeliveredConfigHash persists the confignorm hash of the config the
// agent has confirmed applied (the DELIVERED/APPLIED signal, ADR 0040). Mirrors
// UpdateAgentEffectiveConfig's not-found semantics.
func (s *Storage) UpdateAgentDeliveredConfigHash(ctx context.Context, id uuid.UUID, hash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET delivered_config_hash=$2, updated_at=now() WHERE id=$1`, id.String(), hash)
	if err != nil {
		return fmt.Errorf("update agent delivered config hash: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}
	return nil
}

// UpdateAgentRegistration writes the mutable registration/grouping fields of an
// existing agent (Name, Labels, Version, GroupID, GroupName, Capabilities).
// Capabilities are PRESERVE-ON-EMPTY: overwritten only when the incoming list is
// non-empty, so an OpAMP reconnect re-persists reported capabilities (the PR #35
// root cause — telemetry-discovered rows never got their capabilities column
// re-written) while a capability-less caller can never wipe a previously-stored
// set. nil/empty are normalized to the '[]' sentinel the CASE checks.
func (s *Storage) UpdateAgentRegistration(ctx context.Context, agent *types.Agent) error {
	labels, err := json.Marshal(agent.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	capabilities, err := json.Marshal(agent.Capabilities)
	if err != nil {
		return fmt.Errorf("marshal capabilities: %w", err)
	}
	if len(agent.Capabilities) == 0 {
		capabilities = []byte("[]")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE agents SET name=$2, labels=$3, version=$4, group_id=$5, group_name=$6,
			capabilities = CASE WHEN $7::jsonb = '[]'::jsonb THEN capabilities ELSE $7::jsonb END,
			updated_at=now()
		WHERE id=$1 AND deleted_at IS NULL`,
		agent.ID.String(), agent.Name, labels, agent.Version, agent.GroupID, agent.GroupName,
		string(capabilities))
	if err != nil {
		return fmt.Errorf("update agent registration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent not found: %s", agent.ID.String())
	}
	return nil
}

// DeleteAgent is a SOFT delete (v0.51 tombstone): it stamps deleted_at rather
// than removing the row, so the audit trail survives. Mirrors the sqlite path,
// including the "agent not found" error when the row is missing or already
// tombstoned.
func (s *Storage) DeleteAgent(ctx context.Context, id uuid.UUID) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET deleted_at=$2, updated_at=$3 WHERE id=$1 AND deleted_at IS NULL`,
		id.String(), now, now)
	if err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}
	return nil
}

// RestoreAgent clears the tombstone on a soft-deleted agent (mirrors the
// sqlite store). Operator recovery path for a decommissioned agent that will
// not self-revive on OpAMP reconnect. Errors if no tombstoned row exists.
func (s *Storage) RestoreAgent(ctx context.Context, id uuid.UUID) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET deleted_at=NULL, updated_at=$2 WHERE id=$1 AND deleted_at IS NOT NULL`,
		id.String(), now)
	if err != nil {
		return fmt.Errorf("restore agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent not found or not decommissioned: %s", id.String())
	}
	return nil
}

// PurgeAgent HARD-deletes an agent row (tombstoned or live). Destroys the
// audit-retention row DeleteAgent keeps, so it is gated behind agents:admin at
// the API. Mirrors the sqlite store.
func (s *Storage) PurgeAgent(ctx context.Context, id uuid.UUID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE id=$1`, id.String())
	if err != nil {
		return fmt.Errorf("purge agent: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent not found: %s", id.String())
	}
	return nil
}

func scanAgent(sc scanner) (*types.Agent, error) {
	var (
		a            types.Agent
		idStr        string
		labels       []byte
		capabilities []byte
	)
	if err := sc.Scan(&idStr, &a.Name, &labels, &a.Status, &a.LastSeen,
		&a.GroupID, &a.GroupName, &a.Version, &capabilities, &a.EffectiveConfig,
		&a.DeliveredConfigHash, &a.DiscoverySource, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, fmt.Errorf("parse agent id: %w", err)
	}
	a.ID = id
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &a.Labels); err != nil {
			return nil, fmt.Errorf("unmarshal labels: %w", err)
		}
	}
	if len(capabilities) > 0 {
		if err := json.Unmarshal(capabilities, &a.Capabilities); err != nil {
			return nil, fmt.Errorf("unmarshal capabilities: %w", err)
		}
	}
	return &a, nil
}

// ============================================================================
// Configs (ADR 0033, slice 2). Config history is append-only: the interface has
// Create + reads only (no Update/Delete). Semantics mirror the sqlite backend:
//   - a missing GetConfig / GetLatestConfigForAgent / GetLatestConfigForGroup
//     returns (nil, nil);
//   - the "latest" lookups order by version DESC, created_at DESC and take the
//     first row;
//   - ListConfigs filters by AgentID and/or GroupID, orders by created_at DESC,
//     and applies Limit when > 0.
//
// agent_id / group_id are stored as their canonical string form (uuid string /
// group id) so they join against agents.id and groups.id. As with agents, OSS
// tenant scoping is intentionally omitted (types.Config carries no tenant).
// ============================================================================

const configColumns = `id, name, agent_id, group_id, config_hash, content, version, created_at, archived_at`

func (s *Storage) CreateConfig(ctx context.Context, config *types.Config) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO configs (`+configColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		config.ID, config.Name, config.AgentID, config.GroupID,
		config.ConfigHash, config.Content, config.Version, config.CreatedAt, config.ArchivedAt)
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	return nil
}

func (s *Storage) GetConfig(ctx context.Context, id string) (*types.Config, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+configColumns+` FROM configs WHERE id = $1`, id)
	c, err := scanConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Storage) GetLatestConfigForAgent(ctx context.Context, agentID uuid.UUID) (*types.Config, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+configColumns+` FROM configs
		WHERE agent_id = $1
		ORDER BY version DESC, created_at DESC
		LIMIT 1`, agentID.String())
	c, err := scanConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Storage) GetLatestConfigForGroup(ctx context.Context, groupID string) (*types.Config, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+configColumns+` FROM configs
		WHERE group_id = $1
		ORDER BY version DESC, created_at DESC
		LIMIT 1`, groupID)
	c, err := scanConfig(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Storage) ListConfigs(ctx context.Context, filter types.ConfigFilter) ([]*types.Config, error) {
	query := `SELECT ` + configColumns + ` FROM configs WHERE 1=1`
	var args []any
	n := 0
	// Soft-archived configs are hidden from the default listing; IncludeArchived
	// opts back in. No bind arg needed (static predicate).
	if !filter.IncludeArchived {
		query += " AND archived_at IS NULL"
	}
	if filter.AgentID != nil {
		n++
		query += fmt.Sprintf(" AND agent_id = $%d", n)
		args = append(args, filter.AgentID.String())
	}
	if filter.GroupID != nil {
		n++
		query += fmt.Sprintf(" AND group_id = $%d", n)
		args = append(args, *filter.GroupID)
	}
	query += " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		n++
		query += fmt.Sprintf(" LIMIT $%d", n)
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list configs: %w", err)
	}
	defer rows.Close()
	var out []*types.Config
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConfig removes a single config row by id. NO-OP on a missing id (see the
// ApplicationStore interface doc): cleanup at a rollout terminal state must be
// idempotent, so a zero-row delete returns nil rather than a not-found error. As
// with the other config methods, OSS tenant scoping is omitted (configs carries
// no tenant column).
func (s *Storage) DeleteConfig(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM configs WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete config: %w", err)
	}
	return nil
}

// DeleteConfigsForAgent removes ALL agent-scoped config rows for an agent so the
// agent resumes tracking group config (HA S3d rollout cleanup). Idempotent — a
// zero-row delete is a no-op.
func (s *Storage) DeleteConfigsForAgent(ctx context.Context, agentID uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM configs WHERE agent_id = $1`, agentID.String()); err != nil {
		return fmt.Errorf("delete configs for agent: %w", err)
	}
	return nil
}

func scanConfig(sc scanner) (*types.Config, error) {
	var (
		c          types.Config
		name       sql.NullString
		agentID    sql.NullString
		groupID    sql.NullString
		archivedAt sql.NullTime
	)
	if err := sc.Scan(&c.ID, &name, &agentID, &groupID,
		&c.ConfigHash, &c.Content, &c.Version, &c.CreatedAt, &archivedAt); err != nil {
		return nil, err
	}
	if name.Valid {
		c.Name = name.String
	}
	if agentID.Valid {
		id, err := uuid.Parse(agentID.String)
		if err != nil {
			return nil, fmt.Errorf("parse config agent id: %w", err)
		}
		c.AgentID = &id
	}
	if groupID.Valid {
		gid := groupID.String
		c.GroupID = &gid
	}
	if archivedAt.Valid {
		t := archivedAt.Time
		c.ArchivedAt = &t
	}
	return &c, nil
}

// SetConfigArchived soft-archives (archived=true) or restores (archived=false) a
// single config by id. Stamps archived_at=now (or clears it to NULL) so the
// default ListConfigs hides/shows the row; content/version are untouched (configs
// stay immutable). Returns types.ErrConfigNotFound when no row matches — the API
// layer maps that to a 404. As with the other config methods, OSS tenant scoping
// is intentionally omitted (configs carries no tenant column in Postgres).
func (s *Storage) SetConfigArchived(ctx context.Context, id string, archived bool) error {
	var archivedAt any
	if archived {
		archivedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE configs SET archived_at = $2 WHERE id = $1`, id, archivedAt)
	if err != nil {
		return fmt.Errorf("set config archived: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set config archived: %w", err)
	}
	if n == 0 {
		return types.ErrConfigNotFound
	}
	return nil
}

// ============================================================================
// Rollouts + rollout approvals (ADR 0033, slice 3). Semantics mirror the sqlite
// backend EXACTLY:
//   - CreateRollout defaults CreatedAt/UpdatedAt to now, ProposedBy to
//     "operator", and floors RequiredApprovals to 1; version/pushed_agent_ids
//     are not set at create (they take the column default / NULL);
//   - GetRollout on a missing id returns (nil, nil);
//   - ListRollouts orders by created_at DESC, defaults Limit to 100 and caps it
//     at 1000, and filters by GroupID / State / PlanID when set;
//   - UpdateRollout is optimistic-concurrency guarded: a caller carrying a
//     loaded Version (>0) gets a compare-and-swap that bumps version only if the
//     stored row still matches, returning types.ErrRolloutVersionConflict when a
//     concurrent writer moved it and "rollout not found" when the id is gone;
//     a Version==0 caller takes the blind last-write-wins path (still advancing
//     the counter), returning "rollout not found" on a missing id;
//   - the read side floors RequiredApprovals to 1, decodes an empty/NULL
//     proposed_by to "operator" and an empty/NULL step_kind to
//     types.StepKindRollout, and leaves optional strings/times zero when NULL.
//
// Structured fields (stages, abort_criteria, evidence_refs, pushed_agent_ids)
// are stored as JSONB so the abort_criteria's SLOBurn criterion (ADR 0034)
// round-trips inline. evidence_refs / pushed_agent_ids are stored NULL when
// empty so an empty slice round-trips to nil, matching the sqlite backend's
// nullableString(evidenceJSON) encoding.
//
// The rollout_approvals log is append-only (RecordRolloutApproval is an
// idempotent upsert keyed on (rollout_id, approver)); it is not tenant-scoped,
// mirroring sqlite. As with agents/configs, OSS tenant scoping is intentionally
// omitted here (types.Rollout carries no tenant field) — a later consolidated
// pass adds tenant_id columns + scoping alongside the interface/struct work.
// ============================================================================

// rolloutColumns is the full projection scanRollout reads, in scan order.
const rolloutColumns = `id, name, group_id, target_config_id, previous_config_id, ` +
	`stages, abort_criteria, notification_url, state, current_stage, ` +
	`stage_started_at, abort_reason, created_at, updated_at, completed_at, ` +
	`require_approval, requested_by, approved_by, approved_at, rejected_by, ` +
	`rejected_at, approval_notes, required_approvals, last_blackout_reason, ` +
	`last_blackout_at, proposed_by, proposal_reasoning, evidence_refs, ` +
	`rolled_back_from_id, plan_id, plan_step_index, step_kind, action_request_id, ` +
	`exclude_from_learning, version, pushed_agent_ids`

// nullString returns a sql.NullString that is invalid (→ SQL NULL) for the
// empty string, mirroring the sqlite backend's nullableString helper so empty
// optionals persist as NULL and round-trip to "".
func nullString(s string) any {
	if s == "" {
		return sql.NullString{}
	}
	return s
}

// jsonbOrNull returns nil (→ SQL NULL) for an empty payload, else the raw bytes.
// Used for the nullable JSONB columns (evidence_refs, pushed_agent_ids) so an
// empty slice round-trips to nil rather than an empty-array literal.
func jsonbOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func floorRequiredApprovals(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func (s *Storage) CreateRollout(ctx context.Context, r *types.Rollout) error {
	stagesJSON, err := json.Marshal(r.Stages)
	if err != nil {
		return fmt.Errorf("marshal rollout stages: %w", err)
	}
	criteriaJSON, err := json.Marshal(r.AbortCriteria)
	if err != nil {
		return fmt.Errorf("marshal rollout abort criteria: %w", err)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	proposedBy := r.ProposedBy
	if proposedBy == "" {
		proposedBy = types.RolloutProposedByOperator
	}
	var evidenceJSON []byte
	if len(r.EvidenceRefs) > 0 {
		if evidenceJSON, err = json.Marshal(r.EvidenceRefs); err != nil {
			return fmt.Errorf("marshal rollout evidence refs: %w", err)
		}
	}
	// version and pushed_agent_ids are omitted here — they take the column
	// default (version=1) / NULL, matching the sqlite CreateRollout INSERT.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO rollouts (
			id, name, group_id, target_config_id, previous_config_id,
			stages, abort_criteria, notification_url, state, current_stage,
			stage_started_at, abort_reason, created_at, updated_at, completed_at,
			require_approval, requested_by, approved_by, approved_at, rejected_by,
			rejected_at, approval_notes, required_approvals, last_blackout_reason,
			last_blackout_at, proposed_by, proposal_reasoning, evidence_refs,
			rolled_back_from_id, plan_id, plan_step_index, step_kind,
			action_request_id, exclude_from_learning)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
			$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34)`,
		r.ID, r.Name, r.GroupID, r.TargetConfigID, nullString(r.PreviousConfigID),
		stagesJSON, criteriaJSON, nullString(r.NotificationURL), string(r.State), r.CurrentStage,
		r.StageStartedAt, nullString(r.AbortReason), r.CreatedAt, r.UpdatedAt, r.CompletedAt,
		r.RequireApproval, nullString(r.RequestedBy), nullString(r.ApprovedBy), r.ApprovedAt, nullString(r.RejectedBy),
		r.RejectedAt, nullString(r.ApprovalNotes), floorRequiredApprovals(r.RequiredApprovals), nullString(r.LastBlackoutReason),
		r.LastBlackoutAt, proposedBy, nullString(r.ProposalReasoning), jsonbOrNull(evidenceJSON),
		nullString(r.RolledBackFromID), nullString(r.PlanID), r.PlanStepIndex, nullString(r.StepKind),
		nullString(r.ActionRequestID), r.ExcludeFromLearning)
	if err != nil {
		return fmt.Errorf("create rollout: %w", err)
	}
	return nil
}

func (s *Storage) GetRollout(ctx context.Context, id string) (*types.Rollout, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+rolloutColumns+` FROM rollouts WHERE id = $1`, id)
	r, err := scanRollout(row)
	if errors.Is(err, sql.ErrNoRows) {
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
	query := `SELECT ` + rolloutColumns + ` FROM rollouts WHERE 1=1`
	var args []any
	n := 0
	if filter.GroupID != "" {
		n++
		query += fmt.Sprintf(" AND group_id = $%d", n)
		args = append(args, filter.GroupID)
	}
	if filter.State != "" {
		n++
		query += fmt.Sprintf(" AND state = $%d", n)
		args = append(args, string(filter.State))
	}
	if filter.PlanID != "" {
		n++
		query += fmt.Sprintf(" AND plan_id = $%d", n)
		args = append(args, filter.PlanID)
	}
	n++
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list rollouts: %w", err)
	}
	defer rows.Close()
	var out []*types.Rollout
	for rows.Next() {
		r, err := scanRollout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rolloutSetClause is the shared SET body for UpdateRollout ($1..$33). Both the
// CAS and the legacy path bind the same 33 values; only the version handling and
// WHERE tail differ.
const rolloutSetClause = `UPDATE rollouts SET
	name=$1, group_id=$2, target_config_id=$3, previous_config_id=$4,
	stages=$5, abort_criteria=$6, notification_url=$7, state=$8, current_stage=$9,
	stage_started_at=$10, abort_reason=$11, updated_at=$12, completed_at=$13,
	require_approval=$14, requested_by=$15, approved_by=$16, approved_at=$17,
	rejected_by=$18, rejected_at=$19, approval_notes=$20, required_approvals=$21,
	last_blackout_reason=$22, last_blackout_at=$23, proposed_by=$24,
	proposal_reasoning=$25, evidence_refs=$26, rolled_back_from_id=$27, plan_id=$28,
	plan_step_index=$29, step_kind=$30, action_request_id=$31,
	exclude_from_learning=$32, pushed_agent_ids=$33`

func (s *Storage) UpdateRollout(ctx context.Context, r *types.Rollout) error {
	stagesJSON, err := json.Marshal(r.Stages)
	if err != nil {
		return fmt.Errorf("marshal rollout stages: %w", err)
	}
	criteriaJSON, err := json.Marshal(r.AbortCriteria)
	if err != nil {
		return fmt.Errorf("marshal rollout abort criteria: %w", err)
	}
	r.UpdatedAt = time.Now().UTC()
	proposedBy := r.ProposedBy
	if proposedBy == "" {
		proposedBy = types.RolloutProposedByOperator
	}
	var evidenceJSON []byte
	if len(r.EvidenceRefs) > 0 {
		if evidenceJSON, err = json.Marshal(r.EvidenceRefs); err != nil {
			return fmt.Errorf("marshal rollout evidence refs: %w", err)
		}
	}
	var pushedAgentIDsJSON []byte
	if len(r.PushedAgentIDs) > 0 {
		if pushedAgentIDsJSON, err = json.Marshal(r.PushedAgentIDs); err != nil {
			return fmt.Errorf("marshal rollout pushed agent ids: %w", err)
		}
	}
	setArgs := []any{
		r.Name, r.GroupID, r.TargetConfigID, nullString(r.PreviousConfigID),
		stagesJSON, criteriaJSON, nullString(r.NotificationURL), string(r.State), r.CurrentStage,
		r.StageStartedAt, nullString(r.AbortReason), r.UpdatedAt, r.CompletedAt,
		r.RequireApproval, nullString(r.RequestedBy), nullString(r.ApprovedBy), r.ApprovedAt,
		nullString(r.RejectedBy), r.RejectedAt, nullString(r.ApprovalNotes), floorRequiredApprovals(r.RequiredApprovals),
		nullString(r.LastBlackoutReason), r.LastBlackoutAt, proposedBy,
		nullString(r.ProposalReasoning), jsonbOrNull(evidenceJSON), nullString(r.RolledBackFromID), nullString(r.PlanID),
		r.PlanStepIndex, nullString(r.StepKind), nullString(r.ActionRequestID),
		r.ExcludeFromLearning, jsonbOrNull(pushedAgentIDsJSON),
	}

	// Optimistic-concurrency guard. A caller carrying a loaded Version (>0) gets
	// a CAS: bump to Version+1 only if the stored row still matches, so a write
	// racing a concurrent operator Pause/Abort or an engine tick is rejected with
	// ErrRolloutVersionConflict rather than silently clobbering the other writer.
	// Legacy callers that leave Version==0 take the blind last-write-wins path
	// (still advancing the column). Behavior mirrors the sqlite backend.
	if r.Version > 0 {
		newVersion := r.Version + 1
		stmt := rolloutSetClause + `, version=$34 WHERE id=$35 AND version=$36`
		args := append(append([]any{}, setArgs...), newVersion, r.ID, r.Version)
		res, err := s.db.ExecContext(ctx, stmt, args...)
		if err != nil {
			return fmt.Errorf("update rollout: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Disambiguate a gone id from a version race.
			var one int
			switch qerr := s.db.QueryRowContext(ctx, `SELECT 1 FROM rollouts WHERE id = $1`, r.ID).Scan(&one); {
			case errors.Is(qerr, sql.ErrNoRows):
				return fmt.Errorf("rollout not found: %s", r.ID)
			case qerr == nil:
				return types.ErrRolloutVersionConflict
			default:
				return fmt.Errorf("update rollout: %w", qerr)
			}
		}
		r.Version = newVersion
		return nil
	}

	stmt := rolloutSetClause + `, version = version + 1 WHERE id=$34`
	args := append(append([]any{}, setArgs...), r.ID)
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("update rollout: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("rollout not found: %s", r.ID)
	}
	return nil
}

func scanRollout(sc scanner) (*types.Rollout, error) {
	r := &types.Rollout{}
	var (
		previousConfigID   sql.NullString
		stagesJSON         []byte
		criteriaJSON       []byte
		notificationURL    sql.NullString
		stateStr           string
		stageStartedAt     sql.NullTime
		abortReason        sql.NullString
		completedAt        sql.NullTime
		requestedBy        sql.NullString
		approvedBy         sql.NullString
		approvedAt         sql.NullTime
		rejectedBy         sql.NullString
		rejectedAt         sql.NullTime
		approvalNotes      sql.NullString
		requiredApprovals  int
		lastBlackoutReason sql.NullString
		lastBlackoutAt     sql.NullTime
		proposedBy         sql.NullString
		proposalReasoning  sql.NullString
		evidenceRefsJSON   []byte
		rolledBackFromID   sql.NullString
		planID             sql.NullString
		stepKind           sql.NullString
		actionRequestID    sql.NullString
		pushedAgentIDsJSON []byte
	)
	if err := sc.Scan(
		&r.ID, &r.Name, &r.GroupID, &r.TargetConfigID, &previousConfigID,
		&stagesJSON, &criteriaJSON, &notificationURL, &stateStr, &r.CurrentStage,
		&stageStartedAt, &abortReason, &r.CreatedAt, &r.UpdatedAt, &completedAt,
		&r.RequireApproval, &requestedBy, &approvedBy, &approvedAt, &rejectedBy,
		&rejectedAt, &approvalNotes, &requiredApprovals, &lastBlackoutReason,
		&lastBlackoutAt, &proposedBy, &proposalReasoning, &evidenceRefsJSON,
		&rolledBackFromID, &planID, &r.PlanStepIndex, &stepKind, &actionRequestID,
		&r.ExcludeFromLearning, &r.Version, &pushedAgentIDsJSON,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(stagesJSON, &r.Stages); err != nil {
		return nil, fmt.Errorf("unmarshal rollout stages: %w", err)
	}
	if err := json.Unmarshal(criteriaJSON, &r.AbortCriteria); err != nil {
		return nil, fmt.Errorf("unmarshal abort criteria: %w", err)
	}
	r.State = types.RolloutState(stateStr)
	// ADR 0029 — floor the threshold to 1 on read so rows that stored 0 behave
	// as the two-person default.
	r.RequiredApprovals = floorRequiredApprovals(requiredApprovals)
	if len(evidenceRefsJSON) > 0 {
		if err := json.Unmarshal(evidenceRefsJSON, &r.EvidenceRefs); err != nil {
			return nil, fmt.Errorf("unmarshal evidence refs: %w", err)
		}
	}
	if len(pushedAgentIDsJSON) > 0 {
		if err := json.Unmarshal(pushedAgentIDsJSON, &r.PushedAgentIDs); err != nil {
			return nil, fmt.Errorf("unmarshal pushed agent ids: %w", err)
		}
	}
	if previousConfigID.Valid {
		r.PreviousConfigID = previousConfigID.String
	}
	if notificationURL.Valid {
		r.NotificationURL = notificationURL.String
	}
	if stageStartedAt.Valid {
		t := stageStartedAt.Time
		r.StageStartedAt = &t
	}
	if abortReason.Valid {
		r.AbortReason = abortReason.String
	}
	if completedAt.Valid {
		t := completedAt.Time
		r.CompletedAt = &t
	}
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
	if lastBlackoutReason.Valid {
		r.LastBlackoutReason = lastBlackoutReason.String
	}
	if lastBlackoutAt.Valid {
		t := lastBlackoutAt.Time
		r.LastBlackoutAt = &t
	}
	// v0.53 — empty/NULL proposed_by falls back to operator semantics.
	if proposedBy.Valid && proposedBy.String != "" {
		r.ProposedBy = proposedBy.String
	} else {
		r.ProposedBy = types.RolloutProposedByOperator
	}
	if proposalReasoning.Valid {
		r.ProposalReasoning = proposalReasoning.String
	}
	if rolledBackFromID.Valid {
		r.RolledBackFromID = rolledBackFromID.String
	}
	if planID.Valid {
		r.PlanID = planID.String
	}
	// v0.89.14 — empty/NULL step_kind decodes to the "rollout" sentinel.
	if stepKind.Valid && stepKind.String != "" {
		r.StepKind = stepKind.String
	} else {
		r.StepKind = types.StepKindRollout
	}
	if actionRequestID.Valid {
		r.ActionRequestID = actionRequestID.String
	}
	return r, nil
}

// RecordRolloutApproval records one distinct approver's approval of a rollout
// (ADR 0029). Idempotent: ON CONFLICT DO NOTHING keys off the (rollout_id,
// approver) PK so the same approver approving twice does not double-count.
// Mirrors the sqlite backend's INSERT OR IGNORE.
func (s *Storage) RecordRolloutApproval(ctx context.Context, rolloutID, approver, tokenID, notes string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rollout_approvals (rollout_id, approver, approver_token_id, notes, approved_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (rollout_id, approver) DO NOTHING`,
		rolloutID, approver, nullString(tokenID), nullString(notes), at)
	if err != nil {
		return fmt.Errorf("record rollout approval: %w", err)
	}
	return nil
}

// CountRolloutApprovers returns the number of DISTINCT approvers recorded for a
// rollout (ADR 0029). The composite PK guarantees distinctness, so COUNT(*) is
// the distinct-approver count.
func (s *Storage) CountRolloutApprovers(ctx context.Context, rolloutID string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rollout_approvals WHERE rollout_id = $1`, rolloutID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rollout approvers: %w", err)
	}
	return n, nil
}

// ListRolloutApprovers returns the recorded approvers for a rollout, oldest
// approval first, for audit / UI (ADR 0029).
func (s *Storage) ListRolloutApprovers(ctx context.Context, rolloutID string) ([]types.RolloutApproval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT approver, approver_token_id, notes, approved_at FROM rollout_approvals WHERE rollout_id = $1 ORDER BY approved_at ASC`,
		rolloutID)
	if err != nil {
		return nil, fmt.Errorf("list rollout approvers: %w", err)
	}
	defer rows.Close()
	var out []types.RolloutApproval
	for rows.Next() {
		var a types.RolloutApproval
		var tokenID, notes sql.NullString
		if err := rows.Scan(&a.Approver, &tokenID, &notes, &a.ApprovedAt); err != nil {
			return nil, fmt.Errorf("scan rollout approver: %w", err)
		}
		if tokenID.Valid {
			a.ApproverTokenID = tokenID.String
		}
		if notes.Valid {
			a.Notes = notes.String
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
