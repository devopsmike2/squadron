// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/proposer/verdictsel"
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
	// v0.89.17 (#633) — verdicts seeded by tests. Keyed by group_id
	// so a test can stage approved/rejected AI rollouts on group G
	// and assert the bridge picks them up via assembleVerdicts.
	verdicts map[string][]*types.Rollout
	// v0.89.17 (#633) — captures the (groupID, since, limit) the
	// bridge passed on the last ListAIVerdictsForGroup call so the
	// recency-window test can assert the 30-day cutoff.
	lastVerdictsGroupID string
	lastVerdictsSince   time.Time
	lastVerdictsLimit   int
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

// ListAIVerdictsForGroup returns the seeded verdicts that match the
// supplied filter. Mirrors the SQLite store's selection:
//   - same group_id,
//   - proposed_by="ai",
//   - approved_at OR rejected_at set,
//   - COALESCE(approved_at, rejected_at) >= since,
//   - sorted newest verdict first,
//   - capped at limit.
//
// The fake doesn't model the partial index — it filters in Go on the
// seeded slice — but the result shape matches what the SQLite store
// returns for the same inputs.
func (f *fakeStore) ListAIVerdictsForGroup(_ context.Context, gid string, since time.Time, limit int) ([]*types.Rollout, error) {
	f.lastVerdictsGroupID = gid
	f.lastVerdictsSince = since
	f.lastVerdictsLimit = limit
	if f.verdicts == nil {
		return nil, nil
	}
	verdictAt := func(r *types.Rollout) time.Time {
		if r.ApprovedAt != nil {
			return *r.ApprovedAt
		}
		if r.RejectedAt != nil {
			return *r.RejectedAt
		}
		return time.Time{}
	}
	var out []*types.Rollout
	for _, r := range f.verdicts[gid] {
		if r.ProposedBy != types.RolloutProposedByAI {
			continue
		}
		if r.ApprovedAt == nil && r.RejectedAt == nil {
			continue
		}
		// v0.89.26 (#642) — per-rollout opt-out filter, mirroring
		// the SQLite store's `AND exclude_from_learning = 0`
		// predicate. The fake has to apply the same filter so
		// bridge tests that seed an excluded rollout see it drop
		// out of the few-shot block — otherwise the
		// TestExcludeFromLearning_* acceptance tests below would
		// pass against a fake that's more permissive than the real
		// store.
		if r.ExcludeFromLearning {
			continue
		}
		if verdictAt(r).Before(since) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return verdictAt(out[i]).After(verdictAt(out[j]))
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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
	// v0.81.4 (#546) — step 0's RequestedBy must be "ai-proposer" so
	// services.CreatePlan's plan.created audit emission lands a
	// meaningful actor. Without this the audit row had Actor=""
	// while the same-instant proposal.created event used
	// "ai-proposer", confusing SIEM consumers who key on actor.
	assert.Equal(t, "ai-proposer", rollouts.planSteps[0].RequestedBy,
		"step 0 RequestedBy drives plan.created Actor; must match the bridge's other AI audit emissions")
	// Step 1+ have no need for RequestedBy — services.CreatePlan
	// doesn't read it for non-head steps and the audit trail
	// derives the actor from step 0.
	assert.Empty(t, rollouts.planSteps[1].RequestedBy,
		"step 1+ RequestedBy is unused; keeping it empty avoids a misleading per-step actor in storage")
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

// ---------------------------------------------------------------------------
// v0.89.17 (#633) — proposer learns from accepted/rejected verdicts, slice 1.
// The five acceptance tests from
// docs/proposals/531-proposer-learns-from-accepted-rejected.md §11, plus a
// redaction test for §7. ALL tests below seed the baseline fixture's group
// with LearnFromVerdicts=true UNLESS they're exercising the opt-out path.
// ---------------------------------------------------------------------------

// verdictsFixture is the baseline fixture with LearnFromVerdicts=true
// on the group, since every acceptance test except the opt-out one
// needs the loop turned on. Tests that want the opt-out path flip
// the flag back to false explicitly.
func verdictsFixture() (*fakeStore, *types.CostSpikeEvent) {
	store, spike := baselineFixture()
	for _, g := range store.groups {
		g.LearnFromVerdicts = true
	}
	return store, spike
}

// approvedRollout seeds a verdict with state=APPROVED at the given
// time, the given reasoning, and the given optional approver notes.
// id is what assembleVerdicts and the prompt block will surface back.
func approvedRollout(id, groupID, reasoning, notes string, approvedAt time.Time) *types.Rollout {
	t := approvedAt.UTC()
	return &types.Rollout{
		ID:                id,
		Name:              "AI: " + id,
		GroupID:           groupID,
		ProposedBy:        types.RolloutProposedByAI,
		ProposalReasoning: reasoning,
		ApprovedBy:        "operator@example.com",
		ApprovedAt:        &t,
		ApprovalNotes:     notes,
		State:             types.RolloutStateSucceeded,
	}
}

// rejectedRollout seeds a verdict with state=REJECTED at the given
// time, the given reasoning, and the given rejecter notes. Slice 1
// stores rejection notes in the same approval_notes column as
// approvals (the column was designed by v0.47 to carry both).
func rejectedRollout(id, groupID, reasoning, notes string, rejectedAt time.Time) *types.Rollout {
	t := rejectedAt.UTC()
	return &types.Rollout{
		ID:                id,
		Name:              "AI: " + id,
		GroupID:           groupID,
		ProposedBy:        types.RolloutProposedByAI,
		ProposalReasoning: reasoning,
		RejectedBy:        "operator@example.com",
		RejectedAt:        &t,
		ApprovalNotes:     notes,
		State:             types.RolloutStateRejected,
	}
}

// TestProposerVerdicts_ColdStartParity — Acceptance test §11.1 of
// the slice 1 design (also slice 2 §12 test 1). A group with zero
// AI-originated rollouts: the user message must be byte-for-byte
// identical to what v0.79's buildProposeUserMessage would have
// produced (i.e. no examples block). After the v0.89.35 refactor,
// "no block" means CostSpikeContext.VerdictBlock=="" and the
// rendered prompt must match the empty-block path verbatim.
func TestProposerVerdicts_ColdStartParity(t *testing.T) {
	store, _ := verdictsFixture()
	// Zero seeded verdicts.
	store.verdicts = map[string][]*types.Rollout{}

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal("prod-utility-fleet")},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	// The proposer received a context with an empty VerdictBlock.
	assert.Empty(t, prop.lastCtx.VerdictBlock,
		"cold-start: no verdict block should reach the proposer context")

	// Cold-start parity: rendering the prompt against a context
	// with VerdictBlock=="" must produce a message that omits the
	// §7.2 block entirely.
	rendered := ai.RenderProposeUserMessageForTest(prop.lastCtx)
	assert.NotContains(t, rendered, "Prior verdicts for this group",
		"cold start: the examples block must be omitted entirely")

	// The audit payload carries an empty (not nil, not omitted) array.
	require.Len(t, audit.entries, 2, "successful proposal emits proposal.created + evidence_linked")
	created := audit.entries[0]
	examples, ok := created.Payload["verdict_examples_used"].([]string)
	require.True(t, ok, "verdict_examples_used must be present on cold start (empty array, not omitted)")
	assert.Empty(t, examples, "verdict_examples_used must be empty on cold start")
}

// TestProposerVerdicts_ApprovedExampleSurfaces — Acceptance test §11.2.
// Seed one approved AI rollout in group G with the design's canonical
// example reasoning. Fire a new spike. Assert: the user message
// contains the §7.2 stanza for that rollout (under the cost-spike
// surface's `reference: rollout_id=` shape), and the audit
// verdict_examples_used contains that id.
func TestProposerVerdicts_ApprovedExampleSurfaces(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	const wantID = "rlt_8ax9"
	store.verdicts = map[string][]*types.Rollout{
		gid: {
			approvedRollout(wantID, gid, "drop container.id, canary 10%", "good plan, ship it", time.Now().Add(-2*time.Hour)),
		},
	}

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal(gid)},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	// The proposer context carries the rendered §7.2 block.
	assert.NotEmpty(t, prop.lastCtx.VerdictBlock,
		"one approved verdict should produce a non-empty block on the context")
	assert.Contains(t, prop.lastCtx.VerdictBlock, "[APPROVED] kind=cost-spike-rollout",
		"block must carry the canonical APPROVED stanza header")
	assert.Contains(t, prop.lastCtx.VerdictBlock, "reference: rollout_id="+wantID,
		"block must carry the cost-spike surface's rollout_id reference line")
	assert.Contains(t, prop.lastCtx.VerdictBlock, "container.id",
		"reason line must carry the verdict's reasoning")

	// Rendering the prompt with this context lands the same block
	// inside the full user message at the §7.2 position.
	rendered := ai.RenderProposeUserMessageForTest(prop.lastCtx)
	assert.Contains(t, rendered, "Prior verdicts for this group",
		"prompt must carry the §7.2 header")
	assert.Contains(t, rendered, "reference: rollout_id="+wantID,
		"prompt must carry the canonical reference line")

	// audit.verdict_examples_used contains the id.
	require.Len(t, audit.entries, 2)
	examples, ok := audit.entries[0].Payload["verdict_examples_used"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{wantID}, examples)
}

