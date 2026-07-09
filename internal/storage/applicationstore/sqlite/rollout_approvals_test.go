// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/require"
)

// TestRolloutApprovals_SQLite pins ADR 0029's append-log semantics in the
// SQLite store: RecordRolloutApproval is idempotent on (rollout_id, approver)
// and CountRolloutApprovers returns the distinct-approver count. It also
// confirms the required_approvals column round-trips and defaults to 1.
func TestRolloutApprovals_SQLite(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		now := time.Now().UTC()

		// A rollout created without RequiredApprovals set reads back as 1
		// (the column DEFAULT / read-side floor), preserving the two-person
		// default byte-for-byte.
		r := &types.Rollout{ID: "ro-1", Name: "n", GroupID: "g", TargetConfigID: "c", State: "pending_approval"}
		require.NoError(t, store.CreateRollout(ctx, r))
		got, err := store.GetRollout(ctx, "ro-1")
		require.NoError(t, err)
		require.Equal(t, 1, got.RequiredApprovals, "unset RequiredApprovals must read back as 1")

		// An explicit threshold round-trips.
		r3 := &types.Rollout{ID: "ro-3", Name: "n", GroupID: "g", TargetConfigID: "c", State: "pending_approval", RequiredApprovals: 3}
		require.NoError(t, store.CreateRollout(ctx, r3))
		got3, err := store.GetRollout(ctx, "ro-3")
		require.NoError(t, err)
		require.Equal(t, 3, got3.RequiredApprovals)

		// Zero approvers to start.
		n, err := store.CountRolloutApprovers(ctx, "ro-3")
		require.NoError(t, err)
		require.Equal(t, 0, n)

		// Two distinct approvers → count 2.
		require.NoError(t, store.RecordRolloutApproval(ctx, "ro-3", "bob", "tok-bob", "1", now))
		require.NoError(t, store.RecordRolloutApproval(ctx, "ro-3", "carol", "", "2", now.Add(time.Second)))
		n, err = store.CountRolloutApprovers(ctx, "ro-3")
		require.NoError(t, err)
		require.Equal(t, 2, n)

		// Same approver again is idempotent — count stays 2.
		require.NoError(t, store.RecordRolloutApproval(ctx, "ro-3", "bob", "tok-bob", "dup", now.Add(2*time.Second)))
		n, err = store.CountRolloutApprovers(ctx, "ro-3")
		require.NoError(t, err)
		require.Equal(t, 2, n, "duplicate approver must not double-count")

		// ListRolloutApprovers returns both, oldest first, with first notes kept.
		list, err := store.ListRolloutApprovers(ctx, "ro-3")
		require.NoError(t, err)
		require.Len(t, list, 2)
		require.Equal(t, "bob", list[0].Approver)
		require.Equal(t, "1", list[0].Notes, "idempotent insert keeps the first approval's notes")
		require.Equal(t, "tok-bob", list[0].ApproverTokenID, "approver_token_id round-trips (ADR 0030)")
		require.Equal(t, "carol", list[1].Approver)
	})
}
