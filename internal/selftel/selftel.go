// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package selftel makes Squadron emit its own state changes as
// OpenTelemetry traces — the "dogfood" story for a telemetry control
// plane.
//
// When enabled, every audit event becomes a span exported to a
// caller-configured OTLP endpoint (typically the operator's existing
// observability stack — Tempo, Jaeger, SigNoz, Honeycomb, etc.). The
// audit log in SQLite stays the source of truth; OTel export is
// best-effort and any failure is logged but never blocks the durable
// recording.
//
// Why spans for audit events: Squadron's audit entries are
// instantaneous state changes ("rollout aborted", "config pushed",
// "agent registered"), not bracketing operations. Modeling them as
// point-event-shaped spans (start = end + 1ns) is a small abuse of the
// trace model but matches what operators actually want to see — a
// flat list of events with rich attributes in their Jaeger / Tempo
// view. Native OTel logs would be the more correct primitive, but the
// logs SDK is still stabilizing as of late 2024; we'll add it as a
// second emitter once the API is solid.
//
// What's NOT in scope:
//   - Metrics export. Squadron's Prometheus /metrics endpoint is the
//     primary metrics path; an OTel metrics bridge can come later.
//   - Trace propagation from agents. Agents emit their own telemetry
//     to Squadron's OTLP receiver; this package is about Squadron's
//     control-plane self-monitoring, not agent telemetry forwarding.
package selftel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Config controls a Publisher's behavior. Mirrors internal/config.TelemetryConfig
// so callers don't drag a config dependency into this package.
type Config struct {
	Enabled     bool
	ServiceName string
	Endpoint    string
	Protocol    string            // "grpc" | "http"
	Headers     map[string]string
	Insecure    bool
}

// Publisher emits Squadron audit entries as OTel spans. The zero value
// is a valid no-op publisher — Squadron always constructs one and only
// the operator's enabled flag controls whether spans actually get
// exported.
type Publisher struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
	logger *zap.Logger
}

// New constructs a Publisher. When cfg.Enabled is false, returns a
// no-op Publisher with tp=nil so callers can mount it unconditionally.
// When enabled, the OTLP exporter is configured and connected lazily —
// the first export will block briefly to dial; failures fall back to
// the no-op path with a warning so a wrong endpoint doesn't crash
// Squadron at startup.
func New(ctx context.Context, cfg Config, logger *zap.Logger) (*Publisher, error) {
	if !cfg.Enabled {
		logger.Debug("selftel: disabled, no OTLP export")
		return &Publisher{logger: logger}, nil
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("telemetry.otlp.endpoint is required when telemetry.enabled is true")
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "squadron"
	}

	exporter, err := buildExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build OTLP exporter: %w", err)
	}

	// Resource carries the service.name and any future host/version
	// attributes. semconv.SchemaURL pins the attribute schema version
	// so downstream tools render expected fields.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	// Batcher is fine for audit traffic — Squadron emits a span per
	// state change, not per request. The defaults (5s batch timeout)
	// match the audit log's "durable then maybe export" expectations.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	// Setting the global provider lets other parts of Squadron use
	// otel.Tracer(...) directly later (e.g. rollout engine spans in
	// a future patch) without each caller having to plumb the provider
	// through.
	otel.SetTracerProvider(tp)

	// W3C TraceContext + Baggage propagator. When set as the global
	// propagator, inbound HTTP requests carrying a traceparent header
	// have their span context extracted into the request context
	// automatically (via otelgin / otelhttp middleware). Squadron
	// becomes a participant in the caller's trace rather than always
	// starting a fresh root. Baggage carries non-tracing context
	// (tenant id, deploy version, etc.) operators may already use.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logger.Info("selftel: OTLP trace export enabled",
		zap.String("endpoint", cfg.Endpoint),
		zap.String("protocol", cfg.Protocol),
		zap.String("service_name", cfg.ServiceName))

	return &Publisher{
		tp:     tp,
		tracer: tp.Tracer("squadron/audit"),
		logger: logger,
	}, nil
}