// TestProposerVerdicts_MixCapHonored — Acceptance test §11.3 of
// the slice 1 design, preserved across the slice 2 verdictsel
// refactor. Seed 5 approved + 5 rejected within 30 days. Assert:
// exactly 4 examples (DefaultMaxTotal), at most 2 approved, at most
// 2 rejected (DefaultMaxPerKind = 2 inside each single-kind
// bucket), newest first within each bucket, audit list ordered
// rejected-first.
func TestProposerVerdicts_MixCapHonored(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	now := time.Now()
	// Seed 5 approved + 5 rejected, each with distinct timestamps
	// so the newest-first ordering inside each bucket is determi-
	// nistic. The newest two of each kind should win.
	var seeded []*types.Rollout
	for i := 0; i < 5; i++ {
		// approved_i lands at (now - (10+i)h). i=0 is the newest.
		seeded = append(seeded, approvedRollout(
			fmt.Sprintf("rlt_a%d", i), gid,
			fmt.Sprintf("approved-%d reasoning", i),
			"", // unannotated approvals are valid signal per resolved Q2
			now.Add(-time.Duration(10+i)*time.Hour),
		))
		// rejected_i lands at (now - (5+i)h). i=0 is the newest.
		seeded = append(seeded, rejectedRollout(
			fmt.Sprintf("rlt_r%d", i), gid,
			fmt.Sprintf("rejected-%d reasoning", i),
			fmt.Sprintf("rejecter note %d", i),
			now.Add(-time.Duration(5+i)*time.Hour),
		))
	}
	store.verdicts = map[string][]*types.Rollout{gid: seeded}

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal(gid)},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	// audit.verdict_examples_used carries all 4 ids in the same
	// order they reached the prompt (rejections first, approvals
	// second — §6 step 7 weights rejections higher).
	examples, ok := audit.entries[0].Payload["verdict_examples_used"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"rlt_r0", "rlt_r1", "rlt_a0", "rlt_a1"}, examples,
		"audit list: DefaultMaxTotal=4, rejected-first per §6 step 7, newest-first inside each bucket")

	// The rendered block carries exactly the same four IDs as
	// rollout_id reference lines in the same rejected-first order.
	block := prop.lastCtx.VerdictBlock
	require.NotEmpty(t, block, "non-empty pool must render a block")
	for _, wantID := range []string{"rlt_r0", "rlt_r1", "rlt_a0", "rlt_a1"} {
		assert.Contains(t, block, "reference: rollout_id="+wantID,
			"block must carry rollout_id=%s", wantID)
	}
	// The dropped IDs (rlt_a2..a4, rlt_r2..r4) must NOT appear.
	for _, dropID := range []string{"rlt_a2", "rlt_a3", "rlt_a4", "rlt_r2", "rlt_r3", "rlt_r4"} {
		assert.NotContains(t, block, "rollout_id="+dropID,
			"verdictsel.Select must cap past the per-kind/per-total limits; %s should not appear", dropID)
	}
}

