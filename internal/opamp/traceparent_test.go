// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// capturingConn is a fake types.Connection that records every
// ServerToAgent frame Send() is invoked with. Used by the wire-level
// tests below to assert exactly what would go onto the OpAMP socket.
type capturingConn struct {
	mu       sync.Mutex
	captured []*protobufs.ServerToAgent
}

func (c *capturingConn) Send(_ context.Context, msg *protobufs.ServerToAgent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = append(c.captured, msg)
	return nil
}

func (c *capturingConn) Connection() net.Conn {
	conn, _ := net.Pipe()
	return conn
}

func (c *capturingConn) Disconnect() error { return nil }

// newCapturingAgent returns an Agent backed by a capturingConn plus a
// pointer to the slice of frames the agent has been asked to send.
// The agent is set up with the AcceptsRemoteConfig capability so the
// config-push path doesn't short-circuit on the capability check
// inside ConfigSender (we exercise SetCustomConfig directly here, so
// the capability isn't strictly required, but matches production).
func newCapturingAgent(t *testing.T) (*Agent, *[]*protobufs.ServerToAgent) {
	t.Helper()
	conn := &capturingConn{}
	agent := NewAgent(uuid.New(), conn)
	agent.Status = &protobufs.AgentToServer{
		Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
	}
	return agent, &conn.captured
}

// simpleConfigMap builds the minimal AgentConfigMap shape that
// SetCustomConfig accepts — single empty-name file with the supplied
// body. Mirrors what ConfigSender.SendConfigToAgent constructs.
func simpleConfigMap(body string) *protobufs.AgentConfigMap {
	return &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigFile{
			"": {Body: []byte(body)},
		},
	}
}

// installW3CPropagator wires the global propagator to TraceContext for
// the test and restores the previous one on cleanup. Mirrors what
// selftel.New does at process init in real exports.
func installW3CPropagator(t *testing.T) {
	t.Helper()
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })
}

// activeSpanCtx returns a context carrying a freshly-started OTel span
// from an in-memory provider. Useful for asserting that the injector
// extracts a non-zero trace/span id from the context.
func activeSpanCtx(t *testing.T) (context.Context, func()) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	ctx, span := tp.Tracer("squadron/opamp-test").Start(context.Background(), "test.span")
	cleanup := func() {
		span.End()
		_ = tp.Shutdown(context.Background())
	}
	return ctx, cleanup
}

func TestBuildTraceparentMessage_NoActiveSpanReturnsNil(t *testing.T) {
	// Background context has no span; the injector must return nil so
	// the OpAMP wire frame doesn't carry a sentinel all-zeros
	// traceparent that agent-side instrumentation would mistakenly
	// adopt as a parent.
	installW3CPropagator(t)
	msg := buildTraceparentMessage(context.Background())
	assert.Nil(t, msg)
}

func TestBuildTraceparentMessage_ActiveSpanProducesValidPayload(t *testing.T) {
	// With an active span on the calling context, the injector returns
	// a CustomMessage tagged with the Squadron capability + context
	// type, and the Data is a JSON object containing a real
	// traceparent header (not a placeholder).
	installW3CPropagator(t)
	ctx, done := activeSpanCtx(t)
	defer done()

	msg := buildTraceparentMessage(ctx)
	require.NotNil(t, msg)
	assert.Equal(t, TraceparentCapability, msg.Capability)
	assert.Equal(t, TraceparentMessageType, msg.Type)
	assert.Equal(t, "io.squadron.traceparent.v1", msg.Capability,
		"capability name is part of the wire contract — pin it")

	var payload map[string]string
	require.NoError(t, json.Unmarshal(msg.Data, &payload))
	tp, ok := payload["traceparent"]
	require.True(t, ok, "payload must contain a traceparent header")
	assert.True(t, strings.HasPrefix(tp, "00-"),
		"traceparent must start with the W3C version byte (00-), got %q", tp)
	// W3C format: 00-<32-hex-traceid>-<16-hex-spanid>-<2-hex-flags>
	// Reject the all-zeros sentinel that propagator.Inject sometimes
	// produces when the context has no usable span.
	assert.NotContains(t, tp, "00000000000000000000000000000000",
		"traceparent must not carry the all-zeros sentinel trace id")
	assert.NotContains(t, tp, "0000000000000000",
		"traceparent must not carry the all-zeros sentinel span id")
}

