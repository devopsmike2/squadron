// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/google/uuid"
)

// ErrorRateObservationRow is the storage-layer projection of a single
// per-resource error-rate observation. v0.89.127 (#767 Stream 165,
// slice 1 chunk 1 of the Error rate correlation arc).
//
// Mirrors ColdStartObservationRow (v0.89.113) verbatim in shape and
// rationale: the sqlite package sits at the leaf of the storage tree,
// so the projection lives here rather than reusing
// scanner.AggregateMetricResult directly to avoid a sqlite → scanner
// → credstore → services → applicationstore → sqlite cycle. The
// chunk-2 detection branch adapts the scanner result into this row
// shape at SaveErrorRateObservation call time.
//
// The fields mirror the columns documented on
// error_rate_observation in migrations.go::ErrorRateObservationSchema.
// SnapshotJSON carries the canonical scanner.AggregateMetricResult
// serialization so the chunk-2 per-resource error_rate API endpoint
// can return the raw shape without re-querying the cloud provider.
//
// See docs/proposals/error-rate-correlation-slice1.md §5.
type ErrorRateObservationRow struct {
	// ID is the row primary key — a UUID stamped at insert time.
	// Callers may leave this empty; SaveErrorRateObservation stamps
	// it before the INSERT.
	ID string

	// ConnectionID is the credstore.CloudConnection ID this
	// observation belongs to. Scopes per-resource queries to the
	// caller's connection without leaking other operators' data.
	ConnectionID string

	// Provider is the cloud name — "aws" / "gcp" / "azure" / "oci".
	Provider string

	// Surface is the per-cloud serverless surface identifier —
	// "lambda" / "cloudrun" / "cloudfunc" / "azfunc" / "ocifunc".
	Surface string

	// AccountID is the provider-native primary identifier of the
	// owning connection (account_id / project_id /
	// subscription_id / tenancy OCID).
	AccountID string

	// Region is where the resource lives. Serverless surfaces are
	// per-region on every cloud Squadron supports.
	Region string

	// ResourceARN is the provider-native fully-qualified resource
	// identifier. Lambda ARN / Cloud Run service self-link / Cloud
	// Functions resource path / Azure Functions resource ID / OCI
	// Functions OCID.
	ResourceARN string

	// ObservedAt is the reference timestamp for the aggregation —
	// the time the MetricQuerier ran the query, not the time of
	// the underlying datapoints. Used as part of the UNIQUE
	// constraint with window_hours to distinguish the 24h current-
	// window row from the 168h baseline row at the same observation
	// time.
	ObservedAt time.Time

	// WindowHours is the aggregation window in hours. Slice 1's
	// error-rate detection uses 24 (current window) and 168 (7d
	// baseline). Future slices may add additional windows; the
	// schema does not gate on specific values.
	WindowHours int

	// ErrorCount is the absolute count of error events observed in
	// the window — the numerator of the error-rate ratio. The
	// detection rule gates on ErrorCount >= 50 (slice 1 §3.2) as
	// an absolute floor to avoid firing on 1-2 errors that happen
	// to be 2x baseline of 0.5 errors.
	ErrorCount int

	// InvocationCount is the absolute count of total invocations
	// in the window — the denominator of the error-rate ratio.
	// The detection rule gates on InvocationCount >= 1000 (slice
	// 1 §3) as a noise filter; below the floor the ratio
	// arithmetic is statistical noise rather than operational
	// signal.
	InvocationCount int

	// ErrorRate is ErrorCount / InvocationCount as a float in
	// [0, 1]. Precomputed at save time so the detection-time
	// comparison against the baseline row's ErrorRate stays a
	// single floating-point compare without re-deriving the ratio
	// from the absolute counts.
	ErrorRate float64

	// SnapshotJSON is the canonical scanner.AggregateMetricResult
	// serialization so the chunk-2 per-resource error_rate API
	// endpoint can return the raw shape without re-querying the
	// cloud provider.
	SnapshotJSON string
}

