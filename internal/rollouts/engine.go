// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package rollouts runs the background state machine that advances safe
// staged config rollouts and triggers automatic rollback when their abort
// criteria fire.
//
// The engine is deliberately conservative: it never advances faster than
// the operator's configured dwell, never widens the canary beyond the
// stage's percentage, and aborts on the first criterion match. False
// negatives (a real problem the engine missed) are recoverable via
// operator-initiated abort; false positives (an unnecessary rollback) are
// inconvenient but rarely catastrophic, so we tilt toward aborting.
package rollouts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// tickInterval is how often the engine wakes up to scan rollouts. Per-stage
// dwells are typically minutes, so 5s tick is plenty without being chatty.
const tickInterval = 5 * time.Second

// AgentCommander is the subset of opamp.ConfigSender the engine needs.
// Defined as an interface here so tests can plug in a mock without pulling
// the OpAMP machinery.
type AgentCommander interface {
	SendConfigToAgent(agentID uuid.UUID, content string) error
}

// ConfigStore is the subset of applicationstore the engine needs for
// reading the target + previous configs at apply time.
type ConfigStore interface {
	GetConfig(ctx context.Context, id string) (*applicationstore.Config, error)
}

// TelemetryReader is the subset of telemetrystore.Reader the engine uses
// to evaluate error-rate abort criteria against canary agents.
type TelemetryReader interface {
	// CanaryErrorLogsPerMinute returns the average ERROR-or-higher log
	// records per minute emitted by the given agent ids in the window
	// [since, now). Returns 0 if there are no records (which the engine
	// treats as healthy).
	CanaryErrorLogsPerMinute(ctx context.Context, agentIDs []uuid.UUID, since time.Time) (float64, error)
}

// Engine advances active rollouts and triggers rollback when abort
// criteria fire.
type Engine struct {
	rolloutService services.RolloutService
	agentService   services.AgentService
	auditService   services.AuditService // optional
	configStore    ConfigStore
	telemetry      TelemetryReader // optional; nil disables error-rate criteria
	commander      AgentCommander
	broker         *events.Broker // optional; nil disables SSE publication
	httpClient     *http.Client   // for webhook notifications
	logger         *zap.Logger

	shutdown chan struct{}
	wg       sync.WaitGroup
}

// NewEngine wires up the engine. auditService, telemetry, and broker are
// all optional.
func NewEngine(
	rolloutService services.RolloutService,
	agentService services.AgentService,
	auditService services.AuditService,
	configStore ConfigStore,
	telemetry TelemetryReader,
	commander AgentCommander,
	broker *events.Broker,
	logger *zap.Logger,
) *Engine {
	return &Engine{
		rolloutService: rolloutService,
		agentService:   agentService,
		auditService:   auditService,
		configStore:    configStore,
		telemetry:      telemetry,
		commander:      commander,
		broker:         broker,
		httpClient:     &http.Client{Timeout: 10 * time.Second},
		logger:         logger,
		shutdown:       make(chan struct{}),
	}
}

