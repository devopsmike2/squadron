// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// --- HA S3d convergence-gating test doubles --------------------------------

// convRolloutSvc records every persisted rollout so a test can read the
// terminal state. Persist never conflicts (Get is therefore never consulted).
type convRolloutSvc struct {
	services.RolloutService
	persisted []services.Rollout
}

func (f *convRolloutSvc) Persist(_ context.Context, r *services.Rollout) error {
	f.persisted = append(f.persisted, *r)
	return nil
}

// convAgentSvc implements the AgentService surface the convergence gate + the
// desired-state write use: ListAgents, GetLatestConfigForAgent, CreateConfig.
// Writing an agent-scoped config updates the per-agent latest so idempotency +
// versioning behave like the real store; convergence is simulated by mutating an
// agent's EffectiveConfig.
type convAgentSvc struct {
	services.AgentService
	agents  []*services.Agent
	latest  map[uuid.UUID]*services.Config
	created []*services.Config
}

func newConvAgentSvc(agents []*services.Agent) *convAgentSvc {
	return &convAgentSvc{agents: agents, latest: map[uuid.UUID]*services.Config{}}
}

func (s *convAgentSvc) ListAgents(context.Context) ([]*services.Agent, error) {
	return s.agents, nil
}

func (s *convAgentSvc) GetLatestConfigForAgent(_ context.Context, id uuid.UUID) (*services.Config, error) {
	return s.latest[id], nil
}

func (s *convAgentSvc) CreateConfig(_ context.Context, c *services.Config) error {
	s.created = append(s.created, c)
	if c.AgentID != nil {
		s.latest[*c.AgentID] = c
	}
	return nil
}

// createdForAgent returns the agent-scoped configs the engine wrote for id.
func (s *convAgentSvc) createdForAgent(id uuid.UUID) []*services.Config {
	var out []*services.Config
	for _, c := range s.created {
		if c.AgentID != nil && *c.AgentID == id {
			out = append(out, c)
		}
	}
	return out
}

// mapConfigStore resolves configs by id (target + previous both live here).
type mapConfigStore struct {
	m map[string]*applicationstore.Config
}

func (s *mapConfigStore) GetConfig(_ context.Context, id string) (*applicationstore.Config, error) {
	return s.m[id], nil
}

// fakeConnRegistry is the coverage seam under test.
type fakeConnRegistry struct {
	owners []*applicationstore.ConnectionOwner
	err    error
}

func (f *fakeConnRegistry) ListConnectionOwners(context.Context) ([]*applicationstore.ConnectionOwner, error) {
	return f.owners, f.err
}

// convEngine wires an engine with desired-state writes ON for the convergence
// tests. target/previous content are registered by id in the config store.
func convEngine(t *testing.T, agents []*services.Agent, cfgs map[string]*applicationstore.Config) (*Engine, *convAgentSvc, *convRolloutSvc) {
	t.Helper()
	as := newConvAgentSvc(agents)
	rs := &convRolloutSvc{}
	e := &Engine{
		rolloutService:     rs,
		agentService:       as,
		configStore:        &mapConfigStore{m: cfgs},
		commander:          newRecordingCommander(),
		logger:             zap.NewNop(),
		writeDesiredState:  true,
		coverageGrace:      defaultCoverageGrace,
		convergenceTimeout: defaultConvergenceTimeout,
	}
	return e, as, rs
}

func pastStart(d time.Duration) *time.Time {
	t := time.Now().Add(-d).UTC()
	return &t
}

// --- pure helpers ----------------------------------------------------------

func TestConvergenceRequired(t *testing.T) {
	assert.Equal(t, 4, convergenceRequired(4, 0), "unset percent → all")
	assert.Equal(t, 4, convergenceRequired(4, 100), "100 → all")
	assert.Equal(t, 4, convergenceRequired(4, 150), ">100 → all")
	assert.Equal(t, 2, convergenceRequired(4, 50), "50% of 4 = 2")
	assert.Equal(t, 3, convergenceRequired(4, 60), "60% of 4 = 2.4 → ceil 3")
	assert.Equal(t, 1, convergenceRequired(4, 1), "tiny percent floors to 1")
	assert.Equal(t, 0, convergenceRequired(0, 50), "empty canary → 0")
}

