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

// makeColdStartRow is the test fixture builder. Mirrors the shape the
// chunk-2 detection branch produces when translating from
// scanner.AggregateMetricResult. Slice 1 chunk 1's reference fixture
// is an AWS Lambda function's 24h or 168h InitDuration P95
// observation.
func makeColdStartRow(connID, arn string, observedAt time.Time, windowHours int, p95Ms float64) ColdStartObservationRow {
	return ColdStartObservationRow{
		ConnectionID: connID,
		Provider:     "aws",
		Surface:      "lambda",
		AccountID:    "123456789012",
		Region:       "us-east-1",
		ResourceARN:  arn,
		ObservedAt:   observedAt,
		WindowHours:  windowHours,
		P95Ms:        p95Ms,
		SampleCount:  142,
		SnapshotJSON: `{"resource_arn":"` + arn + `","metric_name":"InitDuration","statistic":"p95","value":` +
			"4230" + `,"unit":"Milliseconds","sample_count":142}`,
	}
}

// TestColdStartObservation_SaveAndList_RoundTrip — slice 1 chunk 1
// acceptance test 10. SaveColdStartObservation then
// ListColdStartObservations returns the saved rows.
func TestColdStartObservation_SaveAndList_RoundTrip(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:order-processor"
		now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		require.NoError(t, store.SaveColdStartObservation(ctx,
			makeColdStartRow("conn-1", arn, now, 24, 4230.5)))

		got, err := store.ListColdStartObservations(ctx, arn, 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "conn-1", got[0].ConnectionID)
		assert.Equal(t, "aws", got[0].Provider)
		assert.Equal(t, "lambda", got[0].Surface)
		assert.Equal(t, arn, got[0].ResourceARN)
		assert.Equal(t, 24, got[0].WindowHours)
		assert.InDelta(t, 4230.5, got[0].P95Ms, 0.001)
		assert.Equal(t, 142, got[0].SampleCount)
		assert.True(t, got[0].ObservedAt.Equal(now), "observed_at round-trip")
		assert.NotEmpty(t, got[0].SnapshotJSON)
	})
}

// TestColdStartObservation_SaveDuplicate_HandlesUniqueConstraint —
// re-saving the same (connection_id, resource_arn, observed_at,
// window_hours) tuple updates the row in place rather than failing
// with a UNIQUE violation. Matches the ON CONFLICT DO UPDATE clause
// documented on SaveColdStartObservation.
func TestColdStartObservation_SaveDuplicate_HandlesUniqueConstraint(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:dup"
		now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		first := makeColdStartRow("conn-1", arn, now, 24, 4230.5)
		require.NoError(t, store.SaveColdStartObservation(ctx, first))

		// Second save with the same (conn, arn, observed_at,
		// window_hours) tuple but a different P95 — ON CONFLICT DO
		// UPDATE refreshes the row.
		updated := makeColdStartRow("conn-1", arn, now, 24, 5500.0)
		updated.SampleCount = 200
		require.NoError(t, store.SaveColdStartObservation(ctx, updated))

		got, err := store.ListColdStartObservations(ctx, arn, 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1, "duplicate upsert must NOT produce a second row")
		assert.InDelta(t, 5500.0, got[0].P95Ms, 0.001, "p95 refreshed")
		assert.Equal(t, 200, got[0].SampleCount, "sample_count refreshed")
	})
}

