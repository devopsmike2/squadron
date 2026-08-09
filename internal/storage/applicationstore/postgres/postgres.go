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
    discovery_source TEXT NOT NULL DEFAULT 'opamp',
    deleted_at       TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agents_group_id ON agents(group_id);
CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);

CREATE TABLE IF NOT EXISTS configs (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL DEFAULT '',
    agent_id    TEXT,
    group_id    TEXT,
    config_hash TEXT NOT NULL DEFAULT '',
    content     TEXT NOT NULL DEFAULT '',
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_configs_agent_id ON configs(agent_id);
CREATE INDEX IF NOT EXISTS idx_configs_group_id ON configs(group_id);
CREATE INDEX IF NOT EXISTS idx_configs_created_at ON configs(created_at DESC);

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
`

func (s *Storage) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, schemaSQL)
	return err
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

func (s *Storage) DeleteGroup(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM groups WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("group not found: %s", id)
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

const agentColumns = `id, name, labels, status, last_seen, group_id, group_name, version, capabilities, effective_config, discovery_source, created_at, updated_at`

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
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agents (id, name, labels, status, last_seen, group_id, group_name, version, capabilities, discovery_source, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		agent.ID.String(), agent.Name, labels, string(agent.Status), agent.LastSeen,
		agent.GroupID, agent.GroupName, agent.Version, capabilities, source,
		agent.CreatedAt, agent.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
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

func (s *Storage) UpdateAgentRegistration(ctx context.Context, agent *types.Agent) error {
	labels, err := json.Marshal(agent.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE agents SET name=$2, labels=$3, version=$4, group_id=$5, group_name=$6, updated_at=now()
		WHERE id=$1 AND deleted_at IS NULL`,
		agent.ID.String(), agent.Name, labels, agent.Version, agent.GroupID, agent.GroupName)
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

func scanAgent(sc scanner) (*types.Agent, error) {
	var (
		a            types.Agent
		idStr        string
		labels       []byte
		capabilities []byte
	)
	if err := sc.Scan(&idStr, &a.Name, &labels, &a.Status, &a.LastSeen,
		&a.GroupID, &a.GroupName, &a.Version, &capabilities, &a.EffectiveConfig,
		&a.DiscoverySource, &a.CreatedAt, &a.UpdatedAt); err != nil {
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

const configColumns = `id, name, agent_id, group_id, config_hash, content, version, created_at`

func (s *Storage) CreateConfig(ctx context.Context, config *types.Config) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO configs (`+configColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		config.ID, config.Name, config.AgentID, config.GroupID,
		config.ConfigHash, config.Content, config.Version, config.CreatedAt)
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

func scanConfig(sc scanner) (*types.Config, error) {
	var (
		c       types.Config
		name    sql.NullString
		agentID sql.NullString
		groupID sql.NullString
	)
	if err := sc.Scan(&c.ID, &name, &agentID, &groupID,
		&c.ConfigHash, &c.Content, &c.Version, &c.CreatedAt); err != nil {
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
	return &c, nil
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
