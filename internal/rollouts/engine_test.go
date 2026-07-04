// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"sort"
	"testing"
	"time"

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

func TestEngine_CumulativePushedAgents_IncludesEarlierStageAgentsDroppedFromSelector(t *testing.T) {
	// ADR 0007 — the label-mode stranding bug. Three agents in group-a:
	// the current stage's selector (role=canary) matches only a1 and a2, but
	// a0 was pushed the new config in an earlier stage (role was "canary"
	// then, re-labeled to "old" since). The cumulative pushed-set must still
	// include a0 so rollback restores it and the health-gate watches it —
	// even though a0 no longer matches the current selector.
	agents := makeAgents(3, "group-a")
	agents[0].Labels = map[string]string{"role": "old"}    // dropped out of the current selector
	agents[1].Labels = map[string]string{"role": "canary"} // current cohort
	agents[2].Labels = map[string]string{"role": "canary"} // current cohort
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"role": "canary"},
		}},
		// a0 + a1 were pushed in earlier stages; a0 has since dropped out of
		// the selector.
		PushedAgentIDs: []string{agents[0].ID.String(), agents[1].ID.String()},
	}

	got, err := e.cumulativePushedAgents(context.Background(), r)
	require.NoError(t, err)
	ids := map[uuid.UUID]bool{}
	for _, a := range got {
		ids[a.ID] = true
	}
	assert.True(t, ids[agents[0].ID], "earlier-stage agent that dropped out of the selector must still be covered")
	assert.True(t, ids[agents[1].ID], "current-cohort agent present")
	assert.True(t, ids[agents[2].ID], "current-cohort agent present")
	assert.Len(t, got, 3, "union of current cohort {a1,a2} and pushed-set {a0,a1}")

	// Sanity: the current-stage-only selection would strand a0.
	current, err := e.canaryAgents(context.Background(), r)
	require.NoError(t, err)
	assert.Len(t, current, 2, "current-stage cohort misses the dropped-out agent (the pre-ADR-0007 bug)")
}

func TestEngine_CumulativePushedAgents_EmptySetFallsBackToCurrentCohort(t *testing.T) {
	// In-flight / pre-v0.90 rollouts have an empty pushed-set. The cumulative
	// resolution must then degrade exactly to the current-stage cohort — no
	// coverage regression relative to the old behavior.
	agents := makeAgents(3, "group-a")
	agents[0].Labels = map[string]string{"role": "old"}
	agents[1].Labels = map[string]string{"role": "canary"}
	agents[2].Labels = map[string]string{"role": "canary"}
	stub := &stubAgentService{agents: agents}
	e := &Engine{agentService: stub, logger: zap.NewNop()}

	r := &services.Rollout{
		GroupID: "group-a",
		Stages: []services.RolloutStage{{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"role": "canary"},
		}},
		PushedAgentIDs: nil,
	}
	got, err := e.cumulativePushedAgents(context.Background(), r)
	require.NoError(t, err)
	assert.Len(t, got, 2, "empty pushed-set degrades to the current-stage cohort")
}

func TestUnionPushedAgentIDs(t *testing.T) {
	a := uuid.UUID{1}
	b := uuid.UUID{2}
	c := uuid.UUID{3}
	// Existing has a,b; adding b (dup) + c. Result should be a,b,c sorted, no dup.
	out := unionPushedAgentIDs([]string{a.String(), b.String()}, []uuid.UUID{b, c})
	assert.Equal(t, []string{a.String(), b.String(), c.String()}, out)
	// Sorted + deduped regardless of input order.
	out2 := unionPushedAgentIDs(nil, []uuid.UUID{c, a, a})
	assert.Equal(t, []string{a.String(), c.String()}, out2)
}

// windowedTelemetry is a faithful test double for TelemetryReader: it holds
// a set of error-event timestamps and computes errors/min over [since, now)
// exactly the way the real DuckDB-backed adapter does (COUNT in window /
// window minutes). It records the `since` it was handed so tests can assert
// how the engine sizes the evaluation window. See ADR 0008.
type windowedTelemetry struct {
	errorTimes []time.Time
	lastSince  time.Time
}

