// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func makeTempDB(t *testing.T) string {
	tmpDir := t.TempDir()
	return filepath.Join(tmpDir, "test.db")
}

func makeTestAgent(id uuid.UUID) *types.Agent {
	return &types.Agent{
		ID:   id,
		Name: "test-agent",
		Labels: map[string]string{
			"env":     "test",
			"service": "demo",
		},
		Status:       types.AgentStatusOnline,
		LastSeen:     time.Now().UTC(),
		Version:      "1.0.0",
		Capabilities: []string{"metrics", "logs", "traces"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
}

func makeTestGroup(id string) *types.Group {
	return &types.Group{
		ID:   id,
		Name: "test-group",
		Labels: map[string]string{
			"env": "test",
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
}

func makeTestConfig(id string, agentID *uuid.UUID, groupID *string) *types.Config {
	return &types.Config{
		ID:         id,
		AgentID:    agentID,
		GroupID:    groupID,
		ConfigHash: "abc123",
		Content:    "receivers:\n  otlp:",
		Version:    1,
		CreatedAt:  time.Now().UTC(),
	}
}

func withSQLiteStore(t *testing.T, f func(store types.ApplicationStore)) {
	dbPath := makeTempDB(t)
	logger := zap.NewNop()

	store, err := NewSQLiteStorage(dbPath, logger)
	require.NoError(t, err)
	defer func() {
		if closer, ok := store.(*Storage); ok {
			closer.Close()
		}
		os.Remove(dbPath)
	}()

	f(store)
}

func withPopulatedSQLiteStore(t *testing.T, f func(store types.ApplicationStore, agentID uuid.UUID)) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		agentID := uuid.New()
		agent := makeTestAgent(agentID)
		err := store.CreateAgent(context.Background(), agent)
		require.NoError(t, err)
		f(store, agentID)
	})
}

// Agent tests

func TestSQLiteCreateAgent(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		agentID := uuid.New()
		agent := makeTestAgent(agentID)

		err := store.CreateAgent(context.Background(), agent)
		require.NoError(t, err)

		// Verify it was created
		retrieved, err := store.GetAgent(context.Background(), agentID)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		assert.Equal(t, agent.ID, retrieved.ID)
		assert.Equal(t, agent.Name, retrieved.Name)
		assert.Equal(t, agent.Status, retrieved.Status)
	})
}

func TestSQLiteGetAgentNotFound(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		retrieved, err := store.GetAgent(context.Background(), uuid.New())
		require.NoError(t, err)
		assert.Nil(t, retrieved)
	})
}

func TestSQLiteListAgents(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		agent1 := makeTestAgent(uuid.New())
		agent2 := makeTestAgent(uuid.New())

		err := store.CreateAgent(context.Background(), agent1)
		require.NoError(t, err)
		err = store.CreateAgent(context.Background(), agent2)
		require.NoError(t, err)

		agents, err := store.ListAgents(context.Background())
		require.NoError(t, err)
		assert.Len(t, agents, 2)
	})
}

// TestSQLiteCreateAgent_RevivesTombstonedRow is the store-layer proof of the
// pilot P1 fix: DeleteAgent soft-deletes (tombstones) the row, and a subsequent
// CreateAgent with the SAME id must REVIVE it (clear deleted_at) rather than
// collide with "UNIQUE constraint failed: agents.id". A LIVE duplicate is still
// rejected. Mirrors what opamp.persistAgent -> CreateAgent does on reconnect.
func TestSQLiteCreateAgent_RevivesTombstonedRow(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		id := uuid.New()

		require.NoError(t, store.CreateAgent(ctx, makeTestAgent(id)))
		require.NoError(t, store.DeleteAgent(ctx, id))

		// Tombstoned: hidden from the operational view.
		got, err := store.GetAgent(ctx, id)
		require.NoError(t, err)
		require.Nil(t, got)

		// Re-register (the exact collision site on main) must now REVIVE, not error.
		revived := makeTestAgent(id)
		revived.Name = "revived-name"
		require.NoError(t, store.CreateAgent(ctx, revived),
			"re-registering a decommissioned agent must revive the tombstoned row, not fail on the PK")

		got, err = store.GetAgent(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, id, got.ID)
		assert.Equal(t, "revived-name", got.Name)

		// Exactly one row — a revive, not a duplicate.
		agents, err := store.ListAgents(ctx)
		require.NoError(t, err)
		assert.Len(t, agents, 1)

		// A LIVE duplicate is still rejected (guard preserved).
		err = store.CreateAgent(ctx, makeTestAgent(id))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})
}

