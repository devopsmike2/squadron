// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testAgentID   = uuid.New()
	testGroupID   = "test-group"
	testConfigID  = "test-config"
	testTimestamp = time.Now().UTC()
)

func makeTestAgent() *types.Agent {
	return &types.Agent{
		ID:   testAgentID,
		Name: "test-agent",
		Labels: map[string]string{
			"env":     "test",
			"service": "demo",
		},
		Status:       types.AgentStatusOnline,
		LastSeen:     testTimestamp,
		Version:      "1.0.0",
		Capabilities: []string{"metrics", "logs", "traces"},
		CreatedAt:    testTimestamp,
		UpdatedAt:    testTimestamp,
	}
}

func makeTestGroup() *types.Group {
	return &types.Group{
		ID:   testGroupID,
		Name: "test-group",
		Labels: map[string]string{
			"env": "test",
		},
		CreatedAt: testTimestamp,
		UpdatedAt: testTimestamp,
	}
}

func makeTestConfig(agentID *uuid.UUID, groupID *string) *types.Config {
	return &types.Config{
		ID:         testConfigID,
		AgentID:    agentID,
		GroupID:    groupID,
		ConfigHash: "abc123",
		Content:    "receivers:\n  otlp:",
		Version:    1,
		CreatedAt:  testTimestamp,
	}
}

func withMemoryStore(f func(store *Store)) {
	f(NewStore())
}

func withPopulatedMemoryStore(f func(store *Store)) {
	store := NewStore()
	agent := makeTestAgent()
	_ = store.CreateAgent(context.Background(), agent)
	f(store)
}

// Agent tests

func TestStoreCreateAgent(t *testing.T) {
	withMemoryStore(func(store *Store) {
		agent := makeTestAgent()
		err := store.CreateAgent(context.Background(), agent)
		require.NoError(t, err)

		// Verify it was created
		retrieved, err := store.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		assert.Equal(t, agent.ID, retrieved.ID)
		assert.Equal(t, agent.Name, retrieved.Name)
		assert.Equal(t, agent.Labels, retrieved.Labels)
		assert.Equal(t, agent.Status, retrieved.Status)
	})
}

func TestStoreCreateAgentDuplicate(t *testing.T) {
	withMemoryStore(func(store *Store) {
		agent := makeTestAgent()
		err := store.CreateAgent(context.Background(), agent)
		require.NoError(t, err)

		// Try to create duplicate
		err = store.CreateAgent(context.Background(), agent)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})
}

// TestStoreCreateAgentRevivesTombstoned proves the pilot P1 fix on the memory
// backend (the one the opamp reconnect harness and CI unit tests use): a
// DeleteAgent-tombstoned row is REVIVED by a subsequent CreateAgent with the
// same id rather than rejected as a duplicate, so a decommissioned agent that
// reconnects re-joins the fleet. A live duplicate is still rejected.
func TestStoreCreateAgentRevivesTombstoned(t *testing.T) {
	withMemoryStore(func(store *Store) {
		ctx := context.Background()
		agent := makeTestAgent()
		require.NoError(t, store.CreateAgent(ctx, agent))
		require.NoError(t, store.DeleteAgent(ctx, agent.ID))

		// Tombstoned -> hidden.
		got, err := store.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.Nil(t, got)

		// Re-register revives (does NOT error).
		revived := makeTestAgent()
		revived.Name = "revived"
		require.NoError(t, store.CreateAgent(ctx, revived))

		got, err = store.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "revived", got.Name)

		agents, err := store.ListAgents(ctx)
		require.NoError(t, err)
		assert.Len(t, agents, 1)

		// Live duplicate still rejected.
		err = store.CreateAgent(ctx, makeTestAgent())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})
}

