// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ServerlessInstanceRow is the storage-layer projection of a single
// serverless function or service inventory row. v0.89.90 (#721
// Stream 119, slice 1 chunk 1 of the Serverless tier arc).
//
// Why this lives in the sqlite package rather than reusing
// scanner.ServerlessInstanceSnapshot directly: the sqlite package
// sits at the leaf of the storage tree (the parent applicationstore
// package imports sqlite via factory.go), so any dependency the
// sqlite package adds propagates up the call graph. Reusing the
// scanner snapshot would introduce
// sqlite → scanner → credstore → services → applicationstore
// → sqlite — a cycle. Instead the storage layer carries the
// projection without importing the scanner package; orchestrator-
// side adapters in the AWS / GCP / Azure / OCI scanner packages
// translate scanner.ServerlessInstanceSnapshot into this row shape
// at SaveServerless call time.
//
// The fields mirror the columns documented on serverless_instance
// in migrations.go::ServerlessInstanceSchema. SnapshotJSON carries
// the canonical scanner.ServerlessInstanceSnapshot serialization so
// per-cloud Inventory tabs can round-trip the surface-specific
// Detail bag without a second join.
type ServerlessInstanceRow struct {
	ConnectionID  string
	ScanID        string
	Provider      string
	Surface       string
	AccountID     string
	Region        string
	ResourceName  string
	ResourceARN   string
	Runtime       string
	HasTraceAxis  bool
	HasOTelDistro bool
	LastSeenAt    *time.Time
	SnapshotJSON  string
}

// SaveServerless — v0.89.90 (#721 Stream 119). Persists the supplied
// serverless rows. The (connection_id, scan_id, resource_arn) UNIQUE
// constraint means the call is idempotent on a per-resource basis
// within a single scan — an INSERT ... ON CONFLICT DO UPDATE
// refreshes the row in place rather than failing.
//
// Whole-batch transactional — a single failure rolls back the pending
// rows so a concurrent ListServerless can't observe a partial write.
// See docs/proposals/serverless-tier-slice1.md §4 for the schema
// rationale.
func (s *Storage) SaveServerless(
	ctx context.Context,
	rows []ServerlessInstanceRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const stmt = `INSERT INTO serverless_instance (
		id, connection_id, scan_id,
		provider, surface, account_id, region,
		resource_name, resource_arn, runtime,
		has_trace_axis, has_otel_distro,
		last_seen_at, snapshot_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(connection_id, scan_id, resource_arn) DO UPDATE SET
		provider        = excluded.provider,
		surface         = excluded.surface,
		account_id      = excluded.account_id,
		region          = excluded.region,
		resource_name   = excluded.resource_name,
		runtime         = excluded.runtime,
		has_trace_axis  = excluded.has_trace_axis,
		has_otel_distro = excluded.has_otel_distro,
		last_seen_at    = excluded.last_seen_at,
		snapshot_json   = excluded.snapshot_json`

	for _, r := range rows {
		if r.ConnectionID == "" {
			return fmt.Errorf("connection_id required on row %q", r.ResourceARN)
		}
		if r.ScanID == "" {
			return fmt.Errorf("scan_id required on row %q", r.ResourceARN)
		}
		var resourceARN any
		if r.ResourceARN != "" {
			resourceARN = r.ResourceARN
		}
		var runtime any
		if r.Runtime != "" {
			runtime = r.Runtime
		}
		var lastSeenAt any
		if r.LastSeenAt != nil {
			lastSeenAt = r.LastSeenAt.UTC()
		}
		if _, err := tx.ExecContext(ctx, stmt,
			uuid.NewString(),
			r.ConnectionID,
			r.ScanID,
			r.Provider,
			r.Surface,
			r.AccountID,
			r.Region,
			r.ResourceName,
			resourceARN,
			runtime,
			boolToInt(r.HasTraceAxis),
			boolToInt(r.HasOTelDistro),
			lastSeenAt,
			r.SnapshotJSON,
		); err != nil {
			return fmt.Errorf("upsert serverless_instance row %q: %w", r.ResourceARN, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit serverless save: %w", err)
	}
	return nil
}

// ListServerless — v0.89.90. Returns rows for the (connection_id,
// scan_id) tuple, newest-created-at first. When scan_id is empty the
// query filters on connection_id only; when connection_id is empty
// the call returns an empty slice (the per-provider Inventory tab
// always scopes by connection_id).
//
// Each returned row carries the full SnapshotJSON column verbatim so
// callers can unmarshal back into their scanner snapshot shape; the
// universal columns are populated for query-time filtering without
// a JSON parse step.
func (s *Storage) ListServerless(
	ctx context.Context,
	connectionID, scanID string,
) ([]ServerlessInstanceRow, error) {
	if connectionID == "" {
		return nil, nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if scanID == "" {
		const stmt = `SELECT
			connection_id, scan_id, provider, surface,
			account_id, region, resource_name,
			COALESCE(resource_arn, ''),
			COALESCE(runtime, ''),
			has_trace_axis, has_otel_distro,
			last_seen_at, snapshot_json
			FROM serverless_instance
			WHERE connection_id = ?
			ORDER BY created_at DESC`
		rows, err = s.db.QueryContext(ctx, stmt, connectionID)
	} else {
		const stmt = `SELECT
			connection_id, scan_id, provider, surface,
			account_id, region, resource_name,
			COALESCE(resource_arn, ''),
			COALESCE(runtime, ''),
			has_trace_axis, has_otel_distro,
			last_seen_at, snapshot_json
			FROM serverless_instance
			WHERE connection_id = ? AND scan_id = ?
			ORDER BY created_at DESC`
		rows, err = s.db.QueryContext(ctx, stmt, connectionID, scanID)
	}
	if err != nil {
		return nil, fmt.Errorf("query serverless_instance: %w", err)
	}
	defer rows.Close()

	var out []ServerlessInstanceRow
	for rows.Next() {
		var (
			r          ServerlessInstanceRow
			hasTrace   int
			hasOTel    int
			lastSeenAt sql.NullTime
		)
		if scanErr := rows.Scan(
			&r.ConnectionID, &r.ScanID, &r.Provider, &r.Surface,
			&r.AccountID, &r.Region, &r.ResourceName,
			&r.ResourceARN, &r.Runtime,
			&hasTrace, &hasOTel,
			&lastSeenAt, &r.SnapshotJSON,
		); scanErr != nil {
			return nil, fmt.Errorf("scan serverless_instance row: %w", scanErr)
		}
		r.HasTraceAxis = hasTrace != 0
		r.HasOTelDistro = hasOTel != 0
		if lastSeenAt.Valid {
			t := lastSeenAt.Time.UTC()
			r.LastSeenAt = &t
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate serverless_instance rows: %w", err)
	}
	return out, nil
}

// DeleteServerlessBefore — v0.89.90. Removes serverless_instance rows
// whose created_at predates the supplied cutoff. The chunk-5
// dashboard's per-connection rollup reads through the most-recent
// scan per connection, so historic rows accumulate forever unless an
// operator-driven GC sweep prunes them. Slice 1 ships the predicate;
// scheduling lives in the chunk-6 runbook.
//
// Returns the number of rows removed for the operator-visible audit
// payload.
func (s *Storage) DeleteServerlessBefore(
	ctx context.Context,
	before time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM serverless_instance WHERE created_at < ?`,
		before.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("delete serverless_instance: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}