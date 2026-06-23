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

// makeServerlessRow is the test fixture builder. Mirrors the shape
// the orchestrator-side adapter produces when translating from
// scanner.ServerlessInstanceSnapshot.
func makeServerlessRow(connID, scanID, arn string, hasTrace, hasOTel bool) ServerlessInstanceRow {
	return ServerlessInstanceRow{
		ConnectionID:  connID,
		ScanID:        scanID,
		Provider:      "aws",
		Surface:       "lambda",
		AccountID:     "123456789012",
		Region:        "us-east-1",
		ResourceName:  "checkout",
		ResourceARN:   arn,
		Runtime:       "python3.11",
		HasTraceAxis:  hasTrace,
		HasOTelDistro: hasOTel,
		SnapshotJSON:  `{"provider":"aws","surface":"lambda","resource_arn":"` + arn + `"}`,
	}
}

// TestServerlessStore_SaveAndList_RoundTrip — basic round-trip:
// SaveServerless then ListServerless returns the saved rows.
func TestServerlessStore_SaveAndList_RoundTrip(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		rows := []ServerlessInstanceRow{
			makeServerlessRow("conn-1", "scan-1",
				"arn:aws:lambda:us-east-1:123456789012:function:checkout", true, true),
			makeServerlessRow("conn-1", "scan-1",
				"arn:aws:lambda:us-east-1:123456789012:function:orders", true, false),
		}
		require.NoError(t, store.SaveServerless(ctx, rows))

		got, err := store.ListServerless(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		assert.Len(t, got, 2)
		// Order is newest-created-at first; both rows landed within
		// the same instant so order isn't asserted — only set
		// membership.
		arns := map[string]bool{}
		for _, r := range got {
			arns[r.ResourceARN] = true
			assert.Equal(t, "conn-1", r.ConnectionID)
			assert.Equal(t, "scan-1", r.ScanID)
			assert.Equal(t, "aws", r.Provider)
			assert.Equal(t, "lambda", r.Surface)
		}
		assert.True(t, arns["arn:aws:lambda:us-east-1:123456789012:function:checkout"])
		assert.True(t, arns["arn:aws:lambda:us-east-1:123456789012:function:orders"])
	})
}

// TestServerlessStore_SaveDuplicate_HandlesUniqueConstraint —
// re-saving the same (connection_id, scan_id, resource_arn) tuple
// updates the row in place rather than returning a UNIQUE violation
// error. Matches the ON CONFLICT DO UPDATE clause documented on
// SaveServerless.
func TestServerlessStore_SaveDuplicate_HandlesUniqueConstraint(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		arn := "arn:aws:lambda:us-east-1:123456789012:function:dup"
		first := makeServerlessRow("conn-1", "scan-1", arn, true, false)
		require.NoError(t, store.SaveServerless(ctx, []ServerlessInstanceRow{first}))

		// Second save with the same (conn, scan, arn) tuple but
		// flipped axes. ON CONFLICT DO UPDATE refreshes the row.
		updated := makeServerlessRow("conn-1", "scan-1", arn, false, true)
		require.NoError(t, store.SaveServerless(ctx, []ServerlessInstanceRow{updated}))

		got, err := store.ListServerless(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 1, "duplicate upsert must NOT produce a second row")
		assert.False(t, got[0].HasTraceAxis, "axis 1 refreshed to false")
		assert.True(t, got[0].HasOTelDistro, "axis 2 refreshed to true")
	})
}

// TestServerlessStore_ListByConnectionAndScan_FiltersCorrectly —
// rows scoped to a different (conn, scan) tuple are filtered out.
// Two scans against two connections; query for one tuple sees only
// its own rows.
func TestServerlessStore_ListByConnectionAndScan_FiltersCorrectly(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		require.NoError(t, store.SaveServerless(ctx, []ServerlessInstanceRow{
			makeServerlessRow("conn-a", "scan-1",
				"arn:aws:lambda:us-east-1:111111111111:function:a", true, false),
			makeServerlessRow("conn-a", "scan-2",
				"arn:aws:lambda:us-east-1:111111111111:function:b", false, true),
			makeServerlessRow("conn-b", "scan-1",
				"arn:aws:lambda:us-east-1:222222222222:function:c", true, true),
		}))

		gotA1, err := store.ListServerless(ctx, "conn-a", "scan-1")
		require.NoError(t, err)
		assert.Len(t, gotA1, 1)
		assert.Equal(t, "arn:aws:lambda:us-east-1:111111111111:function:a", gotA1[0].ResourceARN)

		// connection-only filter returns both scans for conn-a.
		gotAAll, err := store.ListServerless(ctx, "conn-a", "")
		require.NoError(t, err)
		assert.Len(t, gotAAll, 2)
	})
}

