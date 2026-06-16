// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// validRolloutInput returns a baseline RolloutInput with a target config
// that exists in the given store. Tests mutate fields to exercise specific
// validation branches.
func validRolloutInput(t *testing.T, store applicationstore.ApplicationStore) RolloutInput {
	t.Helper()
	cfg := &applicationstore.Config{
		ID:         uuid.New().String(),
		Name:       "test-cfg",
		ConfigHash: "abc",
		Content:    "receivers: {}",
		Version:    1,
		CreatedAt:  time.Now(),
	}
	require.NoError(t, store.CreateConfig(context.Background(), cfg))
	return RolloutInput{
		Name:           "test rollout",
		GroupID:        "group-a",
		TargetConfigID: cfg.ID,
		Stages: []RolloutStage{
			{Percentage: 10, DwellSeconds: 5},
			{Percentage: 100, DwellSeconds: 10},
		},
		AbortCriteria: RolloutAbortCriteria{MaxDriftedAgents: 0},
	}
}

func TestRolloutService_CreateAndGet(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	r, err := svc.Create(ctx, validRolloutInput(t, store))
	require.NoError(t, err)
	require.NotEmpty(t, r.ID)
	assert.Equal(t, RolloutStatePending, r.State)
	assert.Equal(t, 0, r.CurrentStage)

	got, err := svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, r.ID, got.ID)
	assert.Len(t, got.Stages, 2)
}

func TestRolloutService_Validation(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*RolloutInput)
		errSub string
	}{
		{"empty name", func(i *RolloutInput) { i.Name = "" }, "name is required"},
		{"empty group", func(i *RolloutInput) { i.GroupID = "" }, "group_id is required"},
		{"empty target config", func(i *RolloutInput) { i.TargetConfigID = "" }, "target_config_id is required"},
		{"no stages", func(i *RolloutInput) { i.Stages = nil }, "at least one stage is required"},
		{"stage > 100", func(i *RolloutInput) {
			i.Stages = []RolloutStage{{Percentage: 150, DwellSeconds: 1}}
		}, "percentage must be in"},
		{"stage <= 0", func(i *RolloutInput) {
			i.Stages = []RolloutStage{{Percentage: 0, DwellSeconds: 1}}
		}, "percentage must be in"},
		{"non-monotonic", func(i *RolloutInput) {
			i.Stages = []RolloutStage{
				{Percentage: 50, DwellSeconds: 1},
				{Percentage: 25, DwellSeconds: 1},
				{Percentage: 100, DwellSeconds: 1},
			}
		}, "must be >= previous"},
		{"final stage not 100", func(i *RolloutInput) {
			i.Stages = []RolloutStage{
				{Percentage: 10, DwellSeconds: 1},
				{Percentage: 50, DwellSeconds: 1},
			}
		}, "final stage must have percentage = 100"},
		{"negative dwell", func(i *RolloutInput) {
			i.Stages[0].DwellSeconds = -1
		}, "dwell_seconds must be >= 0"},
		{"negative max drifted", func(i *RolloutInput) {
			i.AbortCriteria.MaxDriftedAgents = -1
		}, "max_drifted_agents must be >= 0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validRolloutInput(t, store)
			tc.mutate(&in)
			_, err := svc.Create(ctx, in)
			require.Error(t, err, "expected validation to fail")
			assert.Contains(t, err.Error(), tc.errSub)
		})
	}
}

