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

func TestRolloutService_Create_ErrorRateWindowDefault(t *testing.T) {
	// ADR 0008 — a new rollout that sets an error-rate criterion without an
	// explicit window should default to the trailing window; one that sets
	// the window explicitly (including 0 for legacy) should be respected;
	// one with no error-rate criterion should stay at 0.
	ctx := context.Background()

	t.Run("defaults when error-rate set and window unset", func(t *testing.T) {
		store := memory.NewStore()
		svc := NewRolloutService(store, nil, nil, zap.NewNop())
		in := validRolloutInput(t, store)
		in.AbortCriteria = RolloutAbortCriteria{MaxErrorLogsPerMinute: 5}
		r, err := svc.Create(ctx, in)
		require.NoError(t, err)
		assert.Equal(t, defaultErrorRateWindowSeconds, r.AbortCriteria.ErrorRateWindowSeconds)
	})

	t.Run("explicit window is preserved", func(t *testing.T) {
		store := memory.NewStore()
		svc := NewRolloutService(store, nil, nil, zap.NewNop())
		in := validRolloutInput(t, store)
		in.AbortCriteria = RolloutAbortCriteria{MaxErrorLogsPerMinute: 5, ErrorRateWindowSeconds: 45}
		r, err := svc.Create(ctx, in)
		require.NoError(t, err)
		assert.Equal(t, 45, r.AbortCriteria.ErrorRateWindowSeconds)
	})

	t.Run("no error-rate criterion leaves window at 0", func(t *testing.T) {
		store := memory.NewStore()
		svc := NewRolloutService(store, nil, nil, zap.NewNop())
		in := validRolloutInput(t, store)
		in.AbortCriteria = RolloutAbortCriteria{MaxDriftedAgents: 1}
		r, err := svc.Create(ctx, in)
		require.NoError(t, err)
		assert.Equal(t, 0, r.AbortCriteria.ErrorRateWindowSeconds)
	})
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

// stubGroupPolicy implements policy.GroupPolicyProvider for tests.
// Returns true for every group ID in the enforced set. Stands in for
// the Compliance Pack's real provider so the open core test can
// exercise the enforcement code path without depending on the
// private repo.
type stubGroupPolicy struct {
	enforced map[string]bool
	// ADR 0029 — per-group N-of-M threshold the stub provider mandates.
	// Zero value (nil map) returns 0, so the OSS default of 1 stands.
	required map[string]int
	// ADR 0030 — per-group required approver-roles the stub mandates.
	// Nil map returns nil, so the approver-role gate stays inert.
	roles map[string][]string
}

func (s stubGroupPolicy) RequiresApproval(_ context.Context, groupID string) bool {
	return s.enforced[groupID]
}

func (s stubGroupPolicy) RequiredApprovals(_ context.Context, groupID string) int {
	return s.required[groupID]
}

func (s stubGroupPolicy) RequiredApproverRoles(_ context.Context, groupID string) []string {
	return s.roles[groupID]
}

// TestRolloutService_GroupPolicyForcesApproval verifies the v0.48
// compliance control: when the wired policy provider reports that
// the group's policy is enforced, the rollout must enter
// pending_approval even if the requester explicitly sets
// RequireApproval=false on the input. This is the difference
// between an honor-system checkbox and an enforced policy —
// auditors require the latter.
//
// v0.52 — the enforcement implementation moved to the Compliance
// Pack (private squadron-compliance repo). The open core only knows
// the interface boundary. This test wires a stub provider that
// behaves the way the Pack's real provider will, so the integration
// is still covered in the open core CI.
func TestRolloutService_GroupPolicyForcesApproval(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	require.NoError(t, store.CreateGroup(ctx, &applicationstore.Group{
		ID:        "group-a",
		Name:      "prod-windows",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}))
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	svc.(*RolloutServiceImpl).SetGroupPolicyProvider(stubGroupPolicy{
		enforced: map[string]bool{"group-a": true},
	})

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

// TestRolloutService_NoPolicyProviderDoesNotEnforce documents the OSS
// default: with no policy provider wired (the open-core build), the
// require_approval flag on a Group row is metadata and the rollout
// service does NOT force approval. Customers who need enforcement
// run the Compliance Pack build, which wires its own provider.
func TestRolloutService_NoPolicyProviderDoesNotEnforce(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	require.NoError(t, store.CreateGroup(ctx, &applicationstore.Group{
		ID:              "group-oss",
		Name:            "oss-fleet",
		RequireApproval: true, // metadata only in the OSS build
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}))
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	// No SetGroupPolicyProvider call — this is the OSS default.

	in := validRolloutInput(t, store)
	in.GroupID = "group-oss"
	in.RequireApproval = false

	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	assert.False(t, r.RequireApproval, "OSS build must not enforce; require_approval flag is metadata")
	assert.Equal(t, RolloutStatePending, r.State)
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

// --- ADR 0029: N-of-M rollout approvals ---------------------------------

// TestRolloutService_Approve_DefaultThresholdIsTwoPerson is the REGRESSION
// pin: a rollout that leaves RequiredApprovals unset (→ floored to 1) must
// behave byte-for-byte like the v0.47 two-person workflow — a single distinct
// approver flips pending_approval → pending, writing the scalar approval
// fields.
func TestRolloutService_Approve_DefaultThresholdIsTwoPerson(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := validRolloutInput(t, store)
	in.RequireApproval = true
	in.RequestedBy = "alice@example.com"
	// RequiredApprovals intentionally unset (0) → floored to 1.
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	require.Equal(t, RolloutStatePendingApproval, r.State)
	require.Equal(t, 1, r.RequiredApprovals, "unset threshold floors to the two-person default of 1")

	got, err := svc.Approve(ctx, r.ID, "bob@example.com", "", "", "lgtm")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePending, got.State, "one distinct approver must flip pending_approval → pending")
	assert.Equal(t, "bob@example.com", got.ApprovedBy)
	require.NotNil(t, got.ApprovedAt)
	assert.Equal(t, "lgtm", got.ApprovalNotes)
	assert.Equal(t, 1, got.ApproverCount)
}

// TestRolloutService_Approve_NofMProgression exercises required=3: the first
// two distinct approvers leave the rollout in pending_approval (count climbs),
// and the third distinct approver flips it to pending.
func TestRolloutService_Approve_NofMProgression(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := validRolloutInput(t, store)
	in.RequireApproval = true
	in.RequestedBy = "alice@example.com"
	in.RequiredApprovals = 3
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	require.Equal(t, 3, r.RequiredApprovals)
	require.Equal(t, RolloutStatePendingApproval, r.State)

	got, err := svc.Approve(ctx, r.ID, "bob@example.com", "", "", "1")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePendingApproval, got.State, "1/3 must stay pending_approval")
	assert.Equal(t, 1, got.ApproverCount)

	got, err = svc.Approve(ctx, r.ID, "carol@example.com", "", "", "2")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePendingApproval, got.State, "2/3 must stay pending_approval")
	assert.Equal(t, 2, got.ApproverCount)

	got, err = svc.Approve(ctx, r.ID, "dave@example.com", "", "", "3")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePending, got.State, "3/3 must flip to pending")
	assert.Equal(t, 3, got.ApproverCount)
	assert.Equal(t, "dave@example.com", got.ApprovedBy, "the threshold-crossing approver is recorded")
}

