// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/devopsmike2/squadron/internal/services"
)

// newTestTracer wires a Tracer to an in-memory exporter so tests can
// assert against the exported spans. SimpleSpanProcessor flushes
// synchronously — no batcher means no flush wait needed.
func newTestTracer(t *testing.T) (*Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	return NewTracer(tp.Tracer("squadron/rollouts-test")), exporter
}

func percentStage(pct, dwell int) services.RolloutStage {
	return services.RolloutStage{
		Mode:         services.RolloutStageModePercent,
		Percentage:   pct,
		DwellSeconds: dwell,
	}
}

func newRollout(id, name string, stages ...services.RolloutStage) *services.Rollout {
	return &services.Rollout{
		ID:             id,
		Name:           name,
		GroupID:        "group-a",
		TargetConfigID: "cfg-1",
		Stages:         stages,
	}
}

func TestTracer_NilReceiverIsSafe(t *testing.T) {
	// Engine receives a nil tracer when self-telemetry is disabled.
	// Every method must short-circuit without panicking so the engine
	// can call unconditionally.
	var tr *Tracer
	r := newRollout("r1", "test", percentStage(100, 0))
	require.NotPanics(t, func() {
		tr.BeginRollout(context.Background(), r)
		tr.BeginStage(r, 0, 3)
		tr.RecordEvent("r1", "paused", "")
		tr.EndRollout("r1", services.RolloutStateSucceeded, "")
		tr.Shutdown()
	})
}

func TestTracer_RolloutLifecycle_SucceededTree(t *testing.T) {
	// Happy path: a percent rollout that succeeds produces 1 parent
	// span + 1 child span per stage. Parent ends Ok; children end
	// when the next BeginStage closes them or when EndRollout sweeps
	// the final open one. Pin the shape so refactors can't quietly
	// flatten the tree.
	tr, exporter := newTestTracer(t)
	r := newRollout("r1", "ship-v2",
		percentStage(10, 0),
		percentStage(50, 0),
		percentStage(100, 0),
	)

	tr.BeginRollout(context.Background(), r)
	tr.BeginStage(r, 0, 1)
	tr.BeginStage(r, 1, 5)
	tr.BeginStage(r, 2, 10)
	tr.EndRollout(r.ID, services.RolloutStateSucceeded, "")

	spans := exporter.GetSpans()
	require.Len(t, spans, 4, "expect 1 parent + 3 stage children")

	// Find the parent span (no parent of its own — only span where
	// TraceID == SpanID's trace and parent SpanID is invalid).
	var parent *tracetest.SpanStub
	var children []tracetest.SpanStub
	for i := range spans {
		s := spans[i]
		if !s.Parent.IsValid() {
			parent = &s
			continue
		}
		children = append(children, s)
	}
	require.NotNil(t, parent, "expected exactly one root span")
	assert.Equal(t, "rollout.ship-v2", parent.Name)
	assert.Equal(t, codes.Ok, parent.Status.Code)

	parentAttrs := attrsByKey(parent.Attributes)
	assert.Equal(t, "rollout", parentAttrs["squadron.target_type"])
	assert.Equal(t, "r1", parentAttrs["squadron.target_id"])
	assert.Equal(t, "succeeded", parentAttrs["squadron.rollout.terminal_state"])

	// All stage spans should be children of the parent and named
	// rollout.stage_applied.
	assert.Len(t, children, 3)
	stageIndexes := map[int]bool{}
	for _, s := range children {
		assert.Equal(t, "rollout.stage_applied", s.Name)
		assert.Equal(t, parent.SpanContext.SpanID(), s.Parent.SpanID(),
			"stage span %q should be a direct child of the parent", s.Name)
		attrs := attrsByKey(s.Attributes)
		if idx, ok := attrs["squadron.rollout.stage_index"].(int64); ok {
			stageIndexes[int(idx)] = true
		}
	}
	assert.True(t, stageIndexes[0])
	assert.True(t, stageIndexes[1])
	assert.True(t, stageIndexes[2])
}