// TestSQLiteCreateAgent_RevivePreservesNameOnEmpty: a decommission → revive whose
// reconnect carries NO name (empty Name — the description-less reconnect before
// the opampsupervisor's next fresh identify) must keep the tombstoned row's
// last-known name, not blank the fleet card. A revive that DOES carry a real name
// still updates it. RED before the preserve-on-empty CASE, GREEN after.
func TestSQLiteCreateAgent_RevivePreservesNameOnEmpty(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		id := uuid.New()

		require.NoError(t, store.CreateAgent(ctx, makeTestAgent(id))) // Name "test-agent"
		require.NoError(t, store.DeleteAgent(ctx, id))

		// Revive with an EMPTY name → preserve the stored last-known name.
		nameless := makeTestAgent(id)
		nameless.Name = ""
		require.NoError(t, store.CreateAgent(ctx, nameless))

		got, err := store.GetAgent(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "test-agent", got.Name,
			"a name-less revive must preserve the last-known name, not blank it to \"\"")

		// A subsequent revive carrying a real name still updates it.
		require.NoError(t, store.DeleteAgent(ctx, id))
		renamed := makeTestAgent(id)
		renamed.Name = "web-01-renamed"
		require.NoError(t, store.CreateAgent(ctx, renamed))
		got, err = store.GetAgent(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "web-01-renamed", got.Name)
	})
}

// TestSQLiteUpdateAgentRegistration_PreservesNameOnEmpty: a live agent's name
// must not be blanked by a description-less reconnect flowing through
// UpdateAgentRegistration with an empty name (defense-in-depth mirror of the
// capabilities preserve).
func TestSQLiteUpdateAgentRegistration_PreservesNameOnEmpty(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		ctx := context.Background()
		require.NoError(t, store.UpdateAgentRegistration(ctx, &types.Agent{
			ID: agentID, Name: ""}))

		got, err := store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "test-agent", got.Name,
			"an empty-name registration must preserve the stored name")

		require.NoError(t, store.UpdateAgentRegistration(ctx, &types.Agent{
			ID: agentID, Name: "renamed"}))
		got, err = store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		assert.Equal(t, "renamed", got.Name)
	})
}

// TestSQLiteRestoreAndPurgeAgent covers the operator recovery endpoints'
// store methods: RestoreAgent un-tombstones a decommissioned agent, and
// PurgeAgent hard-deletes the row.
func TestSQLiteRestoreAndPurgeAgent(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		id := uuid.New()

		require.NoError(t, store.CreateAgent(ctx, makeTestAgent(id)))

		// Restore on a live agent is a no-op error (nothing to restore).
		err := store.RestoreAgent(ctx, id)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not decommissioned")

		// Decommission then restore brings it back with the same id.
		require.NoError(t, store.DeleteAgent(ctx, id))
		got, _ := store.GetAgent(ctx, id)
		require.Nil(t, got)
		require.NoError(t, store.RestoreAgent(ctx, id))
		got, err = store.GetAgent(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, id, got.ID)

		// Purge hard-deletes: the row is gone and cannot be restored.
		require.NoError(t, store.PurgeAgent(ctx, id))
		got, err = store.GetAgent(ctx, id)
		require.NoError(t, err)
		require.Nil(t, got)
		err = store.RestoreAgent(ctx, id)
		require.Error(t, err)
		agents, err := store.ListAgents(ctx)
		require.NoError(t, err)
		assert.Empty(t, agents)

		// Purge on a missing id errors.
		err = store.PurgeAgent(ctx, uuid.New())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestSQLiteUpdateAgentStatus(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		err := store.UpdateAgentStatus(context.Background(), agentID, types.AgentStatusOffline)
		require.NoError(t, err)

		// Verify the update
		agent, err := store.GetAgent(context.Background(), agentID)
		require.NoError(t, err)
		assert.Equal(t, types.AgentStatusOffline, agent.Status)
	})
}

func TestSQLiteUpdateAgentStatusNotFound(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		err := store.UpdateAgentStatus(context.Background(), uuid.New(), types.AgentStatusOffline)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestSQLiteUpdateAgentLastSeen(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		newTime := time.Now().Add(time.Hour).UTC()
		err := store.UpdateAgentLastSeen(context.Background(), agentID, newTime)
		require.NoError(t, err)

		// Verify the update
		agent, err := store.GetAgent(context.Background(), agentID)
		require.NoError(t, err)
		assert.Equal(t, newTime.Unix(), agent.LastSeen.Unix())
	})
}

