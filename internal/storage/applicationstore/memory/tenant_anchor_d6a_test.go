// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// tenant_anchor_d6a_test.go — memory-store parity for ADR 0013 D6-a.
// The memory store round-trips the whole struct, so Group.TenantID /
// DeployTarget.TenantID ride through create→get without a query
// change. These tests pin that so the field can't be silently dropped.

func TestGroupTenantID_RoundTrip_memory(t *testing.T) {
	store := NewStore()
	ctx := context.Background()

	require.NoError(t, store.CreateGroup(ctx, &types.Group{
		ID:       "acme-g",
		Name:     "acme-group",
		TenantID: "acme",
	}))

	got, err := store.GetGroup(ctx, "acme-g")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "acme", got.TenantID, "memory GetGroup must preserve tenant_id")

	list, err := store.ListGroups(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "acme", list[0].TenantID)
}

func TestDeployTargetTenantID_RoundTrip_memory(t *testing.T) {
	store := NewStore()
	ctx := context.Background()

	require.NoError(t, store.CreateDeployTarget(ctx, &types.DeployTarget{
		ID:       "dt-acme",
		Name:     "acme-target",
		TenantID: "acme",
	}))

	got, err := store.GetDeployTarget(ctx, "dt-acme")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "acme", got.TenantID, "memory GetDeployTarget must preserve tenant_id")

	list, err := store.ListDeployTargets(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "acme", list[0].TenantID)
}