// TestStoreRestoreAndPurgeAgent covers the memory store's operator-recovery
// methods, keeping them consistent with the sqlite/postgres backends.
func TestStoreRestoreAndPurgeAgent(t *testing.T) {
	withMemoryStore(func(store *Store) {
		ctx := context.Background()
		agent := makeTestAgent()
		require.NoError(t, store.CreateAgent(ctx, agent))

		// Restore on a live agent errors.
		require.Error(t, store.RestoreAgent(ctx, agent.ID))

		// Decommission then restore.
		require.NoError(t, store.DeleteAgent(ctx, agent.ID))
		require.NoError(t, store.RestoreAgent(ctx, agent.ID))
		got, err := store.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.NotNil(t, got)

		// Purge hard-deletes.
		require.NoError(t, store.PurgeAgent(ctx, agent.ID))
		got, err = store.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.Nil(t, got)
		require.Error(t, store.RestoreAgent(ctx, agent.ID))
		require.Error(t, store.PurgeAgent(ctx, uuid.New()))
	})
}

func TestStoreGetAgentNotFound(t *testing.T) {
	withMemoryStore(func(store *Store) {
		retrieved, err := store.GetAgent(context.Background(), uuid.New())
		require.NoError(t, err)
		assert.Nil(t, retrieved)
	})
}

func TestStoreGetAgentIsolation(t *testing.T) {
	withPopulatedMemoryStore(func(store *Store) {
		retrieved, err := store.GetAgent(context.Background(), testAgentID)
		require.NoError(t, err)
		require.NotNil(t, retrieved)

		// Modify the retrieved agent
		retrieved.Name = "modified"
		retrieved.Labels["new"] = "label"

		// Get the agent again and verify it wasn't modified
		retrieved2, err := store.GetAgent(context.Background(), testAgentID)
		require.NoError(t, err)
		require.NotNil(t, retrieved2)
		assert.Equal(t, "test-agent", retrieved2.Name)
		assert.NotContains(t, retrieved2.Labels, "new")
	})
}

func TestStoreListAgents(t *testing.T) {
	withMemoryStore(func(store *Store) {
		// Create multiple agents
		agent1 := makeTestAgent()
		agent2 := makeTestAgent()
		agent2.ID = uuid.New()
		agent2.Name = "test-agent-2"

		err := store.CreateAgent(context.Background(), agent1)
		require.NoError(t, err)
		err = store.CreateAgent(context.Background(), agent2)
		require.NoError(t, err)

		// List agents
		agents, err := store.ListAgents(context.Background())
		require.NoError(t, err)
		assert.Len(t, agents, 2)
	})
}

func TestStoreUpdateAgentStatus(t *testing.T) {
	withPopulatedMemoryStore(func(store *Store) {
		err := store.UpdateAgentStatus(context.Background(), testAgentID, types.AgentStatusOffline)
		require.NoError(t, err)

		// Verify the update
		agent, err := store.GetAgent(context.Background(), testAgentID)
		require.NoError(t, err)
		assert.Equal(t, types.AgentStatusOffline, agent.Status)
	})
}

