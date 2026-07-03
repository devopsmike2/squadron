// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/require"
)

// TestUpdateRollout_VersionCAS_Memory mirrors the SQLite CAS test against the
// in-memory store so both implementations agree on the optimistic-concurrency
// contract: create → version 1; a stale second writer is rejected with
// ErrRolloutVersionConflict; the legacy Version==0 path still writes blindly.
func TestUpdateRollout_VersionCAS_Memory(t *testing.T) {
	store := NewStore()
	ctx := context.Background()
	r := &types.Rollout{ID: "ro-1", Name: "n", GroupID: "g", TargetConfigID: "c", State: "pending"}
	require.NoError(t, store.CreateRollout(ctx, r))

	loaded, err := store.GetRollout(ctx, "ro-1")
	require.NoError(t, err)
	require.Equal(t, 1, loaded.Version, "fresh row starts at version 1")

	a, err := store.GetRollout(ctx, "ro-1")
	require.NoError(t, err)
	b, err := store.GetRollout(ctx, "ro-1")
	require.NoError(t, err)

	a.State = "in_progress"
	require.NoError(t, store.UpdateRollout(ctx, a))
	require.Equal(t, 2, a.Version, "successful CAS bumps the in-memory version")

	b.State = "paused"
	err = store.UpdateRollout(ctx, b)
	require.ErrorIs(t, err, types.ErrRolloutVersionConflict)

	final, err := store.GetRollout(ctx, "ro-1")
	require.NoError(t, err)
	require.Equal(t, "in_progress", string(final.State), "first writer's state survives")
	require.Equal(t, 2, final.Version)

	final.Version = 0
	final.State = "succeeded"
	require.NoError(t, store.UpdateRollout(ctx, final))
	after, err := store.GetRollout(ctx, "ro-1")
	require.NoError(t, err)
	require.Equal(t, "succeeded", string(after.State))
	require.Equal(t, 3, after.Version, "blind path bumps version = version + 1")
}
