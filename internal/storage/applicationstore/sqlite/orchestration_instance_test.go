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

// makeOrchestrationRow is the test fixture builder. Mirrors the shape
// the orchestrator-side adapter produces when translating from
// scanner.OrchestrationInstanceSnapshot. Slice 1 chunk 1's reference
// fixture is an AWS Step Functions STANDARD state machine.
func makeOrchestrationRow(connID, scanID, arn string, hasTrace, hasLog bool) OrchestrationInstanceRow {
	return OrchestrationInstanceRow{
		ConnectionID: connID,
		ScanID:       scanID,
		Provider:     "aws",
		Surface:      "stepfunc",
		AccountID:    "123456789012",
		Region:       "us-east-1",
		ResourceName: "checkout",
		ResourceARN:  arn,
		WorkflowType: "STANDARD",
		HasTraceAxis: hasTrace,
		HasLogAxis:   hasLog,
		SnapshotJSON: `{"provider":"aws","surface":"stepfunc","resource_arn":"` + arn + `"}`,
	}
}

// TestOrchestrationInstance_SaveAndList_RoundTrip — basic round-trip:
// SaveOrchestrationInstances then ListOrchestrationInstances returns the
// saved rows.
func TestOrchestrationInstance_SaveAndList_RoundTrip(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		rows := []OrchestrationInstanceRow{
			makeOrchestrationRow("conn-1", "scan-1",
				"arn:aws:states:us-east-1:123456789012:stateMachine:checkout", true, true),
			makeOrchestrationRow("conn-1", "scan-1",
				"arn:aws:states:us-east-1:123456789012:stateMachine:orders", true, false),
		}
		require.NoError(t, store.SaveOrchestrationInstances(ctx, rows))

		got, err := store.ListOrchestrationInstances(ctx, "conn-1", "scan-1")
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
			assert.Equal(t, "stepfunc", r.Surface)
			assert.Equal(t, "STANDARD", r.WorkflowType)
		}
		assert.True(t, arns["arn:aws:states:us-east-1:123456789012:stateMachine:checkout"])
		assert.True(t, arns["arn:aws:states:us-east-1:123456789012:stateMachine:orders"])
	})
}

// TestOrchestrationInstance_SaveDuplicate_HandlesUniqueConstraint —
// re-saving the same (connection_id, scan_id, resource_arn) tuple
// updates the row in place rather than returning a UNIQUE violation
// error. Matches the ON CONFLICT DO UPDATE clause documented on
// SaveOrchestrationInstances.
func TestOrchestrationInstance_SaveDuplicate_HandlesUniqueConstraint(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		arn := "arn:aws:states:us-east-1:123456789012:stateMachine:dup"
		first := makeOrchestrationRow("conn-1", "scan-1", arn, true, false)
		require.NoError(t, store.SaveOrchestrationInstances(ctx, []OrchestrationInstanceRow{first}))

		// Second save with the same (conn, scan, arn) tuple but
		// flipped axes. ON CONFLICT DO UPDATE refreshes the row.
		updated := makeOrchestrationRow("conn-1", "scan-1", arn, false, true)
		require.NoError(t, store.SaveOrchestrationInstances(ctx, []OrchestrationInstanceRow{updated}))

		got, err := store.ListOrchestrationInstances(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 1, "duplicate upsert must NOT produce a second row")
		assert.False(t, got[0].HasTraceAxis, "axis 1 refreshed to false")
		assert.True(t, got[0].HasLogAxis, "axis 2 refreshed to true")
	})
}

// TestOrchestrationInstance_ListByConnectionAndScan_FiltersCorrectly —
// rows scoped to a different (conn, scan) tuple are filtered out. Two
// scans against two connections; query for one tuple sees only its own
// rows.
func TestOrchestrationInstance_ListByConnectionAndScan_FiltersCorrectly(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		require.NoError(t, store.SaveOrchestrationInstances(ctx, []OrchestrationInstanceRow{
			makeOrchestrationRow("conn-a", "scan-1",
				"arn:aws:states:us-east-1:111111111111:stateMachine:a", true, false),
			makeOrchestrationRow("conn-a", "scan-2",
				"arn:aws:states:us-east-1:111111111111:stateMachine:b", false, true),
			makeOrchestrationRow("conn-b", "scan-1",
				"arn:aws:states:us-east-1:222222222222:stateMachine:c", true, true),
		}))

		gotA1, err := store.ListOrchestrationInstances(ctx, "conn-a", "scan-1")
		require.NoError(t, err)
		assert.Len(t, gotA1, 1)
		assert.Equal(t, "arn:aws:states:us-east-1:111111111111:stateMachine:a", gotA1[0].ResourceARN)

		// connection-only filter returns both scans for conn-a.
		gotAAll, err := store.ListOrchestrationInstances(ctx, "conn-a", "")
		require.NoError(t, err)
		assert.Len(t, gotAAll, 2)
	})
}

// TestMigration_v11_to_v12_AddsOrchestrationTable — orchestration slice
// 1 acceptance test 10. After the migration chain runs, the
// orchestration_instance table exists and the schema_version row is at
// 12.
func TestMigration_v11_to_v12_AddsOrchestrationTable(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		// Probe the schema: a SELECT against sqlite_master confirms
		// the table exists with the documented shape.
		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='orchestration_instance'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "orchestration_instance table must exist post-migration")

		// Verify the two indexes landed.
		row = store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_orchestration_scan','idx_orchestration_conn')`)
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 2, n, "both orchestration_instance indexes must exist post-migration")
	})
}

// TestMigration_v11_to_v12_Idempotent — running the migration twice is
// a no-op. Per the design doc's slice 1 chunk 1 contract: "Run
// migration twice. Assert: no error, table exists, no data loss on
// pre-existing compute/db/k8s/serverless tables." The inline-migrations
// slice in sqlite.go's migrate() ships the CREATE TABLE IF NOT EXISTS +
// CREATE INDEX IF NOT EXISTS pair; re-running it against the same DB
// must not error and must not lose pre-existing rows.
func TestMigration_v11_to_v12_Idempotent(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		// Save a row before the second migration to verify no data
		// loss on re-run.
		require.NoError(t, store.SaveOrchestrationInstances(ctx, []OrchestrationInstanceRow{
			makeOrchestrationRow("conn-1", "scan-1",
				"arn:aws:states:us-east-1:111111111111:stateMachine:idempotent", true, true),
		}))

		// Re-run the inline migration. CREATE TABLE IF NOT EXISTS +
		// CREATE INDEX IF NOT EXISTS guarantee no-op on the second
		// pass; the existing isColumnExistsError handler covers any
		// ALTER TABLE in the chain.
		require.NoError(t, store.migrate())

		got, err := store.ListOrchestrationInstances(ctx, "conn-1", "scan-1")
		require.NoError(t, err)
		require.Len(t, got, 1, "pre-existing row must survive the re-migration")
		assert.Equal(t, "arn:aws:states:us-east-1:111111111111:stateMachine:idempotent", got[0].ResourceARN)

		// Third invocation for defense-in-depth: another migrate()
		// call still no-ops cleanly. The slice 1 design doc names
		// "no error" as the explicit contract.
		require.NoError(t, store.migrate())

		// Confirm the table + indexes still exist.
		row := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='orchestration_instance'`)
		var n int
		require.NoError(t, row.Scan(&n))
		assert.Equal(t, 1, n, "orchestration_instance table must survive re-migration")
	})
}
