// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/extension/reconcilecoord"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// fakeCoordinator records every agent id the engine poked so a test can assert
// applyStage emits a poke for exactly the agents whose desired state it wrote.
type fakeCoordinator struct {
	mu    sync.Mutex
	poked []uuid.UUID
}

func (f *fakeCoordinator) PokeReconcile(_ context.Context, agentID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.poked = append(f.poked, agentID)
}

func (f *fakeCoordinator) ids() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uuid.UUID, len(f.poked))
	copy(out, f.poked)
	return out
}

// TestApplyStagePokesReconcileCoordinator proves the HA S3c wiring: applyStage
// pokes the coordinator with exactly the agent ids whose desired state it wrote,
// and re-poking a percent-superset stage pokes only the newly-assigned delta
// (the already-assigned agents are idempotent no-ops in the desired-state write,
// so there is nothing new to reconcile for them).
func TestApplyStagePokesReconcileCoordinator(t *testing.T) {
	agents := makeAgents(4, "group-a")
	cfgs := map[string]*applicationstore.Config{"cfg-target": {ID: "cfg-target", Content: "TARGET"}}
	e, _, _ := convEngine(t, agents, cfgs)
	coord := &fakeCoordinator{}
	e.SetReconcileCoordinator(coord)

	r := &services.Rollout{
		ID:             "ro-s3c",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		Stages: []services.RolloutStage{
			{Mode: services.RolloutStageModePercent, Percentage: 50, DwellSeconds: 0},
			{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
	}

	// Stage 0 (50%) writes desired state for agents[0],[1] → pokes exactly those.
	_, err := e.applyStage(context.Background(), r, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{agents[0].ID, agents[1].ID}, coord.ids(),
		"stage 0 must poke exactly the agents whose desired state it wrote")

	// Stage 1 (100%) delta is {2,3}: agents 0,1 already desire TARGET (idempotent,
	// no new write) so the cumulative poke set becomes all four exactly once each.
	_, err = e.applyStage(context.Background(), r, 1)
	require.NoError(t, err)
	assert.ElementsMatch(t,
		[]uuid.UUID{agents[0].ID, agents[1].ID, agents[2].ID, agents[3].ID},
		coord.ids(),
		"stage 1 adds pokes for the new delta {2,3}; each agent poked exactly once overall")
}

// TestApplyStageWithNoOpCoordinatorSucceeds pins the OSS behavior: with the
// production no-op coordinator wired, applyStage runs the poke path without error
// and still assigns desired state exactly as before — single-instance behavior is
// unchanged (the no-op poke is inert; delivery happens via the retained direct
// push).
func TestApplyStageWithNoOpCoordinatorSucceeds(t *testing.T) {
	agents := makeAgents(2, "group-a")
	cfgs := map[string]*applicationstore.Config{"cfg-target": {ID: "cfg-target", Content: "TARGET"}}
	e, as, _ := convEngine(t, agents, cfgs)
	e.SetReconcileCoordinator(reconcilecoord.NewNoOp())

	r := &services.Rollout{
		ID:             "ro-s3c-noop",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		Stages:         []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0}},
	}

	_, err := e.applyStage(context.Background(), r, 0)
	require.NoError(t, err)
	for _, a := range agents {
		require.Len(t, as.createdForAgent(a.ID), 1, "desired state still written under the no-op coordinator")
	}
	assert.Len(t, r.PushedAgentIDs, 2, "pushed-set unchanged by the no-op poke")
}

// TestApplyStageNilCoordinatorNoPanic pins that a struct-literal engine with no
// coordinator wired (the pre-S3c default) pokes safely — the nil-guard makes it a
// clean no-op rather than a panic.
func TestApplyStageNilCoordinatorNoPanic(t *testing.T) {
	agents := makeAgents(1, "group-a")
	cfgs := map[string]*applicationstore.Config{"cfg-target": {ID: "cfg-target", Content: "TARGET"}}
	e, _, _ := convEngine(t, agents, cfgs) // no SetReconcileCoordinator

	r := &services.Rollout{
		ID:             "ro-s3c-nil",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		Stages:         []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0}},
	}

	_, err := e.applyStage(context.Background(), r, 0)
	require.NoError(t, err, "a nil coordinator must not break applyStage")
}
