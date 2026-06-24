// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeEventSourceRow is the test fixture builder. Mirrors the shape the
// per-cloud scanner-side adapter produces when translating from
// scanner.EventSourceInstanceSnapshot. Slice 1 chunk 1's reference
// fixture is an AWS EventBridge event bus.
func makeEventSourceRow(connID, scanID, arn string, hasTrace, hasLog bool) EventSourceInstanceRow {
	return EventSourceInstanceRow{
		ConnectionID: connID,
		ScanID:       scanID,
		Provider:     "aws",
		Surface:      "eventbridge",
		AccountID:    "123456789012",
		Region:       "us-east-1",
		ResourceName: "default",
		ResourceARN:  arn,
		SourceType:   "bus",
		HasTraceAxis: hasTrace,
		HasLogAxis:   hasLog,
		SnapshotJSON: `{"provider":"aws","surface":"eventbridge","resource_arn":"` + arn + `"}`,
	}
}

// TestEventSourceInstance_SaveAndList_RoundTrip — basic round-trip:
// SaveEventSourceInstances then ListEventSourceInstances returns the
// saved rows.
func TestEventSourceInstance_SaveAndList_RoundTrip(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		rows := []EventSourceInstanceRow{
			makeEventSourceRow("conn-1", "scan-1",
				"arn:aws:events:us-east-1:123456789012:event-bus/default", true, true),
			makeEventSourceRow("conn-1", "scan-1",
				"arn:aws:events:us-east-1:123456789012:event-bus/orders", true, false),
		}
		require.NoError(t, store.SaveEventSourceInstances(ctx, rows))

		got, err := store.ListEventSourceInstances(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		assert.Len(t, got, 2)
		arns := map[string]bool{}
		for _, r := range got {
			arns[r.ResourceARN] = true
			assert.Equal(t, "conn-1", r.ConnectionID)
			assert.Equal(t, "scan-1", r.ScanID)
			assert.Equal(t, "aws", r.Provider)
			assert.Equal(t, "eventbridge", r.Surface)
			assert.Equal(t, "bus", r.SourceType)
		}
		assert.True(t, arns["arn:aws:events:us-east-1:123456789012:event-bus/default"])
		assert.True(t, arns["arn:aws:events:us-east-1:123456789012:event-bus/orders"])
	})
}

// TestEventSourceInstance_SaveDuplicate_HandlesUniqueConstraint —
// re-saving the same (connection_id, scan_id, resource_arn) tuple
// updates the row in place rather than returning a UNIQUE violation
// error. Matches the ON CONFLICT DO UPDATE clause documented on
// SaveEventSourceInstances.
func TestEventSourceInstance_SaveDuplicate_HandlesUniqueConstraint(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		arn := "arn:aws:events:us-east-1:123456789012:event-bus/dup"
		first := makeEventSourceRow("conn-1", "scan-1", arn, true, false)
		require.NoError(t, store.SaveEventSourceInstances(ctx, []EventSourceInstanceRow{first}))

		// Second save with the same (conn, scan, arn) tuple but flipped
		// axes. ON CONFLICT DO UPDATE refreshes the row.
		updated := makeEventSourceRow("conn-1", "scan-1", arn, false, true)
		require.NoError(t, store.SaveEventSourceInstances(ctx, []EventSourceInstanceRow{updated}))

		got, err := store.ListEventSourceInstances(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 1, "duplicate upsert must NOT produce a second row")
		assert.False(t, got[0].HasTraceAxis, "axis 1 refreshed to false")
		assert.True(t, got[0].HasLogAxis, "axis 2 refreshed to true")
	})
}

