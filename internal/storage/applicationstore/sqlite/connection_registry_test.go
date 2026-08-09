// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connection_registry_test.go — HA S3b (ADR 0035) connection registry, SQLite
// backend. Covers the four acceptance behaviors: (a) connect writes the row
// with the local instance_id; (b) clean disconnect removes the row; (c)
// heartbeat refresh advances last_heartbeat_at while preserving connected_at;
// (d) a stale row is reclaimable and a reconnect re-owns it (last-writer-wins).

func TestConnectionRegistry_UpsertAndGet(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		agentID := uuid.New()
		now := time.Now().UTC().Truncate(time.Millisecond)

		require.NoError(t, store.UpsertConnectionOwner(ctx, agentID, "instance-A", now))

		got, err := store.GetConnectionOwner(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, got, "connect must write the ownership row")
		assert.Equal(t, agentID, got.AgentID)
		assert.Equal(t, "instance-A", got.InstanceID, "row must carry the local instance_id")
		assert.WithinDuration(t, now, got.ConnectedAt, time.Second)
		assert.WithinDuration(t, now, got.LastHeartbeatAt, time.Second)
	})
}

func TestConnectionRegistry_GetMissingReturnsNil(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		got, err := store.GetConnectionOwner(context.Background(), uuid.New())
		require.NoError(t, err)
		assert.Nil(t, got, "missing row must return (nil, nil)")
	})
}

func TestConnectionRegistry_CleanDisconnectRemovesRow(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		agentID := uuid.New()
		require.NoError(t, store.UpsertConnectionOwner(ctx, agentID, "instance-A", time.Now()))

		require.NoError(t, store.DeleteConnectionOwner(ctx, agentID))
		got, err := store.GetConnectionOwner(ctx, agentID)
		require.NoError(t, err)
		assert.Nil(t, got, "clean disconnect must remove the row")

		// Deleting a missing row is a clean no-op.
		require.NoError(t, store.DeleteConnectionOwner(ctx, uuid.New()))
	})
}

func TestConnectionRegistry_HeartbeatAdvancesLastHeartbeat(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		agentID := uuid.New()
		t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
		t1 := time.Now().UTC().Truncate(time.Millisecond)

		require.NoError(t, store.UpsertConnectionOwner(ctx, agentID, "instance-A", t0))
		// Heartbeat from the SAME instance advances last_heartbeat_at but must
		// preserve the original connected_at.
		require.NoError(t, store.UpsertConnectionOwner(ctx, agentID, "instance-A", t1))

		got, err := store.GetConnectionOwner(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "instance-A", got.InstanceID)
		assert.WithinDuration(t, t0, got.ConnectedAt, time.Second, "connected_at preserved across heartbeat")
		assert.WithinDuration(t, t1, got.LastHeartbeatAt, time.Second, "last_heartbeat_at advanced")
		assert.True(t, got.LastHeartbeatAt.After(got.ConnectedAt), "heartbeat is after connect")
	})
}

func TestConnectionRegistry_StaleReclaimAndReadopt(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		agentID := uuid.New()
		stale := time.Now().UTC().Add(-5 * time.Minute)
		fresh := time.Now().UTC()

		// A different agent owned by a live instance stays fresh and must NOT be reclaimed.
		liveAgent := uuid.New()
		require.NoError(t, store.UpsertConnectionOwner(ctx, liveAgent, "instance-live", fresh))

		// agentID's owner (instance-A) died without a clean disconnect: its row
		// is stale (last_heartbeat_at older than the grace cutoff).
		require.NoError(t, store.UpsertConnectionOwner(ctx, agentID, "instance-A", stale))

		cutoff := time.Now().UTC().Add(-time.Minute) // grace = 1m for this test
		n, err := store.ReclaimStaleConnectionOwners(ctx, cutoff)
		require.NoError(t, err)
		assert.Equal(t, 1, n, "exactly the stale row is reclaimed")

		got, err := store.GetConnectionOwner(ctx, agentID)
		require.NoError(t, err)
		assert.Nil(t, got, "stale row is evicted")

		live, err := store.GetConnectionOwner(ctx, liveAgent)
		require.NoError(t, err)
		require.NotNil(t, live, "the live instance's fresh row survives reclaim")

		// Re-running the reclaim with the same cutoff is a clean no-op.
		n, err = store.ReclaimStaleConnectionOwners(ctx, cutoff)
		require.NoError(t, err)
		assert.Equal(t, 0, n)

		// The agent's WebSocket reconnects to instance-B, which re-owns it
		// (last-writer-wins on the PK). connected_at is reset — a new connection.
		readopt := time.Now().UTC().Truncate(time.Millisecond)
		require.NoError(t, store.UpsertConnectionOwner(ctx, agentID, "instance-B", readopt))
		got, err = store.GetConnectionOwner(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "instance-B", got.InstanceID, "reconnect re-owns the agent")
		assert.WithinDuration(t, readopt, got.ConnectedAt, time.Second, "takeover resets connected_at")
	})
}

func TestConnectionRegistry_LastWriterWinsResetsConnectedAt(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		agentID := uuid.New()
		t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
		t1 := time.Now().UTC().Truncate(time.Millisecond)

		require.NoError(t, store.UpsertConnectionOwner(ctx, agentID, "instance-A", t0))
		// A DIFFERENT instance takes over: last-writer-wins, connected_at resets.
		require.NoError(t, store.UpsertConnectionOwner(ctx, agentID, "instance-B", t1))

		got, err := store.GetConnectionOwner(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "instance-B", got.InstanceID)
		assert.WithinDuration(t, t1, got.ConnectedAt, time.Second, "new owner resets connected_at")
	})
}

func TestConnectionRegistry_List(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		now := time.Now().UTC()
		a1 := uuid.New()
		a2 := uuid.New()
		require.NoError(t, store.UpsertConnectionOwner(ctx, a1, "instance-A", now))
		require.NoError(t, store.UpsertConnectionOwner(ctx, a2, "instance-B", now))

		owners, err := store.ListConnectionOwners(ctx)
		require.NoError(t, err)
		assert.Len(t, owners, 2)
	})
}
