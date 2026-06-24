// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ColdStartObservationRow is the storage-layer projection of a single
// per-Lambda cold-start P95 observation. v0.89.113 (#751 Stream 149,
// slice 1 chunk 1 of the Cold-start latency analysis arc).
//
// Why this lives in the sqlite package rather than reusing
// scanner.AggregateMetricResult directly: same rationale as
// EventSourceInstanceRow / OrchestrationInstanceRow — the sqlite
// package sits at the leaf of the storage tree, so any dependency it
// adds propagates up the call graph. Reusing the scanner result would
// introduce a sqlite → scanner → credstore → services →
// applicationstore → sqlite cycle. Instead the storage layer carries
// the projection without importing the scanner package; the chunk 2
// detection branch adapts scanner.AggregateMetricResult into this row
// shape at SaveColdStartObservation call time.
//
// The fields mirror the columns documented on cold_start_observation
// in migrations.go::ColdStartObservationSchema. SnapshotJSON carries
// the canonical scanner.AggregateMetricResult serialization so the
// chunk-2 per-resource cold_start API endpoint can return the raw
// shape without re-querying CloudWatch.
//
// See docs/proposals/cold-start-latency-slice1.md §4.
type ColdStartObservationRow struct {
	// ID is the row primary key — a UUID stamped at insert time.
	// Callers may leave this empty; SaveColdStartObservation stamps
	// it before the INSERT.
	ID string

	// ConnectionID is the credstore.CloudConnection ID this
	// observation belongs to. Scopes per-resource queries to the
	// caller's connection without leaking other operators' data.
	ConnectionID string

	// Provider is the cloud name — "aws" / "gcp" / "azure" / "oci".
	// Slice 1 ships AWS only; future slices populate the other
	// values.
	Provider string

	// Surface is the per-cloud serverless surface identifier —
	// "lambda" / "cloudrun" / "cloudfunc" / "azfunc" / "ocifunc".
	// Slice 1 ships "lambda" only.
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
	// cold-start detection uses 24 (current window) and 168 (7d
	// baseline). Future slices may add additional windows; the
	// schema does not gate on specific values.
	WindowHours int

	// P95Ms is the 95th-percentile aggregated value in
	// milliseconds. The unit is normalized at row-write time —
	// CloudWatch returns "Milliseconds" for InitDuration; the
	// scanner adapter converts to ms before populating this field.
	P95Ms float64

	// SampleCount is the number of underlying datapoints the
	// aggregation was computed over. Zero is valid — the chunk-2
	// detection branch SKIPS rows with insufficient samples without
	// flagging an error (per the MetricQuerier interface
	// empty-result-set contract).
	SampleCount int

	// SnapshotJSON is the canonical scanner.AggregateMetricResult
	// serialization so the chunk-2 per-resource cold_start API
	// endpoint can return the raw shape without re-querying
	// CloudWatch.
	SnapshotJSON string
}

// SaveColdStartObservation — v0.89.113 (#751 Stream 149). Persists a
// single cold-start P95 observation. The (connection_id, resource_arn,
// observed_at, window_hours) UNIQUE constraint means the call is
// idempotent on a per-observation basis — an INSERT ... ON CONFLICT DO
// UPDATE refreshes the row in place rather than failing.
//
// Required fields: ConnectionID, Provider, Surface, AccountID, Region,
// ResourceARN, WindowHours. ObservedAt may be the zero time — the
// caller usually populates it with time.Now().UTC() so the schema's
// CURRENT_TIMESTAMP default doesn't fire; either path is acceptable.
//
// See docs/proposals/cold-start-latency-slice1.md §4 (storage schema)
// and §11 acceptance test 10 (round-trip persistence).
func (s *Storage) SaveColdStartObservation(
	ctx context.Context,
	row ColdStartObservationRow,
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

	const stmt = `INSERT INTO cold_start_observation (
		id, connection_id, provider, surface, account_id, region,
		resource_arn, observed_at, window_hours, p95_ms,
		sample_count, snapshot_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(connection_id, resource_arn, observed_at, window_hours) DO UPDATE SET
		provider      = excluded.provider,
		surface       = excluded.surface,
		account_id    = excluded.account_id,
		region        = excluded.region,
		p95_ms        = excluded.p95_ms,
		sample_count  = excluded.sample_count,
		snapshot_json = excluded.snapshot_json`

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
		row.P95Ms,
		row.SampleCount,
		row.SnapshotJSON,
	); err != nil {
		return fmt.Errorf("upsert cold_start_observation: %w", err)
	}
	return nil
}

