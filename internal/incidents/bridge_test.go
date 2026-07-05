// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package incidents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// ---- fakes ------------------------------------------------------------------

// fakeDrafter records each call and returns a canned response.
type fakeDrafter struct {
	enabled bool
	calls   []ai.IncidentDraftInput
	respond func(ai.IncidentDraftInput) (*ai.IncidentDraftResult, error)
}

func (f *fakeDrafter) Enabled() bool { return f.enabled }
func (f *fakeDrafter) DraftIncidentFromAction(_ context.Context, in ai.IncidentDraftInput) (*ai.IncidentDraftResult, error) {
	f.calls = append(f.calls, in)
	if f.respond != nil {
		return f.respond(in)
	}
	return &ai.IncidentDraftResult{Declined: false, Title: "T", Summary: "S"}, nil
}

// fakeStore implements the Store interface with maps. Tests stage
// action requests and assert on drafts persisted.
type fakeStore struct {
	requests map[string]*types.ActionRequest
	drafts   map[string]*types.IncidentDraft
	groups   map[string]*types.Group
	// ADR 0013 D6-a — capture the tenant resolved from the draft-write
	// ctx so the isolation test can assert the draft landed in the
	// owning group's tenant, not `default`.
	lastCreateTenant string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		requests: map[string]*types.ActionRequest{},
		drafts:   map[string]*types.IncidentDraft{},
		groups:   map[string]*types.Group{},
	}
}

func (s *fakeStore) ListActionRequests(_ context.Context, filter types.ActionRequestFilter) ([]*types.ActionRequest, error) {
	var out []*types.ActionRequest
	for _, r := range s.requests {
		if filter.Status != "" && r.Status != filter.Status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (s *fakeStore) GetActionRequest(_ context.Context, id string) (*types.ActionRequest, error) {
	return s.requests[id], nil
}
func (s *fakeStore) GetIncidentDraftByActionRequestID(_ context.Context, id string) (*types.IncidentDraft, error) {
	for _, d := range s.drafts {
		if d.ActionRequestID == id {
			return d, nil
		}
	}
	return nil, nil
}
func (s *fakeStore) CreateIncidentDraft(ctx context.Context, d *types.IncidentDraft) error {
	s.lastCreateTenant = effectiveWriteTenant(ctx)
	s.drafts[d.ID] = d
	return nil
}
func (s *fakeStore) GetGroup(_ context.Context, id string) (*types.Group, error) {
	return s.groups[id], nil
}

// fakeRollouts implements the optional Rollouts interface.
type fakeRollouts struct {
	rollouts map[string]*services.Rollout
}

func (f *fakeRollouts) Get(_ context.Context, id string) (*services.Rollout, error) {
	return f.rollouts[id], nil
}

// fakeAudit records every entry.
type fakeAudit struct {
	entries []services.AuditEntry
}

func (f *fakeAudit) Record(_ context.Context, entry services.AuditEntry) error {
	f.entries = append(f.entries, entry)
	return nil
}

// helper to build a completed action request.
func completedRequest(id string, status string, completed time.Time) *types.ActionRequest {
	t := completed
	return &types.ActionRequest{
		ID:                  id,
		ProposalID:          "rollout-" + id,
		RunnerID:            "runner-1",
		ActionType:          "restart-systemd-service",
		ParametersJSON:      `{"unit_name":"nginx.service","restart_strategy":"restart"}`,
		Signature:           "sig",
		Phase:               "execute",
		Status:              status,
		ExecutionOutputJSON: `{"ran_command":"systemctl restart nginx.service","exit_code":0}`,
		IssuedAt:            completed.Add(-time.Minute),
		ExpiresAt:           completed.Add(5 * time.Minute),
		StartedAt:           &t,
		CompletedAt:         &t,
	}
}

// ---- tests ------------------------------------------------------------------

func TestBridge_HappyPath_PersistsDraftAndAudit(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.requests["a1"] = completedRequest("a1", "success", now.Add(-time.Minute))
	store.groups["web-group"] = &types.Group{ID: "web-group", Name: "Web Group"}

	rollouts := &fakeRollouts{rollouts: map[string]*services.Rollout{
		"rollout-a1": {ID: "rollout-a1", Name: "AI: pin hashing.rounds=6", GroupID: "web-group", ProposalReasoning: "Containment for the cost spike."},
	}}
	drafter := &fakeDrafter{enabled: true}
	audit := &fakeAudit{}

	b, err := New(drafter, store, rollouts, audit, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })
	b.Tick(context.Background())

	// Drafter was called with the rollout context wired through.
	require.Len(t, drafter.calls, 1)
	got := drafter.calls[0]
	assert.Equal(t, "AI: pin hashing.rounds=6", got.TriggerSummary)
	assert.Equal(t, "Containment for the cost spike.", got.ProposalReasoning)
	assert.Equal(t, "Web Group", got.GroupName)
	assert.True(t, got.StateChanged)
	assert.Contains(t, got.ActionSummary, "systemctl restart nginx.service")
	assert.Contains(t, got.OutcomeBullets, "ran_command: systemctl restart nginx.service")

	// Draft persisted with status=draft and a non-empty body.
	require.Len(t, store.drafts, 1)
	for _, d := range store.drafts {
		assert.Equal(t, "draft", d.Status)
		assert.Equal(t, "a1", d.ActionRequestID)
		assert.Equal(t, "rollout-a1", d.RolloutID)
		assert.NotEmpty(t, d.BodyMarkdown)
		assert.NotEmpty(t, d.DraftContentJSON)
	}

	// One incident.drafted audit event.
	require.Len(t, audit.entries, 1)
	assert.Equal(t, services.AuditEventIncidentDrafted, audit.entries[0].EventType)
	assert.Equal(t, services.AuditActorSystem, audit.entries[0].Actor) // background daemon
	assert.Equal(t, "a1", audit.entries[0].Payload["action_request_id"])
}

func TestBridge_Declined_EmitsAuditNoDraft(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.requests["a1"] = completedRequest("a1", "success", now.Add(-time.Minute))
	drafter := &fakeDrafter{enabled: true, respond: func(_ ai.IncidentDraftInput) (*ai.IncidentDraftResult, error) {
		return &ai.IncidentDraftResult{Declined: true, Reason: "dry run noise"}, nil
	}}
	audit := &fakeAudit{}

	b, err := New(drafter, store, nil, audit, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })
	b.Tick(context.Background())

	assert.Empty(t, store.drafts, "no draft should be persisted when the model declines")
	require.Len(t, audit.entries, 1)
	assert.Equal(t, services.AuditEventIncidentDraftDeclined, audit.entries[0].EventType)
	assert.Equal(t, services.AuditActorSystem, audit.entries[0].Actor) // background daemon
	assert.Equal(t, "dry run noise", audit.entries[0].Payload["reason"])
}