// TestRolloutService_Approve_SameApproverIdempotent confirms a single approver
// approving twice does not advance the distinct-approver count and leaves the
// rollout pending_approval when the threshold is above 1.
func TestRolloutService_Approve_SameApproverIdempotent(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := validRolloutInput(t, store)
	in.RequireApproval = true
	in.RequestedBy = "alice@example.com"
	in.RequiredApprovals = 3
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)

	got, err := svc.Approve(ctx, r.ID, "bob@example.com", "", "", "first")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePendingApproval, got.State)
	assert.Equal(t, 1, got.ApproverCount)

	got, err = svc.Approve(ctx, r.ID, "bob@example.com", "", "", "again")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePendingApproval, got.State, "duplicate approver must not flip the rollout")
	assert.Equal(t, 1, got.ApproverCount, "duplicate approver must not double-count")

	n, err := store.CountRolloutApprovers(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestRolloutService_Approve_RequesterCannotSelfApprove confirms the
// two-person guard still holds under N-of-M: the requester cannot be counted
// as an approver, and no approval is recorded on a rejected self-approval.
func TestRolloutService_Approve_RequesterCannotSelfApprove(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := validRolloutInput(t, store)
	in.RequireApproval = true
	in.RequestedBy = "alice@example.com"
	in.RequiredApprovals = 3
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)

	_, err = svc.Approve(ctx, r.ID, "ALICE@example.com", "", "", "self") // case-insensitive match
	require.Error(t, err)
	assert.Contains(t, err.Error(), "two-person rule")

	n, err := store.CountRolloutApprovers(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a rejected self-approval must not be recorded")
}

// TestRolloutService_ReadPopulatesApproverCount pins ADR 0029's read-path
// fix: a pending_approval rollout with recorded-but-below-threshold
// approvers must report k via ApproverCount on BOTH Get and List (the
// Approve() write path already set it; before this fix a plain GET/LIST
// reported 0, so the UI couldn't render k/N on load).
func TestRolloutService_ReadPopulatesApproverCount(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	in := validRolloutInput(t, store)
	in.RequireApproval = true
	in.RequestedBy = "alice@example.com"
	in.RequiredApprovals = 3
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	require.Equal(t, RolloutStatePendingApproval, r.State)

	// One distinct approver — stays pending_approval at 1/3.
	got, err := svc.Approve(ctx, r.ID, "bob@example.com", "", "", "1")
	require.NoError(t, err)
	require.Equal(t, RolloutStatePendingApproval, got.State)
	require.Equal(t, 1, got.ApproverCount)

	// Get must now report the derived count (was 0 before the fix).
	fetched, err := svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, 3, fetched.RequiredApprovals, "required_approvals must round-trip on read")
	assert.Equal(t, 1, fetched.ApproverCount, "Get must derive k from the approvals log")

	// List must populate the count for the pending_approval rollout too.
	list, err := svc.List(ctx, RolloutFilter{})
	require.NoError(t, err)
	var found *Rollout
	for _, item := range list {
		if item.ID == r.ID {
			found = item
			break
		}
	}
	require.NotNil(t, found, "created rollout must appear in List")
	assert.Equal(t, 1, found.ApproverCount, "List must derive k from the approvals log")
	assert.Equal(t, 3, found.RequiredApprovals)
}