func TestAgentConvergedToHash(t *testing.T) {
	targetHash := confignorm.Hash("TARGET")
	assert.True(t, agentConvergedToHash(&services.Agent{EffectiveConfig: "TARGET"}, targetHash))
	// confignorm normalizes surrounding whitespace / CRLF.
	assert.True(t, agentConvergedToHash(&services.Agent{EffectiveConfig: "  TARGET\n"}, targetHash))
	assert.False(t, agentConvergedToHash(&services.Agent{EffectiveConfig: "OTHER"}, targetHash))
	assert.False(t, agentConvergedToHash(&services.Agent{EffectiveConfig: ""}, targetHash), "no effective config never matches")
	assert.False(t, agentConvergedToHash(nil, targetHash))
}

// --- convergence gate ------------------------------------------------------

// TestStageAdvancesOnlyOnConvergence is the headline S3d proof: dwell elapsed +
// push fired is NOT enough — the stage advances only once the canary agents
// REPORT the target as their effective config.
func TestStageAdvancesOnlyOnConvergence(t *testing.T) {
	agents := makeAgents(2, "group-a")
	cfgs := map[string]*applicationstore.Config{
		"cfg-target": {ID: "cfg-target", Content: "TARGET"},
	}
	e, _, _ := convEngine(t, agents, cfgs)

	r := &services.Rollout{
		ID:             "ro-1",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		State:          services.RolloutStateInProgress,
		CurrentStage:   0,
		StageStartedAt: pastStart(time.Second), // dwell (0) elapsed
		Stages:         []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0}},
	}

	// Phase A: agents have NOT applied the target yet → must NOT advance.
	e.advanceOrCheck(context.Background(), r)
	require.Equal(t, services.RolloutStateInProgress, r.State, "must not advance before convergence")
	require.Equal(t, 0, r.CurrentStage)

	// Phase B: both agents report the target as effective → converged → finish.
	agents[0].EffectiveConfig = "TARGET"
	agents[1].EffectiveConfig = "TARGET"
	e.advanceOrCheck(context.Background(), r)
	require.Equal(t, services.RolloutStateSucceeded, r.State, "must finish once the canary set converges")
}

// TestStageDoesNotAdvancePartialConvergence pins the threshold across stages: a
// multi-stage rollout holds at stage 0 until its canary converges, then moves on.
func TestStageDoesNotAdvancePartialConvergence(t *testing.T) {
	agents := makeAgents(4, "group-a") // sorted by id
	cfgs := map[string]*applicationstore.Config{
		"cfg-target": {ID: "cfg-target", Content: "TARGET"},
	}
	e, _, _ := convEngine(t, agents, cfgs)

	r := &services.Rollout{
		ID:             "ro-2",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		State:          services.RolloutStateInProgress,
		CurrentStage:   0,
		StageStartedAt: pastStart(time.Second),
		Stages: []services.RolloutStage{
			{Mode: services.RolloutStageModePercent, Percentage: 50, DwellSeconds: 0},  // agents[0],[1]
			{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0}, // all
		},
	}

	// Only ONE of the two stage-0 canary agents converged → hold at stage 0.
	agents[0].EffectiveConfig = "TARGET"
	e.advanceOrCheck(context.Background(), r)
	require.Equal(t, 0, r.CurrentStage, "stage 0 must not advance while a canary agent is unconverged")

	// Both stage-0 agents converged → advance to stage 1.
	agents[1].EffectiveConfig = "TARGET"
	e.advanceOrCheck(context.Background(), r)
	require.Equal(t, services.RolloutStateInProgress, r.State)
	require.Equal(t, 1, r.CurrentStage, "stage 0 converged → advance to stage 1")
}