func (w *windowedTelemetry) CanaryErrorLogsPerMinute(_ context.Context, _ []uuid.UUID, since time.Time) (float64, error) {
	w.lastSince = since
	minutes := time.Since(since).Minutes()
	if minutes < 0.05 {
		return 0, nil
	}
	n := 0
	for _, t := range w.errorTimes {
		if !t.Before(since) {
			n++
		}
	}
	return float64(n) / minutes, nil
}

func TestEngine_EvaluateAbortCriteria_ErrorRateTrailingWindow_CatchesLateBurst(t *testing.T) {
	// A stage that has dwelled 30 minutes with a burst of 100 ERROR logs
	// concentrated in the final minute. Whole-stage average dilutes it to
	// ~3.3/min (under a 10/min threshold) — the bug. A 120s trailing window
	// sees 100 errors over 2 minutes = 50/min and aborts — the fix (ADR 0008).
	ctx := context.Background()
	now := time.Now()
	stageStart := now.Add(-30 * time.Minute)

	errs := make([]time.Time, 100)
	for i := range errs {
		errs[i] = now.Add(-30 * time.Second) // all in the last minute
	}
	tel := &windowedTelemetry{errorTimes: errs}

	stub := &stubAgentService{agents: makeAgents(2, "group-a")}
	e := &Engine{agentService: stub, logger: zap.NewNop(), telemetry: tel}

	base := services.Rollout{
		GroupID:        "group-a",
		Stages:         []services.RolloutStage{{Percentage: 100}},
		StageStartedAt: &stageStart,
	}

	// Legacy whole-stage average (window 0): burst is diluted, no abort.
	legacy := base
	legacy.AbortCriteria = services.RolloutAbortCriteria{MaxErrorLogsPerMinute: 10, ErrorRateWindowSeconds: 0}
	assert.Empty(t, e.evaluateAbortCriteria(ctx, &legacy, legacy.Stages[0]),
		"whole-stage average should dilute the late burst and not abort")
	assert.WithinDuration(t, stageStart, tel.lastSince, 2*time.Second,
		"legacy path must evaluate since stage start")

	// Trailing 120s window: burst dominates, abort fires.
	trailing := base
	trailing.AbortCriteria = services.RolloutAbortCriteria{MaxErrorLogsPerMinute: 10, ErrorRateWindowSeconds: 120}
	reason := e.evaluateAbortCriteria(ctx, &trailing, trailing.Stages[0])
	assert.Contains(t, reason, "error log rate", "trailing window should catch the late burst")
	assert.WithinDuration(t, now.Add(-120*time.Second), tel.lastSince, 2*time.Second,
		"trailing path must evaluate over the last window seconds")
}

func TestEngine_EvaluateAbortCriteria_ErrorRateWindowClampedToStageStart(t *testing.T) {
	// Stage started 60s ago (past the 30s warmup) but the configured window
	// is 120s, so the trailing start would precede stage start. The engine
	// must clamp to stage start and never count pre-stage telemetry.
	ctx := context.Background()
	now := time.Now()
	stageStart := now.Add(-60 * time.Second)

	tel := &windowedTelemetry{errorTimes: nil}
	stub := &stubAgentService{agents: makeAgents(2, "group-a")}
	e := &Engine{agentService: stub, logger: zap.NewNop(), telemetry: tel}

	r := services.Rollout{
		GroupID:        "group-a",
		Stages:         []services.RolloutStage{{Percentage: 100}},
		StageStartedAt: &stageStart,
		AbortCriteria:  services.RolloutAbortCriteria{MaxErrorLogsPerMinute: 10, ErrorRateWindowSeconds: 120},
	}
	_ = e.evaluateAbortCriteria(ctx, &r, r.Stages[0])
	assert.WithinDuration(t, stageStart, tel.lastSince, 2*time.Second,
		"window wider than stage age must clamp to stage start")
}