// TestProposerVerdicts_OptOutFlagRespected — Acceptance test §11.4.
// Group.LearnFromVerdicts=false, seed 3 approved + 3 rejected. The
// prompt block must be omitted (no examples reach the proposer
// context) and verdict_examples_used must be the empty array — NOT
// omitted, so SIEM consumers can still filter on it.
func TestProposerVerdicts_OptOutFlagRespected(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	// Flip opt-out on.
	store.groups[gid].LearnFromVerdicts = false

	now := time.Now()
	var seeded []*types.Rollout
	for i := 0; i < 3; i++ {
		seeded = append(seeded, approvedRollout(
			fmt.Sprintf("rlt_a%d", i), gid,
			"would have been an approved example", "",
			now.Add(-time.Duration(2+i)*time.Hour),
		))
		seeded = append(seeded, rejectedRollout(
			fmt.Sprintf("rlt_r%d", i), gid,
			"would have been a rejected example", "no",
			now.Add(-time.Duration(1+i)*time.Hour),
		))
	}
	store.verdicts = map[string][]*types.Rollout{gid: seeded}

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal(gid)},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	assert.Empty(t, prop.lastCtx.VerdictBlock,
		"opt-out: no verdict block must reach the proposer context")

	// Verify the rendered prompt has no examples block.
	rendered := ai.RenderProposeUserMessageForTest(prop.lastCtx)
	assert.NotContains(t, rendered, "Prior verdicts for this group",
		"opt-out: the examples block must be omitted entirely")

	// audit.verdict_examples_used must be present + empty (not nil).
	require.Len(t, audit.entries, 2)
	examples, ok := audit.entries[0].Payload["verdict_examples_used"].([]string)
	require.True(t, ok, "verdict_examples_used must be present on opt-out (empty array, not omitted)")
	assert.NotNil(t, examples, "must be empty array, not nil")
	assert.Empty(t, examples, "opt-out: verdict_examples_used must be empty")
}

