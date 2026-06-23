// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package receiver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/devopsmike2/squadron/internal/traceindex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// recordingQualityIndex is a QualityObserver test double mirroring
// recordingIndex. Captures every SpanObservation so the integration
// test can assert per-span shape after the receiver's hex encoding +
// resource-key derivation pass.
type recordingQualityIndex struct {
	mu       sync.Mutex
	observed []traceindex.SpanObservation
}

func (r *recordingQualityIndex) Observe(obs traceindex.SpanObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, obs)
}

func (r *recordingQualityIndex) calls() []traceindex.SpanObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]traceindex.SpanObservation, len(r.observed))
	copy(out, r.observed)
	return out
}

// TestHandleOTLPTraces_ObservesQuality pins the span-quality slice-1
// chunk-1 hot-path contract: every Span in the batch produces one
// Quality.Observe call, with the resource attrs propagated via
// ComputeResourceKey + the tier inferred from k8s/db attrs.
func TestHandleOTLPTraces_ObservesQuality(t *testing.T) {
	qual := &recordingQualityIndex{}
	s, router := newTestHTTPServer(t, nil)
	s.qualityIndex = qual

	body := buildTraceRequest(t,
		// Compute resource: 3 spans, default tier.
		rsSpec{
			attrs: []*commonpb.KeyValue{
				kvString("cloud.provider", "aws"),
				kvString("cloud.account.id", "12345"),
				kvString("cloud.resource_id", "arn:aws:ec2:us-east-1:12345:instance/i-quality"),
				kvString("service.name", "checkout"),
			},
			spans: 3, roots: 1,
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusAccepted, w.Code)

	calls := qual.calls()
	require.Len(t, calls, 3, "one quality.Observe call per span")
	for _, c := range calls {
		assert.Equal(t, "arn:aws:ec2:us-east-1:12345:instance/i-quality", c.Key)
		assert.Equal(t, "compute", c.Tier)
		assert.Equal(t, "aws", c.Attrs["cloud.provider"])
	}
}

// TestHandleOTLPTraces_NilQualityIndex_StillProcesses verifies the
// disabled-mode escape hatch for span-quality: qualityIndex=nil makes
// the handler skip the quality pass without affecting the traceindex
// path or the worker-pool dispatch.
func TestHandleOTLPTraces_NilQualityIndex_StillProcesses(t *testing.T) {
	idx := newRecordingIndex()
	s, router := newTestHTTPServer(t, idx)
	s.qualityIndex = nil // explicit for documentation

	body := buildTraceRequest(t, rsSpec{
		attrs: []*commonpb.KeyValue{kvString("service.name", "checkout")},
		spans: 2, roots: 1,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	w := httptest.NewRecorder()

	assert.NotPanics(t, func() { router.ServeHTTP(w, req) })
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Len(t, idx.calls(), 1, "traceindex still observes despite nil qualityIndex")
}

// TestTierFromAttrs covers the §3.2 tier label precedence — k8s wins
// over db wins over compute. Keeping the test next to the integration
// (rather than in a unit-only file) makes the precedence visible
// alongside the receiver's per-span fan-out.
func TestTierFromAttrs(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]string
		want  string
	}{
		{"empty", map[string]string{}, "compute"},
		{"compute", map[string]string{"service.name": "x"}, "compute"},
		{"db", map[string]string{"db.system": "postgresql"}, "db"},
		{"k8s", map[string]string{"k8s.cluster.name": "c"}, "k8s"},
		{"k8s_over_db", map[string]string{"db.system": "x", "k8s.cluster.name": "c"}, "k8s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tierFromAttrs(tc.attrs))
		})
	}
}
