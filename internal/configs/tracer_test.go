// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package configs

import (
	"context"
	"testing"

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
	return NewTracer(tp.Tracer("squadron/configs-test")), exporter
}

func TestTracer_NilReceiverIsSafe(t *testing.T) {
	// Callers wrap unconditionally — the engine and handlers don't
	// know whether telemetry is enabled at call time. Every method
	// must short-circuit on a nil receiver.
	var tr *Tracer
	push := tr.BeginPush(context.Background(), "agent-1", "cfg-1", "group-a", SourceRollout)
	require.NotPanics(t, func() {
		push.RecordAck()
		push.RecordNack("boom")
		push.End()
	})
}

func TestTracer_BeginPush_HappyPath_Ok(t *testing.T) {
	// A successful push: BeginPush → RecordAck → End. Span ends with
	// status Ok; the opamp_ack event is recorded; attribute schema
	// matches docs.
	tr, exporter := newTestTracer(t)

	push := tr.BeginPush(context.Background(), "agent-1", "cfg-1", "group-a", SourceRollout)
	push.RecordAck()
	push.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "config.push", span.Name)
	assert.Equal(t, codes.Ok, span.Status.Code)

	attrs := attrsByKey(span.Attributes)
	assert.Equal(t, "agent", attrs["squadron.target_type"])
	assert.Equal(t, "agent-1", attrs["squadron.target_id"])
	assert.Equal(t, "agent-1", attrs["squadron.agent_id"])
	assert.Equal(t, "cfg-1", attrs["squadron.config_id"])
	assert.Equal(t, "group-a", attrs["squadron.group_id"])
	assert.Equal(t, "rollout", attrs["squadron.push_source"])

	require.Len(t, span.Events, 1)
	assert.Equal(t, "opamp_ack", span.Events[0].Name)
}

func TestTracer_BeginPush_NoGroupID_OmitsGroupAttr(t *testing.T) {
	// Direct (single-agent) pushes from the API handlers don't carry
	// a group id. The attribute should be absent rather than empty
	// so trace UI filters like 'group_id != ""' work intuitively.
	tr, exporter := newTestTracer(t)

	push := tr.BeginPush(context.Background(), "agent-1", "cfg-1", "", SourceDirect)
	push.RecordAck()
	push.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := attrsByKey(spans[0].Attributes)
	_, present := attrs["squadron.group_id"]
	assert.False(t, present, "empty group_id should not produce an attribute key")
	assert.Equal(t, "direct", attrs["squadron.push_source"])
}

func TestTracer_RecordNack_FlipsToError(t *testing.T) {
	// A timed-out or rejected push: span ends with Error and the
	// nack reason as the status message so trace UIs render it red
	// with a useful tooltip. opamp_nack event carries the same
	// reason as a structured attribute.
	tr, exporter := newTestTracer(t)

	push := tr.BeginPush(context.Background(), "agent-1", "cfg-1", "", SourceDirect)
	push.RecordNack("timeout waiting for agent to apply config")
	push.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status.Code)
	assert.Contains(t, spans[0].Status.Description, "timeout")

	require.Len(t, spans[0].Events, 1)
	assert.Equal(t, "opamp_nack", spans[0].Events[0].Name)
	evAttrs := attrsByKey(spans[0].Events[0].Attributes)
	assert.Equal(t, "timeout waiting for agent to apply config", evAttrs["squadron.nack_reason"])
}

func TestTracer_AckThenNack_FinalStateIsError(t *testing.T) {
	// Defensive: if a caller records both (shouldn't happen but
	// could after a refactor), the nack wins because the actual
	// outcome is the more recent observation. Pin so a future
	// refactor doesn't quietly flip the semantics.
	tr, exporter := newTestTracer(t)

	push := tr.BeginPush(context.Background(), "agent-1", "cfg-1", "", SourceDirect)
	push.RecordAck()
	push.RecordNack("late timeout")
	push.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status.Code)
}

func TestTracer_PushSources_PinnedAsStrings(t *testing.T) {
	// These constants are part of the attribute wire shape — operators
	// filter trace queries by them ('squadron.push_source = rollout').
	// Pin the values so a rename can't silently break dashboards.
	assert.Equal(t, "rollout", string(SourceRollout))
	assert.Equal(t, "direct", string(SourceDirect))
	assert.Equal(t, "group", string(SourceGroup))
	assert.Equal(t, "drift_remediation", string(SourceDriftRemediation))
}

func attrsByKey(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