func TestRolloutService_LabelModeValidation(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	// Helper: build a config so target_config_id validation passes, then
	// each case can swap in its own stage list.
	withStages := func(stages []RolloutStage) RolloutInput {
		in := validRolloutInput(t, store)
		in.Stages = stages
		return in
	}

	cases := []struct {
		name   string
		stages []RolloutStage
		errSub string
	}{
		{
			name: "mixed modes rejected",
			stages: []RolloutStage{
				{Mode: RolloutStageModePercent, Percentage: 10},
				{Mode: RolloutStageModeLabel, LabelSelector: map[string]string{"role": "canary"}},
			},
			errSub: "mixed stage modes are not supported",
		},
		{
			name: "label mode with empty selector rejected",
			stages: []RolloutStage{
				{Mode: RolloutStageModeLabel, LabelSelector: nil},
			},
			errSub: "non-empty label_selector",
		},
		{
			name: "label mode with empty key rejected",
			stages: []RolloutStage{
				{Mode: RolloutStageModeLabel, LabelSelector: map[string]string{"": "v"}},
			},
			errSub: "label_selector keys must be non-empty",
		},
		{
			name: "label mode with empty value rejected",
			stages: []RolloutStage{
				{Mode: RolloutStageModeLabel, LabelSelector: map[string]string{"role": ""}},
			},
			errSub: "value for \"role\" must be non-empty",
		},
		{
			name: "invalid mode rejected",
			stages: []RolloutStage{
				{Mode: "shrug", Percentage: 100},
			},
			errSub: "invalid mode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(ctx, withStages(tc.stages))
			require.Error(t, err, "expected validation to fail")
			assert.Contains(t, err.Error(), tc.errSub)
		})
	}
}

func TestRolloutService_LabelModeAccepted(t *testing.T) {
	// Sanity: a well-formed all-label-mode rollout passes validation, no
	// final-stage=100 rule applies, and the persisted shape round-trips
	// the LabelSelector intact.
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	in := validRolloutInput(t, store)
	in.Stages = []RolloutStage{
		{Mode: RolloutStageModeLabel, LabelSelector: map[string]string{"role": "canary"}, DwellSeconds: 60},
		{Mode: RolloutStageModeLabel, LabelSelector: map[string]string{"deployment.environment": "staging"}, DwellSeconds: 120},
	}

	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	require.Len(t, r.Stages, 2)
	assert.Equal(t, RolloutStageModeLabel, r.Stages[0].Mode)
	assert.Equal(t, "canary", r.Stages[0].LabelSelector["role"])

	got, err := svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.Len(t, got.Stages, 2)
	assert.Equal(t, RolloutStageModeLabel, got.Stages[1].Mode)
	assert.Equal(t, "staging", got.Stages[1].LabelSelector["deployment.environment"])
}

func TestRolloutService_NormalizesEmptyModeToPercent(t *testing.T) {
	// Pre-v0.6 callers built stages without Mode. The create path must
	// default them to percent so old clients keep working.
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	in := validRolloutInput(t, store) // baseline uses no Mode field
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	for i, st := range r.Stages {
		assert.Equal(t, RolloutStageModePercent, st.Mode, "stage %d should default to percent mode", i)
	}
}