// TestSQLiteUpdateAgentDeliveredConfigHash verifies the ADR 0040
// delivered-config-hash column round-trips: it defaults empty on a freshly
// created agent, persists through GetAgent + ListAgents after an update, and
// errors on a missing agent.
func TestSQLiteUpdateAgentDeliveredConfigHash(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		// Defaults empty on create.
		agent, err := store.GetAgent(context.Background(), agentID)
		require.NoError(t, err)
		require.NotNil(t, agent)
		assert.Empty(t, agent.DeliveredConfigHash)

		// Update persists.
		const hash = "abc123deadbeef"
		err = store.UpdateAgentDeliveredConfigHash(context.Background(), agentID, hash)
		require.NoError(t, err)

		agent, err = store.GetAgent(context.Background(), agentID)
		require.NoError(t, err)
		assert.Equal(t, hash, agent.DeliveredConfigHash)

		// And through ListAgents.
		agents, err := store.ListAgents(context.Background())
		require.NoError(t, err)
		require.Len(t, agents, 1)
		assert.Equal(t, hash, agents[0].DeliveredConfigHash)
	})
}

func TestSQLiteUpdateAgentDeliveredConfigHashNotFound(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		err := store.UpdateAgentDeliveredConfigHash(context.Background(), uuid.New(), "x")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestSQLiteDeleteAgent(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		err := store.DeleteAgent(context.Background(), agentID)
		require.NoError(t, err)

		// Verify it was deleted
		agent, err := store.GetAgent(context.Background(), agentID)
		require.NoError(t, err)
		assert.Nil(t, agent)
	})
}

func TestSQLiteDeleteAgentNotFound(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		err := store.DeleteAgent(context.Background(), uuid.New())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// Group tests

func TestSQLiteCreateGroup(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		group := makeTestGroup("test-group")

		err := store.CreateGroup(context.Background(), group)
		require.NoError(t, err)

		// Verify it was created
		retrieved, err := store.GetGroup(context.Background(), group.ID)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		assert.Equal(t, group.ID, retrieved.ID)
		assert.Equal(t, group.Name, retrieved.Name)
	})
}

func TestSQLiteGetGroupNotFound(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		retrieved, err := store.GetGroup(context.Background(), "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, retrieved)
	})
}

func TestSQLiteListGroups(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		group1 := makeTestGroup("group-1")
		group2 := makeTestGroup("group-2")

		err := store.CreateGroup(context.Background(), group1)
		require.NoError(t, err)
		err = store.CreateGroup(context.Background(), group2)
		require.NoError(t, err)

		groups, err := store.ListGroups(context.Background())
		require.NoError(t, err)
		assert.Len(t, groups, 2)
	})
}

func TestSQLiteDeleteGroup(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		group := makeTestGroup("test-group")
		err := store.CreateGroup(context.Background(), group)
		require.NoError(t, err)

		err = store.DeleteGroup(context.Background(), group.ID)
		require.NoError(t, err)

		// Verify it was deleted
		retrieved, err := store.GetGroup(context.Background(), group.ID)
		require.NoError(t, err)
		assert.Nil(t, retrieved)
	})
}

// Config tests

func TestSQLiteCreateConfig(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		config := makeTestConfig("config-1", &agentID, nil)

		err := store.CreateConfig(context.Background(), config)
		require.NoError(t, err)

		// Verify it was created
		retrieved, err := store.GetConfig(context.Background(), config.ID)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		assert.Equal(t, config.ID, retrieved.ID)
		assert.Equal(t, config.Content, retrieved.Content)
	})
}

func TestSQLiteGetLatestConfigForAgent(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		// Create multiple configs for the same agent
		config1 := makeTestConfig("config-1", &agentID, nil)
		config1.Version = 1
		config1.CreatedAt = time.Now().Add(-time.Hour).UTC()

		config2 := makeTestConfig("config-2", &agentID, nil)
		config2.Version = 2
		config2.CreatedAt = time.Now().UTC()

		err := store.CreateConfig(context.Background(), config1)
		require.NoError(t, err)
		err = store.CreateConfig(context.Background(), config2)
		require.NoError(t, err)

		// Get latest config
		latest, err := store.GetLatestConfigForAgent(context.Background(), agentID)
		require.NoError(t, err)
		require.NotNil(t, latest)
		assert.Equal(t, config2.ID, latest.ID)
		assert.Equal(t, 2, latest.Version)
	})
}