// TestColdStartObservation_LatestForResource_ReturnsMostRecent —
// LatestColdStartObservation returns the highest-observed_at row for
// the (resource_arn, window_hours) tuple, and returns false on a tuple
// with no rows.
func TestColdStartObservation_LatestForResource_ReturnsMostRecent(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:latest-test"
		t0 := time.Date(2026, 6, 23, 14, 0, 0, 0, time.UTC)
		t1 := time.Date(2026, 6, 24, 14, 0, 0, 0, time.UTC)
		t2 := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		for _, ts := range []time.Time{t0, t1, t2} {
			require.NoError(t, store.SaveColdStartObservation(ctx,
				makeColdStartRow("conn-1", arn, ts, 24, 1000.0+float64(ts.Day()))))
		}

		latest, ok, err := store.LatestColdStartObservation(ctx, arn, 24)
		require.NoError(t, err)
		require.True(t, ok, "latest must be found")
		assert.True(t, latest.ObservedAt.Equal(t2), "latest observed_at must be t2")

		// Not-found path — different window_hours value.
		_, ok, err = store.LatestColdStartObservation(ctx, arn, 168)
		require.NoError(t, err)
		assert.False(t, ok, "no rows at window_hours=168 → ok==false")

		// Not-found path — unknown resource_arn.
		_, ok, err = store.LatestColdStartObservation(ctx,
			"arn:aws:lambda:us-east-1:123456789012:function:absent", 24)
		require.NoError(t, err)
		assert.False(t, ok, "absent resource_arn → ok==false")
	})
}

// TestColdStartObservation_ListSince_FiltersCorrectly — the since
// parameter on ListColdStartObservations filters out rows whose
// observed_at predates the cutoff. The chunk-2 detection branch uses
// this to scope its current-window query to "last 24h" without
// fetching the rest of the row history.
func TestColdStartObservation_ListSince_FiltersCorrectly(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:since-test"
		old := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
		recent := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		require.NoError(t, store.SaveColdStartObservation(ctx,
			makeColdStartRow("conn-1", arn, old, 24, 2820.0)))
		require.NoError(t, store.SaveColdStartObservation(ctx,
			makeColdStartRow("conn-1", arn, recent, 24, 4230.0)))

		// since = 2026-06-20 → only the recent row passes.
		cutoff := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
		got, err := store.ListColdStartObservations(ctx, arn, 24, cutoff)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.True(t, got[0].ObservedAt.Equal(recent), "only the recent row must pass")

		// since = zero time → both rows pass.
		got, err = store.ListColdStartObservations(ctx, arn, 24, time.Time{})
		require.NoError(t, err)
		assert.Len(t, got, 2, "zero-time since must drop the lower bound")
	})
}

// TestColdStartObservation_DeleteBefore_RemovesOldRows —
// DeleteColdStartObservationsBefore removes rows whose observed_at
// predates the cutoff and leaves newer rows untouched.
func TestColdStartObservation_DeleteBefore_RemovesOldRows(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:gc-test"
		old := time.Date(2026, 1, 1, 14, 0, 0, 0, time.UTC)
		recent := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		require.NoError(t, store.SaveColdStartObservation(ctx,
			makeColdStartRow("conn-1", arn, old, 24, 1000.0)))
		require.NoError(t, store.SaveColdStartObservation(ctx,
			makeColdStartRow("conn-1", arn, recent, 24, 2000.0)))

		// Sanity — both rows present before the sweep.
		got, err := store.ListColdStartObservations(ctx, arn, 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 2)

		// Sweep rows older than 2026-03-01 — the "old" row drops; the
		// "recent" row survives.
		cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		require.NoError(t, store.DeleteColdStartObservationsBefore(ctx, cutoff))

		got, err = store.ListColdStartObservations(ctx, arn, 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1, "old row must be deleted")
		assert.True(t, got[0].ObservedAt.Equal(recent), "the surviving row must be the recent one")
	})
}