// TestConvergenceThresholdPercentTolerates proves a sub-100 ConvergencePercent
// lets a stage advance without every canary agent converging.
func TestConvergenceThresholdPercentTolerates(t *testing.T) {
	agents := makeAgents(4, "group-a")
	cfgs := map[string]*applicationstore.Config{"cfg-target": {ID: "cfg-target", Content: "TARGET"}}
	e, _, _ := convEngine(t, agents, cfgs)

	r := &services.Rollout{
		ID:             "ro-3",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		State:          services.RolloutStateInProgress,
		CurrentStage:   0,
		StageStartedAt: pastStart(time.Second),
		Stages:         []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0, ConvergencePercent: 50}},
	}

	// 2 of 4 converged = 50% → meets the configured threshold → finish.
	agents[0].EffectiveConfig = "TARGET"
	agents[1].EffectiveConfig = "TARGET"
	e.advanceOrCheck(context.Background(), r)
	require.Equal(t, services.RolloutStateSucceeded, r.State, "50% threshold met with 2/4 converged")
}

// TestConvergenceTimeoutAborts proves the engine does not wait forever: past the
// dwell + convergence-timeout budget, an unconverged stage aborts (→ rollback).
func TestConvergenceTimeoutAborts(t *testing.T) {
	agents := makeAgents(2, "group-a")
	cfgs := map[string]*applicationstore.Config{"cfg-target": {ID: "cfg-target", Content: "TARGET"}}
	e, _, _ := convEngine(t, agents, cfgs)
	e.convergenceTimeout = time.Millisecond // tiny budget

	r := &services.Rollout{
		ID:             "ro-4",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		State:          services.RolloutStateInProgress,
		CurrentStage:   0,
		StageStartedAt: pastStart(time.Minute), // well past dwell + timeout
		Stages:         []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0}},
	}

	// Agents never converge (no effective config reported).
	e.advanceOrCheck(context.Background(), r)
	require.Equal(t, services.RolloutStateAborted, r.State, "unconverged stage past its budget must abort")
	require.Contains(t, r.AbortReason, "convergence timeout")
}

// TestAbortCriteriaStillFireUnderConvergenceGate confirms abort criteria take
// precedence over (and are evaluated before) the new convergence gate: a drift
// breach aborts even though the stage would otherwise be waiting to converge.
func TestAbortCriteriaStillFireUnderConvergenceGate(t *testing.T) {
	agents := makeAgents(2, "group-a")
	agents[0].DriftStatus = services.ConfigDriftStatusDrifted
	agents[1].DriftStatus = services.ConfigDriftStatusDrifted
	cfgs := map[string]*applicationstore.Config{"cfg-target": {ID: "cfg-target", Content: "TARGET"}}
	e, _, _ := convEngine(t, agents, cfgs)

	r := &services.Rollout{
		ID:             "ro-5",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		State:          services.RolloutStateInProgress,
		CurrentStage:   0,
		StageStartedAt: pastStart(time.Second),
		Stages:         []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0}},
		AbortCriteria:  services.RolloutAbortCriteria{MaxDriftedAgents: 0},
	}

	e.advanceOrCheck(context.Background(), r)
	require.Equal(t, services.RolloutStateAborted, r.State)
	require.Contains(t, r.AbortReason, "drifted")
}

// --- desired-state writes + lifecycle --------------------------------------

