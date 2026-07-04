// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// tenant_rebuild_test.go — ADR 0011 slice 3b′ coverage.
//
// Four groups:
//  1. Inert-in-OSS: on a fresh (default-tenant) DB, upsert/uniqueness of each
//     rebuilt table is unchanged — same key updates (not duplicates); a
//     duplicate alert-rule name within a tenant is still rejected.
//  2. Same-key-across-tenants NOW ALLOWED (the point of 3b′): two tenants can
//     hold the same natural key; both coexist and each read is isolated.
//     Covered for a PK-rebuild table (expected_agents, recommendation_dismissals)
//     AND the UNIQUE-rebuild table (alert_rules).
//  3. Idempotency: running migrate() twice against the same DB file does not
//     error and does not double-rebuild (the guard skips).
//  4. Upgrade path: a fresh NewSQLiteStorage is born with the composite key
//     (createTables) and the rebuild guard no-ops — verified by the composite
//     key being present and the store operating normally.

// rawStore casts the ApplicationStore back to *Storage so a test can run raw
// SQL assertions against the rebuilt schema (row counts, PRAGMA probes).
func rawStore(t *testing.T, s types.ApplicationStore) *Storage {
	t.Helper()
	st, ok := s.(*Storage)
	require.True(t, ok, "expected *Storage")
	return st
}

func makeAlertRule(id, name string) *types.AlertRule {
	return &types.AlertRule{
		ID:                id,
		Name:              name,
		Query:             "up",
		ThresholdOperator: types.ThresholdGreater,
		ThresholdValue:    1,
		IntervalSeconds:   60,
		Severity:          types.AlertSeverityWarning,
		Enabled:           true,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
}

// ----------------------------------------------------------------------------
// Group 4 (fold in first): the fresh DB is born with the composite key.
// ----------------------------------------------------------------------------

func TestTenantRebuild_FreshDBHasCompositeKeys(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		st := rawStore(t, s)

		// Five PK-rebuild tables: tenant_id must be part of the PRIMARY KEY.
		for _, rb := range tenantRebuilds {
			if rb.unique {
				continue
			}
			ok, err := hasCompositeTenantKey(st.db, rb)
			require.NoError(t, err)
			assert.Truef(t, ok, "table %q should have a composite (tenant_id, %v) PK on a fresh DB", rb.table, rb.keyCols)
		}

		// alert_rules: a UNIQUE(tenant_id, name) index must exist.
		ok, err := hasCompositeUniqueIndex(st.db, "alert_rules", []string{"tenant_id", "name"})
		require.NoError(t, err)
		assert.True(t, ok, "alert_rules should carry a UNIQUE(tenant_id, name) on a fresh DB")
	})
}

// ----------------------------------------------------------------------------
// Group 1: inert-in-OSS — same key updates (not duplicates) on default tenant.
// ----------------------------------------------------------------------------

func TestTenantRebuild_InertOSS_ExpectedAgentUpsertUpdates(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()
		st := rawStore(t, s)

		require.NoError(t, s.UpsertExpectedAgent(ctx, &types.ExpectedAgent{Hostname: "host-1", Source: "ci", Notes: "first"}))
		require.NoError(t, s.UpsertExpectedAgent(ctx, &types.ExpectedAgent{Hostname: "host-1", Source: "ci", Notes: "second"}))

		var n int
		require.NoError(t, st.db.QueryRow(`SELECT COUNT(*) FROM expected_agents WHERE hostname='host-1'`).Scan(&n))
		assert.Equal(t, 1, n, "same hostname must update, not duplicate, on the default tenant")

		got, err := s.ListExpectedAgents(ctx, "ci")
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "second", got[0].Notes)
	})
}

func TestTenantRebuild_InertOSS_DismissalUpsertUpdates(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()
		st := rawStore(t, s)

		require.NoError(t, s.DismissRecommendation(ctx, &types.RecommendationDismissal{RecommendationID: "rec-1", Reason: "one"}))
		require.NoError(t, s.DismissRecommendation(ctx, &types.RecommendationDismissal{RecommendationID: "rec-1", Reason: "two"}))

		var n int
		require.NoError(t, st.db.QueryRow(`SELECT COUNT(*) FROM recommendation_dismissals WHERE recommendation_id='rec-1'`).Scan(&n))
		assert.Equal(t, 1, n, "same recommendation_id must update, not duplicate, on the default tenant")

		var reason string
		require.NoError(t, st.db.QueryRow(`SELECT reason FROM recommendation_dismissals WHERE recommendation_id='rec-1'`).Scan(&reason))
		assert.Equal(t, "two", reason)
	})
}

