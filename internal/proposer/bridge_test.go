// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeProposer satisfies the Proposer interface with canned results.
type fakeProposer struct {
	enabled bool
	results []*ai.ProposalResult
	errs    []error
	calls   int
	lastCtx ai.CostSpikeContext
}

func (f *fakeProposer) Enabled() bool { return f.enabled }

func (f *fakeProposer) ProposeFromCostSpike(_ context.Context, in ai.CostSpikeContext) (*ai.ProposalResult, error) {
	f.lastCtx = in
	idx := f.calls
	f.calls++
	if idx < len(f.errs) && f.errs[idx] != nil {
		return nil, f.errs[idx]
	}
	if idx < len(f.results) {
		return f.results[idx], nil
	}
	return &ai.ProposalResult{Declined: true, Reason: "no canned result"}, nil
}

// fakeStore satisfies Store with a small in-memory record set.
type fakeStore struct {
	spikes []*types.CostSpikeEvent
	agents map[uuid.UUID]*types.Agent
	cfgs   map[string]*types.Config
	groups map[string]*types.Group
}

func (f *fakeStore) ListCostSpikeEvents(_ context.Context, _ types.CostSpikeFilter) ([]*types.CostSpikeEvent, error) {
	return f.spikes, nil
}

func (f *fakeStore) GetAgent(_ context.Context, id uuid.UUID) (*types.Agent, error) {
	return f.agents[id], nil
}

func (f *fakeStore) GetLatestConfigForGroup(_ context.Context, gid string) (*types.Config, error) {
	return f.cfgs[gid], nil
}

func (f *fakeStore) GetGroup(_ context.Context, id string) (*types.Group, error) {
	return f.groups[id], nil
}

// fakeRollouts satisfies Rollouts and records every Create call.
type fakeRollouts struct {
	inputs []services.RolloutInput
	err    error
	// v0.79 — plan create dispatch path. planSteps records the N
	// steps the bridge handed to CreatePlan; planErr lets tests
	// force a plan create failure independently from the rollout
	// Create path.
	planSteps []services.RolloutInput
	planErr   error
	planID    string
}

func (f *fakeRollouts) Create(_ context.Context, in services.RolloutInput) (*services.Rollout, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.inputs = append(f.inputs, in)
	return &services.Rollout{ID: "rollout-" + in.Name}, nil
}

func (f *fakeRollouts) CreatePlan(_ context.Context, steps []services.RolloutInput) ([]*services.Rollout, string, error) {
	if f.planErr != nil {
		return nil, "", f.planErr
	}
	f.planSteps = append(f.planSteps, steps...)
	planID := f.planID
	if planID == "" {
		planID = "plan-fake"
	}
	out := make([]*services.Rollout, 0, len(steps))
	for i, s := range steps {
		out = append(out, &services.Rollout{
			ID:            fmt.Sprintf("rollout-%s-step-%d", planID, i),
			Name:          s.Name,
			GroupID:       s.GroupID,
			PlanID:        planID,
			PlanStepIndex: i,
		})
	}
	return out, planID, nil
}

// helper to build a baseline spike with two agents both in the
// same group. Test cases mutate the result as needed.
func baselineFixture() (*fakeStore, *types.CostSpikeEvent) {
	gid := "prod-utility-fleet"
	a1 := uuid.New()
	a2 := uuid.New()
	gname := "Prod Utility Fleet"
	store := &fakeStore{
		agents: map[uuid.UUID]*types.Agent{
			a1: {ID: a1, GroupID: &gid, GroupName: &gname},
			a2: {ID: a2, GroupID: &gid, GroupName: &gname},
		},
		cfgs: map[string]*types.Config{
			gid: {ID: "cfg-current"},
		},
		groups: map[string]*types.Group{
			gid: {ID: gid, Name: gname},
		},
	}
	spike := &types.CostSpikeEvent{
		ID:                   "spike-1",
		StartedAt:            time.Now().UTC(),
		Severity:             "critical",
		Signal:               "metrics",
		BaselineMonthlyUSD:   500,
		PeakMonthlyUSD:       1500,
		PeakPctAboveBaseline: 200,
		AttributionJSON:      `{"top_agents":["` + a1.String() + `","` + a2.String() + `"],"top_attributes":["container.id"]}`,
	}
	store.spikes = []*types.CostSpikeEvent{spike}
	return store, spike
}

