// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package receiver

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/traceindex"
	"github.com/devopsmike2/squadron/internal/worker"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// recordingIndex is a TraceObserver test double that records each
// Observe call so the receiver tests can assert per-batch shape
// without standing up the full Index + Store path. Thread-safe so
// the gRPC test (which currently runs sequentially but might
// parallelize in the future) doesn't race.
type recordingIndex struct {
	mu        sync.Mutex
	observed  []traceindex.ResourceObservation
	gotCtxNil bool
}

func newRecordingIndex() *recordingIndex {
	return &recordingIndex{observed: nil}
}

func (r *recordingIndex) Observe(ctx context.Context, obs traceindex.ResourceObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx == nil {
		r.gotCtxNil = true
	}
	r.observed = append(r.observed, obs)
}

func (r *recordingIndex) calls() []traceindex.ResourceObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]traceindex.ResourceObservation, len(r.observed))
	copy(out, r.observed)
	return out
}

// newTestHTTPServer builds a HTTPServer wired to a real (but
// minimally configured) worker pool. The pool runs no workers, so
// Submit just buffers into the queue channel — fine for unit tests
// asserting on the receiver-side state change. queueSize=100 keeps
// the queue from filling for normal request shapes.
func newTestHTTPServer(t *testing.T, idx TraceObserver) (*HTTPServer, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	logger := zap.NewNop()
	pool := worker.NewPool(100, 0, time.Second, nil, nil, nil, logger)
	s := &HTTPServer{
		logger:     logger,
		metrics:    nil,
		port:       0,
		workerPool: pool,
		traceIndex: idx,
	}
	r := gin.New()
	r.POST("/v1/traces", s.handleOTLPTraces)
	return s, r
}

// buildTraceRequest constructs a serialized ExportTraceServiceRequest
// with the supplied ResourceSpans. Each ResourceSpan in the input
// list carries pre-built resource attrs + a configurable span/root
// span count.
type rsSpec struct {
	attrs []*commonpb.KeyValue
	// total spans + spans that have NO parent (root spans)
	spans int
	roots int
}

func buildTraceRequest(t *testing.T, specs ...rsSpec) []byte {
	t.Helper()
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
	body, err := proto.Marshal(req)
	require.NoError(t, err)
	return body
}

func kvString(key, val string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}}}
}

