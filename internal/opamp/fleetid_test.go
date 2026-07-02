// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/agentid"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/mock"
	"go.uber.org/zap"
)

// TestDeriveFleetId pins the OpAMP-side fleet-id derivation: it must match the
// OTLP ingest path (agentid.Derive) so a host that is both OpAMP-managed and
// shipping telemetry converges to ONE id, and it must fall back to the wire
// instance_uid when the description carries no usable identity (no regression).
func TestDeriveFleetId(t *testing.T) {
	s := &Server{}
	instanceId := uuid.New()

	t.Run("nil description -> instance_uid", func(t *testing.T) {
		if got := s.deriveFleetId(instanceId, nil); got != instanceId {
			t.Fatalf("nil desc: got %s want %s", got, instanceId)
		}
	})

	t.Run("UUID service.instance.id wins over instance_uid", func(t *testing.T) {
		svc := uuid.New() // distinct from instance_uid — the common third-party case
		desc := &protobufs.AgentDescription{
			IdentifyingAttributes: []*protobufs.KeyValue{
				strAttr("service.instance.id", svc.String()),
			},
		}
		got := s.deriveFleetId(instanceId, desc)
		if got != svc {
			t.Fatalf("uuid service.instance.id: got %s want %s", got, svc)
		}
		if got == instanceId {
			t.Fatalf("fleet id must diverge from instance_uid here")
		}
	})

	t.Run("host.name -> stable derived UUID, matches agentid.Derive", func(t *testing.T) {
		desc := &protobufs.AgentDescription{
			IdentifyingAttributes: []*protobufs.KeyValue{strAttr("host.name", "app-server-01")},
		}
		got := s.deriveFleetId(instanceId, desc)
		want := agentid.Derive(map[string]string{"host.name": "app-server-01"})
		if got.String() != want {
			t.Fatalf("host.name derive: got %s want %s", got, want)
		}
		if got == instanceId {
			t.Fatalf("host.name should derive a fleet id distinct from instance_uid")
		}
	})

	t.Run("no usable identity -> instance_uid fallback", func(t *testing.T) {
		desc := &protobufs.AgentDescription{
			NonIdentifyingAttributes: []*protobufs.KeyValue{strAttr("tier", "gold")},
		}
		if got := s.deriveFleetId(instanceId, desc); got != instanceId {
			t.Fatalf("no identity: got %s want instance_uid %s", got, instanceId)
		}
	})

	t.Run("identifying service.instance.id beats a non-identifying host.name dup", func(t *testing.T) {
		svc := uuid.New()
		desc := &protobufs.AgentDescription{
			IdentifyingAttributes:    []*protobufs.KeyValue{strAttr("service.instance.id", svc.String())},
			NonIdentifyingAttributes: []*protobufs.KeyValue{strAttr("host.name", "ignored")},
		}
		if got := s.deriveFleetId(instanceId, desc); got != svc {
			t.Fatalf("precedence: got %s want %s", got, svc)
		}
	})
}

// TestAgents_DualMap verifies the two-key index: store-facing lookups resolve by
// fleet id, wire lookups fall back to instance_uid, rebinding drops the stale
// fleet-id entry, and disconnect clears BOTH indexes.
func TestAgents_DualMap(t *testing.T) {
	agents := NewAgents(zap.NewNop())
	conn := &mockConnection{}
	instanceId := uuid.New()
	fleetId := uuid.New()

	agent := agents.FindOrCreateAgent(instanceId, conn)

	// Before any description: fleet id defaults to instance_uid, resolvable by it.
	if got := agents.FindAgent(instanceId); got != agent {
		t.Fatalf("pre-fleet: FindAgent(instanceId) did not return the agent")
	}

	agents.SetFleetId(agent, fleetId)

	// Store-facing lookup by fleet id resolves.
	if got := agents.FindAgent(fleetId); got != agent {
		t.Fatalf("FindAgent(fleetId) did not resolve after SetFleetId")
	}
	// Wire lookup by instance_uid still resolves (fallback index).
	if got := agents.FindAgent(instanceId); got != agent {
		t.Fatalf("FindAgent(instanceId) fallback broke after SetFleetId")
	}
	if agent.FleetId != fleetId || agent.FleetIdStr != fleetId.String() {
		t.Fatalf("agent fleet id not updated: %s / %s", agent.FleetId, agent.FleetIdStr)
	}

	// Rebinding to a new fleet id drops the stale entry.
	newFleetId := uuid.New()
	agents.SetFleetId(agent, newFleetId)
	if got := agents.FindAgent(fleetId); got != nil {
		t.Fatalf("stale fleet-id entry not removed on rebind")
	}
	if got := agents.FindAgent(newFleetId); got != agent {
		t.Fatalf("new fleet-id entry not indexed on rebind")
	}

	// Disconnect clears both indexes.
	agents.RemoveConnection(conn)
	if got := agents.FindAgent(newFleetId); got != nil {
		t.Fatalf("fleet-id index leaked after RemoveConnection")
	}
	if got := agents.FindAgent(instanceId); got != nil {
		t.Fatalf("instance-uid index leaked after RemoveConnection")
	}
}

// TestPersistAgent_KeysStoreByFleetId is the convergence proof: an OpAMP
// registration whose instance_uid differs from its reported UUID
// service.instance.id must persist the store row under the FLEET id (the OTLP
// identity), not the wire instance_uid — so it lands on the same fleet card as
// the host's OTLP telemetry instead of creating a second row.
func TestPersistAgent_KeysStoreByFleetId(t *testing.T) {
	logger := zap.NewNop()
	mockService := new(MockAgentService)
	agents := NewAgents(logger)
	server := &Server{logger: logger, agentService: mockService, agents: agents}

	conn := &mockConnection{}
	instanceId := uuid.New()    // opampextension's own ULID
	svcInstanceId := uuid.New() // the host's OTLP service.instance.id (distinct)

	agent := agents.FindOrCreateAgent(instanceId, conn)
	desc := &protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{
			strAttr("service.instance.id", svcInstanceId.String()),
			strAttr("host.name", "app-server-01"),
			strAttr("service.version", "1.2.3"),
		},
	}
	agents.SetFleetId(agent, server.deriveFleetId(instanceId, desc))

	if agent.FleetId != svcInstanceId {
		t.Fatalf("fleet id should equal the reported service.instance.id: got %s want %s",
			agent.FleetId, svcInstanceId)
	}

	msg := &protobufs.AgentToServer{AgentDescription: desc}

	// New row: GetAgent(fleetId) returns nil, then CreateAgent must be keyed by fleetId.
	mockService.On("GetAgent", mock.Anything, svcInstanceId).Return(nil, nil)
	mockService.On("GetGroupByName", mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	mockService.On("CreateAgent", mock.Anything, mock.MatchedBy(func(a *services.Agent) bool {
		return a.ID == svcInstanceId && a.ID != instanceId && a.Name == "app-server-01"
	})).Return(nil)

	server.persistAgent(context.Background(), agent, msg)

	mockService.AssertCalled(t, "GetAgent", mock.Anything, svcInstanceId)
	mockService.AssertCalled(t, "CreateAgent", mock.Anything, mock.Anything)
	// The wire instance_uid must never be used as the store key.
	mockService.AssertNotCalled(t, "GetAgent", mock.Anything, instanceId)
	mockService.AssertExpectations(t)
}