// goodProposal returns a well-formed ProposalResult for the
// fixture group.
func goodProposal(gid string) *ai.ProposalResult {
	return &ai.ProposalResult{
		Declined: false,
		Proposal: ai.RolloutInputCandidate{
			Name:            "AI: drop container.id from metrics",
			GroupID:         gid,
			TargetConfigID:  "cfg-current",
			RequireApproval: true,
			Stages: []ai.RolloutStageCandidate{
				{Mode: "percentage", Percentage: 10, DwellSeconds: 600},
				{Mode: "percentage", Percentage: 100, DwellSeconds: 0},
			},
			AbortCriteria: ai.AbortCriteriaCandidate{
				MaxDriftedAgents:           5,
				MaxErrorLogsPerMinute:      50,
				MinDwellSecondsBeforeAbort: 120,
			},
		},
		Reasoning: "Container.id is the dominant attribute; drop it from the metrics pipeline.",
		Evidence: []ai.EvidenceRefCandidate{
			{Kind: "alert", ID: "spike-1", Description: "Cost spike"},
		},
		Model:     "claude-sonnet-4-6",
		TokensIn:  200,
		TokensOut: 400,
	}
}

// TestBridge_HappyPath verifies the spike-to-rollout chain end to
// end: the proposer returns a valid draft, the bridge converts it
// and posts to the rollout service with ProposedBy=ai.
func TestBridge_HappyPath(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	require.Len(t, rollouts.inputs, 1, "one rollout proposal should have been posted")
	in := rollouts.inputs[0]
	assert.Equal(t, services.RolloutProposedByAI, in.ProposedBy)
	assert.True(t, in.RequireApproval, "AI proposals must force require_approval")
	assert.Equal(t, "prod-utility-fleet", in.GroupID)
	assert.Equal(t, "cfg-current", in.TargetConfigID)
	assert.Len(t, in.Stages, 2)
	assert.Equal(t, "Container.id is the dominant attribute; drop it from the metrics pipeline.", in.ProposalReasoning)
	require.Len(t, in.EvidenceRefs, 1)
	assert.Equal(t, "alert", in.EvidenceRefs[0].Kind)
	// The proposer received our context.
	assert.Equal(t, "spike-1", prop.lastCtx.SpikeID)
	assert.Equal(t, "prod-utility-fleet", prop.lastCtx.GroupID)
	assert.Equal(t, []string{"container.id"}, prop.lastCtx.TopAttributes)
}

// TestBridge_DeclinePath verifies a declined proposal posts
// nothing to the rollout service.
func TestBridge_DeclinePath(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{{Declined: true, Reason: "Spike below threshold."}},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())
	assert.Empty(t, rollouts.inputs, "declined proposals must not produce a rollout")
}

// TestBridge_Dedup verifies the same spike does not fire twice on
// consecutive ticks.
func TestBridge_Dedup(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{
			goodProposal("prod-utility-fleet"),
			goodProposal("prod-utility-fleet"), // staged in case the bridge erroneously calls twice
		},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())

	b.tick(context.Background())
	b.tick(context.Background())

	assert.Equal(t, 1, prop.calls, "proposer should only be called once per spike across ticks")
	assert.Len(t, rollouts.inputs, 1)
}

// TestBridge_ProposerError logs and continues; the spike is still
// marked seen so we don't retry indefinitely. Future v0.54 work
// will add retry-with-backoff for transient errors.
func TestBridge_ProposerError(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		errs:    []error{errors.New("rate limited")},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())
	assert.Empty(t, rollouts.inputs, "proposer error must not produce a rollout")
	// Second tick: the spike is in the seen set; we don't call
	// the proposer again.
	b.tick(context.Background())
	assert.Equal(t, 1, prop.calls)
}

// TestBridge_NoAttribution skips a spike whose AttributionJSON is
// empty (no top agents → can't infer group). The dedup set still
// records it so we don't loop on it forever.
func TestBridge_NoAttribution(t *testing.T) {
	store, spike := baselineFixture()
	spike.AttributionJSON = ""
	prop := &fakeProposer{enabled: true}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())
	assert.Equal(t, 0, prop.calls)
	assert.Empty(t, rollouts.inputs)
}

// TestBridge_DisabledProposer makes Start a no-op so callers can
// wire the bridge unconditionally and let configuration decide
// whether it does anything.
func TestBridge_DisabledProposer(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{enabled: false}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	// Bridge should have refused to start the goroutine; Stop
	// completes immediately.
	require.NoError(t, b.Stop(time.Second))
	assert.Equal(t, 0, prop.calls)
}