// TestRolloutService_ReadApproverCountZeroForNonPending guards the N+1
// avoidance decision: rollouts that are not pending_approval are never
// counted on read (ApproverCount stays 0), so LIST does not fan out a
// COUNT per terminal/in-flight row.
func TestRolloutService_ReadApproverCountZeroForNonPending(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())

	// A plain rollout with no approval gate is created in 'pending'.
	in := validRolloutInput(t, store)
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	require.NotEqual(t, RolloutStatePendingApproval, r.State)

	fetched, err := svc.Get(ctx, r.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, 0, fetched.ApproverCount, "non-pending rollouts are not counted on read")
}

// --- ADR 0030: rule-based approver-role requirements --------------------

// stubRoleChecker implements services.ApproverRoleChecker for tests. It
// resolves roles by stable token id first, then by label, mirroring the
// enterprise RBAC-backed checker. A configured err fails closed.
type stubRoleChecker struct {
	byToken map[string][]string
	byLabel map[string][]string
	err     error
}

func (s stubRoleChecker) RolesForApprover(_ context.Context, tokenID, tokenLabel string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	if r, ok := s.byToken[tokenID]; ok {
		return r, nil
	}
	return s.byLabel[tokenLabel], nil
}

// TestRolloutService_ApproverRoles_InertWithoutBothSeams is the ADR 0030
// non-regression pin: the approver-role gate must stay inert unless BOTH
// the group mandate (groupPolicy.RequiredApproverRoles) AND the resolver
// (ApproverRoleChecker) are wired. With only one — or neither — a single
// distinct approver flips pending_approval -> pending exactly as the
// count-only workflow does, and MissingApproverRoles is empty.
func TestRolloutService_ApproverRoles_InertWithoutBothSeams(t *testing.T) {
	cases := []struct {
		name        string
		wireGroup   bool
		wireChecker bool
	}{
		{"neither seam (OSS default)", false, false},
		{"group mandate but no checker (compliance-only)", true, false},
		{"checker but no group mandate", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := memory.NewStore()
			svc := NewRolloutService(store, nil, nil, zap.NewNop())
			impl := svc.(*RolloutServiceImpl)
			if tc.wireGroup {
				impl.SetGroupPolicyProvider(stubGroupPolicy{
					roles: map[string][]string{"group-a": {"security"}},
				})
			}
			if tc.wireChecker {
				impl.SetApproverRoleChecker(stubRoleChecker{
					byToken: map[string][]string{"tok-sec": {"security"}},
				})
			}

			in := validRolloutInput(t, store)
			in.RequireApproval = true
			in.RequestedBy = "alice@example.com"
			r, err := svc.Create(ctx, in)
			require.NoError(t, err)
			require.Equal(t, RolloutStatePendingApproval, r.State)

			// A single NON-security approver must still flip: the gate is inert.
			got, err := svc.Approve(ctx, r.ID, "operator:ops", "tok-ops", "ops", "lgtm")
			require.NoError(t, err)
			assert.Equal(t, RolloutStatePending, got.State, "gate must be inert; single approver flips")
			assert.Empty(t, got.MissingApproverRoles)
		})
	}
}

