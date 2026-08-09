// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// connection_registry_test.go — HA S3b (ADR 0035) wiring: the OpAMP server
// records ownership on connect and drops it on clean disconnect, and the S3a
// reconcile loop heartbeats ownership for every agent connected to THIS
// instance. Both write through the connectionRegistry seam, faked here.

// fakeRegistry records the connection-registry writes so tests can assert the
// connect/disconnect/heartbeat wiring drove them with the local instance id.
type fakeRegistry struct {
	mu      sync.Mutex
	upserts map[uuid.UUID]string // agent fleet id -> instance id written
	upsertN int
	deletes map[uuid.UUID]int
	deleteN int
}

func (f *fakeRegistry) UpsertConnectionOwner(_ context.Context, agentID uuid.UUID, instanceID string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upserts == nil {
		f.upserts = map[uuid.UUID]string{}
	}
	f.upserts[agentID] = instanceID
	f.upsertN++
	return nil
}

func (f *fakeRegistry) DeleteConnectionOwner(_ context.Context, agentID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deletes == nil {
		f.deletes = map[uuid.UUID]int{}
	}
	f.deletes[agentID]++
	f.deleteN++
	return nil
}

func (f *fakeRegistry) instanceFor(id uuid.UUID) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.upserts[id]
	return v, ok
}

func (f *fakeRegistry) deleted(id uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes[id] > 0
}

// TestServer_ConnectRecordsOwner: the connect path upserts ownership keyed by
// the agent's fleet id, tagged with THIS instance's id.
func TestServer_ConnectRecordsOwner(t *testing.T) {
	reg := &fakeRegistry{}
	s := &Server{logger: zap.NewNop()}
	s.SetConnectionRegistry(reg, "instance-1")

	fleetID := uuid.New()
	s.recordConnectionOwner(context.Background(), fleetID)

	got, ok := reg.instanceFor(fleetID)
	assert.True(t, ok, "connect must upsert the ownership row")
	assert.Equal(t, "instance-1", got, "row must carry this instance's id")
}

// TestServer_ConnectNilRegistryNoOp: with no registry wired (default /
// single-instance-before-S3b), the connect path is a no-op and never panics.
func TestServer_ConnectNilRegistryNoOp(t *testing.T) {
	s := &Server{logger: zap.NewNop()}
	assert.NotPanics(t, func() {
		s.recordConnectionOwner(context.Background(), uuid.New())
	})
}

// TestServer_DisconnectRemovesOwner: a clean disconnect deletes the row.
func TestServer_DisconnectRemovesOwner(t *testing.T) {
	reg := &fakeRegistry{}
	s := &Server{logger: zap.NewNop()}
	s.SetConnectionRegistry(reg, "instance-1")

	fleetID := uuid.New()
	s.removeConnectionOwner(fleetID)
	assert.True(t, reg.deleted(fleetID), "clean disconnect must delete the ownership row")

	// Nil registry is a no-op.
	s2 := &Server{logger: zap.NewNop()}
	assert.NotPanics(t, func() { s2.removeConnectionOwner(uuid.New()) })
}

// TestReconcileOnce_HeartbeatsAllConnectedAgents: each reconcile tick refreshes
// ownership (heartbeat) for EVERY agent connected to this instance — including
// one without remote-config capability, since ownership is about the socket,
// not the delivery decision.
func TestReconcileOnce_HeartbeatsAllConnectedAgents(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)

	// Two remote-config-capable agents + one without the capability.
	aID := uuid.New()
	bID := uuid.New()
	noCapID := uuid.New()
	agents.agentsById[aID] = newReconcileAgent(aID, wireConfigHash("cfg")) // already delivered => no push
	agents.agentsById[bID] = newReconcileAgent(bID, wireConfigHash("cfg"))
	noCap := NewAgent(noCapID, &mockConnection{})
	noCap.Status = &protobufs.AgentToServer{Capabilities: 0}
	agents.agentsById[noCapID] = noCap

	reg := &fakeRegistry{}
	r := &Reconciler{
		agents:     agents,
		resolve:    staticResolver("cfg", true),
		sender:     &fakePusher{},
		interval:   4 * time.Millisecond, // keep per-agent jitter sub-millisecond
		registry:   reg,
		instanceID: "instance-1",
		logger:     logger,
	}

	r.reconcileOnce(context.Background())

	assert.Equal(t, 3, reg.upsertN, "every connected agent is heartbeated, capability or not")
	for _, id := range []uuid.UUID{aID, bID, noCapID} {
		got, ok := reg.instanceFor(id)
		assert.True(t, ok, "connected agent %s must be heartbeated", id)
		assert.Equal(t, "instance-1", got)
	}
}

// TestReconcileOnce_NilRegistrySkipsHeartbeat: a reconcile loop without a
// registry (delivery-only, pre-S3b behavior) performs no heartbeat writes.
func TestReconcileOnce_NilRegistrySkipsHeartbeat(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)
	agents.agentsById[uuid.New()] = newReconcileAgent(uuid.New(), []byte("stale"))

	r := &Reconciler{
		agents:   agents,
		resolve:  staticResolver("cfg", true),
		sender:   &fakePusher{},
		interval: 4 * time.Millisecond,
		logger:   logger,
	}
	assert.NotPanics(t, func() { r.reconcileOnce(context.Background()) })
}
