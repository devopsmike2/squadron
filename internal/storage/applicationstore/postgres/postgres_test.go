// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	chain "github.com/devopsmike2/squadron/internal/audit/chain"
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
	if _, err := s.db.Exec("TRUNCATE groups, agents, configs, rollouts, rollout_approvals, saved_queries, alert_rules, audit_events, audit_chain_checkpoints"); err != nil {
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

func TestPostgres_RolloutCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	failOpen := false
	// AbortCriteria carries an ADR 0034 SLOBurn criterion to prove the whole
	// nested struct round-trips through JSONB.
	r := &types.Rollout{
		ID:               "r1",
		Name:             "prod-canary",
		GroupID:          "g1",
		TargetConfigID:   "cfg-target",
		PreviousConfigID: "cfg-prev",
		Stages: []types.RolloutStage{
			{Mode: types.RolloutStageModePercent, Percentage: 10, DwellSeconds: 300},
			{Mode: types.RolloutStageModePercent, Percentage: 100, DwellSeconds: 600},
		},
		AbortCriteria: types.RolloutAbortCriteria{
			MaxDriftedAgents:      2,
			MaxErrorLogsPerMinute: 50,
			SLOBurn: &types.RolloutSLOBurnCriterion{
				ConnectorID: "prom-1",
				Signal:      "metrics",
				Selector:    "http_request_errors",
				Matchers: []types.RolloutSLOMatcher{
					{Label: "service", Op: "=", Value: "checkout"},
				},
				Aggregation:   "rate",
				WindowSeconds: 300,
				Threshold:     0.05,
				FailOpen:      &failOpen,
			},
		},
		NotificationURL:   "https://hooks.example/notify",
		State:             types.RolloutStatePendingApproval,
		CurrentStage:      0,
		RequireApproval:   true,
		RequestedBy:       "alice",
		RequiredApprovals: 2,
		ProposedBy:        types.RolloutProposedByAI,
		ProposalReasoning: "error budget trending down",
		EvidenceRefs:      []types.RolloutEvidenceRef{{Kind: "alert", ID: "a-42"}},
		PlanID:            "plan-1",
		PlanStepIndex:     0,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.CreateRollout(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetRollout(ctx, "r1")
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.Name != "prod-canary" || got.GroupID != "g1" || got.TargetConfigID != "cfg-target" ||
		got.PreviousConfigID != "cfg-prev" || got.State != types.RolloutStatePendingApproval ||
		got.RequiredApprovals != 2 || !got.RequireApproval || got.RequestedBy != "alice" ||
		got.ProposedBy != types.RolloutProposedByAI || got.NotificationURL != "https://hooks.example/notify" ||
		len(got.Stages) != 2 || got.Stages[0].Percentage != 10 || len(got.EvidenceRefs) != 1 ||
		got.PlanID != "plan-1" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	// StepKind defaults to the "rollout" sentinel on an empty/NULL column.
	if got.StepKind != types.StepKindRollout {
		t.Fatalf("step_kind should default to %q, got %q", types.StepKindRollout, got.StepKind)
	}
	// SLOBurn criterion must survive the JSONB round-trip in full.
	sb := got.AbortCriteria.SLOBurn
	if sb == nil {
		t.Fatalf("SLOBurn criterion did not round-trip: %+v", got.AbortCriteria)
	}
	if sb.ConnectorID != "prom-1" || sb.Signal != "metrics" || sb.Selector != "http_request_errors" ||
		sb.Aggregation != "rate" || sb.WindowSeconds != 300 || sb.Threshold != 0.05 ||
		len(sb.Matchers) != 1 || sb.Matchers[0].Label != "service" || sb.Matchers[0].Value != "checkout" ||
		sb.FailOpen == nil || *sb.FailOpen != false {
		t.Fatalf("SLOBurn round-trip mismatch: %+v", sb)
	}
	if got.AbortCriteria.MaxDriftedAgents != 2 || got.AbortCriteria.MaxErrorLogsPerMinute != 50 {
		t.Fatalf("abort criteria scalars mismatch: %+v", got.AbortCriteria)
	}

	// Missing row must be (nil, nil), matching the sqlite backend.
	if miss, err := s.GetRollout(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil), got %v %v", miss, err)
	}

	// A second rollout, newer, to exercise list ordering + filters.
	r2 := &types.Rollout{
		ID:             "r2",
		Name:           "prod-canary-2",
		GroupID:        "g2",
		TargetConfigID: "cfg-target-2",
		Stages:         []types.RolloutStage{{Mode: types.RolloutStageModePercent, Percentage: 100, DwellSeconds: 60}},
		AbortCriteria:  types.RolloutAbortCriteria{MaxDriftedAgents: 1},
		State:          types.RolloutStateInProgress,
		PlanID:         "plan-2",
		CreatedAt:      now.Add(time.Second),
		UpdatedAt:      now.Add(time.Second),
	}
	if err := s.CreateRollout(ctx, r2); err != nil {
		t.Fatalf("create r2: %v", err)
	}
	// Empty SLOBurn / evidence must round-trip to nil, not an empty struct.
	if g2, _ := s.GetRollout(ctx, "r2"); g2 == nil || g2.AbortCriteria.SLOBurn != nil || len(g2.EvidenceRefs) != 0 {
		t.Fatalf("empty optionals should round-trip to nil: %+v", g2)
	}

	// Update persists (blind path: Version==0).
	r.State = types.RolloutStateInProgress
	r.CurrentStage = 1
	stageStarted := now.Add(2 * time.Second)
	r.StageStartedAt = &stageStarted
	r.PushedAgentIDs = []string{"agent-a", "agent-b"}
	if err := s.UpdateRollout(ctx, r); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetRollout(ctx, "r1")
	if got.State != types.RolloutStateInProgress || got.CurrentStage != 1 ||
		got.StageStartedAt == nil || len(got.PushedAgentIDs) != 2 {
		t.Fatalf("update didn't persist: %+v", got)
	}
	// The blind path still advances the version counter (1 → 2).
	if got.Version != 2 {
		t.Fatalf("version should advance to 2 after blind update, got %d", got.Version)
	}

	// CAS update with the loaded version succeeds and bumps version.
	got.AbortReason = "manual"
	if err := s.UpdateRollout(ctx, got); err != nil {
		t.Fatalf("CAS update: %v", err)
	}
	if got.Version != 3 {
		t.Fatalf("CAS update should bump version to 3, got %d", got.Version)
	}
	// A stale-version CAS update must return ErrRolloutVersionConflict.
	stale := *got
	stale.Version = 2
	if err := s.UpdateRollout(ctx, &stale); !errors.Is(err, types.ErrRolloutVersionConflict) {
		t.Fatalf("stale CAS should return ErrRolloutVersionConflict, got %v", err)
	}

	// Update / delete-missing semantics: a missing id is "not found" on both the
	// blind and CAS paths.
	missing := &types.Rollout{ID: "ghost", GroupID: "g", TargetConfigID: "c", State: types.RolloutStatePending}
	if err := s.UpdateRollout(ctx, missing); err == nil {
		t.Fatal("update on missing rollout should error (blind path)")
	}
	missing.Version = 5
	if err := s.UpdateRollout(ctx, missing); err == nil || errors.Is(err, types.ErrRolloutVersionConflict) {
		t.Fatalf("update on missing rollout (CAS path) should be not-found, got %v", err)
	}

	// List ordering is created_at DESC (r2 newer than r1).
	all, err := s.ListRollouts(ctx, types.RolloutFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: err=%v len=%d", err, len(all))
	}
	if all[0].ID != "r2" || all[1].ID != "r1" {
		t.Fatalf("list order should be created_at DESC: %s,%s", all[0].ID, all[1].ID)
	}
	// Filter by group.
	byGroup, err := s.ListRollouts(ctx, types.RolloutFilter{GroupID: "g1"})
	if err != nil || len(byGroup) != 1 || byGroup[0].ID != "r1" {
		t.Fatalf("list by group: err=%v got=%v", err, byGroup)
	}
	// Filter by state.
	byState, err := s.ListRollouts(ctx, types.RolloutFilter{State: types.RolloutStateInProgress})
	if err != nil || len(byState) != 2 {
		t.Fatalf("list by state: err=%v len=%d", err, len(byState))
	}
	// Filter by plan id.
	byPlan, err := s.ListRollouts(ctx, types.RolloutFilter{PlanID: "plan-2"})
	if err != nil || len(byPlan) != 1 || byPlan[0].ID != "r2" {
		t.Fatalf("list by plan: err=%v got=%v", err, byPlan)
	}
	// Limit is honored.
	limited, err := s.ListRollouts(ctx, types.RolloutFilter{Limit: 1})
	if err != nil || len(limited) != 1 || limited[0].ID != "r2" {
		t.Fatalf("list limit=1: err=%v got=%v", err, limited)
	}
}

func TestPostgres_RolloutApprovals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	rid := "r-appr"
	// No approvers yet.
	if n, err := s.CountRolloutApprovers(ctx, rid); err != nil || n != 0 {
		t.Fatalf("count on empty should be 0: n=%d err=%v", n, err)
	}
	if list, err := s.ListRolloutApprovers(ctx, rid); err != nil || len(list) != 0 {
		t.Fatalf("list on empty should be []: len=%d err=%v", len(list), err)
	}

	if err := s.RecordRolloutApproval(ctx, rid, "alice", "tok-a", "lgtm", now); err != nil {
		t.Fatalf("record alice: %v", err)
	}
	if err := s.RecordRolloutApproval(ctx, rid, "bob", "tok-b", "", now.Add(time.Second)); err != nil {
		t.Fatalf("record bob: %v", err)
	}
	// Idempotent: alice approving again does not double-count (ON CONFLICT).
	if err := s.RecordRolloutApproval(ctx, rid, "alice", "tok-a", "again", now.Add(2*time.Second)); err != nil {
		t.Fatalf("record alice again: %v", err)
	}

	n, err := s.CountRolloutApprovers(ctx, rid)
	if err != nil || n != 2 {
		t.Fatalf("distinct count should be 2: n=%d err=%v", n, err)
	}

	// List is oldest-first with token/notes round-tripped.
	list, err := s.ListRolloutApprovers(ctx, rid)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}
	if list[0].Approver != "alice" || list[0].ApproverTokenID != "tok-a" || list[0].Notes != "lgtm" {
		t.Fatalf("first approver mismatch: %+v", list[0])
	}
	if list[1].Approver != "bob" || list[1].Notes != "" {
		t.Fatalf("second approver mismatch: %+v", list[1])
	}
}