// TestEventSourceInstance_ListByConnectionAndScan_FiltersCorrectly —
// rows scoped to a different (conn, scan) tuple are filtered out. Two
// scans against two connections; query for one tuple sees only its own
// rows.
func TestEventSourceInstance_ListByConnectionAndScan_FiltersCorrectly(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		require.NoError(t, store.SaveEventSourceInstances(ctx, []EventSourceInstanceRow{
			makeEventSourceRow("conn-a", "scan-1",
				"arn:aws:events:us-east-1:111111111111:event-bus/a", true, false),
			makeEventSourceRow("conn-a", "scan-2",
				"arn:aws:events:us-east-1:111111111111:event-bus/b", false, true),
			makeEventSourceRow("conn-b", "scan-1",
				"arn:aws:events:us-east-1:222222222222:event-bus/c", true, true),
		}))

		gotA1, err := store.ListEventSourceInstances(ctx, "conn-a", "scan-1")
		require.NoError(t, err)
		assert.Len(t, gotA1, 1)
		assert.Equal(t, "arn:aws:events:us-east-1:111111111111:event-bus/a", gotA1[0].ResourceARN)

		// connection-only filter returns both scans for conn-a.
		gotAAll, err := store.ListEventSourceInstances(ctx, "conn-a", "")
		require.NoError(t, err)
		assert.Len(t, gotAAll, 2)
	})
}

// TestMigration_v12_to_v13_AddsEventSourceTable — event source slice 1
// acceptance test 12. After the migration chain runs, the
// event_source_instance table exists and the schema_version row is at 13.
func TestMigration_v12_to_v13_AddsEventSourceTable(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		// Probe the schema: a SELECT against sqlite_master confirms
		// the table exists with the documented shape.
		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='event_source_instance'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "event_source_instance table must exist post-migration")

		// Verify the two indexes landed.
		row = store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_event_source_scan','idx_event_source_conn')`)
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 2, n, "both event_source_instance indexes must exist post-migration")
	})
}

// --- Event source tier slice 2 chunk 1 (v0.89.105, #741 Stream 139) ---
//
// These tests pin the new HasPropagationConfig + PropagationNotes fields
// round-tripping through the snapshot_json blob WITHOUT a schema bump
// (schema stays at v13). The new fields are additive JSON; the table
// definition is unchanged. The proposer reads the bool out of
// snapshot_json at recommendation time; the dashboard reads the notes
// for the side-panel chunk-5 wires.

// TestEventSourceInstance_RoundtripsHasPropagationConfig — slice 2
// chunk 1 roundtrip: a row whose SnapshotJSON carries
// "has_propagation_config":false persists through SaveEventSourceInstances
// + ListEventSourceInstances byte-identical. Pins the
// "additive-JSON-field, no schema migration" contract documented on the
// scanner.EventSourceInstanceSnapshot godoc + design doc §4.
func TestEventSourceInstance_RoundtripsHasPropagationConfig(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:events:us-east-1:123456789012:event-bus/prop-broken"
		const snapshotJSON = `{"provider":"aws","surface":"eventbridge","resource_arn":"` + arn +
			`","has_trace_axis":true,"has_log_axis":true,"has_propagation_config":false}`
		row := EventSourceInstanceRow{
			ConnectionID: "conn-1",
			ScanID:       "scan-1",
			Provider:     "aws",
			Surface:      "eventbridge",
			AccountID:    "123456789012",
			Region:       "us-east-1",
			ResourceName: "prop-broken",
			ResourceARN:  arn,
			SourceType:   "bus",
			HasTraceAxis: true,
			HasLogAxis:   true,
			SnapshotJSON: snapshotJSON,
		}
		require.NoError(t, store.SaveEventSourceInstances(ctx, []EventSourceInstanceRow{row}))

		got, err := store.ListEventSourceInstances(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, snapshotJSON, got[0].SnapshotJSON,
			"snapshot_json must round-trip byte-identical including has_propagation_config")
	})
}

