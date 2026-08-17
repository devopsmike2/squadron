// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package automations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// --- fakes ---------------------------------------------------------------

type fakeStore struct {
	autos  []*apptypes.Automation
	agents []*apptypes.Agent
}

func (f *fakeStore) ListAutomations(context.Context) ([]*apptypes.Automation, error) {
	return f.autos, nil
}
func (f *fakeStore) ListAgents(context.Context) ([]*apptypes.Agent, error) { return f.agents, nil }

type fakeCommander struct {
	calls []uuid.UUID
	err   error
}

func (f *fakeCommander) RestartAgent(id uuid.UUID) error {
	f.calls = append(f.calls, id)
	return f.err
}

type fakeAuditor struct{ events []string }

func (f *fakeAuditor) Record(_ context.Context, e services.AuditEntry) error {
	f.events = append(f.events, e.EventType)
	return nil
}

func (f *fakeAuditor) has(evt string) bool {
	for _, e := range f.events {
		if e == evt {
			return true
		}
	}
	return false
}

// --- helpers -------------------------------------------------------------

const silence = 10 * time.Minute

// silentAgent returns an agent last seen an hour ago (past the silence
// threshold) → classes as silent/unhealthy against the fixed test clock.
func silentAgent(now time.Time, groupID string) *apptypes.Agent {
	a := &apptypes.Agent{
		ID:       uuid.New(),
		Name:     "collector",
		Status:   apptypes.AgentStatusOnline, // still "online" in the store; silence is derived from LastSeen
		LastSeen: now.Add(-1 * time.Hour),
	}
	if groupID != "" {
		g := groupID
		a.GroupID = &g
	}
	return a
}

func healthyAgent(now time.Time, groupID string) *apptypes.Agent {
	a := silentAgent(now, groupID)
	a.LastSeen = now // seen just now → not silent
	a.Status = apptypes.AgentStatusOnline
	return a
}

func groupAutomation(groupID string, cooldownS, maxAttempts int) *apptypes.Automation {
	g := groupID
	return &apptypes.Automation{
		ID:              "auto-" + groupID,
		Name:            "restart " + groupID,
		Enabled:         true,
		GroupID:         &g,
		Trigger:         apptypes.AutomationTriggerAgentUnhealthy,
		Action:          apptypes.AutomationActionSupervisorRestart,
		CooldownSeconds: cooldownS,
		MaxAttempts:     maxAttempts,
	}
}

// newEval wires an evaluator with a controllable clock. The returned pointer's
// now is set to read *clk so the caller advances time by mutating it.
func newEval(t *testing.T, store Store, cmd Commander, audit Auditor, clk *time.Time) *Evaluator {
	t.Helper()
	e := New(Config{SilenceThreshold: silence, PollInterval: time.Second}, store, cmd, audit, zap.NewNop())
	e.now = func() time.Time { return *clk }
	return e
}

// --- tests ---------------------------------------------------------------

func TestEvaluator_TriggersRestartOnUnhealthyTransition(t *testing.T) {
	now := time.Now().UTC()
	agent := silentAgent(now, "g1")
	store := &fakeStore{
		autos:  []*apptypes.Automation{groupAutomation("g1", 300, 3)},
		agents: []*apptypes.Agent{agent},
	}
	cmd := &fakeCommander{}
	audit := &fakeAuditor{}
	e := newEval(t, store, cmd, audit, &now)

	require.NoError(t, e.Tick(context.Background()))

	require.Len(t, cmd.calls, 1, "expected exactly one restart on the unhealthy transition")
	assert.Equal(t, agent.ID, cmd.calls[0])
	assert.True(t, audit.has(services.AuditEventAutomationTriggered))
	assert.True(t, audit.has(services.AuditEventAutomationActionTaken))
}

func TestEvaluator_DebounceSuppressesRepeats(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		autos:  []*apptypes.Automation{groupAutomation("g1", 300, 5)},
		agents: []*apptypes.Agent{silentAgent(now, "g1")},
	}
	cmd := &fakeCommander{}
	e := newEval(t, store, cmd, &fakeAuditor{}, &now)
	ctx := context.Background()

	require.NoError(t, e.Tick(ctx)) // acts
	require.NoError(t, e.Tick(ctx)) // within cooldown → suppressed
	now = now.Add(299 * time.Second)
	require.NoError(t, e.Tick(ctx)) // still within cooldown → suppressed
	assert.Len(t, cmd.calls, 1, "cooldown must suppress repeat restarts")

	now = now.Add(2 * time.Second) // now past the 300s cooldown
	require.NoError(t, e.Tick(ctx))
	assert.Len(t, cmd.calls, 2, "a restart is allowed once the cooldown elapses")
}

