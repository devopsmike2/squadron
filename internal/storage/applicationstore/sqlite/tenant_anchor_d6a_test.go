// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// tenant_anchor_d6a_test.go — ADR 0013 D6-a. The three background
// bridges (proposer→rollouts, incidents→drafts, GHA→expected_agents)
// anchor per-owner writes on an entity that already carries tenant_id
// on disk (the GROUP / the DEPLOY TARGET). Before D6-a the read layer
// stripped that column, so the bridges had nothing to stamp the write
// with and every per-owner row landed in `default`. These tests pin
// that the column now survives the read (both stores) so the bridges
// can anchor on it.

// TestGroupTenantID_SurfacedOnRead_sqlite: a group created under the
// acme tenant round-trips its tenant_id through GetGroup + ListGroups.
func TestGroupTenantID_SurfacedOnRead_sqlite(t *testing.T) {
	appStore, err := NewSQLiteStorage(makeTempDB(t), zap.NewNop())
	require.NoError(t, err)
	store := appStore.(*Storage)

	acme := identity.WithTenant(context.Background(), "acme")
	require.NoError(t, store.CreateGroup(acme, makeTestGroup("acme-g")))

	// Read under the acme tenant.
	got, err := store.GetGroup(acme, "acme-g")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "acme", got.TenantID, "GetGroup must surface the on-disk tenant_id")

	// Read under a system context (the bridge's read ctx): still acme.
	sys := identity.WithSystemContext(context.Background())
	gotSys, err := store.GetGroup(sys, "acme-g")
	require.NoError(t, err)
	require.NotNil(t, gotSys)
	require.Equal(t, "acme", gotSys.TenantID, "system read must still surface the owning tenant")

	// ListGroups surfaces it too.
	list, err := store.ListGroups(sys)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "acme", list[0].TenantID)
}

// TestDeployTargetTenantID_SurfacedOnRead_sqlite: a deploy target
// created under acme round-trips its tenant_id through GetDeployTarget
// + ListDeployTargets.
func TestDeployTargetTenantID_SurfacedOnRead_sqlite(t *testing.T) {
	appStore, err := NewSQLiteStorage(makeTempDB(t), zap.NewNop())
	require.NoError(t, err)
	store := appStore.(*Storage)

	acme := identity.WithTenant(context.Background(), "acme")
	require.NoError(t, store.CreateDeployTarget(acme, &types.DeployTarget{
		ID:             "dt-acme",
		Name:           "acme-target",
		GitHubOwner:    "acme",
		GitHubRepo:     "infra",
		GitHubWorkflow: "deploy.yml",
	}))

	got, err := store.GetDeployTarget(acme, "dt-acme")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "acme", got.TenantID, "GetDeployTarget must surface the on-disk tenant_id")

	// The GHA walker reads under a system ctx — must still see acme.
	sys := identity.WithSystemContext(context.Background())
	gotSys, err := store.GetDeployTarget(sys, "dt-acme")
	require.NoError(t, err)
	require.NotNil(t, gotSys)
	require.Equal(t, "acme", gotSys.TenantID)

	list, err := store.ListDeployTargets(sys)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "acme", list[0].TenantID)
}

// TestExpectedAgentsBackfill_D6a: the idempotent backfill migration
// recovers the owning tenant for walker-written expected_agents rows
// (source='gha-history:<target-id>') by joining back to deploy_targets.
// A non-walker row is untouched.
func TestExpectedAgentsBackfill_D6a(t *testing.T) {
	appStore, err := NewSQLiteStorage(makeTempDB(t), zap.NewNop())
	require.NoError(t, err)
	store := appStore.(*Storage)
	ctx := context.Background()

	// A deploy target owned by acme.
	acme := identity.WithTenant(ctx, "acme")
	require.NoError(t, store.CreateDeployTarget(acme, &types.DeployTarget{
		ID:   "dt-1",
		Name: "acme-target",
	}))

	// Simulate the PRE-fix state: a walker row that landed in `default`
	// (the bug), plus a manually-registered row that must NOT be touched.
	now := time.Now().UTC()
	_, err = store.db.Exec(
		`INSERT INTO expected_agents (hostname, labels_json, source, expected_since, updated_at, notes, tenant_id)
		 VALUES (?, '{}', ?, ?, ?, '', 'default')`,
		"walker-host", "gha-history:dt-1", now, now,
	)
	require.NoError(t, err)
	_, err = store.db.Exec(
		`INSERT INTO expected_agents (hostname, labels_json, source, expected_since, updated_at, notes, tenant_id)
		 VALUES (?, '{}', ?, ?, ?, '', 'default')`,
		"manual-host", "manual", now, now,
	)
	require.NoError(t, err)

	// Re-run the idempotent migration set (contains the D6-a backfill).
	require.NoError(t, store.migrate())

	// The walker row was recovered to acme.
	var walkerTenant string
	require.NoError(t, store.db.QueryRow(
		`SELECT tenant_id FROM expected_agents WHERE hostname = 'walker-host'`).Scan(&walkerTenant))
	require.Equal(t, "acme", walkerTenant, "walker row must be backfilled to the deploy target's tenant")

	// The manual row is untouched.
	var manualTenant string
	require.NoError(t, store.db.QueryRow(
		`SELECT tenant_id FROM expected_agents WHERE hostname = 'manual-host'`).Scan(&manualTenant))
	require.Equal(t, "default", manualTenant, "non-walker row must be left alone")

	// Idempotent: re-running is a no-op (still acme, no error).
	require.NoError(t, store.migrate())
	require.NoError(t, store.db.QueryRow(
		`SELECT tenant_id FROM expected_agents WHERE hostname = 'walker-host'`).Scan(&walkerTenant))
	require.Equal(t, "acme", walkerTenant, "re-running the backfill is a no-op")
}