// TestProposerVerdicts_RecencyWindow — Acceptance test §11.5.
// Seed one approved rollout dated 31 days ago. Assert: that rollout
// does NOT appear in the prompt and is NOT in verdict_examples_used.
func TestProposerVerdicts_RecencyWindow(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	store.verdicts = map[string][]*types.Rollout{
		gid: {
			approvedRollout("rlt_old", gid, "31 days ago — outside window", "",
				time.Now().Add(-31*24*time.Hour)),
		},
	}

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal(gid)},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	assert.Empty(t, prop.lastCtx.VerdictBlock,
		"recency window: a 31-day-old verdict must not surface")

	// audit.verdict_examples_used must be empty array.
	require.Len(t, audit.entries, 2)
	examples, ok := audit.entries[0].Payload["verdict_examples_used"].([]string)
	require.True(t, ok)
	assert.Empty(t, examples)

	// The bridge asked the storage layer for since >= now-30d
	// (verdictsel.DefaultWindow). We captured the actual since on
	// the fake; assert it's within a minute of the design's 30-day
	// window so a future regression (e.g. someone bumping the
	// constant to 7 days) trips here.
	wantSince := time.Now().Add(-verdictsel.DefaultWindow)
	delta := store.lastVerdictsSince.Sub(wantSince)
	if delta < 0 {
		delta = -delta
	}
	assert.Less(t, delta, time.Minute,
		"bridge must call ListAIVerdictsForGroup with since=now-verdictsel.DefaultWindow (30d)")
}