func TestPostgres_SavedQueryCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	q1 := &types.SavedQuery{
		ID:          "sq1",
		Name:        "drifted agents",
		Description: "agents currently drifted",
		Query:       "fleet_drift_status_drifted",
		Tags:        []string{"drift", "fleet"},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateSavedQuery(ctx, q1); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetSavedQuery(ctx, "sq1")
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.Name != "drifted agents" || got.Query != "fleet_drift_status_drifted" ||
		got.Description != "agents currently drifted" || len(got.Tags) != 2 || got.Tags[0] != "drift" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Missing row must be (nil, nil), matching the sqlite backend.
	if miss, err := s.GetSavedQuery(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil), got %v %v", miss, err)
	}

	// Update persists.
	q1.Name = "drifted agents v2"
	q1.Tags = []string{"drift"}
	q1.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateSavedQuery(ctx, q1); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = s.GetSavedQuery(ctx, "sq1"); got.Name != "drifted agents v2" || len(got.Tags) != 1 {
		t.Fatalf("update didn't persist: %+v", got)
	}

	// A second, newer query to exercise list ordering (updated_at DESC).
	q2 := &types.SavedQuery{ID: "sq2", Name: "errors", Query: "error_rate", CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)}
	if err := s.CreateSavedQuery(ctx, q2); err != nil {
		t.Fatalf("create sq2: %v", err)
	}
	list, err := s.ListSavedQueries(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}
	if list[0].ID != "sq2" || list[1].ID != "sq1" {
		t.Fatalf("list order should be updated_at DESC: %s,%s", list[0].ID, list[1].ID)
	}

	// Update / delete on a missing id are "not found".
	if err := s.UpdateSavedQuery(ctx, &types.SavedQuery{ID: "ghost"}); err == nil {
		t.Fatal("update on missing saved query should error")
	}
	if err := s.DeleteSavedQuery(ctx, "ghost"); err == nil {
		t.Fatal("delete on missing saved query should error")
	}

	if err := s.DeleteSavedQuery(ctx, "sq1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteSavedQuery(ctx, "sq1"); err == nil {
		t.Fatal("second delete should error (saved query not found)")
	}
}