// SaveErrorRateObservation — v0.89.127 (#767 Stream 165). Persists a
// single error-rate observation. The (connection_id, resource_arn,
// observed_at, window_hours) UNIQUE constraint means the call is
// idempotent on a per-observation basis — an INSERT ... ON CONFLICT
// DO UPDATE refreshes the row in place rather than failing.
//
// Required fields: ConnectionID, Provider, Surface, AccountID,
// Region, ResourceARN, WindowHours. ObservedAt may be the zero time
// — the caller usually populates it with time.Now().UTC() so the
// schema's CURRENT_TIMESTAMP default doesn't fire; either path is
// acceptable.
//
// See docs/proposals/error-rate-correlation-slice1.md §5 (storage
// schema) and §11 acceptance test 11 (round-trip persistence).
func (s *Storage) SaveErrorRateObservation(
	ctx context.Context,
	row ErrorRateObservationRow,
) error {
	if row.ConnectionID == "" {
		return errors.New("connection_id required")
	}
	if row.ResourceARN == "" {
		return errors.New("resource_arn required")
	}
	if row.WindowHours <= 0 {
		return fmt.Errorf("window_hours must be > 0 (got %d)", row.WindowHours)
	}

	id := row.ID
	if id == "" {
		id = uuid.NewString()
	}

	// ADR 0011 slice 3b — the metric substrate runs under WithSystemContext
	// (apply=false → rowTenant=DefaultTenant). The (connection_id,
	// resource_arn, observed_at, window_hours) conflict target is the
	// observation's natural key and stays.
	scopeTenant, apply, err := tenantScope(ctx)
	if err != nil {
		return err
	}
	rowTenant := scopeTenant
	if !apply {
		rowTenant = identity.DefaultTenant
	}

	const stmt = `INSERT INTO error_rate_observation (
		id, connection_id, provider, surface, account_id, region,
		resource_arn, observed_at, window_hours, error_count,
		invocation_count, error_rate, snapshot_json, tenant_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(connection_id, resource_arn, observed_at, window_hours) DO UPDATE SET
		provider         = excluded.provider,
		surface          = excluded.surface,
		account_id       = excluded.account_id,
		region           = excluded.region,
		error_count      = excluded.error_count,
		invocation_count = excluded.invocation_count,
		error_rate       = excluded.error_rate,
		snapshot_json    = excluded.snapshot_json,
		tenant_id        = excluded.tenant_id`

	observedAt := row.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	} else {
		observedAt = observedAt.UTC()
	}

	if _, err := s.db.ExecContext(ctx, stmt,
		id,
		row.ConnectionID,
		row.Provider,
		row.Surface,
		row.AccountID,
		row.Region,
		row.ResourceARN,
		observedAt,
		row.WindowHours,
		row.ErrorCount,
		row.InvocationCount,
		row.ErrorRate,
		row.SnapshotJSON,
		rowTenant,
	); err != nil {
		return fmt.Errorf("upsert error_rate_observation: %w", err)
	}
	return nil
}

// ListErrorRateObservations — v0.89.127. Returns observations for a
// resource at the specified window_hours, with observed_at >= since.
// Newest-first ordering by observed_at.
//
// When resourceARN is empty the call returns nil — the chunk-2
// detection branch always scopes by resource_arn. When windowHours
// is <= 0 the query is unfiltered on window_hours (returns rows for
// any window). When since is the zero time the lower bound on
// observed_at is dropped.
// connectionID scopes the read to a single connection. The write key is
// UNIQUE(connection_id, resource_arn, observed_at, window_hours), so passing a
// non-empty connectionID keeps a caller from reading another connection's
// observation for the same resource_arn. Empty = unscoped (the resource-ARN-only
// HTTP read endpoints carry no connection dimension yet).
func (s *Storage) ListErrorRateObservations(
	ctx context.Context,
	connectionID string,
	resourceARN string,
	windowHours int,
	since time.Time,
) ([]ErrorRateObservationRow, error) {
	if resourceARN == "" {
		return nil, nil
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}

	// Build the query incrementally so the optional filters compose
	// cleanly. Indexed lookups: idx_errorrate_resource covers the
	// resource_arn filter; observed_at + window_hours stay as
	// secondary predicates evaluated by the SQLite query planner.
	query := `SELECT
		id, connection_id, provider, surface,
		account_id, region, resource_arn,
		observed_at, window_hours, error_count,
		invocation_count, error_rate, snapshot_json
		FROM error_rate_observation
		WHERE resource_arn = ?`
	args := []any{resourceARN}
	if apply {
		query += " AND tenant_id = ?"
		args = append(args, tenant)
	}
	if connectionID != "" {
		query += " AND connection_id = ?"
		args = append(args, connectionID)
	}
	if windowHours > 0 {
		query += " AND window_hours = ?"
		args = append(args, windowHours)
	}
	if !since.IsZero() {
		query += " AND observed_at >= ?"
		args = append(args, since.UTC())
	}
	query += " ORDER BY observed_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query error_rate_observation: %w", err)
	}
	defer rows.Close()

	var out []ErrorRateObservationRow
	for rows.Next() {
		var (
			r          ErrorRateObservationRow
			observedAt time.Time
		)
		if scanErr := rows.Scan(
			&r.ID, &r.ConnectionID, &r.Provider, &r.Surface,
			&r.AccountID, &r.Region, &r.ResourceARN,
			&observedAt, &r.WindowHours, &r.ErrorCount,
			&r.InvocationCount, &r.ErrorRate, &r.SnapshotJSON,
		); scanErr != nil {
			return nil, fmt.Errorf("scan error_rate_observation row: %w", scanErr)
		}
		r.ObservedAt = observedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate error_rate_observation rows: %w", err)
	}
	return out, nil
}