// TestApplyStageWritesDesiredState proves applyStage persists a per-agent
// agent-scoped config assignment pointing at the target content (so the S3a
// reconcile loop on the owning instance delivers it), records the pushed-set,
// and is idempotent across a percent-superset re-apply.
func TestApplyStageWritesDesiredState(t *testing.T) {
	agents := makeAgents(4, "group-a")
	cfgs := map[string]*applicationstore.Config{"cfg-target": {ID: "cfg-target", Content: "TARGET"}}
	e, as, _ := convEngine(t, agents, cfgs)

	r := &services.Rollout{
		ID:             "ro-6",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		Stages: []services.RolloutStage{
			{Mode: services.RolloutStageModePercent, Percentage: 50, DwellSeconds: 0},
			{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
	}

	_, err := e.applyStage(context.Background(), r, 0)
	require.NoError(t, err)
	// Stage 0 (50%) assigns the first two agents the target as desired state.
	for _, a := range agents[:2] {
		got := as.createdForAgent(a.ID)
		require.Len(t, got, 1, "one agent-scoped assignment per canary agent")
		assert.Equal(t, "TARGET", got[0].Content)
		require.NotNil(t, got[0].AgentID)
		assert.Equal(t, a.ID, *got[0].AgentID)
	}
	assert.ElementsMatch(t, []string{agents[0].ID.String(), agents[1].ID.String()}, r.PushedAgentIDs)

	// Stage 1 (100%): delta is {2,3}; agents 0,1 already desire TARGET → NOT
	// re-written (idempotent), agents 2,3 get their assignment.
	_, err = e.applyStage(context.Background(), r, 1)
	require.NoError(t, err)
	assert.Len(t, as.createdForAgent(agents[0].ID), 1, "already-assigned agent not re-written")
	assert.Len(t, as.createdForAgent(agents[1].ID), 1, "already-assigned agent not re-written")
	assert.Len(t, as.createdForAgent(agents[2].ID), 1)
	assert.Len(t, as.createdForAgent(agents[3].ID), 1)
	assert.Len(t, r.PushedAgentIDs, 4, "all four now in the pushed-set")
}

// TestRollbackRevertsViaDesiredState proves rollback writes the PREVIOUS config
// as the per-agent desired state for the pushed set (superseding the target
// assignment), so reconcilers deliver the revert.
func TestRollbackRevertsViaDesiredState(t *testing.T) {
	agents := makeAgents(2, "group-a")
	cfgs := map[string]*applicationstore.Config{
		"cfg-target": {ID: "cfg-target", Content: "TARGET"},
		"cfg-prev":   {ID: "cfg-prev", Content: "PREV"},
	}
	e, as, _ := convEngine(t, agents, cfgs)

	// Simulate the rollout having assigned TARGET during its stages.
	for _, a := range agents {
		agentID := a.ID
		as.latest[a.ID] = &services.Config{ID: uuid.NewString(), AgentID: &agentID, Content: "TARGET", ConfigHash: confignorm.Hash("TARGET"), Version: 1}
	}

	r := &services.Rollout{
		ID:               "ro-7",
		GroupID:          "group-a",
		TargetConfigID:   "cfg-target",
		PreviousConfigID: "cfg-prev",
		State:            services.RolloutStateAborted,
		AbortReason:      "test",
		CurrentStage:     0,
		Stages:           []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0}},
		PushedAgentIDs:   []string{agents[0].ID.String(), agents[1].ID.String()},
	}

	e.rollback(context.Background(), r)

	require.Equal(t, services.RolloutStateRolledBack, r.State)
	for _, a := range agents {
		// The agent's newest desired-state assignment must now be PREV.
		latest := as.latest[a.ID]
		require.NotNil(t, latest)
		assert.Equal(t, "PREV", latest.Content, "rollback supersedes the target assignment with the previous config")
		assert.Equal(t, 2, latest.Version, "revert is a new version superseding the target assignment")
	}
}

// TestWriteDesiredStateIdempotentAndSupersedes pins the lifecycle primitive: a
// repeat write of the same content is a no-op (no version churn); a write of new
// content supersedes with version+1.
func TestWriteDesiredStateIdempotentAndSupersedes(t *testing.T) {
	agents := makeAgents(1, "group-a")
	e, as, _ := convEngine(t, agents, map[string]*applicationstore.Config{})
	r := &services.Rollout{ID: "ro-8", GroupID: "group-a"}

	assigned := e.writeDesiredStateForAgents(context.Background(), r, agents, "V1")
	require.Equal(t, []uuid.UUID{agents[0].ID}, assigned)
	require.Len(t, as.created, 1)
	assert.Equal(t, 1, as.created[0].Version)

	// Same content again → idempotent, still assigned, no new row.
	assigned = e.writeDesiredStateForAgents(context.Background(), r, agents, "V1")
	require.Equal(t, []uuid.UUID{agents[0].ID}, assigned)
	require.Len(t, as.created, 1, "identical content must not create a new config row")

	// New content → supersede with version+1.
	assigned = e.writeDesiredStateForAgents(context.Background(), r, agents, "V2")
	require.Equal(t, []uuid.UUID{agents[0].ID}, assigned)
	require.Len(t, as.created, 2)
	assert.Equal(t, 2, as.created[1].Version)
	assert.Equal(t, "V2", as.created[1].Content)
}

