// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeErrorRateRow is the test fixture builder. Mirrors the shape
// the chunk-2 detection branch produces when translating from
// scanner.AggregateMetricResult pairs (current + baseline). Slice 1
// chunk 1's reference fixture is an AWS Lambda function's 24h or
// 168h Errors / Invocations observation.
func makeErrorRateRow(connID, arn string, observedAt time.Time, windowHours, errorCount, invocationCount int) ErrorRateObservationRow {
	rate := 0.0
	if invocationCount > 0 {
		rate = float64(errorCount) / float64(invocationCount)
	}
	return ErrorRateObservationRow{
		ConnectionID:    connID,
		Provider:        "aws",
		Surface:         "lambda",
		AccountID:       "123456789012",
		Region:          "us-east-1",
		ResourceARN:     arn,
		ObservedAt:      observedAt,
		WindowHours:     windowHours,
		ErrorCount:      errorCount,
		InvocationCount: invocationCount,
		ErrorRate:       rate,
		SnapshotJSON:    `{"resource_arn":"` + arn + `","metric_name":"Errors","window_hours":24}`,
	}
}

// TestErrorRateObservation_SaveAndList_RoundTrip — slice 1 chunk 1
// acceptance test 11. SaveErrorRateObservation then
// ListErrorRateObservations returns the saved rows.
func TestErrorRateObservation_SaveAndList_RoundTrip(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:order-processor"
		now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		require.NoError(t, store.SaveErrorRateObservation(ctx,
			makeErrorRateRow("conn-1", arn, now, 24, 87, 3200)))

		got, err := store.ListErrorRateObservations(ctx, "", arn, 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "conn-1", got[0].ConnectionID)
		assert.Equal(t, "aws", got[0].Provider)
		assert.Equal(t, "lambda", got[0].Surface)
		assert.Equal(t, arn, got[0].ResourceARN)
		assert.Equal(t, 24, got[0].WindowHours)
		assert.Equal(t, 87, got[0].ErrorCount)
		assert.Equal(t, 3200, got[0].InvocationCount)
		assert.InDelta(t, 0.027187, got[0].ErrorRate, 0.0001)
		assert.True(t, got[0].ObservedAt.Equal(now), "observed_at round-trip")
		assert.NotEmpty(t, got[0].SnapshotJSON)
	})
}

// TestErrorRateObservation_SaveDuplicate_HandlesUniqueConstraint —
// re-saving the same (connection_id, resource_arn, observed_at,
// window_hours) tuple updates the row in place rather than failing
// with a UNIQUE violation. Matches the ON CONFLICT DO UPDATE clause
// documented on SaveErrorRateObservation.
func TestErrorRateObservation_SaveDuplicate_HandlesUniqueConstraint(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:dup"
		now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		first := makeErrorRateRow("conn-1", arn, now, 24, 87, 3200)
		require.NoError(t, store.SaveErrorRateObservation(ctx, first))

		// Second save with the same (conn, arn, observed_at,
		// window_hours) tuple but different counts — ON CONFLICT DO
		// UPDATE refreshes the row.
		updated := makeErrorRateRow("conn-1", arn, now, 24, 200, 5000)
		require.NoError(t, store.SaveErrorRateObservation(ctx, updated))

		got, err := store.ListErrorRateObservations(ctx, "", arn, 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1, "duplicate upsert must NOT produce a second row")
		assert.Equal(t, 200, got[0].ErrorCount, "error_count refreshed")
		assert.Equal(t, 5000, got[0].InvocationCount, "invocation_count refreshed")
		assert.InDelta(t, 0.04, got[0].ErrorRate, 0.0001, "error_rate refreshed")
	})
}

