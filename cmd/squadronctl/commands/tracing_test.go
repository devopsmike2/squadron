// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

// installTestTracer swaps in an in-memory tracer provider and W3C
// propagator for the duration of one test. Returns the exporter
// (recorded spans) and a cleanup that restores the global state so
// subsequent tests run untainted.
func installTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tr := tp.Tracer("squadronctl")

	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	prevTracing := tracing

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	tracing = &tracingState{tp: tp, tracer: tr}

	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		tracing = prevTracing
	})
	return exporter
}

func TestOtelEnabled_NeedsEndpointEnv(t *testing.T) {
	// Default state — no env vars — must keep tracing disabled. This
	// is the contract: squadronctl behaves identically to its pre-v0.18
	// self unless the operator explicitly opts in.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	assert.False(t, otelEnabled(), "no endpoint env => disabled")

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317")
	assert.True(t, otelEnabled(), "endpoint env set => enabled")
}

func TestExtractEnvTraceparent_HonorsConvention(t *testing.T) {
	// otel-cli sets TRACEPARENT in the wrapped command's env. We must
	// extract it so squadronctl's root span nests under the CI span
	// rather than starting a fresh trace. Pin the wire shape because
	// other CI tooling depends on this same convention.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	t.Setenv("TRACEPARENT", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	ctx := extractEnvTraceparent(context.Background())
	sc := trace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid(), "TRACEPARENT must produce a valid SpanContext")
	assert.Equal(t, "0123456789abcdef0123456789abcdef", sc.TraceID().String())
	assert.Equal(t, "0123456789abcdef", sc.SpanID().String())
}

func TestExtractEnvTraceparent_NoEnv_NoOp(t *testing.T) {
	// Absent env vars produce no span context. Squadronctl will start
	// its own root rather than running under a phantom parent.
	t.Setenv("TRACEPARENT", "")
	ctx := extractEnvTraceparent(context.Background())
	assert.False(t, trace.SpanContextFromContext(ctx).IsValid())
}

func TestBeginCommandSpan_Disabled_StillForwardsInheritedTraceparent(t *testing.T) {
	// Even with no local OTLP exporter wired, TRACEPARENT in env must
	// flow through the returned context so the outbound HTTP transport
	// can forward it. The end-fn is a safe no-op when there's no
	// local span to close.
	// Reset global tracing state explicitly.
	prev := tracing
	tracing = &tracingState{}
	t.Cleanup(func() { tracing = prev })

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.Setenv("TRACEPARENT", "00-deadbeefdeadbeefdeadbeefdeadbeef-cafebabecafebabe-01")

	ctx, end := beginCommandSpan(context.Background(), "squadronctl agents list")
	defer end(nil)

	sc := trace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid(), "inherited traceparent must propagate through disabled tracer")
	assert.Equal(t, "deadbeefdeadbeefdeadbeefdeadbeef", sc.TraceID().String())
}

func TestBeginCommandSpan_Enabled_ProducesSpanUnderInheritedParent(t *testing.T) {
	// With local tracing on AND TRACEPARENT inherited, squadronctl's
	// span must end up as a child of the wrapping CI span — same trace
	// ID, different span ID. Critical for the four-tier story:
	// CI -> squadronctl -> Squadron API -> agent must all share a
	// trace id end-to-end.
	exporter := installTestTracer(t)

	t.Setenv("TRACEPARENT", "00-aabbccddeeff00112233445566778899-1122334455667788-01")
	_, end := beginCommandSpan(context.Background(), "squadronctl rollouts create")
	end(nil)

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	got := spans[0]
	assert.Equal(t, "squadronctl rollouts create", got.Name)
	assert.Equal(t, "aabbccddeeff00112233445566778899", got.SpanContext.TraceID().String(),
		"squadronctl span must adopt the inherited trace id")
	assert.Equal(t, "1122334455667788", got.Parent.SpanID().String(),
		"parent of squadronctl span must be the inherited CI span")
}

func TestCLIAPI_InjectsTraceparentOnOutboundRequests(t *testing.T) {
	// End-to-end: with an active squadronctl root span, the
	// cliapi.Client's otelhttp-wrapped transport must inject a W3C
	// traceparent header on every API request. Closes the wire
	// contract that lets the server-side otelgin middleware bind
	// Squadron's API span as a child of squadronctl's.
	installTestTracer(t)

	var gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	ctx, end := beginCommandSpan(context.Background(), "squadronctl test req")
	defer end(nil)

	c := cliapi.New(srv.URL, "")
	require.NoError(t, c.Do(ctx, http.MethodGet, "/api/v1/agents", nil, nil, nil))

	assert.NotEmpty(t, gotTraceparent, "outbound request must carry a traceparent")
	// Wire shape: 00-<trace-id>-<span-id>-<flags>
	assert.Regexp(t, `^00-[0-9a-f]{32}-[0-9a-f]{16}-0[01]$`, gotTraceparent)
}

func TestCLIAPI_DisabledTracer_StillForwardsInheritedTraceparent(t *testing.T) {
	// Edge case that matters most for users: CI wraps squadronctl with
	// otel-cli (sets TRACEPARENT) but doesn't configure squadronctl to
	// publish its own spans. The trace chain still has to survive:
	// the outbound API request must carry the inherited traceparent so
	// the server-side gin handler is a child of the CI span. Without
	// this, "wrap squadronctl in otel-cli" — the path the docs
	// recommend — would silently break the trace.

	// Reset to a "disabled" world: no local tracer, only the W3C
	// propagator installed.
	prev := tracing
	prevProp := otel.GetTextMapPropagator()
	prevTP := otel.GetTracerProvider()
	tracing = &tracingState{}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	// Use the global (no-op) provider — what a real process with no
	// OTLP endpoint configured would see.
	otel.SetTracerProvider(prevTP)
	t.Cleanup(func() {
		otel.SetTextMapPropagator(prevProp)
		tracing = prev
	})

	const inheritedTraceID = "11112222333344445555666677778888"
	t.Setenv("TRACEPARENT", "00-"+inheritedTraceID+"-1234567890abcdef-01")

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	ctx, end := beginCommandSpan(context.Background(), "squadronctl x")
	defer end(nil)

	c := cliapi.New(srv.URL, "")
	require.NoError(t, c.Do(ctx, http.MethodGet, "/api/v1/anything", nil, nil, nil))

	require.NotEmpty(t, got, "outbound request must carry traceparent even without local tracer")
	// The trace id must be the inherited one — that's how the
	// server-side span ends up in the right trace tree.
	assert.Contains(t, got, inheritedTraceID,
		"forwarded traceparent must keep the inherited trace id, got %q", got)
}

func TestStripScheme_HandlesCommonFormsOperatorsType(t *testing.T) {
	// Operators frequently set OTEL_EXPORTER_OTLP_ENDPOINT to either
	// "host:port" or a full URL. WithEndpoint expects the bare form;
	// stripScheme is the small adapter. Pin both shapes.
	assert.Equal(t, "otel-collector:4317", stripScheme("otel-collector:4317"))
	assert.Equal(t, "otel-collector:4317", stripScheme("http://otel-collector:4317"))
	assert.Equal(t, "api.honeycomb.io", stripScheme("https://api.honeycomb.io"))
}