func TestPostgres_AlertRuleCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	r1 := &types.AlertRule{
		ID:                "ar1",
		Name:              "high drift rate",
		Description:       "too many drifted agents",
		Query:             "fleet_drift_status_drifted",
		ThresholdOperator: types.ThresholdGreater,
		ThresholdValue:    5,
		IntervalSeconds:   60,
		Severity:          types.AlertSeverityWarning,
		Enabled:           true,
		WebhookURL:        "https://hooks.example/squadron",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.CreateAlertRule(ctx, r1); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetAlertRule(ctx, "ar1")
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.Name != "high drift rate" || got.ThresholdOperator != types.ThresholdGreater ||
		got.ThresholdValue != 5 || got.IntervalSeconds != 60 || got.Severity != types.AlertSeverityWarning ||
		!got.Enabled || got.WebhookURL != "https://hooks.example/squadron" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Missing row must be (nil, nil).
	if miss, err := s.GetAlertRule(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil), got %v %v", miss, err)
	}

	// Update persists (including the enabled flag flipping).
	r1.ThresholdValue = 10
	r1.Enabled = false
	r1.Severity = types.AlertSeverityCritical
	r1.UpdatedAt = now.Add(time.Second)
	if err := s.UpdateAlertRule(ctx, r1); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = s.GetAlertRule(ctx, "ar1"); got.ThresholdValue != 10 || got.Enabled || got.Severity != types.AlertSeverityCritical {
		t.Fatalf("update didn't persist: %+v", got)
	}

	// A second rule to exercise list ordering (name ASC): "abc" sorts before
	// "high drift rate".
	r2 := &types.AlertRule{ID: "ar2", Name: "abc rule", Query: "q", ThresholdOperator: types.ThresholdLess, ThresholdValue: 1, IntervalSeconds: 30, Severity: types.AlertSeverityInfo, Enabled: true, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateAlertRule(ctx, r2); err != nil {
		t.Fatalf("create ar2: %v", err)
	}
	list, err := s.ListAlertRules(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}
	if list[0].Name != "abc rule" || list[1].Name != "high drift rate" {
		t.Fatalf("list order should be name ASC: %s,%s", list[0].Name, list[1].Name)
	}

	// Update / delete on a missing id are "not found".
	if err := s.UpdateAlertRule(ctx, &types.AlertRule{ID: "ghost"}); err == nil {
		t.Fatal("update on missing alert rule should error")
	}
	if err := s.DeleteAlertRule(ctx, "ghost"); err == nil {
		t.Fatal("delete on missing alert rule should error")
	}

	if err := s.DeleteAlertRule(ctx, "ar1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteAlertRule(ctx, "ar1"); err == nil {
		t.Fatal("second delete should error (alert rule not found)")
	}
}