func TestEvaluator_MaxAttemptsThenEscalate(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		autos:  []*apptypes.Automation{groupAutomation("g1", 60, 2)},
		agents: []*apptypes.Agent{silentAgent(now, "g1")},
	}
	cmd := &fakeCommander{}
	audit := &fakeAuditor{}
	e := newEval(t, store, cmd, audit, &now)
	ctx := context.Background()

	require.NoError(t, e.Tick(ctx)) // attempt 1
	now = now.Add(61 * time.Second)
	require.NoError(t, e.Tick(ctx)) // attempt 2 (== max)
	now = now.Add(61 * time.Second)
	require.NoError(t, e.Tick(ctx)) // budget exhausted → escalate, no action
	now = now.Add(61 * time.Second)
	require.NoError(t, e.Tick(ctx)) // still escalated → no action

	assert.Len(t, cmd.calls, 2, "must stop auto-restarting after max attempts")
	assert.True(t, audit.has(services.AuditEventAutomationEscalated), "must escalate after max attempts")
}

func TestEvaluator_RecoveryResetsAttemptBudget(t *testing.T) {
	now := time.Now().UTC()
	agent := silentAgent(now, "g1")
	store := &fakeStore{
		autos:  []*apptypes.Automation{groupAutomation("g1", 60, 1)},
		agents: []*apptypes.Agent{agent},
	}
	cmd := &fakeCommander{}
	e := newEval(t, store, cmd, &fakeAuditor{}, &now)
	ctx := context.Background()

	require.NoError(t, e.Tick(ctx)) // attempt 1 (max=1)
	assert.Len(t, cmd.calls, 1)

	// Agent recovers: last-seen becomes current.
	agent.LastSeen = now
	require.NoError(t, e.Tick(ctx))
	assert.Len(t, cmd.calls, 1, "no action while healthy")

	// Agent breaks again → fresh outage, fresh budget.
	agent.LastSeen = now.Add(-1 * time.Hour)
	now = now.Add(120 * time.Second)
	require.NoError(t, e.Tick(ctx))
	assert.Len(t, cmd.calls, 2, "a new outage gets a fresh attempt budget")
}

func TestEvaluator_DisabledDoesNotFire(t *testing.T) {
	now := time.Now().UTC()
	auto := groupAutomation("g1", 300, 3)
	auto.Enabled = false
	store := &fakeStore{
		autos:  []*apptypes.Automation{auto},
		agents: []*apptypes.Agent{silentAgent(now, "g1")},
	}
	cmd := &fakeCommander{}
	audit := &fakeAuditor{}
	e := newEval(t, store, cmd, audit, &now)

	require.NoError(t, e.Tick(context.Background()))
	assert.Empty(t, cmd.calls)
	assert.Empty(t, audit.events, "a disabled rule produces no audit events")
}

func TestEvaluator_NoMatchingAutomationIsNoop(t *testing.T) {
	now := time.Now().UTC()
	// Automation targets a specific (different) agent id; the unhealthy agent
	// belongs to no matching scope.
	other := "some-other-agent-id"
	auto := &apptypes.Automation{
		ID: "a1", Name: "x", Enabled: true, AgentID: &other,
		Trigger: apptypes.AutomationTriggerAgentUnhealthy, Action: apptypes.AutomationActionSupervisorRestart,
		CooldownSeconds: 300, MaxAttempts: 3,
	}
	store := &fakeStore{
		autos:  []*apptypes.Automation{auto},
		agents: []*apptypes.Agent{silentAgent(now, "g1")},
	}
	cmd := &fakeCommander{}
	e := newEval(t, store, cmd, &fakeAuditor{}, &now)

	require.NoError(t, e.Tick(context.Background()))
	assert.Empty(t, cmd.calls, "an unhealthy agent with no matching rule is a no-op")
}

