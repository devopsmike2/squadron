// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package configs holds OTel tracing primitives for config-push
// operations. Lives in its own package (not internal/opamp/) because
// the push happens at multiple layers — the rollout engine, the
// per-agent API handler, the (future) drift-remediation loop — and
// each wraps the underlying ConfigSender call. The tracer sits next
// to all of those.
//
// Pattern mirrors internal/alerts/tracer.go: thin wrapper over a
// trace.Tracer, nil-receiver-safe. Bracket the synchronous OpAMP
// push call with BeginPush / End; record ack/nack mid-span.
package configs

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// PushSource enumerates the callers that can initiate a config push.
// Recorded as the squadron.push_source attribute so operators can
// filter trace spans by the operation that triggered them.
//
// Adding a new source means a new constant + an updated docs table
// in self-monitoring.md.
type PushSource string

const (
	SourceRollout            PushSource = "rollout"             // rollout engine, stage apply or rollback
	SourceDirect             PushSource = "direct"              // API handler, single-agent push
	SourceGroup              PushSource = "group"               // API handler, whole-group config assignment
	SourceDriftRemediation   PushSource = "drift_remediation"   // (future) auto-resync on drift detection
)

// Tracer wraps an OTel trace.Tracer with the config-push lifecycle.
// Pushes are synchronous (the OpAMP layer blocks until ack/timeout),
// so the span brackets the whole call rather than relying on an
// active-span map. Nil-receiver-safe.
type Tracer struct {
	tracer trace.Tracer
}

// NewTracer returns a tracer wrapper. Pass a nil trace.Tracer to
// disable config-push tracing; pass a real one wired to
// selftel.Publisher.Tracer("squadron/configs") for real export.
func NewTracer(t trace.Tracer) *Tracer {
	if t == nil {
		return nil
	}
	return &Tracer{tracer: t}
}

// Push is the handle the caller holds for the duration of one
// SendConfigToAgent call. RecordAck or RecordNack records the
// outcome as a span event; End closes the span and sets Ok or
// Error status based on whether RecordNack was called.
type Push struct {
	span    trace.Span
	nacked  bool
	reason  string
}

// BeginPush opens a span bracketing one config-push operation. All
// callers pass agent ID + config ID; groupID is optional (empty for
// single-agent direct pushes); source records the operation that
// triggered the push.
//
// Returns a nil-safe Push handle even when t == nil.
func (t *Tracer) BeginPush(ctx context.Context, agentID, configID, groupID string, source PushSource) *Push {
	if t == nil {
		return nil
	}
	attrs := []attribute.KeyValue{
		attribute.String("squadron.target_type", "agent"),
		attribute.String("squadron.target_id", agentID),
		attribute.String("squadron.agent_id", agentID),
		attribute.String("squadron.config_id", configID),
		attribute.String("squadron.push_source", string(source)),
	}
	if groupID != "" {
		attrs = append(attrs, attribute.String("squadron.group_id", groupID))
	}
	_, span := t.tracer.Start(ctx, "config.push",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return &Push{span: span}
}

// RecordAck attaches an opamp_ack event to the push span. The push
// completes successfully — End will close the span with Ok status.
func (p *Push) RecordAck() {
	if p == nil || p.span == nil {
		return
	}
	p.span.AddEvent("opamp_ack")
}

// RecordNack attaches an opamp_nack event with a reason attribute.
// End will close the span with Error status using the reason as the
// status message. Common reasons: "timeout", "agent not found",
// "agent does not support remote config".
func (p *Push) RecordNack(reason string) {
	if p == nil || p.span == nil {
		return
	}
	p.nacked = true
	p.reason = reason
	p.span.AddEvent("opamp_nack",
		trace.WithAttributes(attribute.String("squadron.nack_reason", reason)),
	)
}

// End closes the push span. If RecordNack was called, status flips
// to Error with the recorded reason as the message; otherwise the
// span ends with Ok (a successful push).
func (p *Push) End() {
	if p == nil || p.span == nil {
		return
	}
	if p.nacked {
		p.span.SetStatus(codes.Error, p.reason)
	} else {
		p.span.SetStatus(codes.Ok, "")
	}
	p.span.End()
}
