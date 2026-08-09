// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
)

// connection_registry.go — the Postgres port of the HA S3b connection registry
// (ADR 0035). Semantics mirror the sqlite backend EXACTLY.
//
// SYSTEM-SCOPED: instance identity is orthogonal to tenant, so the table has NO
// tenant_id column and these methods apply no tenant predicate. agent_id is the
// Squadron fleet id and the PRIMARY KEY — one owner per agent, last-writer-wins
// on reconnect.

// UpsertConnectionOwner records (or refreshes) this instance's ownership of
// agentID. Fresh row → connected_at = now; heartbeat from the SAME instance →
// connected_at preserved, only last_heartbeat_at advances; takeover by a
// DIFFERENT instance → connected_at reset to now.
func (s *Storage) UpsertConnectionOwner(ctx context.Context, agentID uuid.UUID, instanceID string, now time.Time) error {
	if instanceID == "" {
		return fmt.Errorf("instance_id required")
	}
	ts := now.UTC()
	const stmt = `
		INSERT INTO connection_registry (agent_id, instance_id, connected_at, last_heartbeat_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (agent_id) DO UPDATE SET
			instance_id = EXCLUDED.instance_id,
			last_heartbeat_at = EXCLUDED.last_heartbeat_at,
			connected_at = CASE
				WHEN connection_registry.instance_id <> EXCLUDED.instance_id
					THEN EXCLUDED.connected_at
				ELSE connection_registry.connected_at
			END`
	if _, err := s.db.ExecContext(ctx, stmt, agentID.String(), instanceID, ts, ts); err != nil {
		return fmt.Errorf("upsert connection owner %q: %w", agentID, err)
	}
	return nil
}

// DeleteConnectionOwner removes agentID's ownership row. Missing row → no-op.
func (s *Storage) DeleteConnectionOwner(ctx context.Context, agentID uuid.UUID) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM connection_registry WHERE agent_id = $1`, agentID.String()); err != nil {
		return fmt.Errorf("delete connection owner %q: %w", agentID, err)
	}
	return nil
}

// GetConnectionOwner returns the ownership row for agentID, or (nil, nil) when
// no row exists.
func (s *Storage) GetConnectionOwner(ctx context.Context, agentID uuid.UUID) (*types.ConnectionOwner, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT agent_id, instance_id, connected_at, last_heartbeat_at
		   FROM connection_registry WHERE agent_id = $1`, agentID.String())
	owner, err := scanConnectionOwner(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get connection owner %q: %w", agentID, err)
	}
	return owner, nil
}

// ListConnectionOwners returns every ownership row ordered by agent_id.
func (s *Storage) ListConnectionOwners(ctx context.Context) ([]*types.ConnectionOwner, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT agent_id, instance_id, connected_at, last_heartbeat_at
		   FROM connection_registry ORDER BY agent_id`)
	if err != nil {
		return nil, fmt.Errorf("list connection owners: %w", err)
	}
	defer rows.Close()

	var owners []*types.ConnectionOwner
	for rows.Next() {
		owner, scanErr := scanConnectionOwner(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan connection owner row: %w", scanErr)
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate connection owner rows: %w", err)
	}
	return owners, nil
}

// ReclaimStaleConnectionOwners deletes rows whose last_heartbeat_at is strictly
// older than olderThan and returns the count removed.
func (s *Storage) ReclaimStaleConnectionOwners(ctx context.Context, olderThan time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM connection_registry WHERE last_heartbeat_at < $1`, olderThan.UTC())
	if err != nil {
		return 0, fmt.Errorf("reclaim stale connection owners: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return int(n), nil
}

func scanConnectionOwner(row scanner) (*types.ConnectionOwner, error) {
	var (
		agentIDStr string
		owner      types.ConnectionOwner
	)
	if err := row.Scan(&agentIDStr, &owner.InstanceID, &owner.ConnectedAt, &owner.LastHeartbeatAt); err != nil {
		return nil, err
	}
	id, err := uuid.Parse(agentIDStr)
	if err != nil {
		return nil, fmt.Errorf("parse agent_id %q: %w", agentIDStr, err)
	}
	owner.AgentID = id
	return &owner, nil
}