func TestSQLiteGetLatestConfigForGroup(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		groupID := "test-group"
		group := makeTestGroup(groupID)
		err := store.CreateGroup(context.Background(), group)
		require.NoError(t, err)

		// Create multiple configs for the same group
		config1 := makeTestConfig("config-1", nil, &groupID)
		config1.Version = 1
		config1.CreatedAt = time.Now().Add(-time.Hour).UTC()

		config2 := makeTestConfig("config-2", nil, &groupID)
		config2.Version = 2
		config2.CreatedAt = time.Now().UTC()

		err = store.CreateConfig(context.Background(), config1)
		require.NoError(t, err)
		err = store.CreateConfig(context.Background(), config2)
		require.NoError(t, err)

		// Get latest config
		latest, err := store.GetLatestConfigForGroup(context.Background(), groupID)
		require.NoError(t, err)
		require.NotNil(t, latest)
		assert.Equal(t, config2.ID, latest.ID)
		assert.Equal(t, 2, latest.Version)
	})
}

func TestSQLiteDeleteConfig(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		config := makeTestConfig("config-1", &agentID, nil)
		require.NoError(t, store.CreateConfig(context.Background(), config))

		// Round-trip: delete then it's gone.
		require.NoError(t, store.DeleteConfig(context.Background(), config.ID))
		got, err := store.GetConfig(context.Background(), config.ID)
		require.NoError(t, err)
		assert.Nil(t, got)

		// Missing id is a no-op (per convention), not an error.
		require.NoError(t, store.DeleteConfig(context.Background(), "does-not-exist"))
	})
}

func TestSQLiteDeleteConfigsForAgent(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		groupID := "test-group"
		require.NoError(t, store.CreateGroup(context.Background(), makeTestGroup(groupID)))

		// Two agent-scoped rows (rollout assignments) + one group-scoped row.
		a1 := makeTestConfig("a1", &agentID, nil)
		a1.Version = 1
		a1.CreatedAt = time.Now().Add(-time.Hour).UTC()
		a2 := makeTestConfig("a2", &agentID, nil)
		a2.Version = 2
		a2.CreatedAt = time.Now().UTC()
		g1 := makeTestConfig("g1", nil, &groupID)
		g1.Version = 5
		require.NoError(t, store.CreateConfig(context.Background(), a1))
		require.NoError(t, store.CreateConfig(context.Background(), a2))
		require.NoError(t, store.CreateConfig(context.Background(), g1))

		require.NoError(t, store.DeleteConfigsForAgent(context.Background(), agentID))

		// All agent-scoped rows gone → the agent now resolves to group config.
		latestAgent, err := store.GetLatestConfigForAgent(context.Background(), agentID)
		require.NoError(t, err)
		assert.Nil(t, latestAgent)
		// Group-scoped row untouched.
		latestGroup, err := store.GetLatestConfigForGroup(context.Background(), groupID)
		require.NoError(t, err)
		require.NotNil(t, latestGroup)
		assert.Equal(t, "g1", latestGroup.ID)

		// Idempotent: a second call is a no-op.
		require.NoError(t, store.DeleteConfigsForAgent(context.Background(), agentID))
	})
}

func TestSQLiteSetConfigArchived(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		ctx := context.Background()
		c := makeTestConfig("cfg-archive", &agentID, nil)
		require.NoError(t, store.CreateConfig(ctx, c))

		// Fresh config is active and lists by default.
		got, err := store.GetConfig(ctx, c.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Nil(t, got.ArchivedAt)

		list, err := store.ListConfigs(ctx, types.ConfigFilter{})
		require.NoError(t, err)
		assert.Len(t, list, 1)

		// Archive → hidden by default, present with IncludeArchived, tombstone set.
		require.NoError(t, store.SetConfigArchived(ctx, c.ID, true))
		got, err = store.GetConfig(ctx, c.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ArchivedAt)

		list, err = store.ListConfigs(ctx, types.ConfigFilter{})
		require.NoError(t, err)
		assert.Empty(t, list, "archived configs are hidden from the default listing")

		list, err = store.ListConfigs(ctx, types.ConfigFilter{IncludeArchived: true})
		require.NoError(t, err)
		assert.Len(t, list, 1, "IncludeArchived surfaces archived configs")

		// Unarchive restores it.
		require.NoError(t, store.SetConfigArchived(ctx, c.ID, false))
		got, err = store.GetConfig(ctx, c.ID)
		require.NoError(t, err)
		assert.Nil(t, got.ArchivedAt)
		list, err = store.ListConfigs(ctx, types.ConfigFilter{})
		require.NoError(t, err)
		assert.Len(t, list, 1)

		// Missing id → ErrConfigNotFound (not the idempotent-on-missing convention).
		err = store.SetConfigArchived(ctx, "does-not-exist", true)
		assert.ErrorIs(t, err, types.ErrConfigNotFound)
	})
}

