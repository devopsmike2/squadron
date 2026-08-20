// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// sptr returns a pointer to s. Group id/name fields are *string; the empty
// pointer (&"") is deliberately distinct from nil because extractGroupInfo
// resolves a group-less agent to an EMPTY (not nil) GroupID.
func sptr(s string) *string { return &s }

// newGroupPreserveServer builds an OpAMP server over a memory-backed real
// AgentService — the same wiring newDriftTestService uses — so persistAgent
// exercises the actual store write path (UpdateAgentRegistration) rather than a
// mock. Returns the server and the live service.
func newGroupPreserveServer(t *testing.T) (*Server, services.AgentService) {
	t.Helper()
	svc := services.NewAgentService(memory.NewStore(), nil, nil, nil, zap.NewNop())
	return &Server{logger: zap.NewNop(), agentService: svc}, svc
}

// createStoredAgent seeds an agent row, optionally with a persisted group
// membership (group == nil means ungrouped). Mirrors an agent that was created
// on first connect and later had a group assigned (e.g. via the operator PATCH).
func createStoredAgent(t *testing.T, svc services.AgentService, id uuid.UUID, group *string, groupName *string) {
	t.Helper()
	now := time.Now()
	if err := svc.CreateAgent(context.Background(), &services.Agent{
		ID: id, Name: "web-01", Status: services.AgentStatusOnline,
		GroupID: group, GroupName: groupName,
		LastSeen: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
}

// descNoGroup is an AgentDescription that carries identity but NO group.*
// attribute — the shape a plain collector reports on reconnect. extractGroupInfo
// resolves it to an empty group, which is what used to wipe the stored group_id.
func descNoGroup() *protobufs.AgentToServer {
	return &protobufs.AgentToServer{
		AgentDescription: &protobufs.AgentDescription{
			IdentifyingAttributes: []*protobufs.KeyValue{strAttr("service.name", "web-01")},
		},
	}
}

// TestReconnect_NoReportedGroup_PreservesManualMembership is the pilot bug:
// an agent with a persisted (manually-assigned) group that reconnects reporting
// NO group must keep its group_id. Before the fix the empty re-resolution flowed
// into UpdateAgentRegistration and NULLed the membership (a control-plane
// restart wiped every manual assignment this way). RED before, GREEN after.
func TestReconnect_NoReportedGroup_PreservesManualMembership(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	createStoredAgent(t, svc, id, sptr("group-prod"), sptr("prod"))

	// Fresh in-memory connection agent (GroupID nil, as on a new socket).
	agent := &Agent{InstanceId: id, InstanceIdStr: id.String()}
	msg := descNoGroup()

	// Full reconnect path: grouping resolution then persistence.
	server.processAgentGrouping(ctx, agent, msg)
	server.persistAgent(ctx, agent, msg)

	got, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.GroupID == nil || *got.GroupID != "group-prod" {
		t.Fatalf("manual group membership was wiped on reconnect: GroupID=%v, want \"group-prod\"", got.GroupID)
	}
	if got.GroupName == nil || *got.GroupName != "prod" {
		t.Fatalf("group name was wiped on reconnect: GroupName=%v, want \"prod\"", got.GroupName)
	}
	// In-memory registry stays consistent with the store so the reconciler /
	// connect-path resolver keep delivering the group's config.
	if agent.GroupID == nil || *agent.GroupID != "group-prod" {
		t.Fatalf("in-memory agent group not restored: GroupID=%v, want \"group-prod\"", agent.GroupID)
	}
}

// TestReconnect_AfterDecommission_RevivesAgent is the pilot P1 data-integrity
// bug: an agent that is decommissioned (soft-deleted) and then reconnects over
// OpAMP was STRANDED permanently. persistAgent's GetAgent hides the tombstoned
// row, so it took the CreateAgent branch, whose plain INSERT collided with the
// still-present primary-key row ("UNIQUE constraint failed: agents.id") on
// EVERY re-register attempt (~30s cadence) — the agent could never re-join the
// fleet and was unrecoverable via the API. This test drives the REAL
// registration path (processAgentGrouping + persistAgent over a real
// memory-backed AgentService, the same wiring the server runs) and asserts the
// agent comes back as ONE agent with the SAME id. RED before the
// revive-on-reconnect fix (the tombstoned row is never revived, GetAgent stays
// nil — stranded), GREEN after.
func TestReconnect_AfterDecommission_RevivesAgent(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	createStoredAgent(t, svc, id, sptr("group-prod"), sptr("prod"))

	// Decommission via the real service soft-delete — the DELETE /agents/:id path.
	if err := svc.DeleteAgent(ctx, id); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	// It must be hidden from the operational view now (tombstoned).
	if got, _ := svc.GetAgent(ctx, id); got != nil {
		t.Fatalf("agent should be hidden after decommission, got %+v", got)
	}

	// The supervisor reconnects: the exact server path OpAMP drives on a message.
	agent := &Agent{InstanceId: id, InstanceIdStr: id.String()}
	msg := descNoGroup()
	server.processAgentGrouping(ctx, agent, msg)
	server.persistAgent(ctx, agent, msg)

	// The agent must be back in the fleet with the SAME id.
	got, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent after reconnect: %v", err)
	}
	if got == nil {
		t.Fatal("decommissioned agent was NOT revived on reconnect — still stranded " +
			"(the UNIQUE-constraint collision on re-register was not fixed)")
	}
	if got.ID != id {
		t.Fatalf("revived agent has a different id: got %s, want %s", got.ID, id)
	}

	// And exactly ONE row carries that id — a revive, not a duplicate.
	all, err := svc.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	count := 0
	for _, a := range all {
		if a.ID == id {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly ONE agent row with id %s after revive, got %d", id, count)
	}
}

// TestReconnect_ReportedGroup_OverridesManualMembership: when the agent DOES
// self-report a group, the agent-declared group wins on reconnect (existing
// behavior) — even over a different manual assignment. The preserve guard only
// fires on an EMPTY report.
func TestReconnect_ReportedGroup_OverridesManualMembership(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	createStoredAgent(t, svc, id, sptr("group-prod"), sptr("prod"))

	// A group the agent self-reports by name; ensureAgentGroup resolves it.
	if err := svc.CreateGroup(ctx, &services.Group{
		ID: "group-staging", Name: "staging", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	agent := &Agent{InstanceId: id, InstanceIdStr: id.String()}
	msg := &protobufs.AgentToServer{
		AgentDescription: &protobufs.AgentDescription{
			IdentifyingAttributes:    []*protobufs.KeyValue{strAttr("service.name", "web-01")},
			NonIdentifyingAttributes: []*protobufs.KeyValue{strAttr("group.name", "staging")},
		},
	}

	server.processAgentGrouping(ctx, agent, msg)
	server.persistAgent(ctx, agent, msg)

	got, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.GroupID == nil || *got.GroupID != "group-staging" {
		t.Fatalf("agent-reported group did not win: GroupID=%v, want \"group-staging\"", got.GroupID)
	}
}

// TestReconnect_NoReportedGroup_NoPersistedGroup_StaysUngrouped: an ungrouped
// agent that reports no group must not gain a spurious group — the preserve
// guard only carries a NON-empty persisted membership forward.
func TestReconnect_NoReportedGroup_NoPersistedGroup_StaysUngrouped(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	createStoredAgent(t, svc, id, nil, nil)

	agent := &Agent{InstanceId: id, InstanceIdStr: id.String()}
	msg := descNoGroup()

	server.processAgentGrouping(ctx, agent, msg)
	server.persistAgent(ctx, agent, msg)

	got, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.GroupID != nil && *got.GroupID != "" {
		t.Fatalf("agent gained a spurious group: GroupID=%v, want empty", *got.GroupID)
	}
}

// TestReconnect_AfterOperatorClear_StaysUngrouped: an operator PATCH that CLEARS
// the group (GroupID nil via UpdateAgentRegistration, exactly what the handler
// does) must stick — a later reconnect reporting no group must NOT resurrect the
// previously-assigned group. The preserve guard keys off the CURRENT persisted
// value, which is empty after the clear.
func TestReconnect_AfterOperatorClear_StaysUngrouped(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	createStoredAgent(t, svc, id, sptr("group-prod"), sptr("prod"))

	// Operator PATCH clear: the handler sets GroupID/GroupName nil and calls
	// UpdateAgentRegistration. Reproduce that authoritative clear directly.
	if err := svc.UpdateAgentRegistration(ctx, &services.Agent{
		ID: id, Name: "web-01", GroupID: nil, GroupName: nil,
	}); err != nil {
		t.Fatalf("operator clear UpdateAgentRegistration: %v", err)
	}

	agent := &Agent{InstanceId: id, InstanceIdStr: id.String()}
	msg := descNoGroup()
	server.processAgentGrouping(ctx, agent, msg)
	server.persistAgent(ctx, agent, msg)

	got, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.GroupID != nil && *got.GroupID != "" {
		t.Fatalf("cleared group was resurrected on reconnect: GroupID=%v, want empty", *got.GroupID)
	}
}
