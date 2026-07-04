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
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/changewindow"
	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/configs"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// tickInterval is how often the engine wakes up to scan rollouts. Per-stage
// dwells are typically minutes, so 5s tick is plenty without being chatty.
const tickInterval = 5 * time.Second

// AgentCommander is the subset of opamp.ConfigSender the engine needs.
// Defined as an interface here so tests can plug in a mock without pulling
// the OpAMP machinery.
//
// SendConfigToAgentWithContext is the trace-aware variant used by
// applyStage and rollback so the per-push OTel span context rides along
// into the OpAMP CustomMessage (see internal/opamp/traceparent.go).
// SendConfigToAgent stays for any non-traced callsite + back-compat for
// existing test mocks.
type AgentCommander interface {
	SendConfigToAgent(agentID uuid.UUID, content string) error
	SendConfigToAgentWithContext(ctx context.Context, agentID uuid.UUID, content string) error
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
	broker         *events.Broker  // optional; nil disables SSE publication
	httpClient     *http.Client    // for webhook notifications
	tracer         *Tracer         // optional; nil disables OTel rollout traces
	configsTracer  *configs.Tracer // optional; nil disables OTel config-push spans
	// changeWindowProvider is the boundary between the open core
	// (which stores change_windows as group metadata) and the
	// Compliance Pack (which enforces them). nil is treated as
	// NoOpProvider — no enforcement. Wired post-construction via
	// SetChangeWindowProvider so the Compliance Pack build can plug
	// in its own implementation without changing the OSS NewEngine
	// signature. Added in v0.52 as part of the open-core split.
	changeWindowProvider changewindow.Provider
	// metrics instruments the tick loop (duration / slow-tick
	// counters). Wired post-construction via SetMetrics; nil
	// disables recording. Added in the v0.89 scale pass.
	metrics *metrics.RolloutMetrics
	// v0.89.14 (#630) — action runner steps in plans, slice 1.
	// actionDispatcher is the boundary the engine uses to sign +
	// persist action_requests for kind=action plan steps and to
	// poll the runner's reported result back into the plan
	// lifecycle. nil disables action-step support — pure rollout
	// plans run unchanged. Wired via SetActionDispatcher post-
	// construction so the OSS NewEngine signature stays stable
	// (mirrors the changeWindowProvider pattern).
	actionDispatcher services.ActionDispatcher
	logger           *zap.Logger

	shutdown chan struct{}
	wg       sync.WaitGroup
}

// SetActionDispatcher wires the plan-engine boundary to the action
// runner substrate. nil is a valid value and disables action-step
// dispatch — the engine's forward walk for action steps becomes a
// no-op (the step sits in pending without ever progressing). v0.89.14.
func (e *Engine) SetActionDispatcher(d services.ActionDispatcher) {
	e.actionDispatcher = d
}

// SetChangeWindowProvider wires the Compliance Pack's blackout
// enforcement. Called by the wire layer (wire_oss.go installs
// NoOpProvider; wire_compliance.go installs the real one). nil is
// a valid value and disables enforcement (the OSS default).
// SetMetrics wires the engine's tick instrumentation. Setter (not a
// NewEngine parameter) for the same back-compat reason as
// SetActionDispatcher — existing constructors and test mocks stay
// untouched; a nil receiver field simply disables recording.
func (e *Engine) SetMetrics(m *metrics.RolloutMetrics) {
	e.metrics = m
}

func (e *Engine) SetChangeWindowProvider(p changewindow.Provider) {
	e.changeWindowProvider = p
}

// RolloutService returns the underlying rollout service. Exposed
// so engine-level extension wiring (Compliance Pack) can recover
// the application store without needing it threaded through a
// widened constructor signature. Read-only by convention.
func (e *Engine) RolloutService() services.RolloutService {
	return e.rolloutService
}

// persist writes the rollout via the service and classifies the outcome. A
// version conflict (ErrRolloutVersionConflict) means a concurrent writer —
// almost always an operator Pause/Abort/Resume — updated the row after this
// tick's List snapshot was taken. That writer's intent takes precedence, so the
// conflict is an EXPECTED event, not an error: the engine yields (records the
// collision metric + a rollout.update_superseded audit, logs at Info) and the
// caller skips the rest of this tick's work. The engine re-lists from storage
// every tick (~5s), so it re-reads the operator's new state and re-decides
// against it next pass — no state is lost, the operator simply wins the race.
// Any other persist error is logged at Warn (the prior behavior).
//
// `action` names the transition for the log/metric/audit. The error is returned
// unchanged so existing call sites keep their control flow (return vs continue).
// Sites where a real side-effect (an agent config push) has already fired use
// persistWithReconcile instead, so the DB converges within the same tick.
func (e *Engine) persist(ctx context.Context, r *services.Rollout, action string) error {
	err := e.rolloutService.Persist(ctx, r)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, applicationstore.ErrRolloutVersionConflict):
		e.noteConflict()
		e.noteYield(ctx, r, action, "skipped; reconciles next tick")
	default:
		e.logger.Warn("rollout engine: persist failed",
			zap.String("rollout_id", r.ID), zap.String("action", action), zap.Error(err))
	}
	return err
}