// fakeAudit captures every Record call so tests can assert.
type fakeAudit struct {
	entries []services.AuditEntry
	err     error
}

func (f *fakeAudit) Record(_ context.Context, e services.AuditEntry) error {
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, e)
	return nil
}

// TestBridge_AuditOnHappyPath verifies proposal.created and
// proposal.evidence_linked both fire on a successful proposal,
// with the cost-spike ID as the target so the audit timeline
// groups everything on the spike.
func TestBridge_AuditOnHappyPath(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	require.Len(t, audit.entries, 2, "successful proposal should emit two audit events")

	created := audit.entries[0]
	assert.Equal(t, "proposal.created", created.EventType)
	assert.Equal(t, "cost_spike", created.TargetType)
	assert.Equal(t, "spike-1", created.TargetID)
	assert.Equal(t, "ai-proposer", created.Actor)
	assert.Equal(t, "ai", created.Payload["origin"])
	assert.Equal(t, "rollout-AI: drop container.id from metrics", created.Payload["rollout_id"])
	assert.Equal(t, "prod-utility-fleet", created.Payload["group_id"])
	assert.Equal(t, true, created.Payload["require_approval"])
	assert.Equal(t, 1, created.Payload["evidence_count"])
	assert.Equal(t, "claude-sonnet-4-6", created.Payload["model"])
	assert.NotEmpty(t, created.Payload["reasoning_summary"])

	evidence := audit.entries[1]
	assert.Equal(t, "proposal.evidence_linked", evidence.EventType)
	assert.Equal(t, "spike-1", evidence.TargetID)
	refs, ok := evidence.Payload["evidence"].([]map[string]any)
	require.True(t, ok, "evidence payload should be a list of refs")
	require.Len(t, refs, 1)
	assert.Equal(t, "alert", refs[0]["kind"])
}

// TestBridge_AuditOnDecline verifies proposal.declined fires with
// the reason captured.
func TestBridge_AuditOnDecline(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{{Declined: true, Reason: "Spike below threshold.", Model: "claude-sonnet-4-6", TokensIn: 100, TokensOut: 20}},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	require.Len(t, audit.entries, 1, "decline path should emit exactly one event")
	e := audit.entries[0]
	assert.Equal(t, "proposal.declined", e.EventType)
	assert.Equal(t, "spike-1", e.TargetID)
	assert.Equal(t, "Spike below threshold.", e.Payload["reason"])
	assert.Equal(t, "claude-sonnet-4-6", e.Payload["model"])
}

// TestBridge_AuditNilSafeWhenAuditUnset verifies the bridge still
// works when audit is nil — for tests, dev mode, and any future
// caller that wires the daemon without the audit dependency.
func TestBridge_AuditNilSafeWhenAuditUnset(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	require.NotPanics(t, func() { b.tick(context.Background()) })
	assert.Len(t, rollouts.inputs, 1, "rollout still posts even when audit is unset")
}

// TestBridge_AuditErrorLoggedButNotPropagated verifies a failing
// audit emit does not block the rollout from being posted.
// Compliance evidence is best-effort by design; a SIEM going down
// must not stop operational state changes from flowing.
func TestBridge_AuditErrorLoggedButNotPropagated(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{err: errors.New("siem down")}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())
	assert.Len(t, rollouts.inputs, 1, "audit failures must not block rollout posting")
}

// TestParseAttribution covers the tolerant parser.
func TestParseAttribution(t *testing.T) {
	agents, attrs := parseAttribution(`{"top_agents":["a","b"],"top_attributes":["x"]}`)
	assert.Equal(t, []string{"a", "b"}, agents)
	assert.Equal(t, []string{"x"}, attrs)

	agents, attrs = parseAttribution("")
	assert.Empty(t, agents)
	assert.Empty(t, attrs)

	agents, attrs = parseAttribution("not json")
	assert.Empty(t, agents)
	assert.Empty(t, attrs)
}

