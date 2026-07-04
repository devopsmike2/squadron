// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/google/uuid"
)

// EventSourceInstanceRow is the storage-layer projection of a single
// inbound event source inventory row. v0.89.100 (#734 Stream 132,
// slice 1 chunk 1 of the Event source tier arc).
//
// Why this lives in the sqlite package rather than reusing
// scanner.EventSourceInstanceSnapshot directly: same rationale as
// OrchestrationInstanceRow — the sqlite package sits at the leaf of the
// storage tree, so any dependency it adds propagates up the call graph.
// Reusing the scanner snapshot would introduce a sqlite → scanner →
// credstore → services → applicationstore → sqlite cycle. Instead the
// storage layer carries the projection without importing the scanner
// package; per-cloud scanner-side adapters translate
// scanner.EventSourceInstanceSnapshot into this row shape at
// SaveEventSourceInstances call time.
//
// The fields mirror the columns documented on event_source_instance in
// migrations.go::EventSourceInstanceSchema. SnapshotJSON carries the
// canonical scanner.EventSourceInstanceSnapshot serialization so
// per-cloud Inventory tabs can round-trip the surface-specific Detail
// bag without a second join.
type EventSourceInstanceRow struct {
	ConnectionID string
	ScanID       string
	Provider     string
	Surface      string
	AccountID    string
	Region       string
	ResourceName string
	ResourceARN  string
	SourceType   string
	HasTraceAxis bool
	HasLogAxis   bool
	LastSeenAt   *time.Time
	SnapshotJSON string
}

// SaveEventSourceInstances — v0.89.100 (#734 Stream 132). Persists the
// supplied event source rows. The (connection_id, scan_id, resource_arn)
// UNIQUE constraint means the call is idempotent on a per-resource basis
// within a single scan — an INSERT ... ON CONFLICT DO UPDATE refreshes
// the row in place rather than failing.
//
// Whole-batch transactional — a single failure rolls back the pending
// rows so a concurrent ListEventSourceInstances can't observe a partial
// write. See docs/proposals/event-source-tier-slice1.md §4 for the
// schema rationale.
func (s *Storage) SaveEventSourceInstances(
	ctx context.Context,
	rows []EventSourceInstanceRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	// ADR 0011 slice 3b — per-cloud scans run under WithSystemContext
	// (apply=false → rowTenant=DefaultTenant). The (connection_id, scan_id,
	// resource_arn) conflict target is the scanner's natural key and stays.
	scopeTenant, apply, err := tenantScope(ctx)
	if err != nil {
		return err
	}
	rowTenant := scopeTenant
	if !apply {
		rowTenant = identity.DefaultTenant
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const stmt = `INSERT INTO event_source_instance (
		id, connection_id, scan_id,
		provider, surface, account_id, region,
		resource_name, resource_arn, source_type,
		has_trace_axis, has_log_axis,
		last_seen_at, snapshot_json, tenant_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(connection_id, scan_id, resource_arn) DO UPDATE SET
		provider        = excluded.provider,
		surface         = excluded.surface,
		account_id      = excluded.account_id,
		region          = excluded.region,
		resource_name   = excluded.resource_name,
		source_type     = excluded.source_type,
		has_trace_axis  = excluded.has_trace_axis,
		has_log_axis    = excluded.has_log_axis,
		last_seen_at    = excluded.last_seen_at,
		snapshot_json   = excluded.snapshot_json,
		tenant_id       = excluded.tenant_id`

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
		var sourceType any
		if r.SourceType != "" {
			sourceType = r.SourceType
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
			sourceType,
			boolToInt(r.HasTraceAxis),
			boolToInt(r.HasLogAxis),
			lastSeenAt,
			r.SnapshotJSON,
			rowTenant,
		); err != nil {
			return fmt.Errorf("upsert event_source_instance row %q: %w", r.ResourceARN, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit event source save: %w", err)
	}
	return nil
}

// ListEventSourceInstances — v0.89.100. Returns rows for the
// (connection_id, scan_id) tuple, newest-created-at first. When scan_id
// is empty the query filters on connection_id only; when connection_id
// is empty the call returns an empty slice (the per-provider Inventory
// tab always scopes by connection_id).
//
// Each returned row carries the full SnapshotJSON column verbatim so
// callers can unmarshal back into their scanner snapshot shape; the
// universal columns are populated for query-time filtering without a
// JSON parse step.
func (s *Storage) ListEventSourceInstances(
	ctx context.Context,
	connectionID, scanID string,
) ([]EventSourceInstanceRow, error) {
	if connectionID == "" {
		return nil, nil
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	stmt := `SELECT
		connection_id, scan_id, provider, surface,
		account_id, region, resource_name,
		COALESCE(resource_arn, ''),
		COALESCE(source_type, ''),
		has_trace_axis, has_log_axis,
		last_seen_at, snapshot_json
		FROM event_source_instance
		WHERE connection_id = ?`
	args := []any{connectionID}
	if scanID != "" {
		stmt += ` AND scan_id = ?`
		args = append(args, scanID)
	}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	stmt += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("query event_source_instance: %w", err)
	}
	defer rows.Close()

	var out []EventSourceInstanceRow
	for rows.Next() {
		var (
			r          EventSourceInstanceRow
			hasTrace   int
			hasLog     int
			lastSeenAt sql.NullTime
		)
		if scanErr := rows.Scan(
			&r.ConnectionID, &r.ScanID, &r.Provider, &r.Surface,
			&r.AccountID, &r.Region, &r.ResourceName,
			&r.ResourceARN, &r.SourceType,
			&hasTrace, &hasLog,
			&lastSeenAt, &r.SnapshotJSON,
		); scanErr != nil {
			return nil, fmt.Errorf("scan event_source_instance row: %w", scanErr)
		}
		r.HasTraceAxis = hasTrace != 0
		r.HasLogAxis = hasLog != 0
		if lastSeenAt.Valid {
			t := lastSeenAt.Time.UTC()
			r.LastSeenAt = &t
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event_source_instance rows: %w", err)
	}
	return out, nil
}

// DeleteEventSourceInstancesBefore — v0.89.100. Removes
// event_source_instance rows whose created_at predates the supplied
// cutoff. Mirrors DeleteOrchestrationInstancesBefore — historic rows
// accumulate forever unless an operator-driven GC sweep prunes them.
// Slice 1 ships the predicate; scheduling lives in the chunk-6 runbook.
//
// Returns the number of rows removed for the operator-visible audit
// payload.
func (s *Storage) DeleteEventSourceInstancesBefore(
	ctx context.Context,
	before time.Time,
) (int64, error) {
	// ADR 0011 slice 3b — GC runs under WithSystemContext (apply=false → no
	// predicate → fleet-wide prune). A non-system caller scopes to its own
	// tenant.
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return 0, err
	}
	stmt := `DELETE FROM event_source_instance WHERE created_at < ?`
	args := []any{before.UTC()}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("delete event_source_instance: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