func TestTracer_RolloutLifecycle_AbortedEndsWithError(t *testing.T) {
	// Failed rollout: aborted state → EndRollout records the reason
	// as the span's status message and flips the status code to Error.
	// Trace UIs render this as a red span, which is the right visual
	// cue when an operator is searching for failures.
	tr, exporter := newTestTracer(t)
	r := newRollout("r2", "broken-config", percentStage(100, 0))

	tr.BeginRollout(context.Background(), r)
	tr.BeginStage(r, 0, 1)
	tr.RecordEvent("r2", "aborted", "2 canary agent(s) drifted (max 1)")
	tr.EndRollout("r2", services.RolloutStateRolledBack, "2 canary agent(s) drifted (max 1)")

	spans := exporter.GetSpans()
	require.Len(t, spans, 2)
	var parent tracetest.SpanStub
	for _, s := range spans {
		if !s.Parent.IsValid() {
			parent = s
		}
	}
	assert.Equal(t, codes.Error, parent.Status.Code)
	assert.Contains(t, parent.Status.Description, "drifted")
	parentAttrs := attrsByKey(parent.Attributes)
	assert.Equal(t, "rolled_back", parentAttrs["squadron.rollout.terminal_state"])

	// The "aborted" event should appear on the parent's events list.
	require.Len(t, parent.Events, 1)
	assert.Equal(t, "aborted", parent.Events[0].Name)
	eventAttrs := attrsByKey(parent.Events[0].Attributes)
	assert.Equal(t, "2 canary agent(s) drifted (max 1)", eventAttrs["squadron.rollout.reason"])
}

func TestTracer_BeginStage_LabelModeAttributes(t *testing.T) {
	// Label-mode stages should record the selector string deterministically
	// so trace search by attribute value works ("filter spans where
	// squadron.rollout.stage.label_selector contains role=canary").
	tr, exporter := newTestTracer(t)
	r := newRollout("r3", "label-mode",
		services.RolloutStage{
			Mode:          services.RolloutStageModeLabel,
			LabelSelector: map[string]string{"role": "canary", "region": "us-east"},
			DwellSeconds:  60,
		},
	)

	tr.BeginRollout(context.Background(), r)
	tr.BeginStage(r, 0, 2)
	tr.EndRollout(r.ID, services.RolloutStateSucceeded, "")

	spans := exporter.GetSpans()
	require.Len(t, spans, 2)
	var stage tracetest.SpanStub
	for _, s := range spans {
		if s.Parent.IsValid() {
			stage = s
		}
	}
	attrs := attrsByKey(stage.Attributes)
	assert.Equal(t, "label", attrs["squadron.rollout.stage.mode"])
	// Deterministic ordering — keys sorted — so a future operator
	// can grep / filter by this value reliably.
	assert.Equal(t, "region=us-east,role=canary", attrs["squadron.rollout.stage.label_selector"])
	// Percent attribute should NOT be present on label-mode stages.
	_, hasPct := attrs["squadron.rollout.stage.percentage"]
	assert.False(t, hasPct, "label-mode stage span should not carry a percentage attribute")
}

func TestTracer_BeginRollout_Idempotent(t *testing.T) {
	// The engine's restart-recovery path calls BeginRollout for any
	// in_progress rollout it picks up. If a previous tick already
	// opened a span, the second call must be a no-op — otherwise
	// we'd leak orphan parent spans.
	tr, exporter := newTestTracer(t)
	r := newRollout("r4", "double-start", percentStage(100, 0))

	tr.BeginRollout(context.Background(), r)
	tr.BeginRollout(context.Background(), r) // second call should be ignored
	tr.EndRollout(r.ID, services.RolloutStateSucceeded, "")

	spans := exporter.GetSpans()
	assert.Len(t, spans, 1, "BeginRollout twice should still produce exactly one parent span")
}

func TestTracer_Shutdown_FlushesOpenSpans(t *testing.T) {
	// engine.Stop calls Shutdown to flush any in-flight rollouts.
	// Spans closed via Shutdown should end with an Error status so
	// they're visible in the trace UI as truncated rather than
	// silently dropped.
	tr, exporter := newTestTracer(t)
	r := newRollout("r5", "mid-flight", percentStage(100, 60))

	tr.BeginRollout(context.Background(), r)
	tr.BeginStage(r, 0, 3)
	tr.Shutdown()

	spans := exporter.GetSpans()
	require.Len(t, spans, 2, "Shutdown should end both the open stage span and the parent")
	var parent tracetest.SpanStub
	for _, s := range spans {
		if !s.Parent.IsValid() {
			parent = s
		}
	}
	assert.Equal(t, codes.Error, parent.Status.Code)
	assert.Contains(t, parent.Status.Description, "engine.shutdown")
}

