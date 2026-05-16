// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"encoding/json"

	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceparentCapability is the Squadron-defined OpAMP custom capability
// for cross-boundary W3C TraceContext propagation. Squadron sends this
// capability inside a CustomMessage alongside config pushes when an
// active OTel span is on the calling context; an OTel-aware agent can
// extract the headers and reparent its own spans to the originating
// trace.
//
// The OpAMP spec doesn't (yet) define a standard capability for this.
// Until it does, the field is Squadron-specific:
//
//	Capability = "io.squadron.traceparent.v1"
//	Type       = "context"
//	Data       = JSON-encoded { "traceparent": "...", "tracestate": "..." }
//
// Agents that don't recognize the capability ignore the message per
// the spec, so this is fully backward-compatible. See
// docs/self-monitoring.md "Tracing across the agent boundary" for the
// agent-side consumption sketch.
const (
	TraceparentCapability  = "io.squadron.traceparent.v1"
	TraceparentMessageType = "context"
	traceparentHeader      = "traceparent"
	tracestateHeader       = "tracestate"
)

// traceparentCarrier adapts a JSON map to OTel's TextMapCarrier
// interface so we can use the global propagator to fill it. Reverse
// of what the otelhttp middleware does on the inbound side.
type traceparentCarrier map[string]string

func (c traceparentCarrier) Get(key string) string { return c[key] }
func (c traceparentCarrier) Set(key, value string) { c[key] = value }
func (c traceparentCarrier) Keys() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// buildTraceparentMessage inspects ctx for an active OTel span and, if
// one is present, returns a *protobufs.CustomMessage carrying the
// propagated trace context. Returns nil when no span is active —
// callers MUST tolerate nil rather than emitting a CustomMessage with
// an all-zeros traceparent (which is a valid-but-invalid sentinel and
// would mislead agent-side instrumentation into starting a fresh
// trace under the bogus parent).
func buildTraceparentMessage(ctx context.Context) *protobufs.CustomMessage {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return nil
	}

	// Inject via the global propagator so the headers Squadron emits
	// match what otelgin extracts on the inbound side. Future-proofs
	// against propagator changes (e.g. operators switching to B3) —
	// they'd flip the global once at selftel init and both sides
	// follow.
	carrier := traceparentCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	// We only care about the W3C trace-context headers for this
	// payload. The composite propagator from selftel.New also writes
	// Baggage entries, which we drop intentionally — baggage often
	// carries operator-private metadata (tenant id, deploy version)
	// and shipping it to every agent in the fleet on every push is
	// unnecessary noise. If operators ever want baggage propagation,
	// flip this to include the full carrier.
	payload := map[string]string{}
	if v := carrier[traceparentHeader]; v != "" {
		payload[traceparentHeader] = v
	}
	if v := carrier[tracestateHeader]; v != "" {
		payload[tracestateHeader] = v
	}
	if len(payload) == 0 {
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal of map[string]string can't fail in practice;
		// if it somehow does, return nil so the OpAMP send proceeds
		// without the trace context rather than blocking the push.
		return nil
	}
	return &protobufs.CustomMessage{
		Capability: TraceparentCapability,
		Type:       TraceparentMessageType,
		Data:       body,
	}
}

// Compile-time assertion: traceparentCarrier satisfies the OTel
// TextMapCarrier interface. Catches accidental signature drift at
// build time.
var _ propagation.TextMapCarrier = traceparentCarrier{}