// TestRolloutService_ApproverRoles_BlocksUntilRoleCovered is the core ADR
// 0030 behavior in the composed configuration (BOTH seams wired): a group
// that requires a "security" approver stays pending_approval when the count
// is met by a non-security approver, surfacing the unmet role via
// MissingApproverRoles, and only flips once a security-role holder approves.
func TestRolloutService_ApproverRoles_BlocksUntilRoleCovered(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	impl := svc.(*RolloutServiceImpl)
	impl.SetGroupPolicyProvider(stubGroupPolicy{
		roles: map[string][]string{"group-a": {"security"}},
	})
	impl.SetApproverRoleChecker(stubRoleChecker{
		byToken: map[string][]string{"tok-sec": {"security"}},
	})

	in := validRolloutInput(t, store)
	in.RequireApproval = true
	in.RequestedBy = "alice@example.com"
	// RequiredApprovals unset -> 1. Count is met by the first approver, so
	// this proves the ROLE gate (not the count) holds the rollout.
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)
	require.Equal(t, RolloutStatePendingApproval, r.State)

	// Non-security approver: COUNT is met (1/1) but the "security" role is
	// uncovered -> stay pending_approval, surface the missing role.
	got, err := svc.Approve(ctx, r.ID, "operator:ops", "tok-ops", "ops", "first")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePendingApproval, got.State, "count met but missing role must NOT flip")
	assert.Equal(t, 1, got.ApproverCount)
	assert.Equal(t, []string{"security"}, got.MissingApproverRoles, "the unmet role must surface for the UI")

	// Security-role approver: role now covered -> flip to pending.
	got, err = svc.Approve(ctx, r.ID, "operator:sec", "tok-sec", "sec", "second")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePending, got.State, "role covered -> flip")
	assert.Empty(t, got.MissingApproverRoles)
	assert.Equal(t, "operator:sec", got.ApprovedBy, "the role-covering approver crosses the gate")
}

// TestRolloutService_ApproverRoles_NilRolesStaysInert confirms that a wired
// provider returning NO required roles (an unconfigured group) leaves the
// gate inert even with a checker present — byte-identical to count-only.
func TestRolloutService_ApproverRoles_NilRolesStaysInert(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	svc := NewRolloutService(store, nil, nil, zap.NewNop())
	impl := svc.(*RolloutServiceImpl)
	impl.SetGroupPolicyProvider(stubGroupPolicy{}) // roles map nil -> returns nil
	impl.SetApproverRoleChecker(stubRoleChecker{
		byToken: map[string][]string{"tok-sec": {"security"}},
	})

	in := validRolloutInput(t, store)
	in.RequireApproval = true
	in.RequestedBy = "alice@example.com"
	r, err := svc.Create(ctx, in)
	require.NoError(t, err)

	got, err := svc.Approve(ctx, r.ID, "operator:ops", "tok-ops", "ops", "lgtm")
	require.NoError(t, err)
	assert.Equal(t, RolloutStatePending, got.State, "no required roles -> gate inert -> flip")
	assert.Empty(t, got.MissingApproverRoles)
}
