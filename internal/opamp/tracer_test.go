// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newTestTracer(t *testing.T) (*Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	return NewTracer(tp.Tracer("squadron/opamp-test")), exporter
}

func TestTracer_NilReceiverIsSafe(t *testing.T) {
	// The OpAMP server constructs the tracer unconditionally and
	// treats nil as the disabled path. Every entry point must
	// short-circuit on a nil receiver without panicking.
	var tr *Tracer
	id := uuid.New()
	require.NotPanics(t, func() {
		tr.BeginAgentConnection(context.Background(), id)
		tr.RecordAgentVersion(id, "v1.2.3")
		tr.EndAgentConnection(id, "client_disconnected")
		tr.Shutdown()
	})
}

func TestTracer_ConnectDisconnect_HappyPath(t *testing.T) {
	// Clean lifecycle: BeginAgentConnection → RecordAgentVersion →
	// EndAgentConnection("client_disconnected"). Span ends Ok, the
	// canonical attribute set is present, and the active-map is
	// empty afterward (no leak).
	tr, exporter := newTestTracer(t)
	id := uuid.New()

	tr.BeginAgentConnection(context.Background(), id)
	tr.RecordAgentVersion(id, "v1.2.3")
	tr.EndAgentConnection(id, "client_disconnected")

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "opamp.agent_connection", span.Name)
	assert.Equal(t, codes.Ok, span.Status.Code)

	attrs := attrsByKey(span.Attributes)
	assert.Equal(t, "agent", attrs["squadron.target_type"])
	assert.Equal(t, id.String(), attrs["squadron.target_id"])
	assert.Equal(t, id.String(), attrs["squadron.agent_id"])
	assert.Equal(t, "v1.2.3", attrs["squadron.agent_version"])
	assert.Equal(t, "client_disconnected", attrs["squadron.disconnect_reason"])
	// Duration is best-effort but it should at least be a float and
	// non-negative. We don't assert a specific value — a 0.0 is
	// fine for a sub-microsecond test loop.
	dur, ok := attrs["squadron.connection_duration_seconds"].(float64)
	require.True(t, ok, "connection_duration_seconds should be a float64")
	assert.GreaterOrEqual(t, dur, 0.0)

	// Active map should be empty — End drops the entry.
	tr.mu.Lock()
	assert.Empty(t, tr.active, "active map should be empty after EndAgentConnection")
	tr.mu.Unlock()
}

func TestTracer_BeginAgentConnection_Idempotent(t *testing.T) {
	// The OpAMP server calls BeginAgentConnection on every onMessage,
	// so the second call for the same agent must be a no-op (no
	// duplicate parent span, no overwrite of the first span's
	// startedAt). Pin this — without it, every message would reset
	// the connection duration.
	tr, exporter := newTestTracer(t)
	id := uuid.New()

	tr.BeginAgentConnection(context.Background(), id)
	tr.BeginAgentConnection(context.Background(), id)
	tr.EndAgentConnection(id, "client_disconnected")

	spans := exporter.GetSpans()
	assert.Len(t, spans, 1, "BeginAgentConnection twice should still produce exactly one span")
}

func TestTracer_EndAgentConnection_UnknownAgentIsNoOp(t *testing.T) {
	// Defensive: an out-of-order disconnect (e.g. agent failed to
	// register but the WebSocket close fired anyway) shouldn't
	// panic or produce phantom spans.
	tr, exporter := newTestTracer(t)
	tr.EndAgentConnection(uuid.New(), "client_disconnected")
	assert.Empty(t, exporter.GetSpans())
}

func TestTracer_ErrorReason_FlipsToError(t *testing.T) {
	// A disconnect reason that's neither empty nor "client_disconnected"
	// nor "normal" represents an operationally-meaningful failure
	// (protocol error, server shutdown, etc.). The span flips to
	// Error so trace UIs render it red.
	tr, exporter := newTestTracer(t)
	id := uuid.New()

	tr.BeginAgentConnection(context.Background(), id)
	tr.EndAgentConnection(id, "protocol_error: bad frame")

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status.Code)
	assert.Equal(t, "protocol_error: bad frame", spans[0].Status.Description)
}

func TestTracer_Shutdown_FlushesOpenSpans(t *testing.T) {
	// Server.Stop calls tracer.Shutdown to flush in-flight
	// connections. Spans closed via Shutdown end with Error +
	// "server_shutdown" reason so they're visible as truncated
	// rather than silently dropped.
	tr, exporter := newTestTracer(t)
	a, b := uuid.New(), uuid.New()

	tr.BeginAgentConnection(context.Background(), a)
	tr.BeginAgentConnection(context.Background(), b)
	tr.Shutdown()

	spans := exporter.GetSpans()
	require.Len(t, spans, 2)
	for _, s := range spans {
		assert.Equal(t, codes.Error, s.Status.Code)
		assert.Equal(t, "server_shutdown", s.Status.Description)
		attrs := attrsByKey(s.Attributes)
		assert.Equal(t, "server_shutdown", attrs["squadron.disconnect_reason"])
	}
	// Active map drained.
	tr.mu.Lock()
	assert.Empty(t, tr.active)
	tr.mu.Unlock()
}

func TestTracer_RecordAgentVersion_UnknownAgentIsNoOp(t *testing.T) {
	// AgentDescription can arrive after the connection has already
	// been ended (race-y disconnect path). Should not panic.
	tr, exporter := newTestTracer(t)
	require.NotPanics(t, func() {
		tr.RecordAgentVersion(uuid.New(), "v1.0.0")
	})
	assert.Empty(t, exporter.GetSpans())
}

func TestTracer_RecordAgentVersion_EmptyIgnored(t *testing.T) {
	// Empty version (no AgentDescription resource attribute) should
	// be ignored rather than producing an empty-string attribute on
	// the span. Trace UI filters like "agent_version != ''" should
	// work cleanly.
	tr, exporter := newTestTracer(t)
	id := uuid.New()
	tr.BeginAgentConnection(context.Background(), id)
	tr.RecordAgentVersion(id, "")
	tr.EndAgentConnection(id, "client_disconnected")

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := attrsByKey(spans[0].Attributes)
	_, present := attrs["squadron.agent_version"]
	assert.False(t, present, "empty version should not produce an attribute")
}

func attrsByKey(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