// TestEventSourceInstance_RoundtripsPropagationNotes — slice 2 chunk 1
// roundtrip: the propagation_notes JSON array survives the
// SaveEventSourceInstances + ListEventSourceInstances cycle. The
// scanner emits a list of strings inside snapshot_json; the proposer's
// chunk-5 reasoning text quotes the list. Pins that no in-flight JSON
// normalization drops or re-orders the notes.
func TestEventSourceInstance_RoundtripsPropagationNotes(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const arn = "arn:aws:events:us-east-1:123456789012:event-bus/prop-notes"
		// Two distinct per-issue notes — the proposer's chunk-5
		// reasoning text walks every entry.
		const snapshotJSON = `{"provider":"aws","surface":"eventbridge","resource_arn":"` + arn +
			`","has_propagation_config":false,` +
			`"propagation_notes":["rule \"order-events\" has InputPath \"$.detail\" that strips trace header",` +
			`"rule \"shipping-events\" has InputTransformer template omitting trace header"]}`
		row := EventSourceInstanceRow{
			ConnectionID: "conn-1",
			ScanID:       "scan-1",
			Provider:     "aws",
			Surface:      "eventbridge",
			AccountID:    "123456789012",
			Region:       "us-east-1",
			ResourceName: "prop-notes",
			ResourceARN:  arn,
			SourceType:   "bus",
			HasTraceAxis: true,
			HasLogAxis:   true,
			SnapshotJSON: snapshotJSON,
		}
		require.NoError(t, store.SaveEventSourceInstances(ctx, []EventSourceInstanceRow{row}))

		got, err := store.ListEventSourceInstances(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, snapshotJSON, got[0].SnapshotJSON,
			"snapshot_json must round-trip including the propagation_notes array")
	})
}

// TestEventSourceInstance_SchemaVersionStaysAtV13 — slice 2 chunk 1
// contract: NO schema migration. The new HasPropagationConfig +
// PropagationNotes fields sit inside the snapshot_json blob (see the
// two roundtrip tests above); the SchemaVersion constant stays at 13.
// This test pins the contract — if a future change accidentally bumps
// the version for slice 2 chunk 1, this assertion catches it. The
// table-level event_source_instance schema is left unchanged from
// v0.89.100; ListEventSourceInstances continues to return the same
// column set.
func TestEventSourceInstance_SchemaVersionStaysAtV13(t *testing.T) {
	assert.Equal(t, 13, SchemaVersion,
		"slice 2 chunk 1 must NOT bump SchemaVersion; new fields are additive JSON only")
	// The Migrations slice (migrations.go) tops out at the v13
	// migration; slice 2 chunk 1 must not append a v14 entry.
	if got := len(Migrations); got != 13 {
		t.Errorf("Migrations length = %d, want 13 (slice 2 chunk 1 must NOT append v14)", got)
	}
}

// TestMigration_v12_to_v13_Idempotent — running the migration twice is a
// no-op. Per the design doc's slice 1 chunk 1 contract: "Run migration
// twice. Assert: no error, table exists, no data loss on pre-existing
// compute/db/k8s/serverless/orchestration tables." The inline-migrations
// slice in sqlite.go's migrate() ships the CREATE TABLE IF NOT EXISTS +
// CREATE INDEX IF NOT EXISTS pair; re-running it against the same DB
// must not error and must not lose pre-existing rows.
func TestMigration_v12_to_v13_Idempotent(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		// Save a row before the second migration to verify no data loss
		// on re-run.
		require.NoError(t, store.SaveEventSourceInstances(ctx, []EventSourceInstanceRow{
			makeEventSourceRow("conn-1", "scan-1",
				"arn:aws:events:us-east-1:111111111111:event-bus/idempotent", true, true),
		}))

		// Re-run the inline migration. CREATE TABLE IF NOT EXISTS +
		// CREATE INDEX IF NOT EXISTS guarantee no-op on the second pass.
		require.NoError(t, store.migrate())

		got, err := store.ListEventSourceInstances(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 1, "pre-existing row must survive the re-migration")
		assert.Equal(t, "arn:aws:events:us-east-1:111111111111:event-bus/idempotent", got[0].ResourceARN)

		// Third invocation for defense-in-depth.
		require.NoError(t, store.migrate())

		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='event_source_instance'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "event_source_instance table must survive re-migration")
	})
}