// appendPGAuditEvent inserts one event with distinct, verifiable content,
// mirroring the sqlite audit_chain_test helper so the two backends exercise the
// chain the same way.
func appendPGAuditEvent(t *testing.T, s *Storage, ctx context.Context, n int) {
	t.Helper()
	if err := s.CreateAuditEvent(ctx, &types.AuditEvent{
		ID:         uuid.NewString(),
		Actor:      fmt.Sprintf("operator:user%d@example.com", n),
		EventType:  "config.applied",
		TargetType: "config",
		TargetID:   fmt.Sprintf("cfg-%d", n),
		Action:     "applied",
		Payload:    map[string]any{"n": n, "note": fmt.Sprintf("event-%d", n)},
	}); err != nil {
		t.Fatalf("append audit event %d: %v", n, err)
	}
}

// TestPostgres_AuditChain_SelfVerify is the KEY audit evidence: append a
// sequence of events to real Postgres, then run the SHARED pure chain package's
// verify over the Postgres-stored chain and assert it passes — proving the
// hash-chain holds on Postgres exactly as on SQLite. It also proves the raw
// payload round-trips byte-for-byte (offline recompute would break otherwise).
func TestPostgres_AuditChain_SelfVerify(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		appendPGAuditEvent(t, s, ctx, i)
	}

	res, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("chain should verify OK: detail=%s", res.Detail)
	}
	if res.RowsVerified != 5 || res.CoversFromSeq != 1 || res.FirstBreakSeq != 0 || res.HeadSeq != 5 {
		t.Fatalf("verify summary mismatch: %+v", res)
	}

	// Offline recompute through the SAME pure package the CLI uses.
	rows, err := s.ListAuditChainRows(ctx)
	if err != nil || len(rows) != 5 {
		t.Fatalf("list chain rows: err=%v len=%d", err, len(rows))
	}
	if off := chain.Verify(rows); !off.OK || off.RowsVerified != 5 {
		t.Fatalf("offline recompute must verify: %+v", off)
	}

	// Byte-exactness: row 1's returned payload equals the RAW stored column
	// verbatim (no re-marshal), which is what makes the hash recompute valid.
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM audit_events WHERE seq = 1`).Scan(&stored); err != nil {
		t.Fatalf("read stored payload: %v", err)
	}
	if rows[0].Payload != stored {
		t.Fatalf("ListAuditChainRows must return the RAW payload byte-for-byte: got %q want %q", rows[0].Payload, stored)
	}

	// Get/List surface the same rows.
	if list, err := s.ListAuditEvents(ctx, types.AuditEventFilter{}); err != nil || len(list) != 5 {
		t.Fatalf("list audit events: err=%v len=%d", err, len(list))
	}
	if one, err := s.GetAuditEvent(ctx, rows[2].ID); err != nil || one == nil || one.ID != rows[2].ID {
		t.Fatalf("get audit event: err=%v got=%v", err, one)
	}
	if miss, err := s.GetAuditEvent(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil), got %v %v", miss, err)
	}
}

// TestPostgres_AuditChain_TamperDetected mutates a stored row's PAYLOAD directly
// (the classic content-edit attack) and asserts verification now FAILS at that
// seq — the hash-chain catches on Postgres exactly as on SQLite.
func TestPostgres_AuditChain_TamperDetected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		appendPGAuditEvent(t, s, ctx, i)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE audit_events SET payload='{"tampered":true}' WHERE seq=3`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	res, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.OK {
		t.Fatal("payload tamper must break the chain")
	}
	if res.FirstBreakSeq != 3 {
		t.Fatalf("break should be at seq 3, got %d (detail=%s)", res.FirstBreakSeq, res.Detail)
	}
	// Offline recompute agrees.
	rows, _ := s.ListAuditChainRows(ctx)
	if off := chain.Verify(rows); off.OK {
		t.Fatal("offline recompute must also detect the tamper")
	}
}

