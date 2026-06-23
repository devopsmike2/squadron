// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package receiver

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/worker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// newTestTraceService mirrors the HTTP server's test helper: a real
// TraceService with a queue-only worker pool (no workers consuming),
// optionally wired to a recordingIndex.
func newTestTraceService(t *testing.T, idx TraceObserver) *TraceService {
	t.Helper()
	logger := zap.NewNop()
	pool := worker.NewPool(100, 0, time.Second, nil, nil, nil, logger)
	svc := NewTraceService(nil, pool, logger)
	if idx != nil {
		svc.SetTraceIndex(idx)
	}
	return svc
}

// buildGRPCTraceRequest constructs an in-memory ExportTraceServiceRequest
// matching the shape buildTraceRequest (HTTP test) produces. The gRPC
// path doesn't marshal upfront — Export takes the request struct
// directly — so this helper returns the struct.
func buildGRPCTraceRequest(specs ...rsSpec) *coltracepb.ExportTraceServiceRequest {
	req := &coltracepb.ExportTraceServiceRequest{}
	for _, sp := range specs {
		rs := &tracepb.ResourceSpans{
			Resource: &resourcepb.Resource{Attributes: sp.attrs},
			ScopeSpans: []*tracepb.ScopeSpans{
				{Spans: make([]*tracepb.Span, 0, sp.spans)},
			},
		}
		for i := 0; i < sp.spans; i++ {
			s := &tracepb.Span{}
			if i >= sp.roots {
				s.ParentSpanId = []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
			}
			rs.ScopeSpans[0].Spans = append(rs.ScopeSpans[0].Spans, s)
		}
		req.ResourceSpans = append(req.ResourceSpans, rs)
	}
	return req
}

// TestTraceServiceExport_ObservesResourceSpans is the gRPC mirror of
// the HTTP TestHandleOTLPTraces_ObservesResourceSpans. Same contract:
// every ResourceSpan in the request produces one Observe call with
// the right counts + attribute carry-through.
func TestTraceServiceExport_ObservesResourceSpans(t *testing.T) {
	idx := newRecordingIndex()
	svc := newTestTraceService(t, idx)

	req := buildGRPCTraceRequest(
		rsSpec{
			attrs: []*commonpb.KeyValue{
				kvString("cloud.provider", "gcp"),
				kvString("cloud.account.id", "project-1"),
				kvString("host.id", "gce-12345"),
				kvString("service.name", "checkout"),
			},
			spans: 6, roots: 3,
		},
		rsSpec{
			attrs: []*commonpb.KeyValue{
				kvString("service.name", "billing"),
			},
			spans: 2, roots: 0,
		},
	)

	resp, err := svc.Export(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	// PartialSuccess should be empty (no rejected spans).
	assert.Nil(t, resp.PartialSuccess)

	calls := idx.calls()
	require.Len(t, calls, 2)

	assert.Equal(t, 6, calls[0].SpanCount)
	assert.Equal(t, 3, calls[0].RootSpanCount)
	assert.Equal(t, "gce-12345", calls[0].Attributes["host.id"])
	assert.Equal(t, "project-1", calls[0].Attributes["cloud.account.id"])

	assert.Equal(t, 2, calls[1].SpanCount)
	assert.Equal(t, 0, calls[1].RootSpanCount)
	assert.Equal(t, "billing", calls[1].Attributes["service.name"])
}

// TestTraceServiceExport_NilIndex_StillProcesses asserts the
// disabled-mode escape hatch for the gRPC transport.
func TestTraceServiceExport_NilIndex_StillProcesses(t *testing.T) {
	svc := newTestTraceService(t, nil)
	req := buildGRPCTraceRequest(rsSpec{
		attrs: []*commonpb.KeyValue{kvString("service.name", "checkout")},
		spans: 2, roots: 1,
	})

	require.NotPanics(t, func() {
		resp, err := svc.Export(context.Background(), req)
		require.NoError(t, err)
		require.NotNil(t, resp)
	})
}

// TestTraceServiceExport_SkipsResourceSpansWithZeroSpans mirrors the
// HTTP-side zero-span guard.
func TestTraceServiceExport_SkipsResourceSpansWithZeroSpans(t *testing.T) {
	idx := newRecordingIndex()
	svc := newTestTraceService(t, idx)

	req := buildGRPCTraceRequest(
		rsSpec{attrs: []*commonpb.KeyValue{kvString("service.name", "empty")}, spans: 0, roots: 0},
		rsSpec{attrs: []*commonpb.KeyValue{kvString("service.name", "nonempty")}, spans: 4, roots: 1},
	)
	_, err := svc.Export(context.Background(), req)
	require.NoError(t, err)

	calls := idx.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, 4, calls[0].SpanCount)
}