// v0.79 — goodPlan returns a valid plan-kind ProposalResult that
// the bridge should dispatch via CreatePlan. Two steps, each with
// an inline_config_snippet; abort criteria honored on each step.
func goodPlan(gid string) *ai.ProposalResult {
	stages := []ai.RolloutStageCandidate{
		{Mode: "percentage", Percentage: 10, DwellSeconds: 600},
		{Mode: "percentage", Percentage: 100, DwellSeconds: 0},
	}
	abort := ai.AbortCriteriaCandidate{
		MaxDriftedAgents: 5, MaxErrorLogsPerMinute: 50, MinDwellSecondsBeforeAbort: 120,
	}
	snippetA := "receivers:\n  otlp: {}\nexporters:\n  debug: {}\nservice:\n  pipelines:\n    metrics: { receivers: [otlp], exporters: [debug] }\n"
	snippetB := "receivers:\n  otlp: {}\nprocessors:\n  batch: {}\nexporters:\n  debug: {}\nservice:\n  pipelines:\n    metrics: { receivers: [otlp], processors: [batch], exporters: [debug] }\n"
	return &ai.ProposalResult{
		Declined: false,
		Kind:     ai.ProposalKindPlan,
		Plan: ai.PlanCandidate{
			Steps: []ai.PlanStepCandidate{
				{
					Name:                "AI plan step 0: drop http.url",
					GroupID:             gid,
					InlineConfigSnippet: snippetA,
					RequireApproval:     true,
					Stages:              stages,
					AbortCriteria:       abort,
				},
				{
					Name:                "AI plan step 1: layer filter for http.flavor",
					GroupID:             gid,
					InlineConfigSnippet: snippetB,
					Stages:              stages,
					AbortCriteria:       abort,
				},
			},
		},
		Reasoning: "Two related cardinality attributes drive the spike; stage drops so operators observe between steps.",
		Evidence: []ai.EvidenceRefCandidate{
			{Kind: "alert", ID: "spike-1", Description: "Cost spike"},
		},
		Model:     "claude-sonnet-4-6",
		TokensIn:  240,
		TokensOut: 600,
	}
}

// TestBridge_PlanKindDispatchesToCreatePlan verifies the v0.79
// discriminated union path: a Kind=plan ProposalResult flows into
// services.RolloutService.CreatePlan, not Create. The bridge
// stamps ProposedBy=ai on each step and surfaces reasoning +
// evidence on step 0 only.
func TestBridge_PlanKindDispatchesToCreatePlan(t *testing.T) {
	store, _ := baselineFixture()
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodPlan("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{planID: "plan-abc"}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	// No single rollout Create call.
	assert.Empty(t, rollouts.inputs, "plan kind must NOT dispatch through Create")
	// Two plan steps recorded by CreatePlan.
	require.Len(t, rollouts.planSteps, 2)
	assert.Equal(t, services.RolloutProposedByAI, rollouts.planSteps[0].ProposedBy)
	assert.Equal(t, services.RolloutProposedByAI, rollouts.planSteps[1].ProposedBy)
	// Step 0 carries the plan's approval gate and the AI reasoning.
	assert.True(t, rollouts.planSteps[0].RequireApproval, "step 0 must carry the plan approval gate")
	assert.Contains(t, rollouts.planSteps[0].ProposalReasoning, "stage drops")
	require.Len(t, rollouts.planSteps[0].EvidenceRefs, 1)
	assert.Equal(t, "alert", rollouts.planSteps[0].EvidenceRefs[0].Kind)
	// Step 1 does NOT carry approval gate or duplicated provenance.
	assert.False(t, rollouts.planSteps[1].RequireApproval, "step 1+ must not carry the approval gate")
	assert.Empty(t, rollouts.planSteps[1].ProposalReasoning)
	assert.Empty(t, rollouts.planSteps[1].EvidenceRefs)
	// Inline snippets flow through to the materializer.
	assert.Contains(t, rollouts.planSteps[0].InlineConfigSnippet, "exporters")
	assert.Contains(t, rollouts.planSteps[1].InlineConfigSnippet, "batch")
	// GroupID is the bridge's inferred group, not whatever the model emitted.
	assert.Equal(t, "prod-utility-fleet", rollouts.planSteps[0].GroupID)
	assert.Equal(t, "prod-utility-fleet", rollouts.planSteps[1].GroupID)
}

// Backwards compat: a ProposalResult with empty Kind (no
// discriminator) defaults to rollout dispatch. v0.79 mustn't
// break pre v0.79 model outputs.
func TestBridge_EmptyKindDefaultsToRollout(t *testing.T) {
	store, _ := baselineFixture()
	// goodProposal does not set Kind — exactly what a pre v0.79
	// fake or live response looks like.
	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	assert.Len(t, rollouts.inputs, 1, "empty kind must dispatch through Create")
	assert.Empty(t, rollouts.planSteps, "empty kind must NOT dispatch through CreatePlan")
}