func TestTracer_RecordEvent_NoActiveRolloutIsNoOp(t *testing.T) {
	// Defensive: an audit-related callback might fire after EndRollout
	// has already removed the rollout from the active map. The event
	// should silently drop rather than panic.
	tr, _ := newTestTracer(t)
	require.NotPanics(t, func() {
		tr.RecordEvent("never-existed", "paused", "")
	})
}

func TestTracer_RecordEvent_PausedResumedLandOnActiveSpan(t *testing.T) {
	// Pause/resume are service-level transitions (the operator hits
	// the API) rather than engine-driven, but they should still
	// appear on the engine-opened parent span as named events. This
	// pins the integration so a trace tells the full pause/resume
	// story without operators having to cross-reference the audit
	// log.
	tr, exporter := newTestTracer(t)
	r := newRollout("r-pr", "pause-resume", percentStage(100, 60))

	tr.BeginRollout(context.Background(), r)
	tr.BeginStage(r, 0, 1)
	tr.RecordEvent(r.ID, "paused", "")
	tr.RecordEvent(r.ID, "resumed", "")
	tr.EndRollout(r.ID, services.RolloutStateSucceeded, "")

	spans := exporter.GetSpans()
	var parent tracetest.SpanStub
	for _, s := range spans {
		if !s.Parent.IsValid() {
			parent = s
		}
	}
	require.NotEmpty(t, parent.Events, "pause + resume should both surface as events on the parent span")
	eventNames := []string{}
	for _, e := range parent.Events {
		eventNames = append(eventNames, e.Name)
	}
	assert.Contains(t, eventNames, "paused")
	assert.Contains(t, eventNames, "resumed")
}

func TestTracer_LinkRolloutToContext_AddsLinkOnBeginRollout(t *testing.T) {
	// The API request's span context, captured at Create time via
	// LinkRolloutToContext, should attach as a Link on the rollout's
	// parent span when the engine opens it. Span links are the
	// OTel-blessed way to thread the originating trace into a
	// long-lived span without making it a parent-child relationship
	// (engine spans live across many ticks; the API span ended
	// seconds ago).
	tr, exporter := newTestTracer(t)
	r := newRollout("r-linked", "linked-rollout", percentStage(100, 0))

	// Synthesize a fake API request span by starting one with the
	// same provider the tracer uses. The span context we capture is
	// what'd be on c.Request.Context() after otelgin middleware.
	apiCtx, apiSpan := tr.tracer.Start(context.Background(), "POST /api/v1/rollouts")
	tr.LinkRolloutToContext(r.ID, apiCtx)
	apiSpan.End()

	tr.BeginRollout(context.Background(), r)
	tr.EndRollout(r.ID, services.RolloutStateSucceeded, "")

	spans := exporter.GetSpans()
	var parent tracetest.SpanStub
	var apiStub tracetest.SpanStub
	for _, s := range spans {
		switch s.Name {
		case "rollout.linked-rollout":
			parent = s
		case "POST /api/v1/rollouts":
			apiStub = s
		}
	}
	require.NotEmpty(t, parent.Name, "rollout span should exist")
	require.NotEmpty(t, apiStub.Name, "api span should exist")
	require.Len(t, parent.Links, 1, "rollout span should carry exactly one link back to the API request")
	assert.Equal(t, apiStub.SpanContext.SpanID(), parent.Links[0].SpanContext.SpanID(),
		"the link should point at the API request span context")
	// Pinned link attribute lets operators distinguish this link
	// from any future links (e.g. spawned-from-another-rollout).
	linkAttrs := attrsByKey(parent.Links[0].Attributes)
	assert.Equal(t, "created_by_request", linkAttrs["squadron.link"])
}

func TestTracer_LinkRolloutToContext_NoSpanInContextIsNoOp(t *testing.T) {
	// A context with no active span (e.g. an admin call without
	// otelgin middleware, or telemetry disabled) shouldn't produce
	// a link with an invalid span context — that would render as a
	// broken arrow in trace UIs.
	tr, exporter := newTestTracer(t)
	r := newRollout("r-nolink", "no-link", percentStage(100, 0))

	tr.LinkRolloutToContext(r.ID, context.Background())
	tr.BeginRollout(context.Background(), r)
	tr.EndRollout(r.ID, services.RolloutStateSucceeded, "")

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Empty(t, spans[0].Links, "no upstream span = no link, no broken arrows")
}

// attrsByKey converts a slice of attribute.KeyValue into a map so
// tests can assert on individual values without iterating.
func attrsByKey(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