// persistWithReconcile is persist for the stage-apply sites (start / advance /
// rollback), where the agent config push has ALREADY fired before the write. A
// plain yield there would leave the stored state one tick behind agent reality
// until the next active tick reconciled it. Instead, on a version conflict the
// engine reloads the fresh row and asks `reapply` whether its transition still
// holds against the concurrent writer's state:
//
//   - reapply returns true → it has re-projected the transition onto `fresh`;
//     the engine persists once more so the DB converges THIS tick, copies the
//     reconciled state back onto r (so the caller's subsequent audit/publish
//     see it), counts a retry, and returns nil.
//   - reapply returns false → the reloaded state no longer permits the
//     transition (an operator Pause/Abort won); the engine yields cleanly
//     (nil error — the operator's intent stands and there is nothing to do).
//
// The retry is bounded to a single attempt: a second conflict, a failed reload,
// or a persist error yields rather than looping, so a hot row can never spin the
// tick. Any non-conflict persist error is returned as before.
func (e *Engine) persistWithReconcile(ctx context.Context, r *services.Rollout, action string, reapply func(fresh *services.Rollout) bool) error {
	err := e.rolloutService.Persist(ctx, r)
	if err == nil {
		return nil
	}
	if !errors.Is(err, applicationstore.ErrRolloutVersionConflict) {
		e.logger.Warn("rollout engine: persist failed",
			zap.String("rollout_id", r.ID), zap.String("action", action), zap.Error(err))
		return err
	}
	e.noteConflict()

	fresh, gErr := e.rolloutService.Get(ctx, r.ID)
	if gErr != nil || fresh == nil {
		e.noteYield(ctx, r, action, "reload failed after conflict; reconciles next tick")
		return err
	}
	if !reapply(fresh) {
		// The concurrent writer's state makes the transition moot — a clean,
		// expected win for the operator; not an error for the caller.
		e.noteYield(ctx, r, action, "concurrent state supersedes transition")
		return nil
	}
	if rErr := e.rolloutService.Persist(ctx, fresh); rErr != nil {
		e.noteYield(ctx, r, action, "reconcile retry re-conflicted; reconciles next tick")
		return rErr
	}
	// Reflect the reconciled row back onto r so the caller's follow-up audit /
	// publishStateChange render the converged state, not the pre-reload copy.
	*r = *fresh
	e.noteRetry()
	e.logger.Info("rollout engine: reconciled after a concurrent write; store converged this tick",
		zap.String("rollout_id", r.ID), zap.String("action", action))
	return nil
}

// noteConflict / noteRetry / noteYield centralize the collision telemetry so the
// metric increments stay nil-safe (metrics are optional) and the yield path
// consistently emits the operator-facing audit event.
func (e *Engine) noteConflict() {
	if e.metrics != nil {
		e.metrics.VersionConflicts.Inc(1)
	}
}

func (e *Engine) noteRetry() {
	if e.metrics != nil {
		e.metrics.VersionConflictsRetried.Inc(1)
	}
}

// noteYield records the guard deferring to a concurrent write: a metric bump, an
// Info log, and a rollout.update_superseded audit event so the operator can SEE
// on the timeline (and via SIEM) that their Pause/Abort took precedence over the
// engine's `action`.
func (e *Engine) noteYield(ctx context.Context, r *services.Rollout, action, resolution string) {
	if e.metrics != nil {
		e.metrics.VersionConflictsYielded.Inc(1)
	}
	e.logger.Info("rollout engine: update superseded by a concurrent write; operator's change stands",
		zap.String("rollout_id", r.ID), zap.String("action", action), zap.String("resolution", resolution))
	e.recordAudit(ctx, r, "rollout.update_superseded", "update_superseded", map[string]any{
		"engine_action": action,
		"resolution":    resolution,
	})
}

// AgentService returns the underlying agent service. Exposed so
// engine-level extension wiring can read group records (with
// parsed ChangeWindows) without depending on the lower-level
// applicationstore. Read-only by convention.
func (e *Engine) AgentService() services.AgentService {
	return e.agentService
}