func TestTenantRebuild_InertOSS_AlertRuleDuplicateNameRejectedWithinTenant(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()

		require.NoError(t, s.CreateAlertRule(ctx, makeAlertRule(uuid.NewString(), "high-cpu")))
		// A second rule with the same name in the SAME (default) tenant must
		// still be rejected by the composite UNIQUE(tenant_id, name).
		err := s.CreateAlertRule(ctx, makeAlertRule(uuid.NewString(), "high-cpu"))
		require.Error(t, err, "duplicate alert-rule name within a tenant must still be rejected")
	})
}

func TestTenantRebuild_InertOSS_TraceUpsertAccumulates(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()
		st := rawStore(t, s)
		now := time.Now().UTC()

		_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{makeTraceRow("k1", now, 10)})
		require.NoError(t, err)
		_, err = s.UpsertTraceResources(ctx, []traceindex.ResourceRow{makeTraceRow("k1", now, 5)})
		require.NoError(t, err)

		var n int
		require.NoError(t, st.db.QueryRow(`SELECT COUNT(*) FROM trace_resource_seen WHERE resource_key='k1'`).Scan(&n))
		assert.Equal(t, 1, n, "same resource_key must accumulate on one row for the default tenant")

		got, err := s.GetTraceResource(ctx, "k1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, int64(15), got.SpanCount24h, "span counts must accumulate across upserts")
	})
}

// ----------------------------------------------------------------------------
// Group 2: same key across tenants NOW ALLOWED — the point of 3b′.
// ----------------------------------------------------------------------------

func TestTenantRebuild_SameKeyAcrossTenants_ExpectedAgents(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		st := rawStore(t, s)
		ctxA := identity.WithTenant(context.Background(), "a")
		ctxB := identity.WithTenant(context.Background(), "b")

		require.NoError(t, s.UpsertExpectedAgent(ctxA, &types.ExpectedAgent{Hostname: "shared-host", Source: "ci", Notes: "tenant-a"}))
		require.NoError(t, s.UpsertExpectedAgent(ctxB, &types.ExpectedAgent{Hostname: "shared-host", Source: "ci", Notes: "tenant-b"}))

		// Both rows coexist (2 rows, one per tenant).
		var total int
		require.NoError(t, st.db.QueryRow(`SELECT COUNT(*) FROM expected_agents WHERE hostname='shared-host'`).Scan(&total))
		assert.Equal(t, 2, total, "same hostname in two tenants must coexist (one row per tenant)")

		// Each tenant's read returns only its own row.
		aRows, err := s.ListExpectedAgents(ctxA, "ci")
		require.NoError(t, err)
		require.Len(t, aRows, 1)
		assert.Equal(t, "tenant-a", aRows[0].Notes)

		bRows, err := s.ListExpectedAgents(ctxB, "ci")
		require.NoError(t, err)
		require.Len(t, bRows, 1)
		assert.Equal(t, "tenant-b", bRows[0].Notes)

		t.Logf("3b′ PROOF (expected_agents): hostname=%q coexists in tenants a+b — total rows=%d, a.notes=%q b.notes=%q",
			"shared-host", total, aRows[0].Notes, bRows[0].Notes)
	})
}

func TestTenantRebuild_SameKeyAcrossTenants_Dismissals(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		st := rawStore(t, s)
		ctxA := identity.WithTenant(context.Background(), "a")
		ctxB := identity.WithTenant(context.Background(), "b")

		require.NoError(t, s.DismissRecommendation(ctxA, &types.RecommendationDismissal{RecommendationID: "rec-shared", Reason: "a"}))
		require.NoError(t, s.DismissRecommendation(ctxB, &types.RecommendationDismissal{RecommendationID: "rec-shared", Reason: "b"}))

		var total int
		require.NoError(t, st.db.QueryRow(`SELECT COUNT(*) FROM recommendation_dismissals WHERE recommendation_id='rec-shared'`).Scan(&total))
		assert.Equal(t, 2, total, "same recommendation_id in two tenants must coexist")

		var reasonA, reasonB string
		require.NoError(t, st.db.QueryRow(`SELECT reason FROM recommendation_dismissals WHERE recommendation_id='rec-shared' AND tenant_id='a'`).Scan(&reasonA))
		require.NoError(t, st.db.QueryRow(`SELECT reason FROM recommendation_dismissals WHERE recommendation_id='rec-shared' AND tenant_id='b'`).Scan(&reasonB))
		assert.Equal(t, "a", reasonA)
		assert.Equal(t, "b", reasonB)

		t.Logf("3b′ PROOF (recommendation_dismissals): recommendation_id=%q coexists in tenants a+b — total rows=%d, a.reason=%q b.reason=%q",
			"rec-shared", total, reasonA, reasonB)
	})
}