// TestProposerVerdicts_SecretsRedacted pins §7's redaction
// requirement: the example's reasoning and notes must flow through
// RedactSecrets before they reach the prompt. Seeds a verdict whose
// reasoning carries a fake Anthropic API key; asserts the placeholder
// shows up instead of the literal.
func TestProposerVerdicts_SecretsRedacted(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	// A reasoning string that contains a credential shape we know
	// RedactSecrets covers. The "sk-ant-…" prefix matches the
	// Anthropic key pattern in internal/ai/redact.go.
	secretReasoning := "drop container.id; pushed via sk-ant-AAAABBBBCCCCDDDDEEEEFFFF1234 from CI"
	store.verdicts = map[string][]*types.Rollout{
		gid: {
			approvedRollout("rlt_with_secret", gid, secretReasoning, "ok ship it", time.Now().Add(-3*time.Hour)),
		},
	}

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal(gid)},
	}
	rollouts := &fakeRollouts{}
	audit := &fakeAudit{}
	b := New(prop, store, rollouts, audit, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	// The block on the context (precomputed by verdictprompt.Render
	// in the bridge) and the rendered prompt both carry the
	// placeholder, not the literal credential. After the v0.89.35
	// refactor, redaction lives in verdictprompt.reasonField so the
	// secret is scrubbed at render time, not in assembleVerdicts.
	assert.NotEmpty(t, prop.lastCtx.VerdictBlock,
		"a verdict with secret-bearing reasoning should still produce a block (with redaction)")
	assert.NotContains(t, prop.lastCtx.VerdictBlock, "sk-ant-AAAABBBB",
		"secret material must not survive into the proposer context")
	assert.Contains(t, prop.lastCtx.VerdictBlock, "<redacted:anthropic_key>",
		"RedactSecrets placeholder must be present in the rendered block")

	// The rendered prompt likewise carries the placeholder, not the
	// literal credential.
	rendered := ai.RenderProposeUserMessageForTest(prop.lastCtx)
	assert.NotContains(t, rendered, "sk-ant-AAAABBBB")
	assert.Contains(t, rendered, "<redacted:anthropic_key>")
}

// TestProposerVerdicts_AssembleVerdicts_InlineSnippetsExcluded pins
// the §7 / item-6 constraint that inline config bodies never enter
// the prompt block. The Rollout's TargetConfigID and stored Stages
// represent the materialized config; assembleVerdicts must never
// touch those fields. We exercise this indirectly: a verdict whose
// reasoning is empty but with a non-trivial TargetConfigID must
// produce a VerdictExample with empty Reasoning (NOT the snippet),
// and the prompt must not contain the snippet bytes.
func TestProposerVerdicts_AssembleVerdicts_InlineSnippetsExcluded(t *testing.T) {
	store, _ := verdictsFixture()
	const gid = "prod-utility-fleet"
	// Seed a verdict with reasoning="" but a fake snippet-shaped
	// TargetConfigID + name so we can pin that the snippet does NOT
	// leak.
	verdict := approvedRollout("rlt_no_reason", gid, "", "", time.Now().Add(-1*time.Hour))
	verdict.TargetConfigID = "INLINE_SNIPPET_DO_NOT_LEAK"
	verdict.Name = "AI: receivers: {otlp: {}} INLINE_SNIPPET_DO_NOT_LEAK"
	store.verdicts = map[string][]*types.Rollout{gid: {verdict}}

	prop := &fakeProposer{
		enabled: true,
		results: []*ai.ProposalResult{goodProposal(gid)},
	}
	rollouts := &fakeRollouts{}
	b := New(prop, store, rollouts, nil, Config{PollInterval: time.Hour}, zap.NewNop())
	b.tick(context.Background())

	// The rendered prompt + the precomputed verdict block must not
	// carry the snippet-shaped identifiers. After the v0.89.35
	// refactor the bridge maps each Rollout into a verdictsel.Verdict
	// reading only ID, ProposalReasoning, ApprovalNotes, and
	// ExcludeFromLearning — TargetConfigID and Name (where the
	// snippet bytes live in this fixture) are never touched.
	assert.NotEmpty(t, prop.lastCtx.VerdictBlock,
		"a verdict with empty reasoning still renders a block (with `(no reason given)` placeholder)")
	assert.NotContains(t, prop.lastCtx.VerdictBlock, "INLINE_SNIPPET_DO_NOT_LEAK",
		"target_config_id (a snippet identifier) must not leak into the verdict block")
	rendered := ai.RenderProposeUserMessageForTest(prop.lastCtx)
	assert.NotContains(t, rendered, "INLINE_SNIPPET_DO_NOT_LEAK",
		"target_config_id (a snippet identifier) must not leak into the prompt block")
}
