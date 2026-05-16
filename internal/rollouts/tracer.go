// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"sort"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/devopsmike2/squadron/internal/services"
)

// Tracer wraps an OTel trace.Tracer with the rollout-specific lifecycle
// the engine cares about: one parent span per rollout, child spans
// per stage application, and span events for transitions (paused,
// resumed, aborted, rollback, empty canary).
//
// Spans live across many engine ticks, so the wrapper keeps an
// in-memory map of (rollout id -> active span handles). The engine
// calls BeginRollout / BeginStage / EndStage / EndRollout at the
// natural transition points; the wrapper handles parenting via
// trace.ContextWithSpan so the OTel SDK builds a proper tree.
//
// All methods are nil-receiver-safe: when self-telemetry is disabled
// the engine receives a nil *Tracer and every call short-circuits.
// Lets the engine treat the tracer as an unconditional dependency.
type Tracer struct {
	tracer trace.Tracer

	mu     sync.Mutex
	active map[string]*activeRollout // keyed by rollout ID
}

// activeRollout holds the span handles for one in-flight rollout. The
// rollout span brackets the full lifecycle; the stage span brackets
// the currently-applied stage (re-opened each time the engine
// advances). We keep the rollout's Context separately so child spans
// can be parented correctly.
type activeRollout struct {
	rolloutSpan trace.Span
	rolloutCtx  context.Context
	stageSpan   trace.Span // may be nil between stages
	stageIndex  int
}

// NewTracer returns a tracer wrapper. Pass a nil *Tracer to the engine
// to disable rollout tracing entirely (the no-op path); pass a real
// one wired to a selftel.Publisher's Tracer("...") for real export.
func NewTracer(t trace.Tracer) *Tracer {
	if t == nil {
		return nil
	}
	return &Tracer{
		tracer: t,
		active: make(map[string]*activeRollout),
	}
}

// BeginRollout opens the parent span for a rollout's lifecycle. The
// span stays open across ticks; EndRollout closes it. Idempotent — a
// second call for the same rollout ID is a no-op (the engine might
// re-process a pending rollout if a previous tick crashed mid-start).
func (t *Tracer) BeginRollout(ctx context.Context, r *services.Rollout) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.active[r.ID]; exists {
		return
	}
	spanCtx, span := t.tracer.Start(ctx, "rollout."+r.Name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(rolloutAttrs(r)...),
	)
	t.active[r.ID] = &activeRollout{
		rolloutSpan: span,
		rolloutCtx:  spanCtx,
		stageIndex:  -1,
	}
}

// BeginStage opens a child span for the stage application. If a
// previous stage span is still open (e.g. the engine advanced from
// stage K to stage K+1), it's closed first with its dwell duration
// recorded.
func (t *Tracer) BeginStage(r *services.Rollout, stageIdx int, canarySize int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.active[r.ID]
	if !ok {
		return
	}
	if a.stageSpan != nil {
		a.stageSpan.End()
	}
	stage := r.Stages[stageIdx]
	attrs := append(rolloutAttrs(r),
		attribute.Int("squadron.rollout.stage_index", stageIdx),
		attribute.Int("squadron.rollout.canary_size", canarySize),
		attribute.Int("squadron.rollout.stage.dwell_seconds", stage.DwellSeconds),
		attribute.String("squadron.rollout.stage.mode", stageModeAttr(stage)),
	)
	switch stage.Mode {
	case services.RolloutStageModeLabel:
		attrs = append(attrs,
			attribute.String("squadron.rollout.stage.label_selector", labelSelectorString(stage.LabelSelector)))
	default:
		// "percent" or empty (legacy = percent). Either way, the
		// percentage attribute is what an operator wants to see.
		attrs = append(attrs, attribute.Int("squadron.rollout.stage.percentage", stage.Percentage))
	}
	_, span := t.tracer.Start(a.rolloutCtx, "rollout.stage_applied",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	a.stageSpan = span
	a.stageIndex = stageIdx
}

// RecordEvent attaches a named event to the rollout's parent span.
// Used for transitions that don't open a new stage span — paused,
// resumed, aborted, rollback_started, empty_canary.
//
// reason is optional; when non-empty it's added as an event attribute.
func (t *Tracer) RecordEvent(rolloutID, name, reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.active[rolloutID]
	if !ok {
		return
	}
	attrs := []attribute.KeyValue{}
	if reason != "" {
		attrs = append(attrs, attribute.String("squadron.rollout.reason", reason))
	}
	a.rolloutSpan.AddEvent(name, trace.WithAttributes(attrs...))
}

// EndRollout closes the rollout's parent span (and any open stage span)
// with the given terminal state. State should be one of the rollout
// terminal states; aborted state gets an error status so the trace
// renders red.
//
// Removes the rollout from the active map; subsequent calls for the
// same ID are no-ops.
func (t *Tracer) EndRollout(rolloutID string, terminalState services.RolloutState, reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.active[rolloutID]
	if !ok {
		return
	}
	if a.stageSpan != nil {
		a.stageSpan.End()
	}
	a.rolloutSpan.SetAttributes(
		attribute.String("squadron.rollout.terminal_state", string(terminalState)),
	)
	switch terminalState {
	case services.RolloutStateSucceeded:
		a.rolloutSpan.SetStatus(codes.Ok, "")
	case services.RolloutStateAborted, services.RolloutStateRolledBack:
		// Both aborted and rolled_back are operationally a failure —
		// the operator started a rollout and we didn't deliver. Trace
		// UIs render Error spans red, which is the right visual cue.
		msg := reason
		if msg == "" {
			msg = string(terminalState)
		}
		a.rolloutSpan.SetStatus(codes.Error, msg)
	}
	a.rolloutSpan.End()
	delete(t.active, rolloutID)
}

// Shutdown ends every still-open rollout span. Called from
// engine.Stop so a graceful shutdown flushes spans for rollouts that
// are still in flight (paused, mid-stage, etc.) rather than abandoning
// them in the in-memory map and silently dropping the export.
//
// Spans closed this way get an "engine.shutdown" status — operators
// see them in the trace UI as truncated rollouts.
func (t *Tracer) Shutdown() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, a := range t.active {
		if a.stageSpan != nil {
			a.stageSpan.End()
		}
		a.rolloutSpan.SetStatus(codes.Error, "engine.shutdown")
		a.rolloutSpan.End()
		delete(t.active, id)
	}
}

// rolloutAttrs builds the canonical attribute set every rollout span
// (parent + stage children) carries. Reuses the squadron.* schema from
// v0.12 audit-event spans so operators can filter across both with the
// same vocabulary.
func rolloutAttrs(r *services.Rollout) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("squadron.target_type", "rollout"),
		attribute.String("squadron.target_id", r.ID),
		attribute.String("squadron.rollout.name", r.Name),
		attribute.String("squadron.rollout.group_id", r.GroupID),
		attribute.String("squadron.rollout.target_config_id", r.TargetConfigID),
		attribute.Int("squadron.rollout.total_stages", len(r.Stages)),
	}
}

// stageModeAttr defaults the empty (pre-v0.6 legacy) mode to "percent"
// so the attribute is never empty in exported traces.
func stageModeAttr(s services.RolloutStage) string {
	if s.Mode == "" {
		return string(services.RolloutStageModePercent)
	}
	return string(s.Mode)
}

// labelSelectorString renders a {k:v} map as "k=v,k=v" deterministically
// so trace search by attribute value works. Same shape the audit
// timeline summarizes selectors with.
func labelSelectorString(sel map[string]string) string {
	if len(sel) == 0 {
		return ""
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ","
		}
		out += k + "=" + sel[k]
	}
	return out
}