// NewEngine wires up the engine. auditService, telemetry, broker,
// tracer, and configsTracer are all optional — pass nil to disable
// that feature.
func NewEngine(
	rolloutService services.RolloutService,
	agentService services.AgentService,
	auditService services.AuditService,
	configStore ConfigStore,
	telemetry TelemetryReader,
	commander AgentCommander,
	broker *events.Broker,
	tracer *Tracer,
	configsTracer *configs.Tracer,
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
		tracer:         tracer,
		configsTracer:  configsTracer,
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
	// v0.53 — proposal provenance. When the rollout came from the
	// AI proposer, surface origin, the natural-language reasoning,
	// the top three evidence references, and a slack_blocks render
	// of the same. Receivers that just want JSON keep their old
	// payload shape (the new fields are additive). Receivers that
	// speak Slack Block Kit can render slack_blocks directly. The
	// engine fires this on every transition so a Slack approver
	// sees the AI's reasoning the moment the rollout enters
	// pending_approval.
	if r.ProposedBy != "" {
		payload["proposed_by"] = r.ProposedBy
	}
	if r.ProposedBy == services.RolloutProposedByAI {
		if r.ProposalReasoning != "" {
			payload["proposal_reasoning"] = r.ProposalReasoning
		}
		if len(r.EvidenceRefs) > 0 {
			payload["evidence_refs"] = topEvidence(r.EvidenceRefs, 3)
		}
		payload["slack_blocks"] = aiProposalSlackBlocks(r, transition)
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
		// Flush any still-open rollout spans so trace exports include
		// the in-flight rollouts as truncated rather than silently
		// dropped.
		e.tracer.Shutdown()
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
//
// Instrumented since the v0.89 scale pass: per-tick work grows with
// fleet size × active rollouts (abort-criteria evaluation lists the
// whole fleet per rollout; a stage advance pushes config to each
// canary agent synchronously), so tick duration against the 5s budget
// is the engine's primary health signal.
func (e *Engine) tick() {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		if e.metrics != nil {
			e.metrics.TickDuration.Record(elapsed)
			e.metrics.TicksTotal.Inc(1)
			if elapsed > tickInterval {
				e.metrics.SlowTicks.Inc(1)
			}
		}
		if elapsed > tickInterval {
			e.logger.Warn("rollout engine tick exceeded tick interval",
				zap.Duration("elapsed", elapsed),
				zap.Duration("budget", tickInterval))
		}
	}()

	// ADR 0011: the engine advances EVERY tenant's rollouts each tick, so it
	// runs on a system (all-tenant) context. Inert in OSS (the pass-through
	// store ignores the tenant); the enterprise scoped store reads a system
	// context as "apply no tenant predicate" so no tenant's rollouts stall.
	ctx, cancel := context.WithTimeout(identity.WithSystemContext(context.Background()), 30*time.Second)
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
	// Restart recovery: an in_progress / aborted rollout might have
	// been started under a previous Squadron process. The trace span
	// doesn't exist in our in-memory map, so reopen one mid-flight.
	// The recovered span will be missing the early stages' history
	// but at least the rest of the lifecycle gets traced.
	if r.State == services.RolloutStateInProgress || r.State == services.RolloutStateAborted {
		e.tracer.BeginRollout(ctx, r)
	}
	// v0.49 — change-window enforcement. Before advancement (start
	// or advanceOrCheck), check whether the target group has an
	// active blackout window. If so, skip this tick. The blackout
	// reason is persisted on the rollout so the UI can render a
	// badge; cleared on the next successful advancement. The
	// aborted path is exempt — rollbacks proceed even during
	// blackouts because the situation that triggered the abort
	// (drift threshold breach, error spike) is more urgent than
	// the change-window policy.
	if r.State == services.RolloutStatePending || r.State == services.RolloutStateInProgress {
		if e.applyBlackoutCheck(ctx, r) {
			return
		}
	}
	// v0.89.14 (#630) — action steps follow a separate lifecycle:
	// pending → dispatched (in_progress) → succeeded / aborted via
	// runner result or engine-side timeout. Branch on StepKind
	// before falling through to the v0.4–v0.89.13 rollout path so
	// the existing state-machine for kind=rollout stays unchanged.
	if r.StepKind == services.StepKindAction {
		e.processActionStep(ctx, r)
		return
	}
	switch r.State {
	case services.RolloutStatePending:
		e.start(ctx, r)
	case services.RolloutStateInProgress:
		e.advanceOrCheck(ctx, r)
	case services.RolloutStateAborted:
		e.rollback(ctx, r)
	}
}

// applyBlackoutCheck consults the configured changewindow.Provider
// (NoOp in the OSS build, real implementation in the Compliance
// Pack) and, if a window is active, persists the blackout reason on
// the rollout and returns true (= skip this tick's advancement).
// If no window is active and the rollout was previously in a
// blackout, clears the reason so the UI badge disappears.
//
// Debounced audit: only records "rollout.blackout_blocked" the
// first time we hit a blackout for a given (rollout, window) pair.
// The tracer span event fires every check so the trace shows the
// full gap; the audit event is once-per-window to keep the audit
// log readable.
//
// Added in v0.49. Moved behind the Provider interface in v0.52.
func (e *Engine) applyBlackoutCheck(ctx context.Context, r *services.Rollout) bool {
	// v0.52 — enforcement moved behind the changewindow.Provider
	// interface so the Compliance Pack (private repo) owns the
	// blocking decision. With no provider wired (the OSS default,
	// NoOpProvider), every check returns nil window and the engine
	// proceeds. Groups can still carry change_windows as metadata
	// in the OSS build; they just don't enforce.
	if e.changeWindowProvider == nil {
		return false
	}
	active := e.changeWindowProvider.ActiveWindow(ctx, r.GroupID, time.Now())
	if active == nil {
		// No active window. If we were previously blocked, clear
		// the reason so the badge disappears.
		if r.LastBlackoutReason != "" {
			r.LastBlackoutReason = ""
			r.LastBlackoutAt = nil
			_ = e.persist(ctx, r, "clear blackout reason")
		}
		return false
	}
	// Active blackout — refuse to advance. Persist the reason
	// (idempotent — same reason on consecutive ticks is fine).
	previousReason := r.LastBlackoutReason
	now := time.Now().UTC()
	r.LastBlackoutReason = active.Name
	r.LastBlackoutAt = &now
	_ = e.persist(ctx, r, "persist blackout reason")
	// Audit once per (rollout, window) pair so the log doesn't
	// thrash every 5s. Detect via reason change.
	if previousReason != active.Name {
		e.recordAudit(ctx, r, "rollout.blackout_blocked", "blackout_blocked",
			map[string]any{
				"window_name":     active.Name,
				"window_timezone": active.Timezone,
				"window_start":    active.StartLocal,
				"window_end":      active.EndLocal,
			})
	}
	// Tracer span event every tick so the trace shows the duration
	// the rollout sat in blackout. Nil-safe — tracer may be no-op.
	e.tracer.RecordEvent(r.ID, "blackout_blocked", active.Name)
	return true
}

// start transitions a pending rollout to in_progress and applies stage 0.
func (e *Engine) start(ctx context.Context, r *services.Rollout) {
	r.State = services.RolloutStateInProgress
	now := time.Now().UTC()
	r.StageStartedAt = &now

	// Open the parent OTel span before applying. The span stays open
	// across many engine ticks; end-of-lifecycle handlers (finish,
	// rollback) close it. If applyStage fails below we leave the span
	// open — the next tick will retry and the operator will see a
	// rollout span that's still recording.
	e.tracer.BeginRollout(ctx, r)

	ids, err := e.applyStage(ctx, r, r.CurrentStage)
	if err != nil {
		e.logger.Warn("rollout engine: failed to apply initial stage; will retry next tick",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}
	// The stage-0 config push already fired; reconcile so the store converges
	// this tick unless an operator moved the rollout off pending in the window.
	targetStage := r.CurrentStage
	startedAt := r.StageStartedAt
	if err := e.persistWithReconcile(ctx, r, "start", func(fresh *services.Rollout) bool {
		if fresh.State != services.RolloutStatePending {
			return false // an operator Pause/Abort won the race — honor it
		}
		fresh.State = services.RolloutStateInProgress
		fresh.CurrentStage = targetStage
		fresh.StageStartedAt = startedAt
		fresh.PushedAgentIDs = r.PushedAgentIDs // ADR 0007 — carry the cumulative pushed-set through reconcile
		return true
	}); err != nil {
		return
	}
	e.recordStageApplied(ctx, r, ids)
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
	// The next-stage config push already fired; reconcile so the store converges
	// this tick. Only re-apply if the fresh row is still in_progress and exactly
	// one stage behind (our expected pre-advance position) — otherwise a
	// concurrent operator Pause/Abort or another writer moved it, and we yield.
	advancedStage := r.CurrentStage
	advancedStartedAt := r.StageStartedAt
	if err := e.persistWithReconcile(ctx, r, "advance", func(fresh *services.Rollout) bool {
		if fresh.State != services.RolloutStateInProgress {
			return false
		}
		if fresh.CurrentStage != advancedStage-1 {
			return false // stage diverged under us — don't force
		}
		fresh.CurrentStage = advancedStage
		fresh.StageStartedAt = advancedStartedAt
		fresh.PushedAgentIDs = r.PushedAgentIDs // ADR 0007 — carry the cumulative pushed-set through reconcile
		return true
	}); err != nil {
		return
	}
	// Emit the stage-applied (and, for a zero-agent stage, empty_canary)
	// audit + trace events. Shared with start() via recordStageApplied so the
	// two paths can't drift.
	e.recordStageApplied(ctx, r, ids)
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
	if err := e.persist(ctx, r, "finish"); err != nil {
		return
	}
	e.recordAudit(ctx, r, "rollout.succeeded", "succeeded", nil)
	// v0.61 — if this rollout was created via the rollback endpoint
	// (RolledBackFromID set on create) and it has now reached
	// succeeded, emit a separate completion event so the audit
	// timeline shows the full arc: rollback_requested → succeeded →
	// rollback_completed. SIEM consumers can alert on
	// rollback_completed independently of generic succeeded events.
	if r.RolledBackFromID != "" {
		e.recordAudit(ctx, r, "rollout.rollback_completed", "rollback_completed", map[string]any{
			"rolled_back_from_id": r.RolledBackFromID,
		})
	}
	e.publishStateChange(r, "succeeded")
	e.tracer.EndRollout(r.ID, services.RolloutStateSucceeded, "")
	e.logger.Info("rollout succeeded", zap.String("rollout_id", r.ID))

	// v0.70 — multi step plan advancement. When the succeeded
	// rollout belongs to a plan, promote the next step out of
	// Queued so the next tick picks it up. When there is no next
	// step, emit plan.completed so the audit timeline closes the
	// arc and SIEM consumers see one terminal event per plan.
	// Standalone rollouts (empty PlanID) skip this branch.
	if r.PlanID != "" {
		e.advancePlan(ctx, r)
	}
}

// advancePlan promotes the next step in r's plan from Queued to
// Pending, or emits plan.completed if r was the final step. Called
// from finish() only — the failure path (cancellation + backwards
// rollback) lands in v0.71.
func (e *Engine) advancePlan(ctx context.Context, r *services.Rollout) {
	next, err := e.rolloutService.NextPlanStep(ctx, r.PlanID, r.PlanStepIndex)
	if err != nil {
		e.logger.Warn("rollout engine: failed to look up next plan step",
			zap.String("rollout_id", r.ID),
			zap.String("plan_id", r.PlanID),
			zap.Int("current_step", r.PlanStepIndex),
			zap.Error(err))
		return
	}
	if next == nil {
		// r was the final step. Emit plan.completed against r so the
		// audit row links to the last forward rollout in the plan.
		e.recordAudit(ctx, r, "plan.completed", "plan_completed", map[string]any{
			"plan_id":     r.PlanID,
			"final_step":  r.PlanStepIndex,
			"total_steps": r.PlanStepIndex + 1,
		})
		e.logger.Info("plan completed",
			zap.String("plan_id", r.PlanID),
			zap.Int("final_step", r.PlanStepIndex))
		return
	}
	// Promote the next step. The expected state is Queued (the
	// service Create logic puts plan steps after step 0 in Queued).
	// If the next step is already in some other state (operator
	// manually intervened, the failure path got there first), leave
	// it alone — better to no op than to clobber an operator action.
	if next.State != services.RolloutStateQueued {
		e.logger.Info("plan engine: next step not in queued state, leaving alone",
			zap.String("plan_id", r.PlanID),
			zap.Int("next_step", next.PlanStepIndex),
			zap.String("next_state", string(next.State)))
		return
	}
	next.State = services.RolloutStatePending
	if err := e.persist(ctx, next, "promote next plan step"); err != nil {
		return
	}
	e.recordAudit(ctx, next, "plan.step_started", "plan_step_started", map[string]any{
		"plan_id":         next.PlanID,
		"plan_step_index": next.PlanStepIndex,
		"previous_step":   r.PlanStepIndex,
	})
	e.logger.Info("plan advanced",
		zap.String("plan_id", r.PlanID),
		zap.Int("from_step", r.PlanStepIndex),
		zap.Int("to_step", next.PlanStepIndex))
}

// triggerAbort flips state to aborted with a reason. The next tick will
// pick it up and perform the actual rollback push.
func (e *Engine) triggerAbort(ctx context.Context, r *services.Rollout, reason string) {
	r.State = services.RolloutStateAborted
	r.AbortReason = reason
	if err := e.persist(ctx, r, "abort"); err != nil {
		return
	}
	e.recordAudit(ctx, r, "rollout.aborted", "aborted", map[string]any{"reason": reason})
	e.publishStateChange(r, "aborted")
	// Mark the rollout span as aborted but DON'T end it here — the
	// engine's next tick will run rollback() which performs the actual
	// rollback push and ends the span with the rolled_back state.
	// Recording the event now means a trace consumer sees the abort
	// reason while the rollback push is still in flight.
	e.tracer.RecordEvent(r.ID, "aborted", reason)
	e.logger.Warn("rollout auto-aborted",
		zap.String("rollout_id", r.ID),
		zap.String("reason", reason))

	// v0.71 + v0.72 — if this aborted rollout belongs to a plan,
	// cancel every queued step that follows and roll back every
	// succeeded forward step. Order matters: cancellation is fast
	// (just state transitions) and stops further forward work
	// before the backwards walk starts spawning rollback rollouts.
	if r.PlanID != "" {
		e.cancelPlanFollowers(ctx, r, "predecessor_aborted")
		e.rollBackPlanPredecessors(ctx, r, "predecessor_aborted")
	}
}

// rollBackPlanPredecessors walks every succeeded forward step in
// r's plan and creates a rollback rollout for each. Emits one
// plan.rolled_back summary event with the list of rollback ids so
// SIEM consumers see the full backwards arc kicking off. The per
// step rollout.rollback_completed events fire as each individual
// rollback rollout completes via the v0.61 hook.
//
// Failure mode: if the walk creates fewer rollback rollouts than
// expected (the storage write for one of them failed), the audit
// payload reports the actual count plus a warning flag so the
// operator knows there's manual cleanup to do. The plan
// terminates either way — we don't retry the walk.
func (e *Engine) rollBackPlanPredecessors(ctx context.Context, r *services.Rollout, reason string) {
	rollbacks, err := e.rolloutService.RollBackPlanPredecessors(ctx, r.PlanID, r.PlanStepIndex, "system:plan_engine")
	if err != nil {
		e.logger.Warn("plan engine: rollback predecessors failed",
			zap.String("plan_id", r.PlanID),
			zap.Int("failed_step", r.PlanStepIndex),
			zap.Error(err))
		return
	}
	if len(rollbacks) == 0 {
		// No succeeded forward steps to roll back. Common when step
		// 0 itself fails — there's nothing yet to undo. No
		// plan.rolled_back event in this case; the per step
		// rollout.aborted plus the v0.71 plan.cancelled summary
		// already tell the full story.
		return
	}
	rollbackIDs := make([]string, 0, len(rollbacks))
	for _, rb := range rollbacks {
		rollbackIDs = append(rollbackIDs, rb.ID)
	}
	e.recordAudit(ctx, r, "plan.rolled_back", "plan_rolled_back", map[string]any{
		"plan_id":              r.PlanID,
		"failed_step_index":    r.PlanStepIndex,
		"reason":               reason,
		"rollback_rollout_ids": rollbackIDs,
		"rollback_count":       len(rollbackIDs),
	})
	e.logger.Warn("plan rolling back",
		zap.String("plan_id", r.PlanID),
		zap.Int("failed_step", r.PlanStepIndex),
		zap.Int("rollback_count", len(rollbackIDs)),
		zap.String("reason", reason))
}

// cancelPlanFollowers transitions every queued step in r's plan
// with index > r.PlanStepIndex to Cancelled and emits one
// plan.step_cancelled per step plus a plan.cancelled summary. The
// failure reason is passed through to the audit payload so SIEM
// consumers can route on (planID, reason) pairs.
//
// Called from triggerAbort and Reject. Safe to call on plans
// whose final step is the one that failed — the walk returns an
// empty list, no events fire, and the plan terminates naturally
// without summary noise.
func (e *Engine) cancelPlanFollowers(ctx context.Context, r *services.Rollout, reason string) {
	cancelled, err := e.rolloutService.CancelPlanFollowers(ctx, r.PlanID, r.PlanStepIndex)
	if err != nil {
		e.logger.Warn("plan engine: cancel followers failed",
			zap.String("plan_id", r.PlanID),
			zap.Int("after_step", r.PlanStepIndex),
			zap.Error(err))
		return
	}
	if len(cancelled) == 0 {
		// The failed step was the last in the plan, no followers to
		// cancel. No plan.cancelled event either — there's nothing
		// for SIEM to act on that the per step rollout.aborted didn't
		// already say.
		return
	}
	cancelledIDs := make([]string, 0, len(cancelled))
	for _, c := range cancelled {
		cancelledIDs = append(cancelledIDs, c.ID)
		e.recordAudit(ctx, c, "plan.step_cancelled", "plan_step_cancelled", map[string]any{
			"plan_id":           c.PlanID,
			"plan_step_index":   c.PlanStepIndex,
			"reason":            reason,
			"failed_step_id":    r.ID,
			"failed_step_index": r.PlanStepIndex,
		})
	}
	e.recordAudit(ctx, r, "plan.cancelled", "plan_cancelled", map[string]any{
		"plan_id":           r.PlanID,
		"failed_step_index": r.PlanStepIndex,
		"reason":            reason,
		"cancelled_count":   len(cancelled),
		"cancelled_ids":     cancelledIDs,
	})
	e.logger.Warn("plan cancelled",
		zap.String("plan_id", r.PlanID),
		zap.Int("failed_step", r.PlanStepIndex),
		zap.Int("cancelled_count", len(cancelled)),
		zap.String("reason", reason))
}

// rollback pushes the previous config back to every canary agent and
// transitions to rolled_back.
func (e *Engine) rollback(ctx context.Context, r *services.Rollout) {
	e.tracer.RecordEvent(r.ID, "rollback_started", r.AbortReason)
	defer func() {
		// Whatever happened in the rollback (success, partial, complete
		// failure), mark rolled_back and move on. The audit trail captures
		// detail.
		r.State = services.RolloutStateRolledBack
		now := time.Now().UTC()
		r.CompletedAt = &now
		// Terminal bookkeeping after the rollback push already fired — force it
		// to land this tick unless the row is somehow already rolled_back.
		completedAt := r.CompletedAt
		_ = e.persistWithReconcile(ctx, r, "rolled_back", func(fresh *services.Rollout) bool {
			if fresh.State == services.RolloutStateRolledBack {
				return false // already recorded — nothing to do
			}
			fresh.State = services.RolloutStateRolledBack
			fresh.CompletedAt = completedAt
			return true
		})
		e.recordAudit(ctx, r, "rollout.rolled_back", "rolled_back", nil)
		e.publishStateChange(r, "rolled_back")
		// End the rollout span with rolled_back status. The Error
		// status on the parent span surfaces the reason recorded at
		// abort time so trace UIs render the failed rollout red.
		e.tracer.EndRollout(r.ID, services.RolloutStateRolledBack, r.AbortReason)
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

	canary, err := e.cumulativePushedAgents(ctx, r)
	if err != nil {
		e.logger.Warn("rollout engine: failed to compute canary set for rollback",
			zap.String("rollout_id", r.ID), zap.Error(err))
		return
	}

	// Same bounded-concurrency fan-out as applyStage — a rollback is
	// the moment the operator most needs the old config back on every
	// canary agent quickly, so it must not serialize ack round-trips
	// either.
	e.pushConfigToAgents(ctx, r, canary, r.PreviousConfigID, previous.Content,
		"rollout engine: rollback push failed for agent")
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
	}
	// Push concurrently (bounded). The audit payload keeps listing
	// every ATTEMPTED agent in canary order — identical to the old
	// serial loop, which also appended before pushing.
	e.pushConfigToAgents(ctx, r, canary, r.TargetConfigID, target.Content,
		"rollout engine: stage push failed for agent")
	// ADR 0007 — record these agents into the cumulative pushed-set so
	// rollback and the health-gate cover every agent this rollout has ever
	// pushed the target config to, not just the current stage's cohort. The
	// caller persists r right after applyStage (start/advance), so the union
	// lands durably this tick.
	r.PushedAgentIDs = unionPushedAgentIDs(r.PushedAgentIDs, ids)
	return ids, nil
}

// unionPushedAgentIDs merges the given agent IDs into the existing
// (stringified) pushed-set, de-duplicating and returning a sorted slice for
// deterministic persistence and audit output. See ADR 0007.
func unionPushedAgentIDs(existing []string, ids []uuid.UUID) []string {
	seen := make(map[string]struct{}, len(existing)+len(ids))
	out := make([]string, 0, len(existing)+len(ids))
	for _, s := range existing {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	for _, id := range ids {
		s := id.String()
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// pushConcurrency bounds the parallel per-agent config pushes within
// one stage application or rollback. The v0.89 scale pass measured
// the old serial loop at one FULL ack round-trip per agent
// (SendConfigToAgentWithContext blocks until the agent confirms, and
// opamp-go clients coalesce status sends), which put a full-fleet
// stage at hours for 1000 agents. Pushes to distinct agents are
// independent WebSocket sends, so overlapping the ack waits is safe;
// the bound keeps goroutine count and OpAMP server load sane.
const pushConcurrency = 128

// pushConfigToAgents fans content out to every agent with bounded
// concurrency, preserving the old loop's per-push semantics exactly:
// one OTel push span per agent (ack/nack recorded), per-agent
// failures logged-and-tolerated (the stage/rollback proceeds — the
// next stage's superset push is the retry path).
func (e *Engine) pushConfigToAgents(ctx context.Context, r *services.Rollout, agents []*services.Agent, configID, content, failureMsg string) {
	sem := make(chan struct{}, pushConcurrency)
	var wg sync.WaitGroup
	for _, agent := range agents {
		wg.Add(1)
		sem <- struct{}{}
		go func(agent *services.Agent) {
			defer wg.Done()
			defer func() { <-sem }()
			// Each per-agent push gets its own OTel span. Bracketing
			// the synchronous SendConfigToAgentWithContext call
			// captures both the ack case (RecordAck) and the timeout /
			// agent-not-found case (RecordNack with the error message).
			push := e.configsTracer.BeginPush(ctx, agent.ID.String(), configID, r.GroupID, configs.SourceRollout)
			if err := e.commander.SendConfigToAgentWithContext(push.Context(), agent.ID, content); err != nil {
				push.RecordNack(err.Error())
				push.End()
				e.logger.Warn(failureMsg,
					zap.String("rollout_id", r.ID),
					zap.String("agent_id", agent.ID.String()),
					zap.Error(err))
				return
			}
			push.RecordAck()
			push.End()
		}(agent)
	}
	wg.Wait()
}

// evaluateAbortCriteria returns a non-empty reason string if the rollout
// should be aborted now, or "" if it should continue.
func (e *Engine) evaluateAbortCriteria(ctx context.Context, r *services.Rollout, stage services.RolloutStage) string {
	// ADR 0007 — evaluate health over the cumulative pushed-set, not just the
	// current stage's cohort, so an earlier-pushed label-mode agent that fell
	// out of the current selector still counts toward drift/error-rate aborts.
	canary, err := e.cumulativePushedAgents(ctx, r)
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
			// ADR 0008 — error-rate window. With ErrorRateWindowSeconds > 0
			// we measure errors/min over a trailing window (the last N
			// seconds) instead of averaging over the whole stage, so a late
			// burst isn't diluted by earlier clean minutes. The window start
			// is clamped to never precede stage start (early in a stage the
			// window is simply "since stage start" — identical to legacy).
			// ErrorRateWindowSeconds == 0 (pre-v0.90 / unset) keeps the
			// legacy whole-stage-average behavior unchanged.
			evalSince := *r.StageStartedAt
			if w := r.AbortCriteria.ErrorRateWindowSeconds; w > 0 {
				trailingStart := time.Now().Add(-time.Duration(w) * time.Second)
				if trailingStart.After(evalSince) {
					evalSince = trailingStart
				}
			}
			rate, err := e.telemetry.CanaryErrorLogsPerMinute(ctx, ids, evalSince)
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

// cumulativePushedAgents returns every live agent this rollout has pushed the
// target config to across all stages — the persisted cumulative pushed-set
// (r.PushedAgentIDs) resolved against the live fleet and unioned with the
// current stage's cohort. Rollback and the health-gate use this instead of the
// current-stage-only canaryAgents so a label-mode agent pushed in an earlier
// stage that no longer matches the current selector is still restored on
// rollback and still counted for abort evaluation. See ADR 0007.
//
// The union guarantees the result is always a superset of canaryAgents, so the
// cumulative set never covers *fewer* agents than the pre-ADR-0007 behavior.
// For an empty pushed-set (pre-v0.90 / in-flight rollouts) this degrades
// exactly to the current-stage cohort. Percent-mode rollouts are unaffected —
// their cohorts are already superset-progressive, so the stored set is a subset
// of the current cohort and the union adds nothing.
func (e *Engine) cumulativePushedAgents(ctx context.Context, r *services.Rollout) ([]*services.Agent, error) {
	current, err := e.canaryAgents(ctx, r)
	if err != nil {
		return nil, err
	}
	if len(r.PushedAgentIDs) == 0 {
		return current, nil
	}
	pushed := make(map[string]struct{}, len(r.PushedAgentIDs))
	for _, id := range r.PushedAgentIDs {
		pushed[id] = struct{}{}
	}
	all, err := e.agentService.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[uuid.UUID]struct{}, len(current))
	out := make([]*services.Agent, 0, len(current)+len(pushed))
	for _, a := range current {
		if _, ok := seen[a.ID]; !ok {
			seen[a.ID] = struct{}{}
			out = append(out, a)
		}
	}
	// Add pushed-set agents still in the target group that aren't already in
	// the current cohort. Group-scoping matches canary selection; departed
	// agents drop out naturally (ListAgents won't return them).
	for _, a := range all {
		if a.GroupID == nil || *a.GroupID != r.GroupID {
			continue
		}
		if _, ok := pushed[a.ID.String()]; !ok {
			continue
		}
		if _, ok := seen[a.ID]; ok {
			continue
		}
		seen[a.ID] = struct{}{}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
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

// recordStageApplied emits the observability events for a freshly-applied
// stage: the OTel BeginStage span transition, a rollout.stage_applied audit
// row, and — when the stage resolved to ZERO agents — a rollout.empty_canary
// audit row plus its trace event. Both the initial-stage path (start) and the
// promote path (advanceOrCheck) route through here so the two can't drift. A
// prior bug did exactly that: advanceOrCheck emitted the empty_canary TRACE
// event but dropped the empty_canary AUDIT row, so a stage that went empty
// mid-rollout (label selector churned to nothing, percentage rounded to zero
// on a shrunk group) left the trace saying empty_canary while the audit
// timeline — the operator's durable post-mortem record — showed only
// stage_applied. Centralizing the emission makes that class of asymmetry
// unrepresentable.
func (e *Engine) recordStageApplied(ctx context.Context, r *services.Rollout, ids []uuid.UUID) {
	// BeginStage ends the previous stage's span and opens a new one so the
	// trace tree shows a clean stage-by-stage progression.
	e.tracer.BeginStage(r, r.CurrentStage, len(ids))
	e.recordAudit(ctx, r, "rollout.stage_applied", "stage_applied", stageAuditPayload(r, r.CurrentStage, ids))
	if len(ids) == 0 {
		e.recordAudit(ctx, r, "rollout.empty_canary", "empty_canary", stageAuditPayload(r, r.CurrentStage, ids))
		e.tracer.RecordEvent(r.ID, "empty_canary", "")
	}
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
