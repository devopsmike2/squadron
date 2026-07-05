// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcpconnstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// tenant_d6b_test.go — ADR 0013 §D6-b: the GCP connection carries a
// Squadron owner tenant (TenantID) so the discovery rescan scheduler
// can scope its discovery_scans write to the owning tenant. Inert in
// OSS (every connection is "default").

// TestGCP_D6b_OwnerTenantRoundTrip: a connection created with an owner
// tenant set round-trips through Get and List on both backends.
func TestGCP_D6b_OwnerTenantRoundTrip(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			conn := sampleConnection("acme-project")
			conn.TenantID = "acme"
			require.NoError(t, store.Create(context.Background(), conn))
			require.Equal(t, "acme", conn.TenantID, "Create should preserve the owner tenant")

			got, err := store.Get(context.Background(), conn.ID)
			require.NoError(t, err)
			require.Equal(t, "acme", got.TenantID, "Get should surface the owner tenant")

			list, err := store.List(context.Background())
			require.NoError(t, err)
			require.Len(t, list, 1)
			require.Equal(t, "acme", list[0].TenantID, "List should surface the owner tenant")
		})
	}
}

// TestGCP_D6b_EmptyOwnerTenantDefaults: a connection created without an
// owner tenant (OSS reality) defaults to "default" — inert.
func TestGCP_D6b_EmptyOwnerTenantDefaults(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			conn := sampleConnection("default-project") // no TenantID
			require.NoError(t, store.Create(context.Background(), conn))
			require.Equal(t, "default", conn.TenantID)

			got, err := store.Get(context.Background(), conn.ID)
			require.NoError(t, err)
			require.Equal(t, "default", got.TenantID)
		})
	}
}

// TestGCP_D6b_MigrateTwiceIdempotent: a second NewSQLiteStore on the
// same DB file does not error (the ADD COLUMN migration re-run is
// tolerated by isDuplicateColumnErr).
func TestGCP_D6b_MigrateTwiceIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gcpconnstore.db")
	s1, err := NewSQLiteStore(Config{DBPath: dbPath, Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(Config{DBPath: dbPath, Logger: zap.NewNop()})
	require.NoError(t, err, "re-opening an already-migrated DB must not error")
	require.NoError(t, s2.Close())
}
