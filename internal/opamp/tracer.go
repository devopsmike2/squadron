// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/google/uuid"
)

// Tracer wraps an OTel trace.Tracer with the per-agent connection
// lifecycle. Each OpAMP agent gets a span spanning the lifetime of
// its connection — opened when the first AgentToServer message
// arrives (the earliest point Squadron knows the agent's instance
// ID), closed when the WebSocket disconnects.
//
// Long-lived spans use an in-memory active map keyed by agent ID,
// same pattern as internal/rollouts/tracer.go. Shutdown flushes any
// still-open spans so trace exports include the in-flight
// connections as truncated rather than silently dropped.
//
// Pattern: thin wrapper, nil-receiver-safe so the OpAMP server
// treats the tracer as an unconditional dep.
type Tracer struct {
	tracer trace.Tracer

	mu     sync.Mutex
	active map[uuid.UUID]*activeAgent // keyed by agent's instance ID
}

// activeAgent is one in-flight connection span. startedAt is recorded
// so we can compute the connection_duration_seconds attribute on End
// — OTel spans carry their own start/end timestamps too, but the
// derived duration as an attribute lets operators filter / sort by
// connection length without joining timestamps in their backend.
type activeAgent struct {
	span      trace.Span
	startedAt time.Time
}

// NewTracer returns a tracer wrapper. Pass a nil trace.Tracer to
// disable OpAMP connection tracing entirely (the no-op path); pass
// a real one wired to selftel.Publisher.Tracer("squadron/opamp") for
// real export.
func NewTracer(t trace.Tracer) *Tracer {
	if t == nil {
		return nil
	}
	return &Tracer{
		tracer: t,
		active: make(map[uuid.UUID]*activeAgent),
	}
}

// BeginAgentConnection opens the parent span for an agent's
// connection lifecycle. Idempotent — calling twice for the same
// agent ID is a no-op (a second OpAMP message from a known agent
// shouldn't restart the span). The OpAMP server calls this on every
// onMessage; the no-op-after-first behavior is what makes that
// safe.
func (t *Tracer) BeginAgentConnection(ctx context.Context, agentID uuid.UUID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.active[agentID]; exists {
		return
	}
	now := time.Now()
	_, span := t.tracer.Start(ctx, "opamp.agent_connection",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("squadron.target_type", "agent"),
			attribute.String("squadron.target_id", agentID.String()),
			attribute.String("squadron.agent_id", agentID.String()),
		),
	)
	t.active[agentID] = &activeAgent{span: span, startedAt: now}
}

// RecordAgentVersion attaches the agent's reported version string
// to its connection span. Called after the first AgentDescription
// arrives (which may be on the first onMessage or a later one).
// Setting on an unknown agent is a no-op — defensive against
// out-of-order calls.
func (t *Tracer) RecordAgentVersion(agentID uuid.UUID, version string) {
	if t == nil || version == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.active[agentID]
	if !ok {
		return
	}
	a.span.SetAttributes(attribute.String("squadron.agent_version", version))
}

// EndAgentConnection closes the connection span with the given
// reason. The duration in seconds lands on the span as a derived
// attribute so operators can filter by connection length.
//
// reason populates the span's status message — empty / "normal" /
// "client_disconnected" keep Ok status, anything else (server
// shutdown, protocol error) flips to Error.
func (t *Tracer) EndAgentConnection(agentID uuid.UUID, reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.active[agentID]
	if !ok {
		return
	}
	a.span.SetAttributes(
		attribute.Float64("squadron.connection_duration_seconds", time.Since(a.startedAt).Seconds()),
		attribute.String("squadron.disconnect_reason", reason),
	)
	if isErrorReason(reason) {
		a.span.SetStatus(codes.Error, reason)
	} else {
		a.span.SetStatus(codes.Ok, "")
	}
	a.span.End()
	delete(t.active, agentID)
}

// Shutdown ends every still-open connection span. Called from
// Server.Stop so a graceful shutdown flushes spans for the agents
// that are still connected at the moment Squadron exits. Spans
// closed this way carry status Error / "server_shutdown" — visible
// in the trace UI as truncated, distinct from a clean client
// disconnect.
func (t *Tracer) Shutdown() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, a := range t.active {
		a.span.SetAttributes(
			attribute.Float64("squadron.connection_duration_seconds", time.Since(a.startedAt).Seconds()),
			attribute.String("squadron.disconnect_reason", "server_shutdown"),
		)
		a.span.SetStatus(codes.Error, "server_shutdown")
		a.span.End()
		delete(t.active, id)
	}
}

// isErrorReason reports whether a disconnect reason should flip the
// span to Error. Clean disconnects (the client closed the
// connection or sent a normal close frame) stay Ok; anything else
// is operationally a failure.
func isErrorReason(reason string) bool {
	switch reason {
	case "", "client_disconnected", "normal":
		return false
	default:
		return true
	}
}