// TestErrorRateObservation_LatestForResource_ReturnsMostRecent —
// LatestErrorRateObservation returns the highest-observed_at row for
// the (resource_arn, window_hours) tuple, and returns false on a
// tuple with no rows.
func TestErrorRateObservation_LatestForResource_ReturnsMostRecent(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:latest-test"
		t0 := time.Date(2026, 6, 23, 14, 0, 0, 0, time.UTC)
		t1 := time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC)
		t2 := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		for i, ts := range []time.Time{t0, t1, t2} {
			require.NoError(t, store.SaveErrorRateObservation(ctx,
				makeErrorRateRow("conn-1", arn, ts, 24, 50+i*10, 2000+i*100)))
		}

		latest, ok, err := store.LatestErrorRateObservation(ctx, "", arn, 24)
		require.NoError(t, err)
		require.True(t, ok, "latest must be found")
		assert.True(t, latest.ObservedAt.Equal(t2), "latest observed_at must be t2")
		assert.Equal(t, 70, latest.ErrorCount, "latest error_count = the t2 row's value")

		// Not-found path — different window_hours value.
		_, ok, err = store.LatestErrorRateObservation(ctx, "", arn, 168)
		require.NoError(t, err)
		assert.False(t, ok, "no rows at window_hours=168 → ok==false")

		// Not-found path — unknown resource_arn.
		_, ok, err = store.LatestErrorRateObservation(ctx, "",
			"arn:aws:lambda:us-east-1:123456789012:function:absent", 24)
		require.NoError(t, err)
		assert.False(t, ok, "absent resource_arn → ok==false")
	})
}

// TestErrorRateObservation_ListSince_FiltersCorrectly — the since
// parameter on ListErrorRateObservations filters out rows whose
// observed_at predates the cutoff. The chunk-2 detection branch
// uses this to scope its current-window query to "last 24h" without
// fetching the rest of the row history.
func TestErrorRateObservation_ListSince_FiltersCorrectly(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:since-test"
		old := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
		recent := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		require.NoError(t, store.SaveErrorRateObservation(ctx,
			makeErrorRateRow("conn-1", arn, old, 24, 60, 3000)))
		require.NoError(t, store.SaveErrorRateObservation(ctx,
			makeErrorRateRow("conn-1", arn, recent, 24, 90, 3000)))

		// since = 2026-06-20 → only the recent row passes.
		cutoff := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
		got, err := store.ListErrorRateObservations(ctx, "", arn, 24, cutoff)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.True(t, got[0].ObservedAt.Equal(recent), "only the recent row must pass")

		// since = zero time → both rows pass.
		got, err = store.ListErrorRateObservations(ctx, "", arn, 24, time.Time{})
		require.NoError(t, err)
		assert.Len(t, got, 2, "zero-time since must drop the lower bound")
	})
}

// TestErrorRateObservation_DeleteBefore_RemovesOldRows —
// DeleteErrorRateObservationsBefore removes rows whose observed_at
// predates the cutoff and leaves newer rows untouched.
func TestErrorRateObservation_DeleteBefore_RemovesOldRows(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:gc-test"
		old := time.Date(2026, 1, 1, 14, 0, 0, 0, time.UTC)
		recent := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		require.NoError(t, store.SaveErrorRateObservation(ctx,
			makeErrorRateRow("conn-1", arn, old, 24, 10, 1000)))
		require.NoError(t, store.SaveErrorRateObservation(ctx,
			makeErrorRateRow("conn-1", arn, recent, 24, 80, 2000)))

		// Sanity — both rows present before the sweep.
		got, err := store.ListErrorRateObservations(ctx, "", arn, 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 2)

		// Sweep rows older than 2026-03-01 — the "old" row drops; the
		// "recent" row survives.
		cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		require.NoError(t, store.DeleteErrorRateObservationsBefore(ctx, cutoff))

		got, err = store.ListErrorRateObservations(ctx, "", arn, 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1, "old row must be deleted")
		assert.True(t, got[0].ObservedAt.Equal(recent), "the surviving row must be the recent one")
	})
}