// TestHandleOTLPTraces_ObservesResourceSpans pins the slice-1
// chunk-2 contract: every ResourceSpan in the incoming batch
// produces one Index.Observe call, with the SpanCount + Attributes
// faithfully carried through.
func TestHandleOTLPTraces_ObservesResourceSpans(t *testing.T) {
	idx := newRecordingIndex()
	_, router := newTestHTTPServer(t, idx)

	body := buildTraceRequest(t,
		// Strong-key ResourceSpan: cloud.resource_id present → tier 1.
		rsSpec{
			attrs: []*commonpb.KeyValue{
				kvString("cloud.provider", "aws"),
				kvString("cloud.account.id", "12345"),
				kvString("cloud.resource_id", "arn:aws:ec2:us-east-1:12345:instance/i-strong"),
				kvString("service.name", "checkout"),
			},
			spans: 5, roots: 2,
		},
		// Weak-key ResourceSpan: only host.name → tier 5.
		rsSpec{
			attrs: []*commonpb.KeyValue{
				kvString("host.name", "db-prod-7"),
				kvString("service.name", "billing"),
			},
			spans: 3, roots: 1,
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	calls := idx.calls()
	require.Len(t, calls, 2)

	// Strong-key call.
	assert.Equal(t, 5, calls[0].SpanCount)
	assert.Equal(t, 2, calls[0].RootSpanCount)
	assert.Equal(t, "arn:aws:ec2:us-east-1:12345:instance/i-strong", calls[0].Attributes["cloud.resource_id"])
	assert.Equal(t, "checkout", calls[0].Attributes["service.name"])
	assert.Equal(t, "aws", calls[0].Attributes["cloud.provider"])
	assert.False(t, calls[0].Timestamp.IsZero(), "timestamp populated from receiver clock")

	// Weak-key call.
	assert.Equal(t, 3, calls[1].SpanCount)
	assert.Equal(t, 1, calls[1].RootSpanCount)
	assert.Equal(t, "db-prod-7", calls[1].Attributes["host.name"])
}

// TestHandleOTLPTraces_NilIndex_StillProcesses verifies the disabled-
// mode escape hatch: traceIndex=nil makes the handler skip the
// observation dispatch entirely while keeping the worker-pool path
// (the existing storage-side flow) intact.
func TestHandleOTLPTraces_NilIndex_StillProcesses(t *testing.T) {
	_, router := newTestHTTPServer(t, nil)

	body := buildTraceRequest(t, rsSpec{
		attrs: []*commonpb.KeyValue{kvString("service.name", "checkout")},
		spans: 2, roots: 1,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	w := httptest.NewRecorder()

	// The handler must not panic with a nil traceIndex.
	assert.NotPanics(t, func() { router.ServeHTTP(w, req) })
	assert.Equal(t, http.StatusAccepted, w.Code)
}

// TestHandleOTLPTraces_SkipsResourceSpansWithZeroSpans verifies the
// helper's zero-span guard. A metadata-only ResourceSpan produces no
// Observe call — the dashboard would render such a row as "seen but
// not emitting" which is the wrong shape.
func TestHandleOTLPTraces_SkipsResourceSpansWithZeroSpans(t *testing.T) {
	idx := newRecordingIndex()
	_, router := newTestHTTPServer(t, idx)

	body := buildTraceRequest(t,
		rsSpec{attrs: []*commonpb.KeyValue{kvString("service.name", "empty")}, spans: 0, roots: 0},
		rsSpec{attrs: []*commonpb.KeyValue{kvString("service.name", "nonempty")}, spans: 4, roots: 1},
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)
	calls := idx.calls()
	require.Len(t, calls, 1, "zero-span ResourceSpan dropped, only non-empty one Observed")
	assert.Equal(t, 4, calls[0].SpanCount)
}

// TestExtractResourceAttributes_CoercesAnyValueVariants pins the
// per-AnyValue-variant coercion contract. The four scalar variants
// (String, Int, Double, Bool) round-trip to a stable string form;
// the helper docs make the contract explicit.
func TestExtractResourceAttributes_CoercesAnyValueVariants(t *testing.T) {
	tests := []struct {
		name string
		kv   *commonpb.KeyValue
		want string
	}{
		{
			name: "string",
			kv:   &commonpb.KeyValue{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "hello"}}},
			want: "hello",
		},
		{
			name: "int",
			kv:   &commonpb.KeyValue{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 12345}}},
			want: "12345",
		},
		{
			name: "double",
			kv:   &commonpb.KeyValue{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 3.5}}},
			want: "3.5",
		},
		{
			name: "bool_true",
			kv:   &commonpb.KeyValue{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}},
			want: "true",
		},
		{
			name: "bool_false",
			kv:   &commonpb.KeyValue{Key: "k", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
			want: "false",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractResourceAttributes([]*commonpb.KeyValue{tc.kv})
			assert.Equal(t, tc.want, got["k"])
		})
	}
}

// TestExtractResourceAttributes_SkipsArrayAndKvList verifies that
// the non-scalar AnyValue variants are silently dropped per the
// slice-1 contract.
func TestExtractResourceAttributes_SkipsArrayAndKvList(t *testing.T) {
	arr := &commonpb.KeyValue{Key: "arr", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{
		ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_StringValue{StringValue: "a"}}}},
	}}}
	kvl := &commonpb.KeyValue{Key: "kvl", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{
		KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{kvString("inner", "v")}},
	}}}
	scalar := kvString("svc", "shop")

	got := extractResourceAttributes([]*commonpb.KeyValue{arr, kvl, scalar})

	_, hasArr := got["arr"]
	_, hasKvl := got["kvl"]
	assert.False(t, hasArr, "ArrayValue silently dropped")
	assert.False(t, hasKvl, "KvListValue silently dropped")
	assert.Equal(t, "shop", got["svc"], "scalar StringValue still landed")
}

// TestExtractResourceAttributes_SkipsNilAndEmptyKeys guards the
// defensive checks in the helper. Nil KeyValue / nil AnyValue /
// empty key all fall through without crashing.
func TestExtractResourceAttributes_SkipsNilAndEmptyKeys(t *testing.T) {
	got := extractResourceAttributes([]*commonpb.KeyValue{
		nil,
		{Key: "", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "ignored"}}},
		{Key: "ok", Value: nil},
		kvString("svc", "shop"),
	})
	assert.Equal(t, "shop", got["svc"])
	assert.Len(t, got, 1)
}
