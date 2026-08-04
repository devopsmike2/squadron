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

	// pgx stdlib driver, registered under the name "pgx".
	_ "github.com/jackc/pgx/v5/stdlib"

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