// notifyWebhook POSTs a structured JSON payload to the rollout's
// NotificationURL on every state transition. No-op if the rollout has no
// URL configured. Failures are logged but don't block engine progress —
// the audit log captures the durable record.
func (e *Engine) notifyWebhook(ctx context.Context, r *services.Rollout, transition string) {
	if r.NotificationURL == "" {
		return
	}
	payload := map[string]any{
		"rollout_id":       r.ID,
		"name":             r.Name,
		"group_id":         r.GroupID,
		"target_config_id": r.TargetConfigID,
		"state":            string(r.State),
		"transition":       transition,
		"current_stage":    r.CurrentStage,
		"total_stages":     len(r.Stages),
		"abort_reason":     r.AbortReason,
		"at":               time.Now().UTC().Format(time.RFC3339Nano),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		e.logger.Warn("rollout engine: failed to marshal webhook payload",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.NotificationURL, bytes.NewReader(body))
	if err != nil {
		e.logger.Warn("rollout engine: failed to build webhook request",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Squadron/rollouts")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		e.logger.Warn("rollout engine: webhook delivery failed",
			zap.String("rollout_id", r.ID),
			zap.String("url", r.NotificationURL),
			zap.Error(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		e.logger.Warn("rollout engine: webhook returned non-2xx",
			zap.String("rollout_id", r.ID),
			zap.Int("status", resp.StatusCode))
	}
}

// publishStateChange emits a RolloutStateChanged event over the broker AND
// POSTs to the rollout's notification webhook if configured. Each
// transition the engine takes goes through here so both channels stay in
// sync.
func (e *Engine) publishStateChange(r *services.Rollout, transition string) {
	if e.broker != nil {
		e.broker.Publish(events.Event{
			Type: events.RolloutStateChanged,
			Data: map[string]any{
				"rollout_id":    r.ID,
				"name":          r.Name,
				"state":         string(r.State),
				"current_stage": r.CurrentStage,
				"transition":    transition,
			},
		})
	}
	// Fire webhook in the background — don't block engine progress on a
	// slow operator endpoint. Use a short timeout context so a hung
	// webhook can't keep the engine goroutine alive past shutdown.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		e.notifyWebhook(ctx, r, transition)
	}()
}

// Start launches the engine loop in a goroutine.
func (e *Engine) Start() {
	e.wg.Add(1)
	go e.loop()
	e.logger.Info("rollout engine started", zap.Duration("tick_interval", tickInterval))
}

// Stop signals shutdown and waits for the loop to exit.
func (e *Engine) Stop(timeout time.Duration) error {
	close(e.shutdown)
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		e.logger.Info("rollout engine stopped")
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("rollout engine shutdown timeout exceeded")
	}
}

func (e *Engine) loop() {
	defer e.wg.Done()
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			e.tick()
		case <-e.shutdown:
			return
		}
	}
}

// tick scans for active rollouts and advances each one's state machine.
func (e *Engine) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pending and in_progress rollouts both need processing. Aborted
	// rollouts also need work (perform the rollback push, then transition
	// to rolled_back). Succeeded and rolled_back are terminal.
	states := []services.RolloutState{
		services.RolloutStatePending,
		services.RolloutStateInProgress,
		services.RolloutStateAborted,
	}
	for _, st := range states {
		list, err := e.rolloutService.List(ctx, services.RolloutFilter{State: st, Limit: 1000})
		if err != nil {
			e.logger.Warn("rollout engine: failed to list rollouts", zap.String("state", string(st)), zap.Error(err))
			continue
		}
		for _, r := range list {
			e.process(ctx, r)
		}
	}
}

// process advances one rollout by one step (start, advance, complete, or
// roll back).
func (e *Engine) process(ctx context.Context, r *services.Rollout) {
	switch r.State {
	case services.RolloutStatePending:
		e.start(ctx, r)
	case services.RolloutStateInProgress:
		e.advanceOrCheck(ctx, r)
	case services.RolloutStateAborted:
		e.rollback(ctx, r)
	}
}

