// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package alerts holds OTel tracing primitives for the alert
// evaluator. Lives in its own package (not internal/alerting/) because
// internal/alerting/ already has a lot going on; keeping the tracer
// concerns next to it but separate makes the evaluator's call sites
// short and the trace plumbing easy to find.
//
// Pattern mirrors internal/rollouts/tracer.go and internal/selftel:
// thin wrapper over a trace.Tracer, nil-receiver-safe so callers can
// treat the tracer as an unconditional dependency. When telemetry is
// disabled, the engine receives a nil *Tracer and every method
// short-circuits.
package alerts

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Tracer wraps an OTel trace.Tracer with the alert-evaluation
// lifecycle. Unlike the rollout tracer, alert evaluations are
// synchronous and short-lived (a single QL query against
// telemetrystore), so no in-memory map of long-running spans is
// needed — the caller holds the Evaluation handle for the duration
// of one evaluateRule call and ends it before returning.
type Tracer struct {
	tracer trace.Tracer
}

// NewTracer returns a tracer wrapper. Pass a nil trace.Tracer to
// disable alert tracing entirely (the no-op path); pass a real one
// wired to selftel.Publisher.Tracer("squadron/alerts") for real
// export.
func NewTracer(t trace.Tracer) *Tracer {
	if t == nil {
		return nil
	}
	return &Tracer{tracer: t}
}

// Rule is the subset of alert-rule attributes the tracer needs.
// Defined here (not as a direct services.AlertRule reference) so this
// package doesn't have to import services — services imports alerts
// in main.go's wiring path and a back-edge would create a cycle if
// we ever moved the tracer dep onto the service.
type Rule struct {
	ID                string
	Name              string
	Query             string
	ThresholdOperator string
	ThresholdValue    float64
	Severity          string
}

// Evaluation is the handle the caller holds for the duration of one
// rule evaluation. Methods on it accumulate attributes and events on
// the underlying span; End closes the span. Nil-receiver-safe.
type Evaluation struct {
	span trace.Span
}

// BeginEvaluation opens a span bracketing one evaluation cycle of a
// rule. Attributes for the rule's static identity (id, name, query,
// threshold) land at start time; observed values and fired/resolved
// state arrive via SetObservedValue once the QL query returns.
//
// Returns a nil-safe Evaluation handle even when t == nil so the
// caller can chain method calls without checking.
func (t *Tracer) BeginEvaluation(ctx context.Context, rule Rule) *Evaluation {
	if t == nil {
		return nil
	}
	_, span := t.tracer.Start(ctx, "alert.evaluate",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("squadron.target_type", "rule"),
			attribute.String("squadron.target_id", rule.ID),
			attribute.String("squadron.rule_id", rule.ID),
			attribute.String("squadron.rule_name", rule.Name),
			attribute.String("squadron.rule.query", rule.Query),
			attribute.String("squadron.operator", rule.ThresholdOperator),
			attribute.Float64("squadron.threshold", rule.ThresholdValue),
			attribute.String("squadron.rule.severity", rule.Severity),
		),
	)
	return &Evaluation{span: span}
}

// RecordQueryError marks the evaluation as failed due to a QL query
// error. The span's status flips to Error with the error string as
// the message — distinct from "fired" (which is a successful
// evaluation that returned a value above the threshold).
func (e *Evaluation) RecordQueryError(err error) {
	if e == nil || e.span == nil {
		return
	}
	e.span.RecordError(err)
	e.span.SetStatus(codes.Error, fmt.Sprintf("query: %v", err))
}

// SetObservedValue records the scalar the QL query produced and
// whether the threshold comparison triggered. A fired alert is still
// a successful evaluation from the tracer's perspective — the QL
// query worked, the threshold was applied, the rule yielded its
// result. The span status stays Ok regardless of fired/!fired.
func (e *Evaluation) SetObservedValue(value float64, fired bool) {
	if e == nil || e.span == nil {
		return
	}
	e.span.SetAttributes(
		attribute.Float64("squadron.observed_value", value),
		attribute.Bool("squadron.fired", fired),
	)
}

// RecordWebhookDispatched attaches an event noting that the firing/
// resolution payload was POSTed to the configured webhook URL. URL
// is logged for cross-referencing the destination but the operator
// can scope it to a host attribute if they don't want full URLs in
// trace payloads.
func (e *Evaluation) RecordWebhookDispatched(url, state string) {
	if e == nil || e.span == nil {
		return
	}
	e.span.AddEvent("dispatched_to_webhook",
		trace.WithAttributes(
			attribute.String("squadron.webhook.url", url),
			attribute.String("squadron.webhook.state", state),
		),
	)
}

// End closes the evaluation span. If RecordQueryError already set
// Error status, it sticks; otherwise the default Ok status applies.
func (e *Evaluation) End() {
	if e == nil || e.span == nil {
		return
	}
	e.span.End()
}