// ListColdStartObservations — v0.89.113. Returns observations for a
// resource at the specified window_hours, with observed_at >= since.
// Newest-first ordering by observed_at.
//
// When resourceARN is empty the call returns nil — the chunk-2 detection
// branch always scopes by resource_arn. When windowHours is <= 0 the
// query is unfiltered on window_hours (returns rows for any window).
// When since is the zero time the lower bound on observed_at is dropped.
func (s *Storage) ListColdStartObservations(
	ctx context.Context,
	resourceARN string,
	windowHours int,
	since time.Time,
) ([]ColdStartObservationRow, error) {
	if resourceARN == "" {
		return nil, nil
	}

	// Build the query incrementally so the optional filters compose
	// cleanly. Indexed lookups: idx_coldstart_resource covers the
	// resource_arn filter; observed_at + window_hours stay as
	// secondary predicates evaluated by the SQLite query planner.
	query := `SELECT
		id, connection_id, provider, surface,
		account_id, region, resource_arn,
		observed_at, window_hours, p95_ms,
		sample_count, snapshot_json
		FROM cold_start_observation
		WHERE resource_arn = ?`
	args := []any{resourceARN}
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
		return nil, fmt.Errorf("query cold_start_observation: %w", err)
	}
	defer rows.Close()

	var out []ColdStartObservationRow
	for rows.Next() {
		var (
			r          ColdStartObservationRow
			observedAt time.Time
		)
		if scanErr := rows.Scan(
			&r.ID, &r.ConnectionID, &r.Provider, &r.Surface,
			&r.AccountID, &r.Region, &r.ResourceARN,
			&observedAt, &r.WindowHours, &r.P95Ms,
			&r.SampleCount, &r.SnapshotJSON,
		); scanErr != nil {
			return nil, fmt.Errorf("scan cold_start_observation row: %w", scanErr)
		}
		r.ObservedAt = observedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cold_start_observation rows: %w", err)
	}
	return out, nil
}

// LatestColdStartObservation — v0.89.113. Returns the most recent
// observation for the supplied (resource_arn, window_hours) tuple. The
// bool return is true when a row was found; false when no observation
// exists yet for the tuple (the chunk-2 detection branch uses the bool
// to skip the resource without flagging an error). No error is returned
// in the not-found case.
//
// The chunk-3 per-resource cold_start API endpoint uses this to populate
// the "current_window" + "baseline_window" sub-shapes (two calls, one
// per window_hours value).
func (s *Storage) LatestColdStartObservation(
	ctx context.Context,
	resourceARN string,
	windowHours int,
) (ColdStartObservationRow, bool, error) {
	if resourceARN == "" {
		return ColdStartObservationRow{}, false, nil
	}

	const stmt = `SELECT
		id, connection_id, provider, surface,
		account_id, region, resource_arn,
		observed_at, window_hours, p95_ms,
		sample_count, snapshot_json
		FROM cold_start_observation
		WHERE resource_arn = ? AND window_hours = ?
		ORDER BY observed_at DESC
		LIMIT 1`

	var (
		r          ColdStartObservationRow
		observedAt time.Time
	)
	err := s.db.QueryRowContext(ctx, stmt, resourceARN, windowHours).Scan(
		&r.ID, &r.ConnectionID, &r.Provider, &r.Surface,
		&r.AccountID, &r.Region, &r.ResourceARN,
		&observedAt, &r.WindowHours, &r.P95Ms,
		&r.SampleCount, &r.SnapshotJSON,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ColdStartObservationRow{}, false, nil
		}
		return ColdStartObservationRow{}, false, fmt.Errorf("query latest cold_start_observation: %w", err)
	}
	r.ObservedAt = observedAt.UTC()
	return r, true, nil
}

// DeleteColdStartObservationsBefore — v0.89.113. Removes
// cold_start_observation rows whose observed_at predates the supplied
// cutoff. Mirrors the slice 2 retention-policy slot the design doc §12
// leaves open — historic rows accumulate forever unless an operator-
// driven GC sweep prunes them. Slice 1 ships the predicate; scheduling
// lives in the chunk-4 runbook.
//
// Returns only the error sentinel — the row count is not surfaced
// because the per-resource cold_start API endpoint that consumes this
// store does not need it, and the chunk-1 contract pins the function
// signature to `error` (per the slice 1 design doc §10 / spec).
func (s *Storage) DeleteColdStartObservationsBefore(
	ctx context.Context,
	before time.Time,
) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM cold_start_observation WHERE observed_at < ?`,
		before.UTC(),
	); err != nil {
		return fmt.Errorf("delete cold_start_observation: %w", err)
	}
	return nil
}