func TestBridge_AlreadyDrafted_SkipsSecondCall(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.requests["a1"] = completedRequest("a1", "success", now.Add(-time.Minute))
	store.drafts["existing"] = &types.IncidentDraft{
		ID: "existing", ActionRequestID: "a1", Status: "draft", Title: "T", BodyMarkdown: "B",
	}
	drafter := &fakeDrafter{enabled: true}

	b, err := New(drafter, store, nil, nil, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })
	b.Tick(context.Background())

	assert.Empty(t, drafter.calls, "drafter should not be called when a draft already exists for this action")
	assert.Len(t, store.drafts, 1)
}

func TestBridge_DrafterError_LogsAndContinues(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.requests["a1"] = completedRequest("a1", "success", now.Add(-time.Minute))
	store.requests["a2"] = completedRequest("a2", "success", now.Add(-2*time.Minute))

	callCount := 0
	drafter := &fakeDrafter{enabled: true, respond: func(in ai.IncidentDraftInput) (*ai.IncidentDraftResult, error) {
		callCount++
		if in.ActionRequestID == "a1" {
			return nil, errors.New("model api blew up")
		}
		return &ai.IncidentDraftResult{Title: "ok", Summary: "ok"}, nil
	}}
	audit := &fakeAudit{}

	b, err := New(drafter, store, nil, audit, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })
	b.Tick(context.Background())

	assert.Equal(t, 2, callCount, "second action should still get a draft call after the first errored")
	// One draft persisted (a2 only); zero for the errored a1.
	require.Len(t, store.drafts, 1)
	for _, d := range store.drafts {
		assert.Equal(t, "a2", d.ActionRequestID)
	}
}

func TestBridge_LookbackWindow_FiltersOldRequests(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	// One in the window, one outside.
	store.requests["recent"] = completedRequest("recent", "success", now.Add(-30*time.Minute))
	store.requests["old"] = completedRequest("old", "success", now.Add(-48*time.Hour))
	drafter := &fakeDrafter{enabled: true}

	b, err := New(drafter, store, nil, nil, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })
	b.Tick(context.Background())

	require.Len(t, drafter.calls, 1)
	assert.Equal(t, "recent", drafter.calls[0].ActionRequestID)
}

func TestBridge_DisabledDrafter_NoOpTick(t *testing.T) {
	store := newFakeStore()
	store.requests["a1"] = completedRequest("a1", "success", time.Now())
	drafter := &fakeDrafter{enabled: false}

	b, err := New(drafter, store, nil, nil, DefaultConfig(), zap.NewNop())
	require.NoError(t, err)
	b.Tick(context.Background())

	assert.Empty(t, drafter.calls, "disabled drafter means the tick is a no-op")
}

func TestBridge_StatusDeniedIsNotDrafted(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	store := newFakeStore()
	store.requests["denied-1"] = completedRequest("denied-1", "denied", now.Add(-time.Minute))
	drafter := &fakeDrafter{enabled: true}

	b, err := New(drafter, store, nil, nil, Config{PollInterval: time.Hour, Lookback: time.Hour}, zap.NewNop())
	require.NoError(t, err)
	b.SetClock(func() time.Time { return now })
	b.Tick(context.Background())

	assert.Empty(t, drafter.calls, "denied requests should not be drafted (no operator action ran)")
}

func TestNew_RequiresDrafterAndStore(t *testing.T) {
	_, err := New(nil, newFakeStore(), nil, nil, DefaultConfig(), nil)
	assert.Error(t, err)
	_, err = New(&fakeDrafter{enabled: true}, nil, nil, nil, DefaultConfig(), nil)
	assert.Error(t, err)
}
