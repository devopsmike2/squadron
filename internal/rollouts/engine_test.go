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

// makeLabeledAgents builds n agents bound to the given group, applying the
// supplied label map to each. Useful for label-selector tests where the
// caller wants every agent to share a base label set; tests that need
// per-agent diverging labels can mutate the returned slice afterward.
func makeLabeledAgents(n int, groupID string, labels map[string]string) []*services.Agent {
	out := makeAgents(n, groupID)
	for _, a := range out {
		// Defensive copy so two callers sharing a labels map can't see
		// each other's mutations through the same backing map.
		copyLabels := make(map[string]string, len(labels))
		for k, v := range labels {
			copyLabels[k] = v
		}
		a.Labels = copyLabels
	}
	return out
}

func TestEngine_CanaryAgentsForStage_LabelMode_SingleSelector(t *testing.T) {
	// 5 agents in group-a; only 2 have role=canary. The label-mode stage
	// should return exactly those 2.
	agents := makeLabeledAgents(5, "group-a", map[string]string{"role": "primary"})
	agents[1].Labels["role"] = "canary"
	agents[3].Labels["role"] = "canary"
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"role": "canary"},
		}},
	}
	got, err := e.canaryAgentsForStage(context.Background(), r, 0)
	require.NoError(t, err)
	require.Len(t, got, 2, "expected only the 2 agents with role=canary")
	for _, a := range got {
		assert.Equal(t, "canary", a.Labels["role"])
	}
}

func TestEngine_CanaryAgentsForStage_LabelMode_MultiKeyAndSemantics(t *testing.T) {
	// Multiple selector keys must AND together — agents need every
	// key=value pair to match. Verifies we don't accidentally OR them.
	agents := makeLabeledAgents(4, "group-a", map[string]string{"region": "us-east", "tier": "free"})
	agents[0].Labels["tier"] = "pro"       // region=us-east, tier=pro — no match
	agents[1].Labels["region"] = "us-west" // region=us-west, tier=free — no match
	agents[2].Labels["region"] = "us-east" // region=us-east, tier=free — MATCH
	agents[3].Labels["region"] = "us-east"
	agents[3].Labels["tier"] = "free" // MATCH
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"region": "us-east", "tier": "free"},
		}},
	}
	got, err := e.canaryAgentsForStage(context.Background(), r, 0)
	require.NoError(t, err)
	assert.Len(t, got, 2, "expected only agents matching both region=us-east AND tier=free")
}

func TestEngine_CanaryAgentsForStage_LabelMode_NoMatchReturnsEmpty(t *testing.T) {
	// Selector that matches nothing should return an empty slice — not
	// fall through to "everyone" and not error.
	agents := makeLabeledAgents(3, "group-a", map[string]string{"role": "primary"})
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"role": "nonexistent"},
		}},
	}
	got, err := e.canaryAgentsForStage(context.Background(), r, 0)
	require.NoError(t, err)
	assert.Len(t, got, 0)
}

func TestEngine_CanaryAgentsForStage_LabelMode_RespectsGroupFilter(t *testing.T) {
	// Label match must still be scoped to the rollout's target group —
	// an agent with the right labels but in a different group is NOT a
	// canary.
	groupA := makeLabeledAgents(2, "group-a", map[string]string{"role": "canary"})
	groupB := makeLabeledAgents(2, "group-b", map[string]string{"role": "canary"})
	stub := &stubAgentService{agents: append(groupA, groupB...)}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"role": "canary"},
		}},
	}
	got, err := e.canaryAgentsForStage(context.Background(), r, 0)
	require.NoError(t, err)
	require.Len(t, got, 2, "should only return agents in group-a, even though group-b has matching labels")
	for _, a := range got {
		require.NotNil(t, a.GroupID)
		assert.Equal(t, "group-a", *a.GroupID)
	}
}

func TestEngine_CanaryAgentsForStage_EmptyModeFallsBackToPercent(t *testing.T) {
	// Backward compat: stages persisted before v0.6 have Mode="". The
	// engine must treat them as percent mode so old rollouts in-flight
	// at upgrade time keep working.
	agents := makeAgents(10, "group-a")
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:       "", // legacy
			Percentage: 30,
		}},
	}
	got, err := e.canaryAgentsForStage(context.Background(), r, 0)
	require.NoError(t, err)
	assert.Len(t, got, 3, "30%% of 10 agents should be 3 via percent-mode fallback")
}

func TestEngine_CanaryAgentsForStage_LabelMode_DeterministicOrder(t *testing.T) {
	// Repeated label-mode resolutions must return the same set in the
	// same order. The audit log "agent_ids" payload is what operators
	// diff against between ticks — flapping order would make every tick
	// look like a churn event in post-mortems.
	agents := makeLabeledAgents(6, "group-a", map[string]string{"role": "canary"})
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"role": "canary"},
		}},
	}
	first, err := e.canaryAgentsForStage(context.Background(), r, 0)
	require.NoError(t, err)
	second, err := e.canaryAgentsForStage(context.Background(), r, 0)
	require.NoError(t, err)
	require.Equal(t, len(first), len(second))
	for i := range first {
		assert.Equal(t, first[i].ID, second[i].ID, "label-mode selection must be stable across calls")
	}
}

func TestEngine_StageAuditPayload_PercentMode(t *testing.T) {
	r := &services.Rollout{
		ID:      "rollout-1",
		Name:    "ship-v2",
		GroupID: "group-a",
		Stages:  []services.RolloutStage{{Mode: services.RolloutStageModePercent, Percentage: 25}},
	}
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		var id uuid.UUID
		id[0] = byte(i + 1)
		ids = append(ids, id)
	}
	payload := stageAuditPayload(r, 0, ids)
	assert.Equal(t, 0, payload["stage"])
	assert.Equal(t, "percent", payload["mode"])
	assert.Equal(t, 25, payload["percentage"])
	assert.Equal(t, 3, payload["canary_size"])
	assert.Len(t, payload["agent_ids"], 3, "audit payload must include resolved agent ids for post-mortems")
}

func TestEngine_StageAuditPayload_LabelMode(t *testing.T) {
	r := &services.Rollout{
		ID:      "rollout-2",
		Name:    "ship-canary",
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"role": "canary"},
		}},
	}
	payload := stageAuditPayload(r, 0, nil)
	assert.Equal(t, "label", payload["mode"])
	assert.Equal(t, map[string]string{"role": "canary"}, payload["label_selector"])
	assert.Equal(t, 0, payload["canary_size"])
	// percentage key should NOT be present for label mode
	_, hasPct := payload["percentage"]
	assert.False(t, hasPct, "label-mode audit payload should not include percentage")
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
