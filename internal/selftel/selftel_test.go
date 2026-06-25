// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package selftel

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// publisherWithInMemoryExporter wires up a Publisher that exports to
// tracetest.InMemoryExporter — same code path as production, just
// without the network. The returned exporter is what tests assert
// against. The Publisher's Shutdown forwards to the SDK so callers
// should defer it after each test.
func publisherWithInMemoryExporter(t *testing.T) (*Publisher, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	// Use SimpleSpanProcessor so spans land in the exporter
	// synchronously — no flush wait needed in the test path.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	return &Publisher{
		tp:     tp,
		tracer: tp.Tracer("squadron/audit"),
		logger: zap.NewNop(),
	}, exporter
}

func TestPublisher_DisabledIsNoOp(t *testing.T) {
	// New() with Enabled=false returns a Publisher whose tracer is
	// nil. PublishAuditEvent must not panic and must not perform any
	// side effects.
	p, err := New(t.Context(), Config{Enabled: false}, nil, zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, p)
	// The whole point of the no-op path is that callers can mount it
	// unconditionally. Verify by calling Publish + Shutdown on a
	// disabled publisher.
	p.PublishAuditEvent(t.Context(), AuditEntry{EventType: "test.event"})
	require.NoError(t, p.Shutdown(t.Context()))
}

func TestPublisher_NilReceiverIsSafe(t *testing.T) {
	// Operators (and tests) sometimes pass a nil *Publisher around.
	// The methods must handle that gracefully — it's a common
	// "auth-disabled-style" pattern.
	var p *Publisher
	require.NotPanics(t, func() {
		p.PublishAuditEvent(context.Background(), AuditEntry{EventType: "test.event"})
	})
	require.NoError(t, p.Shutdown(context.Background()))
}

func TestPublisher_PublishAuditEvent_AttributesMatchSchema(t *testing.T) {
	// Pin the attribute schema: changes to the squadron.* attribute
	// set are wire-shape changes that downstream queries (Grafana
	// dashboards, Tempo searches) will break on. Catch them at test
	// time so the schema doc and the code stay in sync.
	p, exporter := publisherWithInMemoryExporter(t)
	defer func() { _ = p.Shutdown(context.Background()) }()

	p.PublishAuditEvent(context.Background(), AuditEntry{
		Actor:      "operator:ci-bot",
		EventType:  "rollout.created",
		TargetType: "rollout",
		TargetID:   "rollout-abc",
		Action:     "created",
		Payload: map[string]any{
			"name":             "ship v2",
			"stage_count":      3,
			"diff_added_lines": 12.0, // JSON-style float64
			"diff_identical":   false,
			"nested":           map[string]string{"skip": "me"},
		},
	})

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]

	assert.Equal(t, "rollout.created", span.Name)
	attrs := attrsByKey(span.Attributes)
	assert.Equal(t, "operator:ci-bot", attrs["squadron.actor"])
	assert.Equal(t, "rollout.created", attrs["squadron.event_type"])
	assert.Equal(t, "rollout", attrs["squadron.target_type"])
	assert.Equal(t, "rollout-abc", attrs["squadron.target_id"])
	assert.Equal(t, "created", attrs["squadron.action"])
	// Primitive payload values flatten onto the span. Non-primitives
	// are deliberately dropped — verify the nested map didn't sneak
	// through, since map attributes blow up trace UIs and cardinality.
	assert.Equal(t, "ship v2", attrs["squadron.payload.name"])
	assert.Equal(t, int64(3), attrs["squadron.payload.stage_count"])
	assert.Equal(t, 12.0, attrs["squadron.payload.diff_added_lines"])
	assert.Equal(t, false, attrs["squadron.payload.diff_identical"])
	_, hasNested := attrs["squadron.payload.nested"]
	assert.False(t, hasNested, "non-primitive payload fields must not appear in attributes")
}

func TestPublisher_PublishAuditEvent_PointEventShape(t *testing.T) {
	// Audit events are instantaneous, not bracketing. The span's
	// duration should be approximately zero — we don't assert exactly
	// 0 because the SDK records monotonic nanos which usually differ
	// by 1-100ns.
	p, exporter := publisherWithInMemoryExporter(t)
	defer func() { _ = p.Shutdown(context.Background()) }()

	p.PublishAuditEvent(context.Background(), AuditEntry{EventType: "tick"})

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	dur := spans[0].EndTime.Sub(spans[0].StartTime)
	assert.Less(t, dur, 100*time.Microsecond,
		"audit-event spans should be effectively zero-duration")
	assert.Equal(t, trace.SpanKindInternal, spans[0].SpanKind)
}

func TestPublisher_SafeAttrKey_Lowercase(t *testing.T) {
	// safeAttrKey is a small internal helper; verify the contract so
	// a future maintainer doesn't accidentally introduce mixed-case
	// or whitespace into attribute keys (some backends reject those).
	assert.Equal(t, "max_drifted_agents", safeAttrKey("max_drifted_agents"))
	assert.Equal(t, "max_drifted", safeAttrKey("Max Drifted"))
}

func TestNew_EmptyEndpointRejected(t *testing.T) {
	// Enabling self-telemetry without an endpoint is an operator
	// mistake — Squadron rejects it at startup so the misconfiguration
	// fails loudly rather than silently dropping every span.
	_, err := New(t.Context(), Config{Enabled: true}, nil, zap.NewNop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint is required")
}

// attrsByKey converts a slice of attribute.KeyValue into a map keyed
// by string for easy lookup in tests. Returns the AsInterface() value
// so callers can assert against the native Go type (string/int64/etc.).
func attrsByKey(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
