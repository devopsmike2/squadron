// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// tracingState bundles the SDK pieces squadronctl owns at process
// lifetime. When tracing is disabled (the default — no
// OTEL_EXPORTER_OTLP_ENDPOINT), tp is nil and Shutdown is a no-op.
// The W3C propagator is installed regardless so that even with the
// no-op tracer, the outbound HTTP transport can still forward a
// TRACEPARENT env var it inherited from a wrapping CI runner.
type tracingState struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
}

// global state — squadronctl is a one-shot CLI, no concurrency concerns.
var tracing = &tracingState{}

// otelEnabled reports whether the operator wants OTLP export. The CLI
// follows the standard OTel env-var convention: setting
// OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT)
// turns export on. Anything else stays a no-op so squadronctl works
// identically to its pre-v0.18 behavior when no observability is wired
// up.
func otelEnabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// initTracing wires the OTel SDK if OTEL_EXPORTER_OTLP_ENDPOINT (or
// the trace-specific variant) is set. Always sets the global W3C
// propagator so the HTTP transport can forward an inherited
// traceparent even when local export is off — this is the case where
// CI wraps squadronctl with otel-cli but the CLI itself doesn't
// publish.
func initTracing(ctx context.Context) error {
	// Install the propagator unconditionally — cheap, side-effect-free,
	// and lets the cliapi HTTP transport forward inbound traceparent
	// headers even in the no-export path.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if !otelEnabled() {
		return nil
	}

	exporter, err := buildCLIExporter(ctx)
	if err != nil {
		// Non-fatal: emit a stderr note so the operator notices the
		// misconfig, but don't fail the CLI invocation. squadronctl's
		// job is to call the API; export is a nice-to-have.
		fmt.Fprintf(os.Stderr, "squadronctl: trace export disabled: %v\n", err)
		return nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName()),
		),
	)
	if err != nil {
		return fmt.Errorf("build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		// SimpleSpanProcessor: CLI is one-shot, batched export would
		// risk dropping the squadronctl span if the process exits
		// before the next batch flush. Simple is also a single
		// network round-trip per span — fine for the handful of spans
		// a single invocation produces.
		sdktrace.WithSyncer(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	tracing.tp = tp
	tracing.tracer = tp.Tracer("squadronctl")
	return nil
}

// shutdownTracing flushes any pending spans. Bounded by a short
// timeout so a hung exporter can't keep the CLI hanging after the
// user's command completes — they'd rather see the result + lose the
// final span than wait on a dead OTLP endpoint.
func shutdownTracing() {
	if tracing == nil || tracing.tp == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tracing.tp.Shutdown(ctx)
}

// beginCommandSpan opens the per-invocation root span the rest of
// squadronctl runs under. The span name is "squadronctl.<verb> <noun>"
// (e.g. "squadronctl.rollout create") so trace UIs index it next to
// related operations. When tracing is disabled, returns the input ctx
// unchanged and a no-op end function — callers can defer end()
// unconditionally.
//
// Honors a TRACEPARENT env var (the convention from W3C and
// otel-cli's wrapper) so a CI runner wrapping squadronctl with its
// own span gets squadronctl as a child rather than a fresh root.
func beginCommandSpan(ctx context.Context, name string) (context.Context, func(error)) {
	if tracing == nil || tracing.tracer == nil {
		// Even with no local tracer, honor inbound TRACEPARENT so the
		// HTTP request will forward it. Build a no-op span that
		// captures the inherited context for downstream Extract.
		ctx = extractEnvTraceparent(ctx)
		return ctx, func(error) {}
	}
	ctx = extractEnvTraceparent(ctx)
	ctx, span := tracing.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
	)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}
}

// extractEnvTraceparent picks up the W3C traceparent (and tracestate)
// from the standard env vars if present. The convention dates to
// otel-cli and is now de facto: `otel-cli exec` sets TRACEPARENT in
// the child's env so wrapped commands can join the trace.
func extractEnvTraceparent(ctx context.Context) context.Context {
	tp := os.Getenv("TRACEPARENT")
	if tp == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{"traceparent": tp}
	if ts := os.Getenv("TRACESTATE"); ts != "" {
		carrier["tracestate"] = ts
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// serviceName returns the OTel service.name attribute. Operators can
// override via OTEL_SERVICE_NAME — useful when wrapping squadronctl
// inside a CI job that wants its own service identity.
func serviceName() string {
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		return v
	}
	return "squadronctl"
}

// buildCLIExporter mirrors selftel's exporter constructor: pick grpc
// or http based on OTEL_EXPORTER_OTLP_PROTOCOL (default grpc). Reads
// the OTel-standard env vars so squadronctl Just Works under any
// canonical OTel setup the operator's CI already uses.
func buildCLIExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	protocol := strings.ToLower(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	if p := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"); p != "" {
		protocol = strings.ToLower(p)
	}
	insecure := strings.EqualFold(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), "true")

	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	switch protocol {
	case "", "grpc":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(stripScheme(endpoint))}
		if insecure || strings.HasPrefix(endpoint, "http://") {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptrace.New(dialCtx, otlptracegrpc.NewClient(opts...))
	case "http", "http/protobuf":
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(stripScheme(endpoint))}
		if insecure || strings.HasPrefix(endpoint, "http://") {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		return otlptrace.New(dialCtx, otlptracehttp.NewClient(opts...))
	default:
		return nil, fmt.Errorf("unknown OTEL_EXPORTER_OTLP_PROTOCOL %q", protocol)
	}
}

// stripScheme removes "http://" or "https://" prefixes so endpoint
// strings work with the OTel exporters' WithEndpoint(host:port) form.
// Operators frequently set OTEL_EXPORTER_OTLP_ENDPOINT to a full URL;
// we accept both.
func stripScheme(endpoint string) string {
	for _, prefix := range []string{"http://", "https://"} {
		if strings.HasPrefix(endpoint, prefix) {
			return strings.TrimPrefix(endpoint, prefix)
		}
	}
	return endpoint
}
