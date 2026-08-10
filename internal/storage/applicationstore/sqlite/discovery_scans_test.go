// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestDiscoveryScans_sqlite is the sqlite parity test for the memory-store
// TestDiscoveryScans_SaveListGet: newest-first scoped listing with result_json
// omitted, get returns the full inventory, scope filter, and upsert.
func TestDiscoveryScans_sqlite(t *testing.T) {
	appStore, err := NewSQLiteStorage(makeTempDB(t), zap.NewNop())
	require.NoError(t, err)
	store, ok := appStore.(*Storage)
	require.True(t, ok, "expected *Storage")
	ctx := context.Background()
	base := time.Now().UTC()

	mk := func(id, provider, scope string, age time.Duration) *types.ScanRecord {
		return &types.ScanRecord{
			ScanID:        id,
			Provider:      provider,
			ScopeID:       scope,
			Regions:       []string{"us-east-1", "us-west-2"},
			StartedAt:     base.Add(-age),
			CompletedAt:   base.Add(-age).Add(time.Minute),
			Partial:       true,
			PartialReason: "rate limited",
			Summary:       map[string]int{"compute": 2, "functions": 1},
			ResultJSON:    `{"scan_id":"` + id + `"}`,
		}
	}
	require.NoError(t, store.SaveDiscoveryScan(ctx, mk("s-old", "aws", "111", 3*time.Hour)))
	require.NoError(t, store.SaveDiscoveryScan(ctx, mk("s-new", "aws", "111", 1*time.Hour)))
	require.NoError(t, store.SaveDiscoveryScan(ctx, mk("s-gcp", "gcp", "proj", 1*time.Hour)))

	list, err := store.ListDiscoveryScans(ctx, "aws", "111", 10)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "s-new", list[0].ScanID, "newest first")
	require.Equal(t, "s-old", list[1].ScanID)
	require.Empty(t, list[0].ResultJSON, "result_json omitted in list")
	require.Equal(t, 2, list[0].Summary["compute"])
	require.Equal(t, []string{"us-east-1", "us-west-2"}, list[0].Regions)
	require.True(t, list[0].Partial)
	require.Equal(t, "rate limited", list[0].PartialReason)

	got, err := store.GetDiscoveryScan(ctx, "s-new")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, `{"scan_id":"s-new"}`, got.ResultJSON, "get includes result_json")

	missing, err := store.GetDiscoveryScan(ctx, "nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	// Upsert.
	upd := mk("s-new", "aws", "111", 1*time.Hour)
	upd.Summary = map[string]int{"compute": 7}
	require.NoError(t, store.SaveDiscoveryScan(ctx, upd))
	after, err := store.ListDiscoveryScans(ctx, "aws", "111", 10)
	require.NoError(t, err)
	require.Len(t, after, 2, "upsert must not duplicate")
	g2, _ := store.GetDiscoveryScan(ctx, "s-new")
	require.Equal(t, 7, g2.Summary["compute"])
}

// TestDiscoveryScans_sqlite_DeleteScoped is the sqlite parity test for the
// "Clear inventory" store contract: DeleteDiscoveryScans removes exactly the
// targeted (provider, scope) rows, reports the count, and leaves sibling scopes
// and other providers intact; a rescan (Save) repopulates; blank inputs error.
func TestDiscoveryScans_sqlite_DeleteScoped(t *testing.T) {
	appStore, err := NewSQLiteStorage(makeTempDB(t), zap.NewNop())
	require.NoError(t, err)
	store, ok := appStore.(*Storage)
	require.True(t, ok, "expected *Storage")
	ctx := context.Background()
	base := time.Now().UTC()
	mk := func(id, provider, scope string) *types.ScanRecord {
		return &types.ScanRecord{
			ScanID: id, Provider: provider, ScopeID: scope,
			StartedAt: base, CompletedAt: base.Add(time.Minute),
			Summary: map[string]int{}, ResultJSON: `{"scan_id":"` + id + `"}`,
		}
	}
	require.NoError(t, store.SaveDiscoveryScan(ctx, mk("a1", "aws", "111")))
	require.NoError(t, store.SaveDiscoveryScan(ctx, mk("a2", "aws", "111")))
	require.NoError(t, store.SaveDiscoveryScan(ctx, mk("b1", "aws", "222")))
	require.NoError(t, store.SaveDiscoveryScan(ctx, mk("g1", "gcp", "111")))

	n, err := store.DeleteDiscoveryScans(ctx, "aws", "111")
	require.NoError(t, err)
	require.Equal(t, int64(2), n)

	gone, err := store.ListDiscoveryScans(ctx, "aws", "111", 10)
	require.NoError(t, err)
	require.Empty(t, gone, "target scope cleared")

	sibling, err := store.ListDiscoveryScans(ctx, "aws", "222", 10)
	require.NoError(t, err)
	require.Len(t, sibling, 1, "sibling aws scope untouched")

	otherProvider, err := store.ListDiscoveryScans(ctx, "gcp", "111", 10)
	require.NoError(t, err)
	require.Len(t, otherProvider, 1, "gcp scope untouched by aws clear")

	// Rescan repopulates the same connector.
	require.NoError(t, store.SaveDiscoveryScan(ctx, mk("a3", "aws", "111")))
	repop, err := store.ListDiscoveryScans(ctx, "aws", "111", 10)
	require.NoError(t, err)
	require.Len(t, repop, 1)
	require.Equal(t, "a3", repop[0].ScanID)

	// Blank inputs are refused.
	_, err = store.DeleteDiscoveryScans(ctx, "", "111")
	require.Error(t, err)
	_, err = store.DeleteDiscoveryScans(ctx, "aws", "")
	require.Error(t, err)
}