// PublishAuditEvent emits one OTel span for an audit event. Best-effort
// and synchronous: the BatchSpanProcessor handles the actual network
// I/O on its own goroutine, so this call returns quickly even when the
// OTLP endpoint is slow or unreachable.
//
// No-op when the publisher is disabled (tp == nil).
//
// Schema (kept simple on purpose — the audit log is still the source
// of truth; OTel is just for "search Squadron activity in our usual
// observability tools"):
//
//	Span name:    <event_type>
//	Attributes:
//	  squadron.actor       = entry.Actor
//	  squadron.event_type  = entry.EventType
//	  squadron.target_type = entry.TargetType
//	  squadron.target_id   = entry.TargetID
//	  squadron.action      = entry.Action
//	  + every primitive payload key as squadron.payload.<key>
func (p *Publisher) PublishAuditEvent(ctx context.Context, entry AuditEntry) {
	if p == nil || p.tracer == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("squadron.actor", entry.Actor),
		attribute.String("squadron.event_type", entry.EventType),
		attribute.String("squadron.target_type", entry.TargetType),
		attribute.String("squadron.target_id", entry.TargetID),
		attribute.String("squadron.action", entry.Action),
	}
	// Add primitive payload fields as flat attributes. Non-primitive
	// values (maps, slices) would blow up the cardinality and don't
	// search cleanly in trace UIs — we deliberately skip them. The
	// audit log retains the full payload for forensic queries.
	for k, v := range entry.Payload {
		key := "squadron.payload." + safeAttrKey(k)
		switch x := v.(type) {
		case string:
			attrs = append(attrs, attribute.String(key, x))
		case bool:
			attrs = append(attrs, attribute.Bool(key, x))
		case int:
			attrs = append(attrs, attribute.Int(key, x))
		case int64:
			attrs = append(attrs, attribute.Int64(key, x))
		case float64:
			attrs = append(attrs, attribute.Float64(key, x))
		}
	}
	// Point-event span: start now, end immediately. The trace UI
	// renders it as a zero-duration event with the attributes visible
	// for filtering / search.
	_, span := p.tracer.Start(ctx, entry.EventType,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	span.End()
}

// Tracer returns an OTel tracer scoped to the given instrumentation
// name. Callers that want to create their own spans (e.g. the rollout
// engine wrapping a rollout's lifecycle) reach for this rather than
// going through PublishAuditEvent.
//
// When the publisher is disabled (no provider), returns the global
// tracer provider's tracer — which is a no-op by default, so callers
// get a tracer-shaped object that simply discards spans. Callers
// don't need to nil-check the result.
func (p *Publisher) Tracer(name string) trace.Tracer {
	if p == nil || p.tp == nil {
		return otel.Tracer(name)
	}
	return p.tp.Tracer(name)
}

// Shutdown flushes pending spans and tears down the exporter. Safe to
// call on a disabled publisher (no-op).
func (p *Publisher) Shutdown(ctx context.Context) error {
	if p == nil || p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// AuditEntry mirrors services.AuditEntry without taking a dependency
// on the services package. selftel sits below services in the import
// graph; the caller maps its native shape to this one.
type AuditEntry struct {
	Actor      string
	EventType  string
	TargetType string
	TargetID   string
	Action     string
	Payload    map[string]any
}

// buildExporter constructs the OTLP exporter the trace provider feeds
// spans to. Picks between grpc and http based on cfg.Protocol; the
// grpc client is the default since most OTLP endpoints (collectors,
// observability vendors) support it natively.
func buildExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch strings.ToLower(cfg.Protocol) {
	case "", "grpc":
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		// Short dial timeout so a bad endpoint doesn't hang startup.
		// Production OTLP collectors typically accept the connection
		// in <100ms even under load.
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return otlptrace.New(dialCtx, otlptracegrpc.NewClient(opts...))
	case "http":
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(cfg.Endpoint),
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return otlptrace.New(dialCtx, otlptracehttp.NewClient(opts...))
	default:
		return nil, fmt.Errorf("unknown telemetry.otlp.protocol %q (want \"grpc\" or \"http\")", cfg.Protocol)
	}
}

// safeAttrKey rewrites payload keys into OTel-attribute-safe form.
// OTel attribute keys are conventionally dotted lowercase; payload
// keys are already snake_case from the JSON marshaling, so this is
// mostly a defensive lowercase + replace.
func safeAttrKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(k, " ", "_"))
}
