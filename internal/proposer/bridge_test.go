// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"errors"
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
}

func (f *fakeRollouts) Create(_ context.Context, in services.RolloutInput) (*services.Rollout, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.inputs = append(f.inputs, in)
	return &services.Rollout{ID: "rollout-" + in.Name}, nil
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
	b := New(prop, store, rollouts, Config{PollInterval: time.Hour}, zap.NewNop())
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
	b := New(prop, store, rollouts, Config{PollInterval: time.Hour}, zap.NewNop())
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
	b := New(prop, store, rollouts, Config{PollInterval: time.Hour}, zap.NewNop())

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
	b := New(prop, store, rollouts, Config{PollInterval: time.Hour}, zap.NewNop())
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
	b := New(prop, store, rollouts, Config{PollInterval: time.Hour}, zap.NewNop())
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
	b := New(prop, store, rollouts, Config{PollInterval: time.Hour}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.Start(ctx)
	// Bridge should have refused to start the goroutine; Stop
	// completes immediately.
	require.NoError(t, b.Stop(time.Second))
	assert.Equal(t, 0, prop.calls)
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