func TestStoreUpdateAgentStatusNotFound(t *testing.T) {
	withMemoryStore(func(store *Store) {
		err := store.UpdateAgentStatus(context.Background(), uuid.New(), types.AgentStatusOffline)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestStoreUpdateAgentLastSeen(t *testing.T) {
	withPopulatedMemoryStore(func(store *Store) {
		newTime := time.Now().Add(time.Hour)
		err := store.UpdateAgentLastSeen(context.Background(), testAgentID, newTime)
		require.NoError(t, err)

		// Verify the update
		agent, err := store.GetAgent(context.Background(), testAgentID)
		require.NoError(t, err)
		assert.Equal(t, newTime.Unix(), agent.LastSeen.Unix())
	})
}

func TestStoreDeleteAgent(t *testing.T) {
	withPopulatedMemoryStore(func(store *Store) {
		err := store.DeleteAgent(context.Background(), testAgentID)
		require.NoError(t, err)

		// Verify it was deleted
		agent, err := store.GetAgent(context.Background(), testAgentID)
		require.NoError(t, err)
		assert.Nil(t, agent)
	})
}

func TestStoreDeleteAgentNotFound(t *testing.T) {
	withMemoryStore(func(store *Store) {
		err := store.DeleteAgent(context.Background(), uuid.New())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// Group tests

func TestStoreCreateGroup(t *testing.T) {
	withMemoryStore(func(store *Store) {
		group := makeTestGroup()
		err := store.CreateGroup(context.Background(), group)
		require.NoError(t, err)

		// Verify it was created
		retrieved, err := store.GetGroup(context.Background(), group.ID)
		require.NoError(t, err)
		require.NotNil(t, retrieved)
		assert.Equal(t, group.ID, retrieved.ID)
		assert.Equal(t, group.Name, retrieved.Name)
		assert.Equal(t, group.Labels, retrieved.Labels)
	})
}

func TestStoreCreateGroupDuplicate(t *testing.T) {
	withMemoryStore(func(store *Store) {
		group := makeTestGroup()
		err := store.CreateGroup(context.Background(), group)
		require.NoError(t, err)

		// Try to create duplicate
		err = store.CreateGroup(context.Background(), group)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})
}

func TestStoreGetGroupNotFound(t *testing.T) {
	withMemoryStore(func(store *Store) {
		retrieved, err := store.GetGroup(context.Background(), "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, retrieved)
	})
}

func TestStoreListGroups(t *testing.T) {
	withMemoryStore(func(store *Store) {
		group1 := makeTestGroup()
		group2 := makeTestGroup()
		group2.ID = "test-group-2"
		group2.Name = "test-group-2"

		err := store.CreateGroup(context.Background(), group1)
		require.NoError(t, err)
		err = store.CreateGroup(context.Background(), group2)
		require.NoError(t, err)

		groups, err := store.ListGroups(context.Background())
		require.NoError(t, err)
		assert.Len(t, groups, 2)
	})
}

func TestStoreDeleteGroup(t *testing.T) {
	withMemoryStore(func(store *Store) {
		group := makeTestGroup()
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

func TestStoreCreateConfig(t *testing.T) {
	withMemoryStore(func(store *Store) {
		agentID := testAgentID
		config := makeTestConfig(&agentID, nil)
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

func TestStoreGetLatestConfigForAgent(t *testing.T) {
	withMemoryStore(func(store *Store) {
		agentID := testAgentID

		// Create multiple configs for the same agent
		config1 := makeTestConfig(&agentID, nil)
		config1.ID = "config-1"
		config1.Version = 1
		config1.CreatedAt = time.Now().Add(-time.Hour)

		config2 := makeTestConfig(&agentID, nil)
		config2.ID = "config-2"
		config2.Version = 2
		config2.CreatedAt = time.Now()

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

func TestStoreGetLatestConfigForGroup(t *testing.T) {
	withMemoryStore(func(store *Store) {
		groupID := testGroupID

		// Create multiple configs for the same group
		config1 := makeTestConfig(nil, &groupID)
		config1.ID = "config-1"
		config1.Version = 1
		config1.CreatedAt = time.Now().Add(-time.Hour)

		config2 := makeTestConfig(nil, &groupID)
		config2.ID = "config-2"
		config2.Version = 2
		config2.CreatedAt = time.Now()

		err := store.CreateConfig(context.Background(), config1)
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

func TestStoreDeleteConfig(t *testing.T) {
	withMemoryStore(func(store *Store) {
		agentID := testAgentID
		config := makeTestConfig(&agentID, nil)
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

func TestStoreDeleteConfigsForAgent(t *testing.T) {
	withMemoryStore(func(store *Store) {
		agentID := testAgentID
		groupID := testGroupID

		// Two agent-scoped rows (rollout assignments) + one group-scoped row.
		a1 := makeTestConfig(&agentID, nil)
		a1.ID = "a1"
		a1.Version = 1
		a2 := makeTestConfig(&agentID, nil)
		a2.ID = "a2"
		a2.Version = 2
		g1 := makeTestConfig(nil, &groupID)
		g1.ID = "g1"
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

func TestStoreSetConfigArchived(t *testing.T) {
	withMemoryStore(func(store *Store) {
		ctx := context.Background()
		agentID := testAgentID
		c := makeTestConfig(&agentID, nil)
		c.ID = "cfg-archive"
		require.NoError(t, store.CreateConfig(ctx, c))

		// Fresh config is active and shows in the default list.
		got, err := store.GetConfig(ctx, c.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Nil(t, got.ArchivedAt, "a freshly created config is not archived")

		list, err := store.ListConfigs(ctx, types.ConfigFilter{})
		require.NoError(t, err)
		assert.Len(t, list, 1)

		// Archive it → hidden from the default list, visible with IncludeArchived.
		require.NoError(t, store.SetConfigArchived(ctx, c.ID, true))
		got, err = store.GetConfig(ctx, c.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ArchivedAt, "archived config carries an archived_at tombstone")

		list, err = store.ListConfigs(ctx, types.ConfigFilter{})
		require.NoError(t, err)
		assert.Empty(t, list, "archived configs are hidden from the default listing")

		list, err = store.ListConfigs(ctx, types.ConfigFilter{IncludeArchived: true})
		require.NoError(t, err)
		assert.Len(t, list, 1, "IncludeArchived surfaces archived configs")

		// Unarchive restores it to the default list.
		require.NoError(t, store.SetConfigArchived(ctx, c.ID, false))
		got, err = store.GetConfig(ctx, c.ID)
		require.NoError(t, err)
		assert.Nil(t, got.ArchivedAt, "unarchive clears the tombstone")
		list, err = store.ListConfigs(ctx, types.ConfigFilter{})
		require.NoError(t, err)
		assert.Len(t, list, 1)

		// Missing id is a not-found error (NOT the idempotent-on-missing convention).
		err = store.SetConfigArchived(ctx, "does-not-exist", true)
		assert.ErrorIs(t, err, types.ErrConfigNotFound)
	})
}

func TestStoreListConfigsWithFilter(t *testing.T) {
	withMemoryStore(func(store *Store) {
		agentID1 := testAgentID
		agentID2 := uuid.New()
		groupID := testGroupID

		config1 := makeTestConfig(&agentID1, nil)
		config1.ID = "config-1"

		config2 := makeTestConfig(&agentID2, nil)
		config2.ID = "config-2"

		config3 := makeTestConfig(nil, &groupID)
		config3.ID = "config-3"

		err := store.CreateConfig(context.Background(), config1)
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

func TestStoreListConfigsWithLimit(t *testing.T) {
	withMemoryStore(func(store *Store) {
		agentID := testAgentID

		for i := 0; i < 5; i++ {
			config := makeTestConfig(&agentID, nil)
			config.ID = uuid.New().String()
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

// Purge test

func TestStorePurge(t *testing.T) {
	withMemoryStore(func(store *Store) {
		// Add some data
		agent := makeTestAgent()
		group := makeTestGroup()
		agentID := testAgentID
		config := makeTestConfig(&agentID, nil)

		err := store.CreateAgent(context.Background(), agent)
		require.NoError(t, err)
		err = store.CreateGroup(context.Background(), group)
		require.NoError(t, err)
		err = store.CreateConfig(context.Background(), config)
		require.NoError(t, err)

		// Purge
		store.purge(context.Background())

		// Verify all data was removed
		agents, err := store.ListAgents(context.Background())
		require.NoError(t, err)
		assert.Empty(t, agents)

		groups, err := store.ListGroups(context.Background())
		require.NoError(t, err)
		assert.Empty(t, groups)

		configs, err := store.ListConfigs(context.Background(), types.ConfigFilter{})
		require.NoError(t, err)
		assert.Empty(t, configs)
	})
}

// TestStoreUpdateAgentRegistration: the registration UPDATE writes
// Name/Labels/Version/GroupID/GroupName by ID, deep-copies Labels, and
// leaves Status untouched.
func TestStoreUpdateAgentRegistration(t *testing.T) {
	withPopulatedMemoryStore(func(store *Store) {
		ctx := context.Background()
		gid, gname := "grp-7", "prod"
		newLabels := map[string]string{"tier": "gold"}

		err := store.UpdateAgentRegistration(ctx, &types.Agent{
			ID:        testAgentID,
			Name:      "renamed",
			Labels:    newLabels,
			Version:   "2.3.4",
			GroupID:   &gid,
			GroupName: &gname,
		})
		require.NoError(t, err)

		got, err := store.GetAgent(ctx, testAgentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "renamed", got.Name)
		assert.Equal(t, "2.3.4", got.Version)
		require.NotNil(t, got.GroupID)
		assert.Equal(t, "grp-7", *got.GroupID)
		require.NotNil(t, got.GroupName)
		assert.Equal(t, "prod", *got.GroupName)
		assert.Equal(t, map[string]string{"tier": "gold"}, got.Labels)
		// Status is NOT part of the registration update.
		assert.Equal(t, types.AgentStatusOnline, got.Status)

		// Mutating the caller's label map must not bleed into the store.
		newLabels["tier"] = "mutated"
		got2, err := store.GetAgent(ctx, testAgentID)
		require.NoError(t, err)
		assert.Equal(t, "gold", got2.Labels["tier"])
	})
}

func TestStoreUpdateAgentRegistrationNotFound(t *testing.T) {
	withMemoryStore(func(store *Store) {
		err := store.UpdateAgentRegistration(context.Background(), &types.Agent{ID: uuid.New(), Name: "x"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// TestStoreUpdateAgentRegistration_PersistsCapabilities: a reconnect re-reports
// capabilities and they persist (PR #35 root cause was that they didn't).
func TestStoreUpdateAgentRegistration_PersistsCapabilities(t *testing.T) {
	withPopulatedMemoryStore(func(store *Store) {
		ctx := context.Background()
		require.NoError(t, store.UpdateAgentRegistration(ctx, &types.Agent{
			ID: testAgentID, Name: "agent", Capabilities: []string{"accepts_remote_config"}}))
		got, err := store.GetAgent(ctx, testAgentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.ElementsMatch(t, []string{"accepts_remote_config"}, got.Capabilities)
	})
}

// TestStoreUpdateAgentRegistration_PreservesCapabilitiesOnEmpty: a later
// capability-less registration must not wipe the stored set.
func TestStoreUpdateAgentRegistration_PreservesCapabilitiesOnEmpty(t *testing.T) {
	withPopulatedMemoryStore(func(store *Store) {
		ctx := context.Background()
		require.NoError(t, store.UpdateAgentRegistration(ctx, &types.Agent{
			ID: testAgentID, Name: "agent", Capabilities: []string{"accepts_remote_config"}}))
		require.NoError(t, store.UpdateAgentRegistration(ctx, &types.Agent{
			ID: testAgentID, Name: "agent", Capabilities: nil}))
		got, err := store.GetAgent(ctx, testAgentID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.ElementsMatch(t, []string{"accepts_remote_config"}, got.Capabilities)
	})
}

// TestStoreDeleteGroup_NullsMembersAndRemovesGroupConfigs: deleting a group nulls
// member agents' membership (no dangling ref) and removes the group's config,
// leaving agent-scoped configs untouched.
func TestStoreDeleteGroup_NullsMembersAndRemovesGroupConfigs(t *testing.T) {
	withMemoryStore(func(store *Store) {
		ctx := context.Background()
		gid := "grp-del"
		require.NoError(t, store.CreateGroup(ctx, &types.Group{ID: gid, Name: "grp"}))

		aid := uuid.New()
		gname := "grp"
		require.NoError(t, store.CreateAgent(ctx, &types.Agent{
			ID: aid, Name: "a1", GroupID: &gid, GroupName: &gname,
			Status: types.AgentStatusOnline, LastSeen: testTimestamp,
			CreatedAt: testTimestamp, UpdatedAt: testTimestamp}))
		require.NoError(t, store.CreateConfig(ctx, &types.Config{ID: "cfg-group", GroupID: &gid, ConfigHash: "h", Content: "c", Version: 1, CreatedAt: testTimestamp}))
		require.NoError(t, store.CreateConfig(ctx, &types.Config{ID: "cfg-agent", AgentID: &aid, ConfigHash: "h", Content: "c", Version: 1, CreatedAt: testTimestamp}))

		require.NoError(t, store.DeleteGroup(ctx, gid))

		got, err := store.GetAgent(ctx, aid)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Nil(t, got.GroupID, "member agent group_id must be nulled, not left dangling")

		gcfg, err := store.GetLatestConfigForGroup(ctx, gid)
		require.NoError(t, err)
		assert.Nil(t, gcfg, "group config must be removed with the group")

		acfg, err := store.GetLatestConfigForAgent(ctx, aid)
		require.NoError(t, err)
		require.NotNil(t, acfg, "agent-scoped config must be untouched")
	})
}
