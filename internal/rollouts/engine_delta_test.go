// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// --- pure helpers ----------------------------------------------------------

func TestDeltaToPush(t *testing.T) {
	agents := makeAgents(4, "g")
	idStr := func(i int) string { return agents[i].ID.String() }

	t.Run("empty pushed → whole canary", func(t *testing.T) {
		got := deltaToPush(agents, nil)
		assert.Len(t, got, 4)
	})
	t.Run("skips already-pushed, preserves order", func(t *testing.T) {
		got := deltaToPush(agents, []string{idStr(0), idStr(2)})
		require.Len(t, got, 2)
		assert.Equal(t, agents[1].ID, got[0].ID)
		assert.Equal(t, agents[3].ID, got[1].ID)
	})
	t.Run("all pushed → empty", func(t *testing.T) {
		got := deltaToPush(agents, []string{idStr(0), idStr(1), idStr(2), idStr(3)})
		assert.Empty(t, got)
	})
}

func TestSubtractIDs(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	assert.Equal(t, []uuid.UUID{a, c}, subtractIDs([]uuid.UUID{a, b, c}, []uuid.UUID{b}))
	assert.Equal(t, []uuid.UUID{a, b}, subtractIDs([]uuid.UUID{a, b}, nil))
	assert.Empty(t, subtractIDs([]uuid.UUID{a}, []uuid.UUID{a}))
}

// --- test doubles for applyStage -------------------------------------------

type stubConfigStore struct{ cfg *applicationstore.Config }

func (s *stubConfigStore) GetConfig(_ context.Context, _ string) (*applicationstore.Config, error) {
	return s.cfg, nil
}

// recordingCommander records per-agent push attempts and can be told to fail an
// agent's first attempt (to exercise targeted retry) or every attempt.
type recordingCommander struct {
	mu         sync.Mutex
	attempts   map[uuid.UUID]int
	failFirst  map[uuid.UUID]bool
	failAlways map[uuid.UUID]bool
}

func newRecordingCommander() *recordingCommander {
	return &recordingCommander{
		attempts:   map[uuid.UUID]int{},
		failFirst:  map[uuid.UUID]bool{},
		failAlways: map[uuid.UUID]bool{},
	}
}

func (c *recordingCommander) SendConfigToAgent(agentID uuid.UUID, content string) error {
	return c.SendConfigToAgentWithContext(context.Background(), agentID, content)
}

func (c *recordingCommander) SendConfigToAgentWithContext(_ context.Context, agentID uuid.UUID, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[agentID]++
	if c.failAlways[agentID] {
		return fmt.Errorf("push failed (always)")
	}
	if c.failFirst[agentID] && c.attempts[agentID] == 1 {
		return fmt.Errorf("push failed (first attempt)")
	}
	return nil
}

func (c *recordingCommander) count(id uuid.UUID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[id]
}

func newDeltaEngine(agents []*services.Agent, cmd *recordingCommander) *Engine {
	return &Engine{
		agentService:  &stubAgentService{agents: agents},
		configStore:   &stubConfigStore{cfg: &applicationstore.Config{ID: "cfg-1", Content: "body"}},
		commander:     cmd,
		configsTracer: nil, // nil-safe
		logger:        zap.NewNop(),
	}
}

// TestApplyStage_DeltaPush_NoOverPush proves percent-stage supersets deliver
// each config exactly once: stage 2 pushes ONLY the newly-added agents, not the
// full prefix (rollout follow-up A).
func TestApplyStage_DeltaPush_NoOverPush(t *testing.T) {
	agents := makeAgents(4, "group-a") // sorted by id
	cmd := newRecordingCommander()
	e := newDeltaEngine(agents, cmd)

	r := &services.Rollout{
		GroupID:        "group-a",
		TargetConfigID: "cfg-1",
		Stages: []services.RolloutStage{
			{Percentage: 50},  // first 2
			{Percentage: 100}, // all 4
		},
	}

	// Stage 0: 50% → agents[0],[1].
	_, err := e.applyStage(context.Background(), r, 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{agents[0].ID.String(), agents[1].ID.String()}, r.PushedAgentIDs)

	// Stage 1: 100% canary is {0,1,2,3}; delta is {2,3} — 0 and 1 are NOT re-pushed.
	_, err = e.applyStage(context.Background(), r, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, cmd.count(agents[0].ID), "already-delivered agent must not be re-pushed")
	assert.Equal(t, 1, cmd.count(agents[1].ID), "already-delivered agent must not be re-pushed")
	assert.Equal(t, 1, cmd.count(agents[2].ID))
	assert.Equal(t, 1, cmd.count(agents[3].ID))
	assert.Len(t, r.PushedAgentIDs, 4, "all four delivered exactly once")
}

// TestApplyStage_FailedPush_RetriedNextStage proves a failed push stays OUT of
// the cumulative set and is re-included by the next stage's delta (targeted
// retry replacing the old implicit superset re-push).
func TestApplyStage_FailedPush_RetriedNextStage(t *testing.T) {
	agents := makeAgents(4, "group-a")
	cmd := newRecordingCommander()
	cmd.failFirst[agents[1].ID] = true // agent[1] fails its first push, succeeds on retry
	e := newDeltaEngine(agents, cmd)

	r := &services.Rollout{
		GroupID:        "group-a",
		TargetConfigID: "cfg-1",
		Stages: []services.RolloutStage{
			{Percentage: 50},
			{Percentage: 100},
		},
	}

	// Stage 0: push {0,1}; agent[1] fails → only agent[0] recorded delivered.
	_, err := e.applyStage(context.Background(), r, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{agents[0].ID.String()}, r.PushedAgentIDs,
		"failed agent must not be recorded as delivered")

	// Stage 1: delta = {0,1,2,3} \ {0} = {1,2,3} → agent[1] is retried.
	_, err = e.applyStage(context.Background(), r, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, cmd.count(agents[1].ID), "failed agent must be retried by the next stage")
	assert.Len(t, r.PushedAgentIDs, 4, "retry succeeded → all four now delivered")
}

// TestApplyStage_PermanentFailure_StaysOutOfSet proves an agent that never acks
// is never recorded as delivered (so rollback/health-gate don't count it).
func TestApplyStage_PermanentFailure_StaysOutOfSet(t *testing.T) {
	agents := makeAgents(2, "group-a")
	cmd := newRecordingCommander()
	cmd.failAlways[agents[1].ID] = true
	e := newDeltaEngine(agents, cmd)

	r := &services.Rollout{
		GroupID:        "group-a",
		TargetConfigID: "cfg-1",
		Stages:         []services.RolloutStage{{Percentage: 100}},
	}
	_, err := e.applyStage(context.Background(), r, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{agents[0].ID.String()}, r.PushedAgentIDs)
}
