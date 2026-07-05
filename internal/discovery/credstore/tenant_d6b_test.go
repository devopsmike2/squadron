// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// tenant_d6b_test.go — ADR 0013 §D6-b: the shared cloud_connections
// substrate (AWS at runtime) carries a Squadron owner tenant
// (CloudConnection.TenantID) so the discovery rescan scheduler can
// scope its discovery_scans write to the owning tenant. Inert in OSS.

// TestCredstore_D6b_OwnerTenantRoundTrip: a connection stored with an
// owner tenant round-trips through Get and List.
func TestCredstore_D6b_OwnerTenantRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	conn := sampleAWSConnection(t, key, "111111111111")
	conn.TenantID = "acme"
	require.NoError(t, store.StoreConnection(ctx, conn))

	got, err := store.GetConnection(ctx, "111111111111")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "acme", got.TenantID, "GetConnection surfaces the owner tenant")

	list, err := store.ListConnections(ctx, ListFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "acme", list[0].TenantID, "ListConnections surfaces the owner tenant")
}

// TestCredstore_D6b_EmptyOwnerTenantDefaults: an empty TenantID defaults
// to "default" — inert in OSS.
func TestCredstore_D6b_EmptyOwnerTenantDefaults(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	conn := sampleAWSConnection(t, key, "222222222222") // no TenantID
	require.NoError(t, store.StoreConnection(ctx, conn))

	got, err := store.GetConnection(ctx, "222222222222")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "default", got.TenantID)
}

// TestCredstore_D6b_OwnershipImmutableOnUpsert: a re-save of an existing
// connection preserves the owner tenant that first created it — the
// UPSERT deliberately OMITS tenant_id from DO UPDATE SET (ownership is
// immutable per ADR 0013 §D6-b).
func TestCredstore_D6b_OwnershipImmutableOnUpsert(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	key := newTestKey(t)

	first := sampleAWSConnection(t, key, "333333333333")
	first.TenantID = "acme"
	require.NoError(t, store.StoreConnection(ctx, first))

	// A second save under a different tenant must NOT re-home the row.
	second := sampleAWSConnection(t, key, "333333333333")
	second.DisplayName = "renamed"
	second.TenantID = "intruder"
	require.NoError(t, store.StoreConnection(ctx, second))

	got, err := store.GetConnection(ctx, "333333333333")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "renamed", got.DisplayName, "mutable columns update on re-save")
	require.Equal(t, "acme", got.TenantID, "ownership is immutable — the first tenant wins")
}

// TestCredstore_D6b_MigrateTwiceIdempotent: a second NewSQLiteStore on
// the same DB file does NOT error. This proves the isDuplicateColumnErr
// guard added to credstore's migrate loop tolerates the ADD COLUMN
// re-run (credstore had no such guard before D6-b, so a bare re-run
// would have errored on the second boot).
func TestCredstore_D6b_MigrateTwiceIdempotent(t *testing.T) {
	setTestKey(t)
	audit := &spyAudit{}
	dbPath := filepath.Join(t.TempDir(), "credstore.db")

	s1, err := NewSQLiteStore(Config{DBPath: dbPath, Audit: audit, Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := NewSQLiteStore(Config{DBPath: dbPath, Audit: audit, Logger: zap.NewNop()})
	require.NoError(t, err, "re-opening an already-migrated DB must not error (isDuplicateColumnErr guard)")
	require.NoError(t, s2.Close())
}
