// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/require"
)

// TestUpdateRollout_VersionCAS_SQLite pins the optimistic-concurrency guard:
// a create starts at version 1, a load-then-update carrying that version bumps
// it to 2, and a second writer holding the now-stale version 1 is rejected
// with ErrRolloutVersionConflict instead of clobbering the first writer. This
// is the storage half of the engine-vs-operator lost-update fix. The legacy
// path (Version==0) still writes blindly so pre-opt-in callers are unaffected.
func TestUpdateRollout_VersionCAS_SQLite(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		r := &types.Rollout{ID: "ro-1", Name: "n", GroupID: "g", TargetConfigID: "c", State: "pending"}
		require.NoError(t, store.CreateRollout(ctx, r))

		loaded, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)
		require.Equal(t, 1, loaded.Version, "fresh row starts at version 1")

		// Two readers load the same version-1 snapshot.
		a, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)
		b, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)

		// First write wins and advances the version.
		a.State = "in_progress"
		require.NoError(t, store.UpdateRollout(ctx, a))
		require.Equal(t, 2, a.Version, "successful CAS bumps the in-memory version")

		// Second write holds the stale version 1 → conflict, no clobber.
		b.State = "paused"
		err = store.UpdateRollout(ctx, b)
		require.ErrorIs(t, err, types.ErrRolloutVersionConflict)

		final, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)
		require.Equal(t, "in_progress", string(final.State), "first writer's state survives")
		require.Equal(t, 2, final.Version)

		// Legacy path: Version==0 falls back to a blind update that still
		// advances the counter, so pre-opt-in callers keep working.
		final.Version = 0
		final.State = "succeeded"
		require.NoError(t, store.UpdateRollout(ctx, final))
		after, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)
		require.Equal(t, "succeeded", string(after.State))
		require.Equal(t, 3, after.Version, "blind path bumps version = version + 1")
	})
}