// TestErrorRateObservation_DistinctWindowHours_DontCollide — the
// design doc §3 prescribes two windows: 24h (current) and 168h
// (7d baseline) per resource. Both observations land at the same
// observed_at timestamp; the (connection_id, resource_arn,
// observed_at, window_hours) UNIQUE constraint MUST allow both
// rows (because window_hours differs) instead of treating them as
// a duplicate.
func TestErrorRateObservation_DistinctWindowHours_DontCollide(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:dual-window"
		now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		// 24h current window — 87/3200 ≈ 2.7%.
		require.NoError(t, store.SaveErrorRateObservation(ctx,
			makeErrorRateRow("conn-1", arn, now, 24, 87, 3200)))
		// 168h baseline window — 192/22400 ≈ 0.86%. Same observed_at,
		// different window_hours.
		require.NoError(t, store.SaveErrorRateObservation(ctx,
			makeErrorRateRow("conn-1", arn, now, 168, 192, 22400)))

		// Unfiltered query (windowHours=0) returns both rows.
		got, err := store.ListErrorRateObservations(ctx, "", arn, 0, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 2, "both window_hours rows must coexist at the same observed_at")

		// Per-window queries return the right row each.
		current, ok, err := store.LatestErrorRateObservation(ctx, "", arn, 24)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, 87, current.ErrorCount, "current row carries the 24h count")
		assert.Equal(t, 3200, current.InvocationCount)

		baseline, ok, err := store.LatestErrorRateObservation(ctx, "", arn, 168)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, 192, baseline.ErrorCount, "baseline row carries the 168h count")
		assert.Equal(t, 22400, baseline.InvocationCount)
	})
}

// TestMigration_v14_to_v15_AddsErrorRateObservationTable — slice 1
// chunk 1 acceptance test 10 (companion to the migration idempotence
// test below). After the migration chain runs, the
// error_rate_observation table exists and both indexes landed.
func TestMigration_v14_to_v15_AddsErrorRateObservationTable(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='error_rate_observation'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "error_rate_observation table must exist post-migration")

		// Verify the two indexes landed.
		row = store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_errorrate_resource','idx_errorrate_observed')`)
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 2, n, "both error_rate_observation indexes must exist post-migration")

		// SchemaVersion + Migrations slice both reflect the v15 bump.
		assert.Equal(t, 15, SchemaVersion, "SchemaVersion must bump to 15")
		assert.Len(t, Migrations, 15, "Migrations slice must contain 15 entries")
	})
}

// TestMigration_v14_to_v15_Idempotent — slice 1 chunk 1 acceptance
// test 10: running the migration twice is a no-op. Per the design
// doc chunk 1 contract: "Run migration twice. Assert: no error,
// table exists, no data loss on pre-existing tables." The inline-
// migrations slice in sqlite.go's migrate() ships CREATE TABLE IF
// NOT EXISTS + CREATE INDEX IF NOT EXISTS; re-running must not error
// and must not lose pre-existing rows.
func TestMigration_v14_to_v15_Idempotent(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		// Save a row before the second migration to verify no data loss.
		now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)
		require.NoError(t, store.SaveErrorRateObservation(ctx,
			makeErrorRateRow("conn-1",
				"arn:aws:lambda:us-east-1:111111111111:function:idempotent",
				now, 24, 87, 3200)))

		// Re-run the inline migration.
		require.NoError(t, store.migrate())

		got, err := store.ListErrorRateObservations(ctx, "",
			"arn:aws:lambda:us-east-1:111111111111:function:idempotent", 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1, "pre-existing row must survive the re-migration")

		// Third invocation for defense-in-depth.
		require.NoError(t, store.migrate())

		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='error_rate_observation'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "error_rate_observation table must survive re-migration")

		// Sibling tables from previous migrations still exist —
		// defense-in-depth that the v15 migration didn't disturb the
		// v10 / v11 / v12 / v13 / v14 tables.
		for _, name := range []string{
			"trace_resource_seen",
			"serverless_instance",
			"orchestration_instance",
			"event_source_instance",
			"cold_start_observation",
		} {
			row := store.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name)
			var n int
			require.NoError(t, row.Scan(&n))
			assert.Equal(t, 1, n, name+" must survive the v15 migration")
		}
	})
}
