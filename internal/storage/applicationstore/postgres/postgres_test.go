// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// testStore opens a real Postgres from TEST_POSTGRES_DSN (a services container
// in CI) and truncates the tables it uses. Skips when the DSN is unset so local
// `go test ./...` stays green without a Postgres.
func testStore(t *testing.T) *Storage {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping Postgres integration test")
	}
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.db.Exec("TRUNCATE groups, agents, configs"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func TestPostgres_GroupCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	g := &types.Group{
		ID:                "g1",
		Name:              "Prod",
		Labels:            map[string]string{"env": "prod", "tier": "1"},
		RequireApproval:   true,
		LearnFromVerdicts: true,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.CreateGroup(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetGroup(ctx, "g1")
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.Name != "Prod" || got.Labels["env"] != "prod" || !got.RequireApproval {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Missing row must be (nil, nil), matching the sqlite backend.
	if miss, err := s.GetGroup(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil), got %v %v", miss, err)
	}

	g.Name = "Prod-2"
	if err := s.UpdateGroup(ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = s.GetGroup(ctx, "g1"); got.Name != "Prod-2" {
		t.Fatalf("update didn't persist: %+v", got)
	}

	if list, err := s.ListGroups(ctx); err != nil || len(list) != 1 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}

	if err := s.DeleteGroup(ctx, "g1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Second delete must error (matches sqlite "group not found").
	if err := s.DeleteGroup(ctx, "g1"); err == nil {
		t.Fatal("second delete should error (group not found)")
	}
}

func TestPostgres_AgentCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	gid := "g1"
	gname := "Prod"
	a := &types.Agent{
		ID:              uuid.New(),
		Name:            "collector-1",
		Labels:          map[string]string{"env": "prod", "host": "node-a"},
		Status:          types.AgentStatusOnline,
		LastSeen:        now,
		GroupID:         &gid,
		GroupName:       &gname,
		Version:         "1.2.3",
		Capabilities:    []string{"AcceptsRemoteConfig", "ReportsHealth"},
		DiscoverySource: "opamp",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.CreateAgent(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetAgent(ctx, a.ID)
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.Name != "collector-1" || got.Labels["env"] != "prod" ||
		got.Status != types.AgentStatusOnline || got.Version != "1.2.3" ||
		got.GroupID == nil || *got.GroupID != "g1" || len(got.Capabilities) != 2 ||
		got.DiscoverySource != "opamp" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Missing row must be (nil, nil), matching the sqlite backend.
	if miss, err := s.GetAgent(ctx, uuid.New()); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil), got %v %v", miss, err)
	}

	// CreateAgent defaults an empty DiscoverySource to "opamp".
	a2 := &types.Agent{ID: uuid.New(), Name: "collector-2", Status: types.AgentStatusOffline, LastSeen: now, CreatedAt: now.Add(time.Second), UpdatedAt: now}
	if err := s.CreateAgent(ctx, a2); err != nil {
		t.Fatalf("create a2: %v", err)
	}
	if got2, _ := s.GetAgent(ctx, a2.ID); got2 == nil || got2.DiscoverySource != "opamp" {
		t.Fatalf("empty discovery source should default to opamp: %+v", got2)
	}

	// Status update persists.
	if err := s.UpdateAgentStatus(ctx, a.ID, types.AgentStatusError); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if got, _ = s.GetAgent(ctx, a.ID); got.Status != types.AgentStatusError {
		t.Fatalf("status update didn't persist: %+v", got)
	}

	// Registration update persists.
	a.Name = "collector-1b"
	a.Version = "2.0.0"
	if err := s.UpdateAgentRegistration(ctx, a); err != nil {
		t.Fatalf("update registration: %v", err)
	}
	if got, _ = s.GetAgent(ctx, a.ID); got.Name != "collector-1b" || got.Version != "2.0.0" {
		t.Fatalf("registration update didn't persist: %+v", got)
	}

	// Effective-config + last-seen updates persist.
	if err := s.UpdateAgentEffectiveConfig(ctx, a.ID, "receivers: {}"); err != nil {
		t.Fatalf("update effective config: %v", err)
	}
	if err := s.UpdateAgentLastSeen(ctx, a.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("update last seen: %v", err)
	}
	if got, _ = s.GetAgent(ctx, a.ID); got.EffectiveConfig != "receivers: {}" {
		t.Fatalf("effective config didn't persist: %+v", got)
	}

	// Updates on a missing agent return "agent not found".
	if err := s.UpdateAgentStatus(ctx, uuid.New(), types.AgentStatusOnline); err == nil {
		t.Fatal("update status on missing agent should error")
	}
	if err := s.UpdateAgentRegistration(ctx, &types.Agent{ID: uuid.New()}); err == nil {
		t.Fatal("update registration on missing agent should error")
	}

	// List ordering is created_at DESC (a2 created after a).
	list, err := s.ListAgents(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}
	if list[0].ID != a2.ID {
		t.Fatalf("list order should be created_at DESC; got first=%s want=%s", list[0].ID, a2.ID)
	}

	// DeleteAgent is a soft delete: the row leaves the operational view.
	if err := s.DeleteAgent(ctx, a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ = s.GetAgent(ctx, a.ID); got != nil {
		t.Fatalf("soft-deleted agent should not be gettable: %+v", got)
	}
	if list, _ = s.ListAgents(ctx); len(list) != 1 {
		t.Fatalf("soft-deleted agent should be excluded from list, len=%d", len(list))
	}
	// Second delete must error (matches sqlite "agent not found").
	if err := s.DeleteAgent(ctx, a.ID); err == nil {
		t.Fatal("second delete should error (agent not found)")
	}
}

func TestPostgres_ConfigCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	agentID := uuid.New()
	groupID := "g1"

	c1 := &types.Config{
		ID:         "c1",
		Name:       "agent-cfg-v1",
		AgentID:    &agentID,
		ConfigHash: "hash1",
		Content:    "receivers: {}",
		Version:    1,
		CreatedAt:  now,
	}
	c2 := &types.Config{
		ID:         "c2",
		Name:       "agent-cfg-v2",
		AgentID:    &agentID,
		ConfigHash: "hash2",
		Content:    "receivers: {otlp: {}}",
		Version:    2,
		CreatedAt:  now.Add(time.Second),
	}
	cg := &types.Config{
		ID:         "cg",
		Name:       "group-cfg-v1",
		GroupID:    &groupID,
		ConfigHash: "hashg",
		Content:    "processors: {}",
		Version:    1,
		CreatedAt:  now.Add(2 * time.Second),
	}
	for _, c := range []*types.Config{c1, c2, cg} {
		if err := s.CreateConfig(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.ID, err)
		}
	}

	// Get roundtrip preserves the nullable agent_id / group_id mapping.
	got, err := s.GetConfig(ctx, "c1")
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.Name != "agent-cfg-v1" || got.AgentID == nil || *got.AgentID != agentID ||
		got.GroupID != nil || got.ConfigHash != "hash1" || got.Version != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if gotg, _ := s.GetConfig(ctx, "cg"); gotg == nil || gotg.GroupID == nil ||
		*gotg.GroupID != "g1" || gotg.AgentID != nil {
		t.Fatalf("group config roundtrip mismatch: %+v", gotg)
	}

	// Missing row must be (nil, nil).
	if miss, err := s.GetConfig(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil), got %v %v", miss, err)
	}

	// Latest-for-agent picks the highest version.
	latest, err := s.GetLatestConfigForAgent(ctx, agentID)
	if err != nil || latest == nil || latest.ID != "c2" {
		t.Fatalf("latest for agent should be c2, got=%v err=%v", latest, err)
	}
	if miss, err := s.GetLatestConfigForAgent(ctx, uuid.New()); err != nil || miss != nil {
		t.Fatalf("latest for missing agent should be (nil,nil), got %v %v", miss, err)
	}

	// Latest-for-group.
	latestG, err := s.GetLatestConfigForGroup(ctx, "g1")
	if err != nil || latestG == nil || latestG.ID != "cg" {
		t.Fatalf("latest for group should be cg, got=%v err=%v", latestG, err)
	}
	if miss, err := s.GetLatestConfigForGroup(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("latest for missing group should be (nil,nil), got %v %v", miss, err)
	}

	// List all, ordered created_at DESC (cg newest, then c2, then c1).
	all, err := s.ListConfigs(ctx, types.ConfigFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("list all: err=%v len=%d", err, len(all))
	}
	if all[0].ID != "cg" || all[1].ID != "c2" || all[2].ID != "c1" {
		t.Fatalf("list order should be created_at DESC: %s,%s,%s", all[0].ID, all[1].ID, all[2].ID)
	}

	// Filter by agent id.
	byAgent, err := s.ListConfigs(ctx, types.ConfigFilter{AgentID: &agentID})
	if err != nil || len(byAgent) != 2 {
		t.Fatalf("list by agent: err=%v len=%d", err, len(byAgent))
	}

	// Filter by group id.
	byGroup, err := s.ListConfigs(ctx, types.ConfigFilter{GroupID: &groupID})
	if err != nil || len(byGroup) != 1 || byGroup[0].ID != "cg" {
		t.Fatalf("list by group: err=%v got=%v", err, byGroup)
	}

	// Limit is honored.
	limited, err := s.ListConfigs(ctx, types.ConfigFilter{Limit: 1})
	if err != nil || len(limited) != 1 || limited[0].ID != "cg" {
		t.Fatalf("list limit=1: err=%v got=%v", err, limited)
	}
}

// TestOpen_EmptyDSN needs no database and always runs.
func TestOpen_EmptyDSN(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open with empty DSN must error")
	}
}