func TestTenantRebuild_SameKeyAcrossTenants_AlertRules(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		st := rawStore(t, s)
		ctxA := identity.WithTenant(context.Background(), "a")
		ctxB := identity.WithTenant(context.Background(), "b")

		// The UNIQUE-rebuild table: the SAME rule name in two tenants must
		// BOTH succeed (pre-3b′ this collided on the global UNIQUE(name)).
		require.NoError(t, s.CreateAlertRule(ctxA, makeAlertRule(uuid.NewString(), "high-cpu")))
		require.NoError(t, s.CreateAlertRule(ctxB, makeAlertRule(uuid.NewString(), "high-cpu")),
			"same alert-rule name in a second tenant must succeed under UNIQUE(tenant_id, name)")

		var total int
		require.NoError(t, st.db.QueryRow(`SELECT COUNT(*) FROM alert_rules WHERE name='high-cpu'`).Scan(&total))
		assert.Equal(t, 2, total, "same alert-rule name in two tenants must coexist")

		// Each tenant reads only its own rule.
		aRules, err := s.ListAlertRules(ctxA)
		require.NoError(t, err)
		require.Len(t, aRules, 1)
		bRules, err := s.ListAlertRules(ctxB)
		require.NoError(t, err)
		require.Len(t, bRules, 1)
		assert.NotEqual(t, aRules[0].ID, bRules[0].ID, "each tenant owns a distinct rule row")

		t.Logf("3b′ PROOF (alert_rules): name=%q coexists in tenants a+b — total rows=%d, a.id=%q b.id=%q",
			"high-cpu", total, aRules[0].ID, bRules[0].ID)
	})
}

// ----------------------------------------------------------------------------
// Group 3: idempotency — migrate() twice against the same DB file is a no-op.
// ----------------------------------------------------------------------------

func TestTenantRebuild_IdempotentAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "idem.db")
	logger := zap.NewNop()

	// First construction: runs createTables + migrations + rebuilds.
	s1, err := NewSQLiteStorage(dbPath, logger)
	require.NoError(t, err)

	ctxA := identity.WithTenant(context.Background(), "a")
	ctxB := identity.WithTenant(context.Background(), "b")
	require.NoError(t, s1.UpsertExpectedAgent(ctxA, &types.ExpectedAgent{Hostname: "h", Source: "ci", Notes: "a"}))
	require.NoError(t, s1.UpsertExpectedAgent(ctxB, &types.ExpectedAgent{Hostname: "h", Source: "ci", Notes: "b"}))
	rawStore(t, s1).Close()

	// Second construction against the SAME file: migrate() runs again. The
	// rebuild guard must skip (no error, no double-rebuild) and existing rows
	// must survive untouched.
	s2, err := NewSQLiteStorage(dbPath, logger)
	require.NoError(t, err, "reopening an already-migrated DB must not error")
	st2 := rawStore(t, s2)
	defer st2.Close()

	// Guard: the composite key is still there and rows are intact.
	ok, err := hasCompositeTenantKey(st2.db, tenantRebuild{table: "expected_agents", keyCols: []string{"hostname"}})
	require.NoError(t, err)
	assert.True(t, ok, "composite key must survive a reopen")

	var total int
	require.NoError(t, st2.db.QueryRow(`SELECT COUNT(*) FROM expected_agents WHERE hostname='h'`).Scan(&total))
	assert.Equal(t, 2, total, "both tenants' rows must survive the second migrate()")

	// A third migrate() invoked directly (not via reopen) must also no-op.
	require.NoError(t, st2.migrate(), "an explicit re-run of migrate() must be a no-op")
	require.NoError(t, st2.db.QueryRow(`SELECT COUNT(*) FROM expected_agents WHERE hostname='h'`).Scan(&total))
	assert.Equal(t, 2, total, "a redundant migrate() must not disturb existing rows")
}