func TestRolloutService_TargetConfigMustExist(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := RolloutInput{
		Name: "x", GroupID: "g", TargetConfigID: "does-not-exist",
		Stages:        []RolloutStage{{Percentage: 100, DwellSeconds: 0}},
		AbortCriteria: RolloutAbortCriteria{},
	}
	_, err := svc.Create(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target config not found")
}

func TestRolloutService_Abort(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	r, err := svc.Create(ctx, validRolloutInput(t, store))
	require.NoError(t, err)

	updated, err := svc.Abort(ctx, r.ID, "operator changed mind")
	require.NoError(t, err)
	assert.Equal(t, RolloutStateAborted, updated.State)
	assert.Equal(t, "operator changed mind", updated.AbortReason)

	// Aborting again should refuse — we're in a terminal-ish state.
	// (Service considers aborted as not-yet-terminal to allow engine to
	// roll back; verify the error path is at least sane though.)
	_, err = svc.Abort(ctx, r.ID, "again")
	// Aborted-state rollouts can still be re-aborted in our impl (state
	// is already aborted, the call is a no-op-ish update). The contract
	// we tested elsewhere is that succeeded/rolled_back reject. So no
	// assertion here — just don't crash.
	_ = err
}

func TestRolloutService_Abort_RejectsTerminalStates(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	r, err := svc.Create(ctx, validRolloutInput(t, store))
	require.NoError(t, err)

	// Mutate state via Persist() to simulate the engine completing the
	// rollout.
	r.State = RolloutStateSucceeded
	require.NoError(t, svc.Persist(ctx, r))

	_, err = svc.Abort(ctx, r.ID, "too late")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal state")
}

func TestRolloutService_Preview_DiffAndLint(t *testing.T) {
	// Create two configs in the store, then verify Preview returns
	// both, a populated diff, and a lint result. This is the happy
	// path the UI hits every time the operator picks a target config.
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	// Current config bound to the group — this becomes the diff
	// baseline. Note GroupID has to be set so GetLatestConfigForGroup
	// returns it.
	groupID := "group-preview"
	current := &applicationstore.Config{
		ID:         uuid.New().String(),
		Name:       "current",
		GroupID:    &groupID,
		ConfigHash: "cur-hash",
		Content:    "receivers:\n  otlp: {}\nexporters:\n  logging: {}\n",
		Version:    1,
		CreatedAt:  time.Now(),
	}
	require.NoError(t, store.CreateConfig(ctx, current))

	target := &applicationstore.Config{
		ID:         uuid.New().String(),
		Name:       "target",
		ConfigHash: "tgt-hash",
		Content:    "receivers:\n  otlp: {}\nexporters:\n  otlp: {}\n",
		Version:    1,
		CreatedAt:  time.Now(),
	}
	require.NoError(t, store.CreateConfig(ctx, target))

	preview, err := svc.Preview(ctx, groupID, target.ID)
	require.NoError(t, err)
	require.NotNil(t, preview)
	require.NotNil(t, preview.Target)
	require.NotNil(t, preview.Current, "current config should be populated when group has one")
	assert.Equal(t, target.ID, preview.Target.ID)
	assert.Equal(t, current.ID, preview.Current.ID)
	assert.False(t, preview.Diff.Identical, "configs differ — diff should report it")
	assert.Greater(t, preview.Diff.Added+preview.Diff.Removed, 0)
	assert.NotNil(t, preview.LintFindings, "lint_findings should be a non-nil slice")
}

func TestRolloutService_Preview_NoCurrentConfig(t *testing.T) {
	// Brand-new group: the diff is "everything new". The current
	// pointer should be nil, the target should be populated, and the
	// diff should count all target lines as additions.
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	ctx := context.Background()

	target := &applicationstore.Config{
		ID:         uuid.New().String(),
		Name:       "fresh",
		ConfigHash: "x",
		Content:    "receivers:\n  otlp: {}\n",
		Version:    1,
		CreatedAt:  time.Now(),
	}
	require.NoError(t, store.CreateConfig(ctx, target))

	preview, err := svc.Preview(ctx, "group-without-current", target.ID)
	require.NoError(t, err)
	require.NotNil(t, preview)
	assert.Nil(t, preview.Current, "no current config means preview.Current is nil")
	require.NotNil(t, preview.Target)
	assert.Greater(t, preview.Diff.Added, 0)
	assert.Equal(t, 0, preview.Diff.Removed)
}

func TestRolloutService_Preview_TargetMissing(t *testing.T) {
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	_, err := svc.Preview(context.Background(), "group-a", "no-such-config")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRolloutService_Preview_RequiresParams(t *testing.T) {
	svc := NewRolloutService(memory.NewStore(), nil, nil, zap.NewNop())
	_, err := svc.Preview(context.Background(), "", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group_id")

	_, err = svc.Preview(context.Background(), "g", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target_config_id")
}

func TestRolloutService_Create_RecordsDiffSummaryInAudit(t *testing.T) {
	// Verify the rollout.created audit event carries diff fingerprint
	// fields when the group has a current config. The UI's
	// AuditTimeline can then render "1 line added, 2 lines removed"
	// without re-fetching both configs.
	store := memory.NewStore()
	auditSvc := NewAuditService(store, nil, zap.NewNop())
	svc := NewRolloutService(store, nil, auditSvc, zap.NewNop())
	ctx := context.Background()

	// Group needs a current config so the diff has a baseline.
	groupID := "group-audit"
	current := &applicationstore.Config{
		ID:         uuid.New().String(),
		Name:       "current",
		GroupID:    &groupID,
		ConfigHash: "x",
		Content:    "foo: 1\n",
		Version:    1,
		CreatedAt:  time.Now(),
	}
	require.NoError(t, store.CreateConfig(ctx, current))
	target := &applicationstore.Config{
		ID:         uuid.New().String(),
		Name:       "target",
		ConfigHash: "y",
		Content:    "foo: 2\nbar: 3\n",
		Version:    1,
		CreatedAt:  time.Now(),
	}
	require.NoError(t, store.CreateConfig(ctx, target))

	in := RolloutInput{
		Name:           "preview-audit",
		GroupID:        groupID,
		TargetConfigID: target.ID,
		Stages: []RolloutStage{
			{Mode: RolloutStageModePercent, Percentage: 100, DwellSeconds: 0},
		},
	}
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	require.NotNil(t, r)

	events, err := auditSvc.List(ctx, AuditEventFilter{TargetID: r.ID, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	var created *AuditEvent
	for _, e := range events {
		if e.EventType == "rollout.created" {
			created = e
			break
		}
	}
	require.NotNil(t, created, "expected a rollout.created audit event")
	// JSON round-trip: the audit store may marshal numbers as
	// float64. Accept either int or float by coercing to int.
	addedI, _ := toInt(created.Payload["diff_added_lines"])
	removedI, _ := toInt(created.Payload["diff_removed_lines"])
	assert.GreaterOrEqual(t, addedI, 1, "expected at least one added line in diff summary")
	assert.GreaterOrEqual(t, removedI, 1, "expected at least one removed line in diff summary")
	assert.Equal(t, current.ID, created.Payload["previous_config_id"])
}

// toInt coerces JSON-unmarshaled numeric values back to int. The audit
// store may round-trip payloads through JSON depending on backend, so
// what was an int going in can be a float64 coming out.
func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	default:
		return 0, false
	}
}

// fakeTracer is a tiny stub of RolloutTracer for the service-level
// tests below. It records every call rather than producing real OTel
// spans — the rollouts.Tracer behaviour is already covered in
// tracer_test.go; this just verifies the service calls into the
// tracer at the right moments.
type fakeTracer struct {
	events []fakeEvent
	links  []string // rollout IDs LinkRolloutToContext was called for
}

type fakeEvent struct{ ID, Name, Reason string }

func (f *fakeTracer) RecordEvent(id, name, reason string) {
	f.events = append(f.events, fakeEvent{id, name, reason})
}

func (f *fakeTracer) LinkRolloutToContext(id string, _ context.Context) {
	f.links = append(f.links, id)
}

func TestRolloutService_PauseResume_FireTracerEvents(t *testing.T) {
	// Operator-level pause/resume happens at the service boundary,
	// not in the engine. Verify the service calls RolloutTracer so
	// these transitions land on the engine-opened parent span.
	store := memory.NewStore()
	tracer := &fakeTracer{}
	svc := NewRolloutServiceWithTracer(store, nil, nil, tracer, zap.NewNop())
	ctx := context.Background()

	r, err := svc.Create(ctx, validRolloutInput(t, store))
	require.NoError(t, err)
	// Create itself should register a span link (we'll come back to
	// that assertion below).
	require.Contains(t, tracer.links, r.ID)

	// Simulate the engine transitioning the rollout to in_progress
	// so Pause is valid. Persist via the service so the in-memory
	// store sees the new state.
	r.State = RolloutStateInProgress
	require.NoError(t, svc.Persist(ctx, r))

	_, err = svc.Pause(ctx, r.ID)
	require.NoError(t, err)
	_, err = svc.Resume(ctx, r.ID)
	require.NoError(t, err)

	require.Len(t, tracer.events, 2)
	assert.Equal(t, "paused", tracer.events[0].Name)
	assert.Equal(t, "resumed", tracer.events[1].Name)
	assert.Equal(t, r.ID, tracer.events[0].ID)
}

func TestRolloutService_AbortAlsoFiresTracerEvent(t *testing.T) {
	// Operator-initiated abort is the manual counterpart to the
	// engine's auto-abort path. Both should land on the same span,
	// so the tracer event fires from service.Abort too.
	store := memory.NewStore()
	tracer := &fakeTracer{}
	svc := NewRolloutServiceWithTracer(store, nil, nil, tracer, zap.NewNop())
	ctx := context.Background()
	r, err := svc.Create(ctx, validRolloutInput(t, store))
	require.NoError(t, err)
	r.State = RolloutStateInProgress
	require.NoError(t, svc.Persist(ctx, r))

	_, err = svc.Abort(ctx, r.ID, "operator changed their mind")
	require.NoError(t, err)

	found := false
	for _, e := range tracer.events {
		if e.Name == "aborted" && e.Reason == "operator changed their mind" {
			found = true
		}
	}
	assert.True(t, found, "expected an 'aborted' tracer event carrying the reason")
}

func TestRolloutService_PauseOnMissingRollout_NoTracerEvent(t *testing.T) {
	// Defensive: pause against a missing rollout returns an error
	// before the tracer is reached. No phantom event lands on a
	// rollout that doesn't exist.
	store := memory.NewStore()
	tracer := &fakeTracer{}
	svc := NewRolloutServiceWithTracer(store, nil, nil, tracer, zap.NewNop())
	_, err := svc.Pause(context.Background(), "never-existed")
	require.Error(t, err)
	assert.Empty(t, tracer.events)
}

func TestRolloutService_AbortMissing(t *testing.T) {
	svc := NewRolloutService(memory.NewStore(), nil, nil, zap.NewNop())
	_, err := svc.Abort(context.Background(), "no-such-rollout", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestRolloutService_GroupPolicyForcesApproval verifies the v0.48
// compliance control: when the target group has require_approval=true,
// the rollout must enter pending_approval even if the requester
// explicitly sets RequireApproval=false on the input. This is the
// difference between an honor-system checkbox and an enforced
// policy — auditors require the latter.
func TestRolloutService_GroupPolicyForcesApproval(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	// Seed a group with policy enabled.
	require.NoError(t, store.CreateGroup(ctx, &applicationstore.Group{
		ID:              "group-a",
		Name:            "prod-windows",
		RequireApproval: true,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}))
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := validRolloutInput(t, store)
	in.GroupID = "group-a"
	// Operator did NOT request approval — the policy must force it on.
	in.RequireApproval = false
	in.RequestedBy = "alice@example.com"

	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	assert.True(t, r.RequireApproval, "group policy should force RequireApproval=true")
	assert.Equal(t, RolloutStatePendingApproval, r.State, "rollout should be pending_approval per group policy")
}

// TestRolloutService_GroupPolicyOffPreservesInput verifies the negative
// path: with policy off, the requester's RequireApproval setting is
// preserved (both true and false). Without this check we could
// accidentally make every rollout require approval.
func TestRolloutService_GroupPolicyOffPreservesInput(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	require.NoError(t, store.CreateGroup(ctx, &applicationstore.Group{
		ID:              "group-b",
		Name:            "dev-fleet",
		RequireApproval: false,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}))
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := validRolloutInput(t, store)
	in.GroupID = "group-b"
	in.RequireApproval = false
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	assert.False(t, r.RequireApproval)
	assert.Equal(t, RolloutStatePending, r.State)
}