// start transitions a pending rollout to in_progress and applies stage 0.
func (e *Engine) start(ctx context.Context, r *services.Rollout) {
	r.State = services.RolloutStateInProgress
	now := time.Now().UTC()
	r.StageStartedAt = &now

	ids, err := e.applyStage(ctx, r, r.CurrentStage)
	if err != nil {
		e.logger.Warn("rollout engine: failed to apply initial stage; will retry next tick",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	if err := e.rolloutService.Persist(ctx, r); err != nil {
		e.logger.Warn("rollout engine: failed to persist start", zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	e.recordAudit(ctx, r, "rollout.stage_applied", "stage_applied", stageAuditPayload(r, r.CurrentStage, ids))
	// Surface zero-canary stages as a distinct audit event so post-mortems
	// don't have to guess why nothing happened. Common case: label
	// selector typo at create time, group emptied after creation.
	if len(ids) == 0 {
		e.recordAudit(ctx, r, "rollout.empty_canary", "empty_canary", stageAuditPayload(r, r.CurrentStage, ids))
	}
	e.publishStateChange(r, "stage_applied")
	e.logger.Info("rollout started",
		zap.String("rollout_id", r.ID),
		zap.Int("stage", r.CurrentStage),
		zap.Int("canary_size", len(ids)))
}

// advanceOrCheck inspects an in-progress rollout: if dwell hasn't elapsed,
// check abort criteria; if dwell elapsed, advance (or finish).
func (e *Engine) advanceOrCheck(ctx context.Context, r *services.Rollout) {
	if r.CurrentStage >= len(r.Stages) {
		// Defensive — shouldn't happen.
		e.finish(ctx, r)
		return
	}
	stage := r.Stages[r.CurrentStage]

	// Check abort criteria first; an abort can short-circuit a stage even
	// before dwell finishes.
	if reason := e.evaluateAbortCriteria(ctx, r, stage); reason != "" {
		e.triggerAbort(ctx, r, reason)
		return
	}

	dwellElapsed := r.StageStartedAt != nil &&
		time.Since(*r.StageStartedAt) >= time.Duration(stage.DwellSeconds)*time.Second

	if !dwellElapsed {
		return
	}

	// Dwell elapsed and criteria fine — promote.
	if r.CurrentStage == len(r.Stages)-1 {
		// Last stage cleared its dwell window — done.
		e.finish(ctx, r)
		return
	}
	r.CurrentStage++
	now := time.Now().UTC()
	r.StageStartedAt = &now
	ids, err := e.applyStage(ctx, r, r.CurrentStage)
	if err != nil {
		e.logger.Warn("rollout engine: failed to apply next stage; will retry",
			zap.String("rollout_id", r.ID), zap.Error(err))
		// Roll back the in-memory advance so the next tick retries the
		// same stage. Don't persist this transient failure.
		r.CurrentStage--
		return
	}
	if err := e.rolloutService.Persist(ctx, r); err != nil {
		e.logger.Warn("rollout engine: failed to persist advance", zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	e.recordAudit(ctx, r, "rollout.stage_applied", "stage_applied", stageAuditPayload(r, r.CurrentStage, ids))
	e.publishStateChange(r, "stage_applied")
	e.logger.Info("rollout advanced",
		zap.String("rollout_id", r.ID),
		zap.Int("stage", r.CurrentStage),
		zap.Int("canary_size", len(ids)))
}

// finish marks the rollout as succeeded.
func (e *Engine) finish(ctx context.Context, r *services.Rollout) {
	r.State = services.RolloutStateSucceeded
	now := time.Now().UTC()
	r.CompletedAt = &now
	if err := e.rolloutService.Persist(ctx, r); err != nil {
		e.logger.Warn("rollout engine: failed to persist completion", zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	e.recordAudit(ctx, r, "rollout.succeeded", "succeeded", nil)
	e.publishStateChange(r, "succeeded")
	e.logger.Info("rollout succeeded", zap.String("rollout_id", r.ID))
}

// triggerAbort flips state to aborted with a reason. The next tick will
// pick it up and perform the actual rollback push.
func (e *Engine) triggerAbort(ctx context.Context, r *services.Rollout, reason string) {
	r.State = services.RolloutStateAborted
	r.AbortReason = reason
	if err := e.rolloutService.Persist(ctx, r); err != nil {
		e.logger.Warn("rollout engine: failed to persist abort", zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	e.recordAudit(ctx, r, "rollout.aborted", "aborted", map[string]any{"reason": reason})
	e.publishStateChange(r, "aborted")
	e.logger.Warn("rollout auto-aborted",
		zap.String("rollout_id", r.ID),
		zap.String("reason", reason))
}

// rollback pushes the previous config back to every canary agent and
// transitions to rolled_back.
func (e *Engine) rollback(ctx context.Context, r *services.Rollout) {
	defer func() {
		// Whatever happened in the rollback (success, partial, complete
		// failure), mark rolled_back and move on. The audit trail captures
		// detail.
		r.State = services.RolloutStateRolledBack
		now := time.Now().UTC()
		r.CompletedAt = &now
		if err := e.rolloutService.Persist(ctx, r); err != nil {
			e.logger.Warn("rollout engine: failed to persist rolled_back state",
				zap.String("rollout_id", r.ID), zap.Error(err))
		}
		e.recordAudit(ctx, r, "rollout.rolled_back", "rolled_back", nil)
		e.publishStateChange(r, "rolled_back")
	}()

	if r.PreviousConfigID == "" {
		e.logger.Warn("rollout engine: no previous config snapshot — canary will stay on target config",
			zap.String("rollout_id", r.ID))
		return
	}

	previous, err := e.configStore.GetConfig(ctx, r.PreviousConfigID)
	if err != nil || previous == nil {
		e.logger.Warn("rollout engine: previous config unreadable — skipping rollback push",
			zap.String("rollout_id", r.ID),
			zap.String("previous_config_id", r.PreviousConfigID),
			zap.Error(err))
		return
	}

	canary, err := e.canaryAgents(ctx, r)
	if err != nil {
		e.logger.Warn("rollout engine: failed to compute canary set for rollback",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}

	for _, agent := range canary {
		if err := e.commander.SendConfigToAgent(agent.ID, previous.Content); err != nil {
			e.logger.Warn("rollout engine: rollback push failed for agent",
				zap.String("rollout_id", r.ID),
				zap.String("agent_id", agent.ID.String()),
				zap.Error(err))
		}
	}
}

// applyStage pushes the target config to the resolved canary set for the
// given stage. Selection is delegated to canaryAgentsForStage (percent or
// label depending on stage.Mode). The resolved agent IDs are returned so
// the caller can attach them to the stage_applied audit payload —
// operators reading a post-mortem need to see exactly which hosts got the
// new config, regardless of how they were selected.
//
// An empty canary set is treated as a soft warning, not a failure: the
// stage still "applies" (dwell starts, the rollout will advance), but
// the warning surfaces in logs + audit so operators notice a
// misconfigured label selector or an emptied group. The alternative —
// failing the stage — risks getting stuck retrying forever when a
// percent-mode rollout happens to fire against an empty group.
func (e *Engine) applyStage(ctx context.Context, r *services.Rollout, stageIdx int) ([]uuid.UUID, error) {
	target, err := e.configStore.GetConfig(ctx, r.TargetConfigID)
	if err != nil {
		return nil, fmt.Errorf("failed to load target config: %w", err)
	}
	if target == nil {
		return nil, fmt.Errorf("target config %s no longer exists", r.TargetConfigID)
	}
	canary, err := e.canaryAgentsForStage(ctx, r, stageIdx)
	if err != nil {
		return nil, err
	}
	if len(canary) == 0 {
		stage := r.Stages[stageIdx]
		fields := []zap.Field{
			zap.String("rollout_id", r.ID),
			zap.String("group_id", r.GroupID),
			zap.Int("stage", stageIdx),
			zap.String("mode", string(stage.Mode)),
		}
		if stage.Mode == services.RolloutStageModeLabel {
			fields = append(fields, zap.Any("label_selector", stage.LabelSelector))
		}
		e.logger.Warn("rollout engine: stage resolved to zero canary agents", fields...)
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(canary))
	for _, agent := range canary {
		ids = append(ids, agent.ID)
		if err := e.commander.SendConfigToAgent(agent.ID, target.Content); err != nil {
			e.logger.Warn("rollout engine: stage push failed for agent",
				zap.String("rollout_id", r.ID),
				zap.String("agent_id", agent.ID.String()),
				zap.Error(err))
			// We tolerate per-agent failures — the next tick can retry
			// when the agent reconnects. We don't fail the whole stage.
		}
	}
	return ids, nil
}

// evaluateAbortCriteria returns a non-empty reason string if the rollout
// should be aborted now, or "" if it should continue.
func (e *Engine) evaluateAbortCriteria(ctx context.Context, r *services.Rollout, stage services.RolloutStage) string {
	canary, err := e.canaryAgents(ctx, r)
	if err != nil {
		// Don't auto-abort on transient list failures.
		return ""
	}

	driftedCount := 0
	for _, a := range canary {
		if a.DriftStatus == services.ConfigDriftStatusDrifted {
			driftedCount++
		}
	}
	if driftedCount > r.AbortCriteria.MaxDriftedAgents {
		return fmt.Sprintf("%d canary agent(s) drifted (max %d)",
			driftedCount, r.AbortCriteria.MaxDriftedAgents)
	}

	// Error-rate abort: only if both the criterion and a telemetry reader
	// are wired in, and only after the configurable warmup window (so
	// newly-pushed agents have time to flush startup noise).
	if r.AbortCriteria.MaxErrorLogsPerMinute > 0 && e.telemetry != nil {
		warmup := time.Duration(r.AbortCriteria.MinDwellSecondsBeforeAbort) * time.Second
		if warmup == 0 {
			warmup = 30 * time.Second
		}
		if r.StageStartedAt != nil && time.Since(*r.StageStartedAt) >= warmup {
			ids := make([]uuid.UUID, 0, len(canary))
			for _, a := range canary {
				ids = append(ids, a.ID)
			}
			rate, err := e.telemetry.CanaryErrorLogsPerMinute(ctx, ids, *r.StageStartedAt)
			if err == nil && rate > float64(r.AbortCriteria.MaxErrorLogsPerMinute) {
				return fmt.Sprintf("canary error log rate %.1f/min exceeded max %d/min",
					rate, r.AbortCriteria.MaxErrorLogsPerMinute)
			}
		}
	}

	_ = stage // additional criteria can plug in here
	return ""
}

// canaryAgents returns the canary set for the rollout's CURRENT stage —
// the agents that have actually had the new config pushed to them.
func (e *Engine) canaryAgents(ctx context.Context, r *services.Rollout) ([]*services.Agent, error) {
	return e.canaryAgentsForStage(ctx, r, r.CurrentStage)
}

// canaryAgentsForStage returns the deterministic canary set for the given
// stage index. The selection strategy depends on the stage's Mode:
//
//   - percent: take the first stage.Percentage % of the group's agents
//     sorted by ID. Stage K+1's canary is a guaranteed superset of stage
//     K's, so an agent that received a stage-K push will continue to
//     receive subsequent pushes.
//
//   - label: AND-match the stage's LabelSelector against each agent's
//     labels (all key=value pairs must equal). The selector is evaluated
//     fresh each tick so newly-added agents with matching labels join the
//     canary automatically; conversely, an agent re-labeled mid-rollout
//     can drop out. This is intentional — label-mode rollouts pick
//     agents by intent ("the canary host", "the staging shard"), not by
//     historical membership.
//
// In both modes the result is filtered to the rollout's target group and
// sorted by ID for stable output (deterministic test fixtures, audit-log
// reproducibility).
func (e *Engine) canaryAgentsForStage(ctx context.Context, r *services.Rollout, stageIdx int) ([]*services.Agent, error) {
	if stageIdx < 0 || stageIdx >= len(r.Stages) {
		return nil, fmt.Errorf("stage index %d out of range (rollout has %d stages)", stageIdx, len(r.Stages))
	}
	allAgents, err := e.agentService.ListAgents(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list agents: %w", err)
	}

	// Filter to this group first — regardless of mode, the canary is
	// always scoped to the rollout's target group. Operators rely on this
	// to keep label-mode selectors short (no need to repeat group filters
	// in every selector).
	groupAgents := make([]*services.Agent, 0)
	for _, a := range allAgents {
		if a.GroupID != nil && *a.GroupID == r.GroupID {
			groupAgents = append(groupAgents, a)
		}
	}
	if len(groupAgents) == 0 {
		return nil, nil
	}
	// Deterministic order. Done before selection so test fixtures and
	// audit-log "agent_ids" lists come out in the same order on every run.
	sort.Slice(groupAgents, func(i, j int) bool {
		return groupAgents[i].ID.String() < groupAgents[j].ID.String()
	})

	stage := r.Stages[stageIdx]
	mode := stage.Mode
	if mode == "" {
		// Backward compatibility: pre-v0.6 rollouts stored stages without
		// a mode field. Treat them as percent mode.
		mode = services.RolloutStageModePercent
	}

	switch mode {
	case services.RolloutStageModeLabel:
		return matchByLabel(groupAgents, stage.LabelSelector), nil
	case services.RolloutStageModePercent:
		fallthrough
	default:
		pct := stage.Percentage
		// ceil so 1 agent at 10% still gets pushed (rather than rounding to zero).
		n := (len(groupAgents)*pct + 99) / 100
		if n > len(groupAgents) {
			n = len(groupAgents)
		}
		return groupAgents[:n], nil
	}
}

// matchByLabel returns agents whose Labels map contains every key=value
// pair in selector (AND semantics). Empty selector returns no agents —
// validation rejects empty selectors at create time, so reaching this
// with an empty selector is a programmer error; we fail closed to avoid
// accidentally pushing to the whole group.
func matchByLabel(agents []*services.Agent, selector map[string]string) []*services.Agent {
	if len(selector) == 0 {
		return nil
	}
	out := make([]*services.Agent, 0, len(agents))
agentLoop:
	for _, a := range agents {
		for k, v := range selector {
			if got, ok := a.Labels[k]; !ok || got != v {
				continue agentLoop
			}
		}
		out = append(out, a)
	}
	return out
}

// stageAuditPayload builds the audit payload for a stage_applied event.
// Captures stage index, mode, selection criteria, and the resolved agent
// id list — the post-mortem-critical bit. We stringify agent IDs because
// the audit Payload eventually round-trips through JSON, and uuid.UUID
// marshals as a string anyway; keeping it uniform here means the SQLite
// row matches the wire format byte-for-byte.
func stageAuditPayload(r *services.Rollout, stageIdx int, ids []uuid.UUID) map[string]any {
	stage := r.Stages[stageIdx]
	agentIDs := make([]string, len(ids))
	for i, id := range ids {
		agentIDs[i] = id.String()
	}
	mode := stage.Mode
	if mode == "" {
		mode = services.RolloutStageModePercent
	}
	out := map[string]any{
		"stage":       stageIdx,
		"mode":        string(mode),
		"canary_size": len(agentIDs),
		"agent_ids":   agentIDs,
	}
	switch mode {
	case services.RolloutStageModePercent:
		out["percentage"] = stage.Percentage
	case services.RolloutStageModeLabel:
		out["label_selector"] = stage.LabelSelector
	}
	return out
}

func (e *Engine) recordAudit(ctx context.Context, r *services.Rollout, eventType, action string, payload map[string]any) {
	if e.auditService == nil {
		return
	}
	full := map[string]any{
		"name":     r.Name,
		"group_id": r.GroupID,
		"state":    string(r.State),
	}
	for k, v := range payload {
		full[k] = v
	}
	_ = e.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  eventType,
		TargetType: "rollout",
		TargetID:   r.ID,
		Action:     action,
		Payload:    full,
	})
}
