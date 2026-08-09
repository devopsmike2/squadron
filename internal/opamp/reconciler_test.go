// Copyright (c) 2024 Squadron Contributors
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

// fakePusher records the config pushes the reconcile loop makes so tests can
// assert delivery vs no-op. It is the delivery seam (configPusher) faked.
type fakePusher struct {
	mu   sync.Mutex
	sent map[uuid.UUID]string // agent fleet id -> content pushed
	err  error
}

func (f *fakePusher) SendConfigToAgentWithContext(_ context.Context, agentID uuid.UUID, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent == nil {
		f.sent = map[uuid.UUID]string{}
	}
	f.sent[agentID] = content
	return f.err
}

func (f *fakePusher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakePusher) contentFor(id uuid.UUID) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.sent[id]
	return c, ok
}

// newReconcileAgent builds a connected agent with the remote-config capability
// and, optionally, a previously-delivered remote config hash. lastHash nil means
// the agent has never reported a RemoteConfigStatus (nothing delivered yet).
func newReconcileAgent(id uuid.UUID, lastHash []byte) *Agent {
	a := NewAgent(id, &mockConnection{})
	a.Status = &protobufs.AgentToServer{
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
	}
	if lastHash != nil {
		a.Status.RemoteConfigStatus = &protobufs.RemoteConfigStatus{
			LastRemoteConfigHash: lastHash,
		}
	}
	return a
}

func staticResolver(content string, found bool) desiredConfigResolver {
	return func(_ context.Context, _ *Agent) (string, bool) {
		return content, found
	}
}

// TestReconcileAgent_DeliversWhenNotDelivered: an agent whose
// LastRemoteConfigHash != the desired config's hash gets pushed (DELIVERY).
func TestReconcileAgent_DeliversWhenNotDelivered(t *testing.T) {
	const desired = "desired-config-v2"
	agentID := uuid.New()
	agent := newReconcileAgent(agentID, []byte("stale-or-different-hash"))

	pusher := &fakePusher{}
	r := &Reconciler{
		resolve: staticResolver(desired, true),
		sender:  pusher,
		logger:  zap.NewNop(),
	}

	r.reconcileAgent(context.Background(), agent)

	assert.Equal(t, 1, pusher.count(), "should push exactly once to deliver the desired config")
	got, ok := pusher.contentFor(agent.storeID())
	assert.True(t, ok, "push should target the agent's fleet id (storeID)")
	assert.Equal(t, desired, got, "pushed content should be the resolved desired config")
}

// TestReconcileAgent_NoOpWhenAlreadyDelivered: an agent whose
// LastRemoteConfigHash already equals the desired hash is NOT pushed
// (idempotent no-op — the single-instance steady-state guarantee).
func TestReconcileAgent_NoOpWhenAlreadyDelivered(t *testing.T) {
	const desired = "desired-config-v2"
	agentID := uuid.New()
	// Delivered hash == the wire hash of the desired content.
	agent := newReconcileAgent(agentID, wireConfigHash(desired))

	pusher := &fakePusher{}
	r := &Reconciler{
		resolve: staticResolver(desired, true),
		sender:  pusher,
		logger:  zap.NewNop(),
	}

	r.reconcileAgent(context.Background(), agent)

	assert.Equal(t, 0, pusher.count(), "already-delivered agent must produce zero pushes")
}

// TestReconcileAgent_EffectiveDriftDoesNotTriggerRepush: the delivered hash
// matches the desired config, but the agent's EffectiveConfig differs (applied
// drift). The loop is delivery-scoped, not auto-heal, so it must NOT re-push.
func TestReconcileAgent_EffectiveDriftDoesNotTriggerRepush(t *testing.T) {
	const desired = "desired-config-v2"
	agentID := uuid.New()
	agent := newReconcileAgent(agentID, wireConfigHash(desired)) // delivered == desired
	// Applied signal diverges from delivered/desired — this is drift.
	agent.EffectiveConfig = "totally-different-effective-config"

	pusher := &fakePusher{}
	r := &Reconciler{
		resolve: staticResolver(desired, true),
		sender:  pusher,
		logger:  zap.NewNop(),
	}

	r.reconcileAgent(context.Background(), agent)

	assert.Equal(t, 0, pusher.count(),
		"effective-config drift alone must NOT trigger a re-push (delivery-scoped, not auto-heal)")
}

// TestReconcileAgent_SkipsWhenNoStoredConfig: no agent/group config exists
// (resolver found=false). The loop must never synthesize/deliver a default.
func TestReconcileAgent_SkipsWhenNoStoredConfig(t *testing.T) {
	agentID := uuid.New()
	agent := newReconcileAgent(agentID, nil) // nothing delivered

	pusher := &fakePusher{}
	r := &Reconciler{
		resolve: staticResolver("", false), // no stored config
		sender:  pusher,
		logger:  zap.NewNop(),
	}

	r.reconcileAgent(context.Background(), agent)

	assert.Equal(t, 0, pusher.count(), "no stored config => nothing to deliver (never synthesize)")
}

// TestReconcileAgent_SkipsWithoutRemoteConfigCapability: an agent that does not
// advertise AcceptsRemoteConfig is left alone.
func TestReconcileAgent_SkipsWithoutRemoteConfigCapability(t *testing.T) {
	agentID := uuid.New()
	agent := NewAgent(agentID, &mockConnection{})
	agent.Status = &protobufs.AgentToServer{Capabilities: 0} // no capability

	pusher := &fakePusher{}
	r := &Reconciler{
		resolve: staticResolver("desired", true),
		sender:  pusher,
		logger:  zap.NewNop(),
	}

	r.reconcileAgent(context.Background(), agent)

	assert.Equal(t, 0, pusher.count(), "agent without remote-config capability must not be pushed")
}

// TestReconcileOnce_OnlyThisInstancesConnectedAgents: reconcileOnce enumerates
// GetAllAgentsReadonlyClone — the in-memory registry of agents connected to THIS
// instance — so an agent that is not connected here is never touched, even if it
// has a stored config.
func TestReconcileOnce_OnlyThisInstancesConnectedAgents(t *testing.T) {
	logger := zap.NewNop()
	agents := NewAgents(logger)

	// Two agents connected to THIS instance, both needing delivery.
	aID := uuid.New()
	bID := uuid.New()
	agents.agentsById[aID] = newReconcileAgent(aID, []byte("stale-a"))
	agents.agentsById[bID] = newReconcileAgent(bID, []byte("stale-b"))

	// An agent that is NOT connected to this instance (not in the registry).
	phantomID := uuid.New()

	pusher := &fakePusher{}
	r := &Reconciler{
		agents:   agents,
		resolve:  staticResolver("cfg", true),
		sender:   pusher,
		interval: 4 * time.Millisecond, // keep per-agent jitter sub-millisecond
		logger:   logger,
	}

	r.reconcileOnce(context.Background())

	assert.Equal(t, 2, pusher.count(), "should touch exactly the two connected agents")
	_, aPushed := pusher.contentFor(aID)
	_, bPushed := pusher.contentFor(bID)
	_, phantomPushed := pusher.contentFor(phantomID)
	assert.True(t, aPushed, "connected agent A should be delivered to")
	assert.True(t, bPushed, "connected agent B should be delivered to")
	assert.False(t, phantomPushed, "an agent not connected to this instance must never be touched")
}
