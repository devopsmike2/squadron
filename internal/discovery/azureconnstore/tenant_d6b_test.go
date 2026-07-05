// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azureconnstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// tenant_d6b_test.go — ADR 0013 §D6-b: the Azure connection carries a
// Squadron owner tenant (SquadronTenantID) DISTINCT from the Azure-AD
// TenantID, so the discovery rescan scheduler can scope its
// discovery_scans write to the owning tenant. Inert in OSS.

// TestAzure_D6b_OwnerTenantRoundTrip: SquadronTenantID round-trips
// through Get and List, and does NOT collide with the Azure-AD TenantID.
func TestAzure_D6b_OwnerTenantRoundTrip(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			conn := sampleConnection("acme-sub")
			conn.SquadronTenantID = "acme"
			require.NoError(t, store.Create(context.Background(), conn))
			require.Equal(t, "acme", conn.SquadronTenantID)
			// The Azure-AD tenant is a separate namespace and is untouched.
			require.Equal(t, "00000000-0000-0000-0000-000000000001", conn.TenantID)

			got, err := store.Get(context.Background(), conn.ID)
			require.NoError(t, err)
			require.Equal(t, "acme", got.SquadronTenantID, "Get surfaces the Squadron owner tenant")
			require.Equal(t, "00000000-0000-0000-0000-000000000001", got.TenantID, "Azure-AD tenant unchanged")

			list, err := store.List(context.Background())
			require.NoError(t, err)
			require.Len(t, list, 1)
			require.Equal(t, "acme", list[0].SquadronTenantID)
		})
	}
}

// TestAzure_D6b_EmptyOwnerTenantDefaults: an empty SquadronTenantID
// defaults to "default" — inert in OSS.
func TestAzure_D6b_EmptyOwnerTenantDefaults(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			conn := sampleConnection("default-sub") // no SquadronTenantID
			require.NoError(t, store.Create(context.Background(), conn))
			require.Equal(t, "default", conn.SquadronTenantID)

			got, err := store.Get(context.Background(), conn.ID)
			require.NoError(t, err)
			require.Equal(t, "default", got.SquadronTenantID)
		})
	}
}

// TestAzure_D6b_MigrateTwiceIdempotent: re-opening an already-migrated
// DB does not error.
func TestAzure_D6b_MigrateTwiceIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "azureconnstore.db")
	s1, err := NewSQLiteStore(Config{DBPath: dbPath, Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(Config{DBPath: dbPath, Logger: zap.NewNop()})
	require.NoError(t, err, "re-opening an already-migrated DB must not error")
	require.NoError(t, s2.Close())
}
