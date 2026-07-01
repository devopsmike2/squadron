// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package receiver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// TestCountSpans pins the fix: the traces gRPC path must count actual SPANS
// (ResourceSpans → ScopeSpans → Spans), not ResourceSpans containers. The
// otlp_traces_received_total / _processed_total counters and the OTLP
// PartialSuccess.RejectedSpans field are span counts by definition; the path
// previously used len(ResourceSpans), undercounting throughput by the batch's
// spans-per-resource factor.
func TestCountSpans(t *testing.T) {
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{ScopeSpans: []*tracepb.ScopeSpans{
				{Spans: make([]*tracepb.Span, 3)},
				{Spans: make([]*tracepb.Span, 2)},
			}},
			{ScopeSpans: []*tracepb.ScopeSpans{
				{Spans: make([]*tracepb.Span, 100)},
			}},
		},
	}

	got := countSpans(req.ResourceSpans)
	assert.Equal(t, 105, got, "3+2+100 spans across two ResourceSpans")
	assert.NotEqual(t, len(req.ResourceSpans), got,
		"must not collapse to the ResourceSpans-container count (2)")
}

// TestCountSpans_EmptyAndNil confirms the zero cases: nil input, empty
// ResourceSpans, and metadata-only ResourceSpans (no ScopeSpans) all count 0.
func TestCountSpans_EmptyAndNil(t *testing.T) {
	assert.Equal(t, 0, countSpans(nil))
	assert.Equal(t, 0, countSpans([]*tracepb.ResourceSpans{}))
	assert.Equal(t, 0, countSpans([]*tracepb.ResourceSpans{
		{ScopeSpans: nil},
		{ScopeSpans: []*tracepb.ScopeSpans{{Spans: nil}}},
	}))
}