func TestBuildTraceparentMessage_PropagatesTracestateWhenPresent(t *testing.T) {
	// When the caller's context has a non-empty W3C tracestate (e.g.
	// originating from an external system with vendor-specific
	// entries), it must ride along on the OpAMP CustomMessage so the
	// downstream agent sees the full propagation state.
	installW3CPropagator(t)

	// Use a propagator-Extract round-trip to seed the context with a
	// tracestate. propagation.TraceContext consumes a "tracestate"
	// header from the carrier on Extract.
	carrier := propagation.MapCarrier{
		"traceparent": "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01",
		"tracestate":  "vendor=abc",
	}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)

	msg := buildTraceparentMessage(ctx)
	require.NotNil(t, msg)
	var payload map[string]string
	require.NoError(t, json.Unmarshal(msg.Data, &payload))
	assert.Equal(t, "vendor=abc", payload["tracestate"],
		"tracestate must propagate alongside traceparent")
}

func TestBuildTraceparentMessage_DropsBaggage(t *testing.T) {
	// Composite propagator from selftel writes both TraceContext AND
	// Baggage. We deliberately drop baggage from the OpAMP payload —
	// operators don't want tenant-id / deploy-version riding along
	// with every fleet push. Lock that decision down so a future
	// "include all carrier entries" refactor doesn't quietly leak it.
	installW3CPropagator(t)
	ctx, done := activeSpanCtx(t)
	defer done()

	// Attach a baggage entry that the composite propagator would
	// inject under the "baggage" header.
	mem, err := baggage.NewMember("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(mem)
	require.NoError(t, err)
	ctx = baggage.ContextWithBaggage(ctx, b)

	msg := buildTraceparentMessage(ctx)
	require.NotNil(t, msg)
	var payload map[string]string
	require.NoError(t, json.Unmarshal(msg.Data, &payload))
	_, present := payload["baggage"]
	assert.False(t, present, "baggage must NOT be propagated to agents")
}

func TestSetCustomConfigWithContext_AttachesCustomMessage(t *testing.T) {
	// Wire-level assertion: when an active span is present, the
	// outbound ServerToAgent frame built by SetCustomConfigWithContext
	// carries both the RemoteConfig AND the traceparent
	// CustomMessage on the same frame. Capture via a recording Agent.
	installW3CPropagator(t)
	ctx, done := activeSpanCtx(t)
	defer done()

	agent, captured := newCapturingAgent(t)
	notify := make(chan struct{}, 1)

	agent.SetCustomConfigWithContext(ctx, simpleConfigMap("inject me"), notify)

	require.Len(t, *captured, 1, "exactly one ServerToAgent frame")
	frame := (*captured)[0]
	require.NotNil(t, frame.RemoteConfig, "RemoteConfig must still be set")
	require.NotNil(t, frame.CustomMessage, "CustomMessage must carry the traceparent")
	assert.Equal(t, TraceparentCapability, frame.CustomMessage.Capability)
	assert.Equal(t, TraceparentMessageType, frame.CustomMessage.Type)

	var payload map[string]string
	require.NoError(t, json.Unmarshal(frame.CustomMessage.Data, &payload))
	assert.Contains(t, payload, "traceparent")
}

func TestSetCustomConfigWithContext_NoActiveSpan_OmitsCustomMessage(t *testing.T) {
	// Symmetric to the above: with no span, the outbound frame must
	// have CustomMessage == nil. We must NOT emit a CustomMessage
	// with an empty/all-zeros traceparent — that would mislead
	// agent-side instrumentation into starting fresh under a bogus
	// parent.
	installW3CPropagator(t)

	agent, captured := newCapturingAgent(t)
	notify := make(chan struct{}, 1)

	agent.SetCustomConfigWithContext(context.Background(), simpleConfigMap("no-span"), notify)

	require.Len(t, *captured, 1)
	frame := (*captured)[0]
	require.NotNil(t, frame.RemoteConfig)
	assert.Nil(t, frame.CustomMessage,
		"CustomMessage must be nil when ctx carries no active span")
}

func TestSetCustomConfig_BackwardsCompat_NoCustomMessage(t *testing.T) {
	// The original (non-ctx) SetCustomConfig must keep producing the
	// same wire frame as before v0.16 — RemoteConfig only, no
	// CustomMessage — so existing callers that don't have a
	// meaningful trace context (the group fan-out path, scripts, old
	// tests) stay byte-compatible.
	installW3CPropagator(t)

	agent, captured := newCapturingAgent(t)
	notify := make(chan struct{}, 1)

	agent.SetCustomConfig(simpleConfigMap("plain"), notify)

	require.Len(t, *captured, 1)
	assert.NotNil(t, (*captured)[0].RemoteConfig)
	assert.Nil(t, (*captured)[0].CustomMessage,
		"non-ctx SetCustomConfig must not attach a CustomMessage")
}