// --- coverage --------------------------------------------------------------

func TestUncoveredCanaryCount(t *testing.T) {
	agents := makeAgents(3, "group-a")
	now := time.Now()

	t.Run("nil registry fails open (0)", func(t *testing.T) {
		e := &Engine{logger: zap.NewNop()}
		assert.Equal(t, 0, e.uncoveredCanaryCount(context.Background(), agents))
	})

	t.Run("counts agents with no live owner", func(t *testing.T) {
		reg := &fakeConnRegistry{owners: []*applicationstore.ConnectionOwner{
			{AgentID: agents[0].ID, LastHeartbeatAt: now},                        // live
			{AgentID: agents[1].ID, LastHeartbeatAt: now.Add(-10 * time.Minute)}, // stale
			// agents[2] absent entirely
		}}
		e := &Engine{logger: zap.NewNop(), connRegistry: reg, coverageGrace: defaultCoverageGrace}
		assert.Equal(t, 2, e.uncoveredCanaryCount(context.Background(), agents),
			"stale + absent agents are uncovered; the fresh one is covered")
	})

	t.Run("registry error fails open (0)", func(t *testing.T) {
		reg := &fakeConnRegistry{err: assertErr{}}
		e := &Engine{logger: zap.NewNop(), connRegistry: reg}
		assert.Equal(t, 0, e.uncoveredCanaryCount(context.Background(), agents))
	})
}

type assertErr struct{}

func (assertErr) Error() string { return "boom" }

// --- single-instance end-to-end -------------------------------------------

// TestSingleInstanceEndToEndRollout drives the full lifecycle through the same
// entry point the tick loop uses (process): start → converge stage 0 → advance →
// converge stage 1 → succeed. It proves the convergence-gated engine still
// completes a rollout on one instance (leader owns all agents; its reconcile
// would deliver; convergence is observed locally).
func TestSingleInstanceEndToEndRollout(t *testing.T) {
	agents := makeAgents(2, "group-a")
	cfgs := map[string]*applicationstore.Config{"cfg-target": {ID: "cfg-target", Content: "TARGET"}}
	e, as, _ := convEngine(t, agents, cfgs)

	r := &services.Rollout{
		ID:             "ro-e2e",
		GroupID:        "group-a",
		TargetConfigID: "cfg-target",
		State:          services.RolloutStatePending,
		Stages: []services.RolloutStage{
			{Mode: services.RolloutStageModePercent, Percentage: 50, DwellSeconds: 0},  // agents[0]
			{Mode: services.RolloutStageModePercent, Percentage: 100, DwellSeconds: 0}, // both
		},
	}
	ctx := context.Background()

	// Tick 1: pending → in_progress, stage 0 applied (desired state written).
	e.process(ctx, r)
	require.Equal(t, services.RolloutStateInProgress, r.State)
	require.Equal(t, 0, r.CurrentStage)
	require.NotEmpty(t, as.createdForAgent(agents[0].ID), "stage 0 wrote desired state for its canary")

	// Tick 2: stage 0 not converged yet → hold.
	e.process(ctx, r)
	require.Equal(t, 0, r.CurrentStage, "no advance before the agent applies the target")

	// The owning instance delivered; the agent reports the target as effective.
	agents[0].EffectiveConfig = "TARGET"

	// Tick 3: stage 0 converged → advance to stage 1 (applies to both agents).
	e.process(ctx, r)
	require.Equal(t, 1, r.CurrentStage)
	require.NotEmpty(t, as.createdForAgent(agents[1].ID), "stage 1 wrote desired state for the newly-added agent")

	// Tick 4: stage 1 not fully converged (agent[1] hasn't applied) → hold.
	e.process(ctx, r)
	require.Equal(t, services.RolloutStateInProgress, r.State)
	require.Equal(t, 1, r.CurrentStage)

	// agent[1] applies too → full convergence.
	agents[1].EffectiveConfig = "TARGET"

	// Tick 5: last stage converged → succeed.
	e.process(ctx, r)
	require.Equal(t, services.RolloutStateSucceeded, r.State, "single-instance rollout completes end-to-end")
}