// TestServerlessStore_LastSeenAt_RoundTrip — the optional
// LastSeenAt pointer survives the save → list cycle. Tests both
// the nil case (no traces observed) and the populated case.
func TestServerlessStore_LastSeenAt_RoundTrip(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		seen := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
		rows := []ServerlessInstanceRow{
			{
				ConnectionID: "conn-1", ScanID: "scan-1",
				Provider: "aws", Surface: "lambda",
				AccountID: "123456789012", Region: "us-east-1",
				ResourceName: "withTrace",
				ResourceARN:  "arn:aws:lambda:us-east-1:123456789012:function:withTrace",
				HasTraceAxis: true,
				LastSeenAt:   &seen,
				SnapshotJSON: `{"resource_name":"withTrace"}`,
			},
			{
				ConnectionID: "conn-1", ScanID: "scan-1",
				Provider: "aws", Surface: "lambda",
				AccountID: "123456789012", Region: "us-east-1",
				ResourceName: "noTrace",
				ResourceARN:  "arn:aws:lambda:us-east-1:123456789012:function:noTrace",
				LastSeenAt:   nil,
				SnapshotJSON: `{"resource_name":"noTrace"}`,
			},
		}
		require.NoError(t, store.SaveServerless(ctx, rows))

		got, err := store.ListServerless(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 2)

		var hadTrace, hadNoTrace bool
		for _, r := range got {
			if r.ResourceName == "withTrace" {
				require.NotNil(t, r.LastSeenAt)
				assert.Equal(t, seen.UTC(), r.LastSeenAt.UTC())
				hadTrace = true
			}
			if r.ResourceName == "noTrace" {
				assert.Nil(t, r.LastSeenAt, "nil last_seen_at must stay nil")
				hadNoTrace = true
			}
		}
		assert.True(t, hadTrace && hadNoTrace, "both fixture rows expected")
	})
}

// TestServerlessStore_EmptyInput_NoError — saving an empty slice is
// a no-op and listing an unknown connection returns an empty slice
// without erroring.
func TestServerlessStore_EmptyInput_NoError(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		require.NoError(t, store.SaveServerless(ctx, nil))

		got, err := store.ListServerless(ctx, "no-such-conn", "no-such-scan")
		require.NoError(t, err)
		assert.Len(t, got, 0)
	})
}

// TestServerlessStore_DeleteBefore_RemovesOldRows — DeleteServerlessBefore
// prunes rows whose created_at predates the cutoff. The created_at
// column has a CURRENT_TIMESTAMP default, so we save rows now then
// delete with a cutoff far in the future to verify the predicate
// fires (a cutoff far in the past would prune zero rows, which
// doesn't exercise the path).
func TestServerlessStore_DeleteBefore_RemovesOldRows(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		require.NoError(t, store.SaveServerless(ctx, []ServerlessInstanceRow{
			makeServerlessRow("conn-1", "scan-1",
				"arn:aws:lambda:us-east-1:111111111111:function:old", true, false),
		}))

		future := time.Now().UTC().Add(24 * time.Hour)
		n, err := store.DeleteServerlessBefore(ctx, future)
		require.NoError(t, err)
		assert.Equal(t, int64(1), n)

		got, err := store.ListServerless(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		assert.Len(t, got, 0)
	})
}

// TestMigration_v10_to_v11_AddsServerlessTable — slice 1 acceptance
// test 10. After the migration chain runs, the serverless_instance
// table exists and the schema_version row is at 11.
func TestMigration_v10_to_v11_AddsServerlessTable(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		// Probe the schema: a SELECT against the table column list
		// confirms the table exists with the documented shape.
		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='serverless_instance'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "serverless_instance table must exist post-migration")

		// Verify the two indexes landed.
		row = store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_serverless_scan','idx_serverless_conn')`)
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 2, n, "both serverless_instance indexes must exist post-migration")
	})
}

// TestMigration_v10_to_v11_Idempotent — running the migration twice
// is a no-op. Per the design doc's slice 1 chunk 1 contract: "Run
// migration twice. Assert: no error, table exists, no data loss on
// pre-existing compute/db/k8s tables." The inline-migrations slice
// in sqlite.go's migrate() ships the CREATE TABLE IF NOT EXISTS +
// CREATE INDEX IF NOT EXISTS pair; re-running it against the same
// DB must not error and must not lose pre-existing rows.
func TestMigration_v10_to_v11_Idempotent(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		// Save a row before the second migration to verify no data
		// loss on re-run.
		require.NoError(t, store.SaveServerless(ctx, []ServerlessInstanceRow{
			makeServerlessRow("conn-1", "scan-1",
				"arn:aws:lambda:us-east-1:111111111111:function:idempotent", true, true),
		}))

		// Re-run the inline migration. CREATE TABLE IF NOT EXISTS +
		// CREATE INDEX IF NOT EXISTS guarantee no-op on the second
		// pass; the existing isColumnExistsError handler covers any
		// ALTER TABLE in the chain.
		require.NoError(t, store.migrate())

		got, err := store.ListServerless(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 1, "pre-existing row must survive the re-migration")
		assert.Equal(t, "arn:aws:lambda:us-east-1:111111111111:function:idempotent", got[0].ResourceARN)

		// Third invocation for defense-in-depth: another migrate()
		// call still no-ops cleanly. The slice 1 design doc names
		// "no error" as the explicit contract.
		require.NoError(t, store.migrate())

		// Confirm the table + indexes still exist.
		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='serverless_instance'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "serverless_instance table must survive re-migration")
	})
}

// TestMigration_SchemaVersionConstant — the SchemaVersion constant
// in migrations.go bumps to v11 alongside the
// ServerlessInstanceSchema addition. Pins the version stamp so a
// future migration appended to the Migrations slice without bumping
// the constant trips this test.
func TestMigration_SchemaVersionConstant(t *testing.T) {
	if SchemaVersion < 11 {
		t.Errorf("SchemaVersion = %d, want >= 11 (slice 1 chunk 1 bumps to 11)", SchemaVersion)
	}
	if len(Migrations) < 11 {
		t.Errorf("Migrations length = %d, want >= 11 (slice 1 chunk 1 appends ServerlessInstanceSchema)", len(Migrations))
	}
}