// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package automations implements the leader-only evaluator for Agent
// Automations (ADR 0038, first slice): condition-triggered auto-remediation
// rules of the shape trigger → action → target, under guardrails.
//
// The evaluator is a singleton loop that sits alongside the alert evaluator and
// the silent-agent watcher (all gated leader-only via the ADR 0035 elector, so
// N app instances don't each fire the same restart). Each tick it lists ENABLED
// automations and the agent table, derives each agent's health the same way the
// silent-agent watcher and drift detection do (last-seen wall clock + persisted
// status), and for an agent whose health TRANSITIONS into the rule's trigger
// condition it dispatches the action through the existing OpAMP restart path —
// under the non-negotiable guardrails:
//
//   - cooldown: a minimum interval between actions per (rule, agent), so a
//     flapping condition can't drive a restart storm;
//   - max-attempts-then-escalate: after N actions in one outage the evaluator
//     STOPS auto-acting on that agent and records an escalation, so a
//     config-bricking crash is never masked by infinite restarts;
//   - dry-run: evaluate + audit the decision without acting, to prove a rule
//     before arming it.
//
// Every decision (triggered / action_taken / action_failed / suppressed /
// escalated) is recorded on the tamper-evident audit chain.
//
// Scope boundary (ADR 0038): auto-restart-via-supervisor only reaches an agent
// whose supervisor is UP but whose collector child is unhealthy. If the
// supervisor or host is down the OpAMP channel is gone and Squadron cannot act —
// that is local systemd's job and/or human escalation, not this feature.
package automations

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// DefaultSilenceThreshold mirrors silentagents.DefaultSilenceThreshold: the
// wall-clock gap after which an agent that was checking in is classed silent /
// unhealthy. Kept in sync deliberately so both surfaces agree on "silent".
const DefaultSilenceThreshold = 10 * time.Minute

// DefaultPollInterval is how often the evaluator wakes. Evaluation is
// transition-driven (an action fires on the edge into the trigger condition,
// then only again after the cooldown), so the poll cadence bounds detection
// latency, not action volume.
const DefaultPollInterval = 30 * time.Second

// Store is the slice of the application store the evaluator needs.
type Store interface {
	ListAutomations(ctx context.Context) ([]*apptypes.Automation, error)
	ListAgents(ctx context.Context) ([]*apptypes.Agent, error)
}

// Commander dispatches the action. The first slice's only action is
// supervisor_restart, so this is the exact seam the /agents/:id/restart handler
// and squadronctl use — the OpAMP RestartCommand (accepts_restart_command).
// The OpAMP ConfigSender satisfies it. Narrow interface so the evaluator does
// not import the api/opamp packages (and stays trivially fakeable in tests).
type Commander interface {
	RestartAgent(agentID uuid.UUID) error
}

// Auditor is the subset of services.AuditService the evaluator records through.
// nil disables audit (still safe; useful in tests). services.AuditService
// satisfies it.
type Auditor interface {
	Record(ctx context.Context, entry services.AuditEntry) error
}

// Config tunes the evaluator loop. Zero values fall back to sensible defaults.
type Config struct {
	SilenceThreshold time.Duration
	PollInterval     time.Duration
}

// ruleAgentState tracks the per-(automation, agent) outage so the guardrails
// can bound actions. It resets when the agent recovers (transition out of the
// trigger condition), so max-attempts is "attempts per outage".
type ruleAgentState struct {
	unhealthy     bool      // currently matching the trigger?
	attempts      int       // actions taken in the current outage
	lastAttemptAt time.Time // for the cooldown gate
	escalated     bool      // max-attempts hit; stop auto-acting until recovery
}

// Evaluator runs the automation loop. Construct with New and run Run in a
// goroutine (under the leader-election elector); Tick is exported for tests and
// an on-demand path.
type Evaluator struct {
	cfg       Config
	store     Store
	commander Commander
	audit     Auditor // optional
	logger    *zap.Logger

	// now is the clock, injectable for deterministic tests.
	now func() time.Time

	mu    sync.Mutex
	state map[string]*ruleAgentState // "<automationID>|<agentID>" → state
	stop  chan struct{}
}