// TestColdStartObservation_DistinctWindowHours_DontCollide — the
// design doc §3 prescribes two windows: 24h (current) and 168h
// (7d baseline) per Lambda. Both observations land at the same
// observed_at timestamp; the (connection_id, resource_arn,
// observed_at, window_hours) UNIQUE constraint MUST allow both rows
// (because window_hours differs) instead of treating them as a
// duplicate.
func TestColdStartObservation_DistinctWindowHours_DontCollide(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:lambda:us-east-1:123456789012:function:dual-window"
		now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

		// 24h current window
		require.NoError(t, store.SaveColdStartObservation(ctx,
			makeColdStartRow("conn-1", arn, now, 24, 4230.0)))
		// 168h baseline window — same observed_at, different window_hours.
		require.NoError(t, store.SaveColdStartObservation(ctx,
			makeColdStartRow("conn-1", arn, now, 168, 2820.0)))

		// Unfiltered query (windowHours=0) returns both rows.
		got, err := store.ListColdStartObservations(ctx, arn, 0, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 2, "both window_hours rows must coexist at the same observed_at")

		// Per-window queries return the right row each.
		current, ok, err := store.LatestColdStartObservation(ctx, arn, 24)
		require.NoError(t, err)
		require.True(t, ok)
		assert.InDelta(t, 4230.0, current.P95Ms, 0.001)

		baseline, ok, err := store.LatestColdStartObservation(ctx, arn, 168)
		require.NoError(t, err)
		require.True(t, ok)
		assert.InDelta(t, 2820.0, baseline.P95Ms, 0.001)
	})
}

// TestMigration_v13_to_v14_AddsColdStartObservationTable — slice 1
// chunk 1 acceptance test 9 (companion to the migration idempotence
// test below). After the migration chain runs, the
// cold_start_observation table exists and both indexes landed.
func TestMigration_v13_to_v14_AddsColdStartObservationTable(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cold_start_observation'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "cold_start_observation table must exist post-migration")

		// Verify the two indexes landed.
		row = store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_coldstart_resource','idx_coldstart_observed')`)
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 2, n, "both cold_start_observation indexes must exist post-migration")

		// SchemaVersion + Migrations slice both reflect the latest
		// schema version. v0.89.113's test originally pinned v14; the
		// v0.89.127 chunk-1 bump to v15 moves the floor up by one but
		// the v14 → v15 ratchet leaves the cold_start_observation
		// table itself unchanged.
		assert.GreaterOrEqual(t, SchemaVersion, 14, "SchemaVersion must be >= 14 (cold_start_observation landed at v14)")
		assert.GreaterOrEqual(t, len(Migrations), 14, "Migrations slice must contain >= 14 entries")
	})
}

// TestMigration_v13_to_v14_Idempotent — slice 1 chunk 1 acceptance
// test 9: running the migration twice is a no-op. Per the design doc
// chunk 1 contract: "Run migration twice. Assert: no error, table
// exists, no data loss on pre-existing tables." The inline-migrations
// slice in sqlite.go's migrate() ships CREATE TABLE IF NOT EXISTS +
// CREATE INDEX IF NOT EXISTS; re-running must not error and must not
// lose pre-existing rows.
func TestMigration_v13_to_v14_Idempotent(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		// Save a row before the second migration to verify no data loss.
		now := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)
		require.NoError(t, store.SaveColdStartObservation(ctx,
			makeColdStartRow("conn-1",
				"arn:aws:lambda:us-east-1:111111111111:function:idempotent",
				now, 24, 4230.0)))

		// Re-run the inline migration.
		require.NoError(t, store.migrate())

		got, err := store.ListColdStartObservations(ctx,
			"arn:aws:lambda:us-east-1:111111111111:function:idempotent", 24, time.Time{})
		require.NoError(t, err)
		require.Len(t, got, 1, "pre-existing row must survive the re-migration")

		// Third invocation for defense-in-depth.
		require.NoError(t, store.migrate())

		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cold_start_observation'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "cold_start_observation table must survive re-migration")

		// Sibling tables from previous migrations still exist —
		// defense-in-depth that the v14 migration didn't disturb the
		// v10 / v11 / v12 / v13 tables.
		for _, name := range []string{
			"trace_resource_seen",
			"serverless_instance",
			"orchestration_instance",
			"event_source_instance",
		} {
			row := store.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name)
			var n int
			require.NoError(t, row.Scan(&n))
			assert.Equal(t, 1, n, name+" must survive the v14 migration")
		}
	})
}
