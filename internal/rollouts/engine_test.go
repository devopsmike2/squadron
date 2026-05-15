// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// stubAgentService is a tiny test double for services.AgentService that
// only implements ListAgents (the one method the engine's canary selection
// actually calls). Everything else panics — if the engine ever reaches for
// it under test, we want to know.
type stubAgentService struct {
	services.AgentService
	agents []*services.Agent
}

func (s *stubAgentService) ListAgents(_ context.Context) ([]*services.Agent, error) {
	return s.agents, nil
}

// makeAgents builds n agents bound to the given group, with deterministic
// UUIDs (i becomes the leading byte) so test assertions don't flake on
// random ordering.
func makeAgents(n int, groupID string) []*services.Agent {
	out := make([]*services.Agent, n)
	for i := 0; i < n; i++ {
		var id uuid.UUID
		id[0] = byte(i + 1) // deterministic per index
		gid := groupID
		out[i] = &services.Agent{
			ID:      id,
			Name:    "agent-" + id.String()[:8],
			GroupID: &gid,
		}
	}
	return out
}

func TestEngine_CanaryAgentsForStage_PercentageMath(t *testing.T) {
	agents := makeAgents(10, "group-a")
	stub := &stubAgentService{agents: agents}

	e := &Engine{
		agentService: stub,
		logger:       zap.NewNop(),
		telemetry:    nil, // not used by this test
	}

	cases := []struct {
		name       string
		percentage int
		want       int
	}{
		{"10% of 10 = 1", 10, 1},
		{"25% of 10 = 3 (ceil)", 25, 3},
		{"50% of 10 = 5", 50, 5},
		{"99% of 10 = 10 (ceil)", 99, 10},
		{"100% of 10 = 10", 100, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &services.Rollout{
				GroupID: "group-a",
				Stages:  []services.RolloutStage{{Percentage: tc.percentage}},
			}
			got, err := e.canaryAgentsForStage(context.Background(), r, 0)
			require.NoError(t, err)
			assert.Len(t, got, tc.want)
		})
	}
}

func TestEngine_CanaryAgentsForStage_DeterministicSupersetProgression(t *testing.T) {
	// 10 agents; stages [10, 50, 100] — stage K+1's canary set must be a
	// superset of stage K's. Crucial property: we never miss re-pushing
	// to an agent because the selection drifted.
	agents := makeAgents(10, "group-a")
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{
			{Percentage: 10},
			{Percentage: 50},
			{Percentage: 100},
		},
	}

	var prev []string
	for i := range r.Stages {
		got, err := e.canaryAgentsForStage(context.Background(), r, i)
		require.NoError(t, err)
		ids := make([]string, len(got))
		for k, a := range got {
			ids[k] = a.ID.String()
		}
		sort.Strings(ids)
		// Every id from the previous stage must still be present here.
		for _, p := range prev {
			assert.Contains(t, ids, p, "stage %d should keep all previous canary agents", i)
		}
		prev = ids
	}
}

func TestEngine_CanaryAgentsForStage_FiltersToGroup(t *testing.T) {
	// Half the agents belong to a different group; the engine must only
	// canary the rollout's target group.
	agents := append(makeAgents(5, "group-a"), makeAgents(5, "group-b")...)
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages:  []services.RolloutStage{{Percentage: 100}},
	}
	got, err := e.canaryAgentsForStage(context.Background(), r, 0)
	require.NoError(t, err)
	require.Len(t, got, 5, "expected only the 5 agents in group-a")
	for _, a := range got {
		require.NotNil(t, a.GroupID)
		assert.Equal(t, "group-a", *a.GroupID)
	}
}

func TestEngine_EvaluateAbortCriteria_TriggersOnDrift(t *testing.T) {
	// Set up two agents in the target group, both in drift. Max drifted
	// allowed is 0, so the criteria must trigger.
	agents := makeAgents(2, "group-a")
	agents[0].DriftStatus = services.ConfigDriftStatusDrifted
	agents[1].DriftStatus = services.ConfigDriftStatusDrifted
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID:       "group-a",
		Stages:        []services.RolloutStage{{Percentage: 100}},
		AbortCriteria: services.RolloutAbortCriteria{MaxDriftedAgents: 0},
	}
	reason := e.evaluateAbortCriteria(context.Background(), r, r.Stages[0])
	assert.NotEmpty(t, reason, "expected an abort reason when canary drift exceeds threshold")
	assert.Contains(t, reason, "drifted")
}

func TestEngine_EvaluateAbortCriteria_NoAbortBelowThreshold(t *testing.T) {
	agents := makeAgents(5, "group-a")
	agents[0].DriftStatus = services.ConfigDriftStatusDrifted
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID:       "group-a",
		Stages:        []services.RolloutStage{{Percentage: 100}},
		AbortCriteria: services.RolloutAbortCriteria{MaxDriftedAgents: 2},
	}
	reason := e.evaluateAbortCriteria(context.Background(), r, r.Stages[0])
	assert.Empty(t, reason, "1 drifted agent under threshold of 2 should not abort")
}
