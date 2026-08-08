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