// New constructs an evaluator. audit may be nil to disable audit.
func New(cfg Config, store Store, commander Commander, audit Auditor, logger *zap.Logger) *Evaluator {
	if cfg.SilenceThreshold <= 0 {
		cfg.SilenceThreshold = DefaultSilenceThreshold
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	return &Evaluator{
		cfg:       cfg,
		store:     store,
		commander: commander,
		audit:     audit,
		logger:    logger,
		now:       func() time.Time { return time.Now() },
		state:     map[string]*ruleAgentState{},
		stop:      make(chan struct{}),
	}
}

// Run blocks until Stop is called or ctx is cancelled. Under the elector, ctx
// is cancelled when this instance loses leadership.
func (e *Evaluator) Run(ctx context.Context) {
	e.logger.Info("automation evaluator started",
		zap.Duration("poll_interval", e.cfg.PollInterval),
		zap.Duration("silence_threshold", e.cfg.SilenceThreshold))
	t := time.NewTicker(e.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Tick(ctx); err != nil {
				e.logger.Warn("automation evaluator tick failed", zap.Error(err))
			}
		}
	}
}

// Stop signals the Run loop to exit at its next tick. Idempotent.
func (e *Evaluator) Stop() {
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
}

// Tick runs one evaluation pass: classify each in-scope agent against each
// enabled rule, act (or suppress) under the guardrails, GC dead state.
func (e *Evaluator) Tick(ctx context.Context) error {
	autos, err := e.store.ListAutomations(ctx)
	if err != nil {
		return fmt.Errorf("list automations: %w", err)
	}
	agents, err := e.store.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	now := e.now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()

	seen := map[string]struct{}{}
	for _, a := range autos {
		if a == nil || !a.Enabled {
			continue
		}
		for _, agent := range scopeAgents(a, agents) {
			key := a.ID + "|" + agent.ID.String()
			seen[key] = struct{}{}
			st := e.state[key]
			if st == nil {
				st = &ruleAgentState{}
				e.state[key] = st
			}

			matched := matchesTrigger(agent, a.Trigger, now, e.cfg.SilenceThreshold)
			if !matched {
				// Recovered (or never broken). Reset the outage so the next
				// break starts a fresh attempt budget.
				st.unhealthy = false
				st.attempts = 0
				st.escalated = false
				st.lastAttemptAt = time.Time{}
				continue
			}

			if !st.unhealthy {
				// Edge into the trigger condition — a new outage.
				st.unhealthy = true
				st.attempts = 0
				st.escalated = false
				st.lastAttemptAt = time.Time{}
				e.record(ctx, services.AuditEventAutomationTriggered, a, agent, "trigger condition observed")
			}

			e.maybeAct(ctx, a, agent, st, now)
		}
	}

	// GC state for (rule, agent) pairs no longer in scope (rule deleted /
	// disabled, agent removed) so the map can't grow unbounded.
	for k := range e.state {
		if _, ok := seen[k]; !ok {
			delete(e.state, k)
		}
	}
	return nil
}

