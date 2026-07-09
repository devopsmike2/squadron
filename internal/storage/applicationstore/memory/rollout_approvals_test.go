// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/require"
)

// TestRolloutApprovals_Memory mirrors the SQLite store's ADR 0029 test:
// RecordRolloutApproval is idempotent on (rollout_id, approver),
// CountRolloutApprovers returns the distinct-approver count, and an unset
// RequiredApprovals round-trips as the two-person default of 1.
func TestRolloutApprovals_Memory(t *testing.T) {
	ctx := context.Background()
	store := NewStore()
	now := time.Now().UTC()

	r := &types.Rollout{ID: "ro-1", Name: "n", GroupID: "g", TargetConfigID: "c", State: "pending_approval", RequiredApprovals: 3}
	require.NoError(t, store.CreateRollout(ctx, r))

	n, err := store.CountRolloutApprovers(ctx, "ro-1")
	require.NoError(t, err)
	require.Equal(t, 0, n)

	require.NoError(t, store.RecordRolloutApproval(ctx, "ro-1", "bob", "", "1", now))
	require.NoError(t, store.RecordRolloutApproval(ctx, "ro-1", "carol", "", "2", now.Add(time.Second)))
	n, err = store.CountRolloutApprovers(ctx, "ro-1")
	require.NoError(t, err)
	require.Equal(t, 2, n)

	// Idempotent duplicate.
	require.NoError(t, store.RecordRolloutApproval(ctx, "ro-1", "bob", "", "dup", now.Add(2*time.Second)))
	n, err = store.CountRolloutApprovers(ctx, "ro-1")
	require.NoError(t, err)
	require.Equal(t, 2, n, "duplicate approver must not double-count")

	list, err := store.ListRolloutApprovers(ctx, "ro-1")
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "bob", list[0].Approver)
	require.Equal(t, "1", list[0].Notes)
	require.Equal(t, "carol", list[1].Approver)
}