// LatestErrorRateObservation — v0.89.127. Returns the most recent
// observation for the supplied (resource_arn, window_hours) tuple.
// The bool return is true when a row was found; false when no
// observation exists yet for the tuple (the chunk-2 detection
// branch uses the bool to skip the resource without flagging an
// error). No error is returned in the not-found case.
//
// The chunk-2 per-resource error_rate API endpoint uses this to
// populate the "current_window" + "baseline_window" sub-shapes
// (two calls, one per window_hours value).
// connectionID scopes the read to a single connection (empty = unscoped); see
// ListErrorRateObservations for the rationale.
func (s *Storage) LatestErrorRateObservation(
	ctx context.Context,
	connectionID string,
	resourceARN string,
	windowHours int,
) (ErrorRateObservationRow, bool, error) {
	if resourceARN == "" {
		return ErrorRateObservationRow{}, false, nil
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return ErrorRateObservationRow{}, false, err
	}

	query := `SELECT
		id, connection_id, provider, surface,
		account_id, region, resource_arn,
		observed_at, window_hours, error_count,
		invocation_count, error_rate, snapshot_json
		FROM error_rate_observation
		WHERE resource_arn = ? AND window_hours = ?`
	args := []any{resourceARN, windowHours}
	if apply {
		query += " AND tenant_id = ?"
		args = append(args, tenant)
	}
	if connectionID != "" {
		query += " AND connection_id = ?"
		args = append(args, connectionID)
	}
	query += " ORDER BY observed_at DESC LIMIT 1"

	var (
		r          ErrorRateObservationRow
		observedAt time.Time
	)
	err = s.db.QueryRowContext(ctx, query, args...).Scan(
		&r.ID, &r.ConnectionID, &r.Provider, &r.Surface,
		&r.AccountID, &r.Region, &r.ResourceARN,
		&observedAt, &r.WindowHours, &r.ErrorCount,
		&r.InvocationCount, &r.ErrorRate, &r.SnapshotJSON,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrorRateObservationRow{}, false, nil
		}
		return ErrorRateObservationRow{}, false, fmt.Errorf("query latest error_rate_observation: %w", err)
	}
	r.ObservedAt = observedAt.UTC()
	return r, true, nil
}

// DeleteErrorRateObservationsBefore — v0.89.127. Removes
// error_rate_observation rows whose observed_at predates the supplied
// cutoff. Mirrors the slice 2 retention-policy slot the design doc
// §12 leaves open — historic rows accumulate forever unless an
// operator-driven GC sweep prunes them. Slice 1 ships the predicate;
// scheduling lives in the chunk-4 runbook.
//
// Returns only the error sentinel — the row count is not surfaced
// because the per-resource error_rate API endpoint that consumes
// this store does not need it.
func (s *Storage) DeleteErrorRateObservationsBefore(
	ctx context.Context,
	before time.Time,
) error {
	// ADR 0011 slice 3b — GC runs under WithSystemContext (apply=false → no
	// predicate → fleet-wide prune). A non-system caller scopes to its own
	// tenant.
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return err
	}
	stmt := `DELETE FROM error_rate_observation WHERE observed_at < ?`
	args := []any{before.UTC()}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	if _, err := s.db.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("delete error_rate_observation: %w", err)
	}
	return nil
}
