// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestOtelGinMiddleware_ExtractsInboundTraceparent verifies the
// traceparent-propagation chain wired by selftel.New + the API
// server: an inbound W3C traceparent header should land on the
// request context as a span context, and the otelgin-created server
// span should be a child of it.
//
// We don't wire the full API server here — we'd need a Squadron-
// shaped config to do that and the propagation behavior is purely a
// middleware concern. A minimal gin app with the same middleware
// stack is sufficient to pin the contract.
func TestOtelGinMiddleware_ExtractsInboundTraceparent(t *testing.T) {
	// Set up a tracer provider with an in-memory exporter so we can
	// inspect spans without a network exporter.
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	// otelgin pulls from the global provider. selftel.New does the
	// same SetTracerProvider in production; we duplicate here for
	// test isolation.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(otelgin.Middleware("squadron"))
	var capturedSpan trace.Span
	router.GET("/ping", func(c *gin.Context) {
		capturedSpan = trace.SpanFromContext(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// Forge an inbound traceparent. Version 00, valid trace/span IDs,
	// "sampled" flag (01). Format documented at
	// https://www.w3.org/TR/trace-context/#traceparent-header.
	const inboundTraceID = "0af7651916cd43dd8448eb211c80319c"
	const inboundSpanID = "b7ad6b7169203331"
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("traceparent", "00-"+inboundTraceID+"-"+inboundSpanID+"-01")
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, capturedSpan)
	sc := capturedSpan.SpanContext()
	require.True(t, sc.IsValid(), "handler should see a valid span context on the request")
	assert.Equal(t, inboundTraceID, sc.TraceID().String(),
		"inbound traceparent's trace ID must be inherited so the server span is part of the caller's trace")

	// The otelgin-emitted server span should be a child of the
	// inbound span ID.
	spans := exporter.GetSpans()
	require.NotEmpty(t, spans, "otelgin should emit a server span for the request")
	var serverSpan tracetest.SpanStub
	for _, s := range spans {
		if s.Name != "" {
			serverSpan = s
			break
		}
	}
	assert.Equal(t, inboundSpanID, serverSpan.Parent.SpanID().String(),
		"server span's parent should be the inbound traceparent's span ID")
}

func TestOtelGinMiddleware_NoTraceparentStartsFreshRoot(t *testing.T) {
	// A request without traceparent should still produce a span,
	// just as a fresh root rather than child of anything. Covers the
	// "internal cron / curl-with-no-otel" path.
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(otelgin.Middleware("squadron"))
	router.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	router.ServeHTTP(w, req)

	spans := exporter.GetSpans()
	require.NotEmpty(t, spans)
	// Root spans have no parent. otelgin emits one of these when no
	// inbound traceparent is present.
	assert.False(t, spans[0].Parent.IsValid(), "no inbound traceparent should yield a root span")
}