func TestSQLiteListConfigsWithFilter(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		agentID1 := uuid.New()
		agentID2 := uuid.New()
		groupID := "test-group"

		// Create agents and group
		agent1 := makeTestAgent(agentID1)
		agent2 := makeTestAgent(agentID2)
		group := makeTestGroup(groupID)

		err := store.CreateAgent(context.Background(), agent1)
		require.NoError(t, err)
		err = store.CreateAgent(context.Background(), agent2)
		require.NoError(t, err)
		err = store.CreateGroup(context.Background(), group)
		require.NoError(t, err)

		// Create configs
		config1 := makeTestConfig("config-1", &agentID1, nil)
		config2 := makeTestConfig("config-2", &agentID2, nil)
		config3 := makeTestConfig("config-3", nil, &groupID)

		err = store.CreateConfig(context.Background(), config1)
		require.NoError(t, err)
		err = store.CreateConfig(context.Background(), config2)
		require.NoError(t, err)
		err = store.CreateConfig(context.Background(), config3)
		require.NoError(t, err)

		// Filter by agent ID
		configs, err := store.ListConfigs(context.Background(), types.ConfigFilter{
			AgentID: &agentID1,
		})
		require.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, config1.ID, configs[0].ID)

		// Filter by group ID
		configs, err = store.ListConfigs(context.Background(), types.ConfigFilter{
			GroupID: &groupID,
		})
		require.NoError(t, err)
		assert.Len(t, configs, 1)
		assert.Equal(t, config3.ID, configs[0].ID)

		// No filter
		configs, err = store.ListConfigs(context.Background(), types.ConfigFilter{})
		require.NoError(t, err)
		assert.Len(t, configs, 3)
	})
}

func TestSQLiteListConfigsWithLimit(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		for i := 0; i < 5; i++ {
			config := makeTestConfig(uuid.New().String(), &agentID, nil)
			err := store.CreateConfig(context.Background(), config)
			require.NoError(t, err)
		}

		// List with limit
		configs, err := store.ListConfigs(context.Background(), types.ConfigFilter{
			Limit: 3,
		})
		require.NoError(t, err)
		assert.Len(t, configs, 3)
	})
}

// Schema migration tests

func TestSQLiteMigration(t *testing.T) {
	dbPath := makeTempDB(t)
	defer os.Remove(dbPath)

	logger := zap.NewNop()

	// Create first instance - should run migrations
	store1, err := NewSQLiteStorage(dbPath, logger)
	require.NoError(t, err)
	storage1 := store1.(*Storage)
	storage1.Close()

	// Create second instance - should not fail on existing schema
	store2, err := NewSQLiteStorage(dbPath, logger)
	require.NoError(t, err)
	storage2 := store2.(*Storage)
	defer storage2.Close()
}

// Concurrency test

func TestSQLiteConcurrentAccess(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		// Create some test data concurrently
		done := make(chan bool)
		for i := 0; i < 10; i++ {
			go func(idx int) {
				agent := makeTestAgent(uuid.New())
				err := store.CreateAgent(context.Background(), agent)
				assert.NoError(t, err)
				done <- true
			}(i)
		}

		// Wait for all goroutines
		for i := 0; i < 10; i++ {
			<-done
		}

		// Verify all agents were created
		agents, err := store.ListAgents(context.Background())
		require.NoError(t, err)
		assert.Len(t, agents, 10)
	})
}