// maybeAct applies the guardrails and either dispatches the action, suppresses
// it (cooldown or dry-run), or escalates. Caller holds e.mu.
func (e *Evaluator) maybeAct(ctx context.Context, a *apptypes.Automation, agent *apptypes.Agent, st *ruleAgentState, now time.Time) {
	if st.escalated {
		return // already handed off to a human; wait for recovery
	}

	// Cooldown gate first, so the cadence is identical for real and dry-run and
	// a flapping condition can't storm. Silent wait — no audit spam per tick.
	if cd := e.cooldown(a); cd > 0 && !st.lastAttemptAt.IsZero() && now.Sub(st.lastAttemptAt) < cd {
		return
	}

	// Max-attempts backstop: after N actions in this outage, STOP and escalate.
	if st.attempts >= e.maxAttempts(a) {
		st.escalated = true
		e.record(ctx, services.AuditEventAutomationEscalated, a, agent,
			fmt.Sprintf("max attempts (%d) reached in one outage; auto-action stopped, escalating to human", e.maxAttempts(a)))
		e.logger.Warn("automation escalated: max attempts reached",
			zap.String("automation_id", a.ID), zap.String("agent_id", agent.ID.String()),
			zap.Int("max_attempts", e.maxAttempts(a)))
		return
	}

	// This attempt counts against the budget whether real or dry-run.
	st.attempts++
	st.lastAttemptAt = now

	if a.DryRun {
		e.record(ctx, services.AuditEventAutomationSuppressed, a, agent, "dry_run: action not taken")
		return
	}

	if err := e.commander.RestartAgent(agent.ID); err != nil {
		e.record(ctx, services.AuditEventAutomationActionFailed, a, agent, "restart dispatch failed: "+err.Error())
		e.logger.Warn("automation action failed",
			zap.String("automation_id", a.ID), zap.String("agent_id", agent.ID.String()), zap.Error(err))
		return
	}
	e.record(ctx, services.AuditEventAutomationActionTaken, a, agent, "supervisor restart dispatched")
	e.logger.Info("automation action taken: supervisor restart",
		zap.String("automation_id", a.ID), zap.String("automation_name", a.Name),
		zap.String("agent_id", agent.ID.String()), zap.Int("attempt", st.attempts))
}

func (e *Evaluator) cooldown(a *apptypes.Automation) time.Duration {
	if a.CooldownSeconds <= 0 {
		return 0
	}
	return time.Duration(a.CooldownSeconds) * time.Second
}

func (e *Evaluator) maxAttempts(a *apptypes.Automation) int {
	if a.MaxAttempts <= 0 {
		return 1
	}
	return a.MaxAttempts
}

// record writes one audit row (best-effort; nil auditor is a no-op). The action
// events target the AGENT (TargetID = agent id) with the rule in the payload,
// so the agent timeline shows why it was acted on.
func (e *Evaluator) record(ctx context.Context, eventType string, a *apptypes.Automation, agent *apptypes.Agent, reason string) {
	if e.audit == nil {
		return
	}
	_ = e.audit.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  eventType,
		TargetType: services.AuditTargetAgent,
		TargetID:   agent.ID.String(),
		Action:     "automation",
		Payload: map[string]any{
			"automation_id":   a.ID,
			"automation_name": a.Name,
			"trigger":         string(a.Trigger),
			"action":          string(a.Action),
			"dry_run":         a.DryRun,
			"reason":          reason,
		},
	})
}

// scopeAgents returns the agents an automation targets: the single agent (by
// id) for an agent-scoped rule, or every current member for a group-scoped rule
// (agent → group resolved via Agent.GroupID, exactly like config resolution).
func scopeAgents(a *apptypes.Automation, agents []*apptypes.Agent) []*apptypes.Agent {
	var out []*apptypes.Agent
	for _, ag := range agents {
		if ag == nil {
			continue
		}
		switch {
		case a.AgentID != nil && *a.AgentID != "":
			if ag.ID.String() == *a.AgentID {
				out = append(out, ag)
			}
		case a.GroupID != nil && *a.GroupID != "":
			if ag.GroupID != nil && *ag.GroupID == *a.GroupID {
				out = append(out, ag)
			}
		}
	}
	return out
}

// matchesTrigger classifies an agent against a trigger. Health is derived from
// the same signals the silent-agent watcher and drift detection use: an agent
// is silent when it hasn't been seen within the threshold; unhealthy is silent
// OR a persisted offline/error status.
func matchesTrigger(ag *apptypes.Agent, trig apptypes.AutomationTrigger, now time.Time, silence time.Duration) bool {
	silent := now.Sub(ag.LastSeen) > silence
	switch trig {
	case apptypes.AutomationTriggerAgentSilent:
		return silent
	case apptypes.AutomationTriggerAgentUnhealthy:
		return silent ||
			ag.Status == apptypes.AgentStatusOffline ||
			ag.Status == apptypes.AgentStatusError
	default:
		return false
	}
}
