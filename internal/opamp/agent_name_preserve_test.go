// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
)

// TestReconnect_AfterDecommission_PreservesName is the pilot P3 cosmetic finding
// (v0.89.478 acceptance): after a decommission → revive (the #48
// revive-on-reconnect path), the fleet card's DISPLAY NAME blanked to "unknown"
// until the agent next re-identified. The opampsupervisor re-sends its full
// AgentDescription only on a fresh identify, not on every reconnect, so the FIRST
// reconnect after decommission carries NO description — extractAgentName then
// returns the "unknown" sentinel, which the CreateAgent revive-upsert wrote over
// the stored last-known name. This drives the REAL registration path
// (processAgentGrouping + persistAgent over a real memory-backed AgentService)
// and asserts the revived agent still serves its last-known name. RED before the
// preserve-on-empty fix (name comes back "unknown"), GREEN after.
func TestReconnect_AfterDecommission_PreservesName(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	createStoredAgent(t, svc, id, sptr("group-prod"), sptr("prod")) // Name "web-01"

	if err := svc.DeleteAgent(ctx, id); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	// The supervisor reconnects with NO AgentDescription — the description-less
	// reconnect the opampsupervisor sends before its next fresh identify.
	agent := &Agent{InstanceId: id, InstanceIdStr: id.String()}
	msg := &protobufs.AgentToServer{}
	server.processAgentGrouping(ctx, agent, msg)
	server.persistAgent(ctx, agent, msg)

	got, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent after reconnect: %v", err)
	}
	if got == nil {
		t.Fatal("agent was not revived on reconnect")
	}
	if got.ID != id {
		t.Fatalf("revived agent has a different id: got %s, want %s", got.ID, id)
	}
	if got.Name != "web-01" {
		t.Fatalf("display name was blanked on revive: got %q, want \"web-01\" "+
			"(a description-less reconnect must preserve the last-known name, not overwrite it with \"unknown\"/\"\")", got.Name)
	}
}

// TestReconnect_WithNewName_UpdatesName is the counterpart invariant: when the
// reconnect DOES carry a fresh AgentDescription with a real name, that name still
// wins — the preserve-on-empty guard only fires on an empty/omitted name, exactly
// like the group- and capabilities-preserve guards.
func TestReconnect_WithNewName_UpdatesName(t *testing.T) {
	ctx := context.Background()
	server, svc := newGroupPreserveServer(t)

	id := uuid.New()
	createStoredAgent(t, svc, id, sptr("group-prod"), sptr("prod")) // Name "web-01"

	if err := svc.DeleteAgent(ctx, id); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	// Reconnect carrying a fresh identify with a NEW name.
	agent := &Agent{InstanceId: id, InstanceIdStr: id.String()}
	msg := &protobufs.AgentToServer{
		AgentDescription: &protobufs.AgentDescription{
			IdentifyingAttributes: []*protobufs.KeyValue{strAttr("host.name", "web-01-renamed")},
		},
	}
	server.processAgentGrouping(ctx, agent, msg)
	server.persistAgent(ctx, agent, msg)

	got, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent after reconnect: %v", err)
	}
	if got == nil {
		t.Fatal("agent was not revived on reconnect")
	}
	if got.Name != "web-01-renamed" {
		t.Fatalf("a real reported name must update the display name: got %q, want \"web-01-renamed\"", got.Name)
	}
}
