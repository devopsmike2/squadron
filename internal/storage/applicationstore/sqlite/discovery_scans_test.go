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