func TestEvaluator_ScopeMatching_AgentAndGroup(t *testing.T) {
	now := time.Now().UTC()
	// Two unhealthy agents in g1, one in g2.
	a1 := silentAgent(now, "g1")
	a2 := silentAgent(now, "g1")
	a3 := silentAgent(now, "g2")

	// Group rule for g1 → matches a1 + a2 only.
	groupRule := groupAutomation("g1", 300, 3)
	// Agent rule for a3 → matches a3 only.
	a3id := a3.ID.String()
	agentRule := &apptypes.Automation{
		ID: "agentrule", Name: "restart a3", Enabled: true, AgentID: &a3id,
		Trigger: apptypes.AutomationTriggerAgentUnhealthy, Action: apptypes.AutomationActionSupervisorRestart,
		CooldownSeconds: 300, MaxAttempts: 3,
	}
	store := &fakeStore{
		autos:  []*apptypes.Automation{groupRule, agentRule},
		agents: []*apptypes.Agent{a1, a2, a3},
	}
	cmd := &fakeCommander{}
	e := newEval(t, store, cmd, &fakeAuditor{}, &now)

	require.NoError(t, e.Tick(context.Background()))

	restarted := map[uuid.UUID]bool{}
	for _, id := range cmd.calls {
		restarted[id] = true
	}
	assert.Len(t, cmd.calls, 3, "two group members + one agent-scoped target")
	assert.True(t, restarted[a1.ID])
	assert.True(t, restarted[a2.ID])
	assert.True(t, restarted[a3.ID])
}

func TestEvaluator_DryRunAuditsButDoesNotAct(t *testing.T) {
	now := time.Now().UTC()
	auto := groupAutomation("g1", 300, 3)
	auto.DryRun = true
	store := &fakeStore{
		autos:  []*apptypes.Automation{auto},
		agents: []*apptypes.Agent{silentAgent(now, "g1")},
	}
	cmd := &fakeCommander{}
	audit := &fakeAuditor{}
	e := newEval(t, store, cmd, audit, &now)

	require.NoError(t, e.Tick(context.Background()))
	assert.Empty(t, cmd.calls, "dry-run must not dispatch the action")
	assert.True(t, audit.has(services.AuditEventAutomationTriggered))
	assert.True(t, audit.has(services.AuditEventAutomationSuppressed))
}

func TestEvaluator_HealthyAgentNoAction(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		autos:  []*apptypes.Automation{groupAutomation("g1", 300, 3)},
		agents: []*apptypes.Agent{healthyAgent(now, "g1")},
	}
	cmd := &fakeCommander{}
	audit := &fakeAuditor{}
	e := newEval(t, store, cmd, audit, &now)

	require.NoError(t, e.Tick(context.Background()))
	assert.Empty(t, cmd.calls)
	assert.Empty(t, audit.events)
}

func TestEvaluator_UnhealthyTrigger_PersistedErrorStatus(t *testing.T) {
	now := time.Now().UTC()
	// Recently seen (not silent) but persisted status is error.
	agent := healthyAgent(now, "g1")
	agent.Status = apptypes.AgentStatusError

	// agent_unhealthy trigger matches on the error status.
	unhealthy := groupAutomation("g1", 300, 3) // trigger = agent_unhealthy
	store := &fakeStore{autos: []*apptypes.Automation{unhealthy}, agents: []*apptypes.Agent{agent}}
	cmd := &fakeCommander{}
	e := newEval(t, store, cmd, &fakeAuditor{}, &now)
	require.NoError(t, e.Tick(context.Background()))
	assert.Len(t, cmd.calls, 1, "agent_unhealthy matches a persisted error status")

	// agent_silent trigger does NOT match (last-seen is recent).
	silentRule := groupAutomation("g1", 300, 3)
	silentRule.ID = "silent-rule"
	silentRule.Trigger = apptypes.AutomationTriggerAgentSilent
	store2 := &fakeStore{autos: []*apptypes.Automation{silentRule}, agents: []*apptypes.Agent{agent}}
	cmd2 := &fakeCommander{}
	e2 := newEval(t, store2, cmd2, &fakeAuditor{}, &now)
	require.NoError(t, e2.Tick(context.Background()))
	assert.Empty(t, cmd2.calls, "agent_silent only fires on last-seen staleness, not error status")
}

func TestEvaluator_ActionFailedIsAudited(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{
		autos:  []*apptypes.Automation{groupAutomation("g1", 300, 3)},
		agents: []*apptypes.Agent{silentAgent(now, "g1")},
	}
	cmd := &fakeCommander{err: errors.New("agent does not support restart command")}
	audit := &fakeAuditor{}
	e := newEval(t, store, cmd, audit, &now)

	require.NoError(t, e.Tick(context.Background()))
	assert.Len(t, cmd.calls, 1, "dispatch is attempted")
	assert.True(t, audit.has(services.AuditEventAutomationActionFailed))
	assert.False(t, audit.has(services.AuditEventAutomationActionTaken))
}
