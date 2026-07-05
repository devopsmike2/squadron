// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ociconnstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// tenant_d6b_test.go — ADR 0013 §D6-b: the OCI connection carries a
// Squadron owner tenant (OwnerTenantID) DISTINCT from the OCI
// TenancyOCID, so the discovery rescan scheduler can scope its
// discovery_scans write to the owning tenant. Inert in OSS.

// TestOCI_D6b_OwnerTenantRoundTrip: OwnerTenantID round-trips through
// Get and List, and does NOT collide with the OCI TenancyOCID.
func TestOCI_D6b_OwnerTenantRoundTrip(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			conn := sampleConnection("acme")
			conn.OwnerTenantID = "acme"
			require.NoError(t, store.Create(context.Background(), conn))
			require.Equal(t, "acme", conn.OwnerTenantID)
			// The OCI tenancy OCID is a separate namespace and is untouched.
			require.Equal(t, "ocid1.tenancy.oc1..aaaaacme", conn.TenancyOCID)

			got, err := store.Get(context.Background(), conn.ID)
			require.NoError(t, err)
			require.Equal(t, "acme", got.OwnerTenantID, "Get surfaces the Squadron owner tenant")
			require.Equal(t, "ocid1.tenancy.oc1..aaaaacme", got.TenancyOCID, "OCI tenancy OCID unchanged")

			list, err := store.List(context.Background())
			require.NoError(t, err)
			require.Len(t, list, 1)
			require.Equal(t, "acme", list[0].OwnerTenantID)
		})
	}
}

// TestOCI_D6b_EmptyOwnerTenantDefaults: an empty OwnerTenantID defaults
// to "default" — inert in OSS.
func TestOCI_D6b_EmptyOwnerTenantDefaults(t *testing.T) {
	for _, backend := range storeBackends() {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			store := backend.factory(t)
			conn := sampleConnection("def") // no OwnerTenantID
			require.NoError(t, store.Create(context.Background(), conn))
			require.Equal(t, "default", conn.OwnerTenantID)

			got, err := store.Get(context.Background(), conn.ID)
			require.NoError(t, err)
			require.Equal(t, "default", got.OwnerTenantID)
		})
	}
}

// TestOCI_D6b_MigrateTwiceIdempotent: re-opening an already-migrated DB
// does not error.
func TestOCI_D6b_MigrateTwiceIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ociconnstore.db")
	s1, err := NewSQLiteStore(Config{DBPath: dbPath, Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(Config{DBPath: dbPath, Logger: zap.NewNop()})
	require.NoError(t, err, "re-opening an already-migrated DB must not error")
	require.NoError(t, s2.Close())
}
