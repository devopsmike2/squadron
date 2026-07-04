// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRollout_PushedAgentIDs_RoundTrip pins the ADR 0007 cumulative
// pushed-agent set through the SQLite storage layer: a fresh row has an empty
// set, UpdateRollout persists the set, and GetRollout / ListRollouts read it
// back intact. Empty round-trips as nil (not "[]") so pre-v0.90 rows and empty
// sets are indistinguishable to the engine's fallback logic.
func TestRollout_PushedAgentIDs_RoundTrip(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		r := &types.Rollout{ID: "ro-1", Name: "n", GroupID: "g", TargetConfigID: "c", State: "pending"}
		require.NoError(t, store.CreateRollout(ctx, r))

		// Fresh row: no cumulative set yet.
		loaded, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)
		require.Empty(t, loaded.PushedAgentIDs, "fresh row has no pushed-set")

		// Persist a cumulative set via UpdateRollout.
		loaded.PushedAgentIDs = []string{"agent-a", "agent-b"}
		require.NoError(t, store.UpdateRollout(ctx, loaded))

		got, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)
		assert.Equal(t, []string{"agent-a", "agent-b"}, got.PushedAgentIDs)

		// ListRollouts reads the same column via scanRollout.
		list, err := store.ListRollouts(ctx, types.RolloutFilter{GroupID: "g"})
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Equal(t, []string{"agent-a", "agent-b"}, list[0].PushedAgentIDs)

		// Clearing the set round-trips back to empty (NULL column).
		got.PushedAgentIDs = nil
		require.NoError(t, store.UpdateRollout(ctx, got))
		cleared, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)
		assert.Empty(t, cleared.PushedAgentIDs)
	})
}
