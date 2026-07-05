// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacconnstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTenantIDRoundTrips exercises ADR 0012 §Decision 3: a connection
// created with a tenant round-trips that tenant through Get / List /
// GetByRepoFullName; a connection created without a tenant defaults to
// "default" (the OSS single-tenant sentinel, matching the ADD COLUMN
// default). Runs across both store backends.
func TestTenantIDRoundTrips(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			t.Run("create_with_tenant_round_trips", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				in := sampleConnection("acme/infra")
				in.TenantID = "acme"
				require.NoError(t, store.Create(ctx, in))
				assert.Equal(t, "acme", in.TenantID, "Create keeps the supplied tenant")

				got, err := store.Get(ctx, in.ConnectionID)
				require.NoError(t, err)
				assert.Equal(t, "acme", got.TenantID)

				byRepo, err := store.GetByRepoFullName(ctx, "acme/infra")
				require.NoError(t, err)
				assert.Equal(t, "acme", byRepo.TenantID)

				listed, err := store.List(ctx)
				require.NoError(t, err)
				require.Len(t, listed, 1)
				assert.Equal(t, "acme", listed[0].TenantID)
			})

			t.Run("create_without_tenant_defaults_to_default", func(t *testing.T) {
				store := backend.factory(t)
				ctx := context.Background()

				in := sampleConnection("acme/infra-b")
				// TenantID left empty: the OSS single-tenant path.
				require.NoError(t, store.Create(ctx, in))
				assert.Equal(t, "default", in.TenantID, "Create defaults empty tenant to 'default'")

				got, err := store.Get(ctx, in.ConnectionID)
				require.NoError(t, err)
				assert.Equal(t, "default", got.TenantID)
			})
		})
	}
}

// TestTenantIDBackfillsForPreExistingRows exercises the migration path:
// a row written by the pre-3d schema (no tenant_id column) reads back as
// "default" after the ADD COLUMN migration runs. We simulate this by
// writing a row through a schema that stops at migration0003 (before the
// tenant_id add), then opening the same DB through NewStore (which runs
// migration0004) and reading the row back. ADR 0012 §Decision 3.
func TestTenantIDBackfillsForPreExistingRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "iacconnstore-backfill.db")

	// Phase 1: open a raw DB and apply only the pre-tenant migrations
	// (0001..0003), then insert a row directly with the pre-3d column set.
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	for _, stmt := range []string{
		migration0001IaCConnections,
		migration0002LearnFromAcceptedRecommendations,
		migration0003WebhookSecretSealed,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}
	_, err = db.Exec(`
		INSERT INTO iac_connections (
			connection_id, provider, auth_kind, repo_full_name, default_branch,
			repo_layout, branch_prefix, reviewer_team_handle,
			placement_map_json, cred_ciphertext,
			learn_from_accepted_recommendations,
			created_at, updated_at
		) VALUES ('pre-3d-id', 'github', 'pat', 'legacy/repo', 'main',
			'mono', NULL, NULL, '[]', X'00', 1,
			'2020-01-01T00:00:00Z', '2020-01-01T00:00:00Z')
	`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Phase 2: open the same DB through NewStore, which runs the full
	// migration chain including migration0004 (tenant_id ADD COLUMN).
	db2, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	store, err := NewStore(context.Background(), db2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	got, err := store.Get(context.Background(), "pre-3d-id")
	require.NoError(t, err)
	assert.Equal(t, "default", got.TenantID,
		"pre-3d row must backfill to the 'default' tenant via the ADD COLUMN default")
}