// TestSQLiteUpdateAgentRegistration: the registration UPDATE writes
// Name/Labels/Version/GroupID/GroupName by ID and leaves Status alone.
// group_id has a FK to groups(id) (foreign_keys=ON), so the target
// group is created first.
func TestSQLiteUpdateAgentRegistration(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		ctx := context.Background()
		require.NoError(t, store.CreateGroup(ctx, makeTestGroup("grp-prod")))

		gid, gname := "grp-prod", "prod"
		err := store.UpdateAgentRegistration(ctx, &types.Agent{
			ID:        agentID,
			Name:      "renamed",
			Labels:    map[string]string{"tier": "gold"},
			Version:   "2.3.4",
			GroupID:   &gid,
			GroupName: &gname,
		})
		require.NoError(t, err)

		got, err := store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "renamed", got.Name)
		assert.Equal(t, "2.3.4", got.Version)
		require.NotNil(t, got.GroupID)
		assert.Equal(t, "grp-prod", *got.GroupID)
		require.NotNil(t, got.GroupName)
		assert.Equal(t, "prod", *got.GroupName)
		assert.Equal(t, "gold", got.Labels["tier"])
		assert.Equal(t, types.AgentStatusOnline, got.Status)
	})
}

func TestSQLiteUpdateAgentRegistrationNotFound(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		err := store.UpdateAgentRegistration(context.Background(), &types.Agent{ID: uuid.New(), Name: "x"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// TestSQLiteUpdateAgentRegistration_PersistsCapabilities: a reconnect that
// re-reports the agent's capabilities persists them (they used to be dropped by
// this path — the drift "2nd miss" root cause, PR #35). RED before the fix
// (capabilities stayed the create-time set).
func TestSQLiteUpdateAgentRegistration_PersistsCapabilities(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		ctx := context.Background()
		err := store.UpdateAgentRegistration(ctx, &types.Agent{
			ID:           agentID,
			Name:         "agent",
			Capabilities: []string{"accepts_remote_config", "reports_status"},
		})
		require.NoError(t, err)

		got, err := store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.ElementsMatch(t, []string{"accepts_remote_config", "reports_status"}, got.Capabilities)
	})
}

// TestSQLiteUpdateAgentRegistration_PreservesCapabilitiesOnEmpty: a later
// capability-less registration (e.g. an operator group PATCH) must NOT wipe a
// previously-persisted capability set — the preserve-on-empty guard.
func TestSQLiteUpdateAgentRegistration_PreservesCapabilitiesOnEmpty(t *testing.T) {
	withPopulatedSQLiteStore(t, func(store types.ApplicationStore, agentID uuid.UUID) {
		ctx := context.Background()
		require.NoError(t, store.UpdateAgentRegistration(ctx, &types.Agent{
			ID: agentID, Name: "agent", Capabilities: []string{"accepts_remote_config"}}))
		// Empty/nil capabilities on a subsequent write must preserve, not clear.
		require.NoError(t, store.UpdateAgentRegistration(ctx, &types.Agent{
			ID: agentID, Name: "agent", Capabilities: nil}))

		got, err := store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.ElementsMatch(t, []string{"accepts_remote_config"}, got.Capabilities)
	})
}

// TestSQLiteDeleteGroup_NullsMembersAndRemovesGroupConfigs: deleting a group must
// not leave a member agent pointing at a now-missing group (dangling membership)
// nor orphan its group-scoped config. RED before the fix (agent.group_id stayed
// set to the deleted group's id).
func TestSQLiteDeleteGroup_NullsMembersAndRemovesGroupConfigs(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		gid := "grp-del"
		require.NoError(t, store.CreateGroup(ctx, makeTestGroup(gid)))

		aid := uuid.New()
		a := makeTestAgent(aid)
		gname := "grp"
		a.GroupID = &gid
		a.GroupName = &gname
		require.NoError(t, store.CreateAgent(ctx, a))

		require.NoError(t, store.CreateConfig(ctx, makeTestConfig("cfg-group", nil, &gid)))
		require.NoError(t, store.CreateConfig(ctx, makeTestConfig("cfg-agent", &aid, nil)))

		require.NoError(t, store.DeleteGroup(ctx, gid))

		got, err := store.GetAgent(ctx, aid)
		require.NoError(t, err)
		require.NotNil(t, got, "member agent must survive its group's deletion")
		assert.Nil(t, got.GroupID, "member agent group_id must be nulled, not left dangling")

		gcfg, err := store.GetLatestConfigForGroup(ctx, gid)
		require.NoError(t, err)
		assert.Nil(t, gcfg, "group config must be removed with the group")

		acfg, err := store.GetLatestConfigForAgent(ctx, aid)
		require.NoError(t, err)
		require.NotNil(t, acfg, "agent-scoped config must be untouched")
		assert.Equal(t, "cfg-agent", acfg.ID)
	})
}
