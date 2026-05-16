// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package alerts

import (
	"context"
	"errors"
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
	return NewTracer(tp.Tracer("squadron/alerts-test")), exporter
}

func sampleRule() Rule {
	return Rule{
		ID:                "rule-1",
		Name:              "high drift rate",
		Query:             "fleet_drift_status_drifted",
		ThresholdOperator: ">",
		ThresholdValue:    5,
		Severity:          "warning",
	}
}

func TestTracer_NilReceiverIsSafe(t *testing.T) {
	// Evaluator constructs the tracer unconditionally and treats nil
	// as the disabled path. Every method must short-circuit without
	// panicking.
	var tr *Tracer
	eval := tr.BeginEvaluation(context.Background(), sampleRule())
	require.NotPanics(t, func() {
		eval.RecordQueryError(errors.New("boom"))
		eval.SetObservedValue(7, true)
		eval.RecordWebhookDispatched("https://example.com", "firing")
		eval.End()
	})
}

func TestTracer_BeginEvaluation_HappyPathSpanShape(t *testing.T) {
	// A successful evaluation that fired: span ends Ok with the
	// canonical squadron.* attribute set. A fired alert is still a
	// successful evaluation from the tracer's perspective (the QL
	// query worked) — we don't flip to Error just because the
	// threshold tripped.
	tr, exporter := newTestTracer(t)

	eval := tr.BeginEvaluation(context.Background(), sampleRule())
	eval.SetObservedValue(7.0, true)
	eval.RecordWebhookDispatched("https://hooks.example.com/squadron", "firing")
	eval.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "alert.evaluate", span.Name)
	assert.Equal(t, codes.Unset, span.Status.Code,
		"a fired alert is still a successful evaluation; status should be Unset (treated as Ok)")

	attrs := attrsByKey(span.Attributes)
	assert.Equal(t, "rule-1", attrs["squadron.target_id"])
	assert.Equal(t, "rule-1", attrs["squadron.rule_id"])
	assert.Equal(t, "high drift rate", attrs["squadron.rule_name"])
	assert.Equal(t, ">", attrs["squadron.operator"])
	assert.Equal(t, 5.0, attrs["squadron.threshold"])
	assert.Equal(t, 7.0, attrs["squadron.observed_value"])
	assert.Equal(t, true, attrs["squadron.fired"])

	// The dispatched_to_webhook event should appear on the span's
	// events list with the URL + state attributes.
	require.Len(t, span.Events, 1)
	assert.Equal(t, "dispatched_to_webhook", span.Events[0].Name)
	evAttrs := attrsByKey(span.Events[0].Attributes)
	assert.Equal(t, "https://hooks.example.com/squadron", evAttrs["squadron.webhook.url"])
	assert.Equal(t, "firing", evAttrs["squadron.webhook.state"])
}

func TestTracer_BeginEvaluation_NotFired_StillOk(t *testing.T) {
	// A below-threshold value is a successful evaluation. fired=false
	// records on the span; status stays Unset/Ok.
	tr, exporter := newTestTracer(t)
	eval := tr.BeginEvaluation(context.Background(), sampleRule())
	eval.SetObservedValue(3.0, false)
	eval.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	attrs := attrsByKey(spans[0].Attributes)
	assert.Equal(t, false, attrs["squadron.fired"])
	assert.Equal(t, codes.Unset, spans[0].Status.Code)
}

func TestTracer_RecordQueryError_FlipsToError(t *testing.T) {
	// A QL query that errored out is a failed evaluation. Status
	// flips to Error so trace UIs render the span red; operators
	// searching for "alert evaluations that errored" filter on
	// status code rather than parsing message text.
	tr, exporter := newTestTracer(t)
	eval := tr.BeginEvaluation(context.Background(), sampleRule())
	eval.RecordQueryError(errors.New("parse error: unexpected token"))
	eval.End()

	spans := exporter.GetSpans()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status.Code)
	assert.Contains(t, spans[0].Status.Description, "query:")
	// Span errors should also be recorded as span events for
	// trace UIs that surface them separately from status.
	assert.NotEmpty(t, spans[0].Events, "RecordError should add a span event")
}

func TestTracer_End_TwiceIsSafe(t *testing.T) {
	// Defensive: deferred End() in the evaluator might double-fire
	// if a future refactor adds an explicit early-return End. The
	// OTel SDK tolerates double-end, but we pin the behavior so a
	// future maintainer doesn't worry about it.
	tr, _ := newTestTracer(t)
	eval := tr.BeginEvaluation(context.Background(), sampleRule())
	eval.End()
	require.NotPanics(t, func() { eval.End() })
}

func attrsByKey(kvs []attribute.KeyValue) map[string]any {
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}
