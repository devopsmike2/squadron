// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connection_registry_test.go — HA S3b (ADR 0035) connection registry, memory
// backend. Mirrors the sqlite acceptance suite so the two backends stay in
// lock-step.

func TestConnectionRegistry_Memory_ConnectDisconnect(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	agentID := uuid.New()
	now := time.Now().UTC()

	require.NoError(t, s.UpsertConnectionOwner(ctx, agentID, "instance-A", now))
	got, err := s.GetConnectionOwner(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "instance-A", got.InstanceID)
	assert.WithinDuration(t, now, got.ConnectedAt, time.Second)
	assert.WithinDuration(t, now, got.LastHeartbeatAt, time.Second)

	require.NoError(t, s.DeleteConnectionOwner(ctx, agentID))
	got, err = s.GetConnectionOwner(ctx, agentID)
	require.NoError(t, err)
	assert.Nil(t, got, "clean disconnect removes the row")
	// Missing-row delete is a no-op.
	require.NoError(t, s.DeleteConnectionOwner(ctx, uuid.New()))
}

func TestConnectionRegistry_Memory_HeartbeatAdvances(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	agentID := uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour)
	t1 := time.Now().UTC()

	require.NoError(t, s.UpsertConnectionOwner(ctx, agentID, "instance-A", t0))
	require.NoError(t, s.UpsertConnectionOwner(ctx, agentID, "instance-A", t1))

	got, err := s.GetConnectionOwner(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.WithinDuration(t, t0, got.ConnectedAt, time.Second, "connected_at preserved on heartbeat")
	assert.WithinDuration(t, t1, got.LastHeartbeatAt, time.Second, "last_heartbeat_at advanced")
}

func TestConnectionRegistry_Memory_StaleReclaimAndReadopt(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	agentID := uuid.New()
	live := uuid.New()

	require.NoError(t, s.UpsertConnectionOwner(ctx, live, "instance-live", time.Now().UTC()))
	require.NoError(t, s.UpsertConnectionOwner(ctx, agentID, "instance-A", time.Now().UTC().Add(-5*time.Minute)))

	cutoff := time.Now().UTC().Add(-time.Minute)
	n, err := s.ReclaimStaleConnectionOwners(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	got, err := s.GetConnectionOwner(ctx, agentID)
	require.NoError(t, err)
	assert.Nil(t, got, "stale row evicted")
	liveGot, err := s.GetConnectionOwner(ctx, live)
	require.NoError(t, err)
	assert.NotNil(t, liveGot, "fresh row survives")

	// Re-run is a no-op.
	n, err = s.ReclaimStaleConnectionOwners(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// Reconnect re-owns (last-writer-wins), resetting connected_at.
	readopt := time.Now().UTC()
	require.NoError(t, s.UpsertConnectionOwner(ctx, agentID, "instance-B", readopt))
	got, err = s.GetConnectionOwner(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "instance-B", got.InstanceID)
	assert.WithinDuration(t, readopt, got.ConnectedAt, time.Second)

	owners, err := s.ListConnectionOwners(ctx)
	require.NoError(t, err)
	assert.Len(t, owners, 2, "agentID (re-owned) + live")
}