// TestPostgres_AuditChain_HashTamperDetected mutates a stored ROW_HASH directly
// (an attacker rewriting the stored digest) and asserts verification fails at
// that seq.
func TestPostgres_AuditChain_HashTamperDetected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		appendPGAuditEvent(t, s, ctx, i)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE audit_events SET row_hash='deadbeef' WHERE seq=2`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	res, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.OK {
		t.Fatal("row_hash tamper must break the chain")
	}
	if res.FirstBreakSeq != 2 {
		t.Fatalf("break should be at seq 2, got %d (detail=%s)", res.FirstBreakSeq, res.Detail)
	}
}

// TestPostgres_AuditCheckpoint covers the ADR 0027 slice 2 checkpoint substrate:
// the (tenant, seq) upsert + newest-first list, AND the positive anchoring of a
// prefix-pruned chain-start to a recorded checkpoint.
func TestPostgres_AuditCheckpoint(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	// Upsert semantics, keyed by (tenant, checkpoint_seq).
	if err := s.WriteAuditCheckpoint(ctx, types.AuditCheckpoint{Tenant: "t1", CheckpointSeq: 2, CheckpointRowHash: "h2", RowsPruned: 2, Kind: "retention-cut", CreatedAt: now}); err != nil {
		t.Fatalf("write cp seq2: %v", err)
	}
	if err := s.WriteAuditCheckpoint(ctx, types.AuditCheckpoint{Tenant: "t1", CheckpointSeq: 5, CheckpointRowHash: "h5", RowsPruned: 5, Kind: "retention-cut", CreatedAt: now}); err != nil {
		t.Fatalf("write cp seq5: %v", err)
	}
	// Re-record seq=2 with a new row_hash: upsert, not a duplicate.
	if err := s.WriteAuditCheckpoint(ctx, types.AuditCheckpoint{Tenant: "t1", CheckpointSeq: 2, CheckpointRowHash: "h2b", RowsPruned: 3, Kind: "retention-cut", CreatedAt: now}); err != nil {
		t.Fatalf("upsert cp seq2: %v", err)
	}
	cps, err := s.ListAuditCheckpoints(ctx, "t1")
	if err != nil || len(cps) != 2 {
		t.Fatalf("list checkpoints: err=%v len=%d", err, len(cps))
	}
	if cps[0].CheckpointSeq != 5 || cps[1].CheckpointSeq != 2 {
		t.Fatalf("checkpoints should be newest seq first: %d,%d", cps[0].CheckpointSeq, cps[1].CheckpointSeq)
	}
	if cps[1].CheckpointRowHash != "h2b" {
		t.Fatalf("upsert should have overwritten the row_hash: %s", cps[1].CheckpointRowHash)
	}

	// Anchoring: append a chain, capture seq 1's row_hash, prune it (legitimate
	// prefix GC), record a checkpoint for the pruned head, then verify the
	// surviving chain-start is POSITIVELY anchored to that checkpoint.
	for i := 1; i <= 4; i++ {
		appendPGAuditEvent(t, s, ctx, i)
	}
	var seq1Hash string
	if err := s.db.QueryRowContext(ctx, `SELECT row_hash FROM audit_events WHERE seq=1`).Scan(&seq1Hash); err != nil {
		t.Fatalf("read seq1 hash: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_events WHERE seq=1`); err != nil {
		t.Fatalf("prune seq1: %v", err)
	}
	// The default tenant is what the unstamped context resolves to on append.
	if err := s.WriteAuditCheckpoint(ctx, types.AuditCheckpoint{Tenant: "default", CheckpointSeq: 1, CheckpointRowHash: seq1Hash, RowsPruned: 1, Kind: "retention-cut", CreatedAt: now}); err != nil {
		t.Fatalf("write anchoring checkpoint: %v", err)
	}

	res, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify after prune: %v", err)
	}
	if !res.OK {
		t.Fatalf("prefix GC must not be a false positive: detail=%s", res.Detail)
	}
	if res.CoversFromSeq != 2 || res.RowsVerified != 3 {
		t.Fatalf("post-prune summary mismatch: %+v", res)
	}
	if !res.AnchoredByCheckpoint || res.CheckpointSeq != 1 {
		t.Fatalf("chain-start should be anchored to checkpoint seq 1: %+v", res)
	}
}

// TestOpen_EmptyDSN needs no database and always runs.
func TestOpen_EmptyDSN(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open with empty DSN must error")
	}
}
