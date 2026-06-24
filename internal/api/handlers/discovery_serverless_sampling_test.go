// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/proposer"
)

// discovery_serverless_sampling_test.go — Sampling rate analysis
// slice 1 chunk 2 (v0.89.123, #763 Stream 161). Pins §6.1 wire shape
// + the would_fire_recommendation invariant (acceptance test 12 +
// the chunk 2 report-back question).

type stubSamplingLookup struct {
	entries map[string]struct{ surface, key string }
}

func (s *stubSamplingLookup) LookupSamplingResource(provider, resourceID string) (string, string, bool) {
	e, ok := s.entries[provider+":"+resourceID]
	if !ok {
		return "", "", false
	}
	return e.surface, e.key, true
}

func (s *stubSamplingLookup) set(provider, id, surface, key string) {
	if s.entries == nil {
		s.entries = map[string]struct{ surface, key string }{}
	}
	s.entries[provider+":"+id] = struct{ surface, key string }{surface, key}
}

type stubSamplingDetector struct {
	result proposer.SamplingRateDetectionResult
	err    error
}

func (s *stubSamplingDetector) DetectSampling(
	_ context.Context, resourceARN, surface, _ string,
) (proposer.SamplingRateDetectionResult, error) {
	if s.err != nil {
		return proposer.SamplingRateDetectionResult{}, s.err
	}
	r := s.result
	if r.ResourceARN == "" {
		r.ResourceARN = resourceARN
	}
	if r.Surface == "" {
		r.Surface = surface
	}
	return r, nil
}

func newSamplingHandler(lookup SamplingResourceLookup, det SamplingDetector) *gin.Engine {
	h := NewDiscoveryServerlessSamplingHandlers(lookup, det, nil)
	r := gin.New()
	r.GET("/api/v1/discovery/:provider/inventory/serverless/:id/sampling", h.HandleSampling)
	return r
}

// Test 12 — §6.1 wire shape.
func TestSamplingEndpoint_ShapeMatches_Section6_1(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123:function:order-processor"
	observed := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)
	lookup := &stubSamplingLookup{}
	lookup.set("aws", arn, "lambda", "aws/lambda/order-processor")
	det := &stubSamplingDetector{
		result: proposer.SamplingRateDetectionResult{
			ResourceARN: arn, Surface: "lambda",
			ObservedSpanCount: 142, ExpectedInvocationCount: 3500, Ratio: 0.0406,
			ExceedsFloor: true, ExceedsMinimumInvocations: true, ObservedAt: observed,
		},
	}
	r := newSamplingHandler(lookup, det)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/sampling", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp SamplingResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ResourceARN != arn || resp.WindowHours != 24 ||
		resp.ObservedSpanCount != 142 || resp.ExpectedInvocationCount != 3500 {
		t.Errorf("response fields wrong: %+v", resp)
	}
	if resp.SamplingRatio < 0.04 || resp.SamplingRatio > 0.05 {
		t.Errorf("SamplingRatio = %v, want ~0.0406", resp.SamplingRatio)
	}
	if !resp.ExceedsFloor || !resp.ExceedsMinimumInvocations || !resp.WouldFireRecommendation {
		t.Error("predicate flags wrong")
	}
	// Pin every §6.1 JSON field name.
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	for _, key := range []string{
		"resource_arn", "window_hours", "observed_span_count",
		"expected_invocation_count", "sampling_ratio", "exceeds_floor",
		"exceeds_minimum_invocations", "would_fire_recommendation", "observed_at",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing field %q (§6.1)", key)
		}
	}
}

func TestSamplingEndpoint_404WhenNoData(t *testing.T) {
	r := newSamplingHandler(&stubSamplingLookup{}, &stubSamplingDetector{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/never-seen/sampling", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestSamplingEndpoint_NilLookup_Returns404(t *testing.T) {
	r := newSamplingHandler(nil, &stubSamplingDetector{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/foo/sampling", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (nil lookup)", w.Code)
	}
}

func TestSamplingEndpoint_NilDetector_Returns404(t *testing.T) {
	r := newSamplingHandler(&stubSamplingLookup{}, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/foo/sampling", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (nil detector)", w.Code)
	}
}

func TestSamplingEndpoint_DetectorErrorReturns500(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123:function:err"
	lookup := &stubSamplingLookup{}
	lookup.set("aws", arn, "lambda", "k")
	det := &stubSamplingDetector{err: errors.New("simulated CloudWatch failure")}
	r := newSamplingHandler(lookup, det)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/sampling", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestSamplingEndpoint_WouldFireReflectsDetectionLogic — pins the
// chunk 2 report-back invariant: would_fire_recommendation MUST
// cleanly distinguish (below floor, above minimum) → fires from
// (below floor, below minimum) → does NOT fire (noise filter). Also
// pins the at-floor and above-floor non-firing cases.
func TestSamplingEndpoint_WouldFireReflectsDetectionLogic(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:1:function:x"
	cases := []struct {
		name           string
		exceedsFloor   bool
		exceedsMinimum bool
		wantWouldFire  bool
	}{
		{"below floor AND above minimum → fires", true, true, true},
		{"below floor BUT below minimum → does NOT fire (noise filter)", true, false, false},
		{"at floor exactly → does NOT fire (strict-less-than)", false, true, false},
		{"above floor → does NOT fire (acceptable rate)", false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lookup := &stubSamplingLookup{}
			lookup.set("aws", arn, "lambda", "k")
			det := &stubSamplingDetector{result: proposer.SamplingRateDetectionResult{
				ResourceARN: arn, Surface: "lambda",
				ExceedsFloor: c.exceedsFloor, ExceedsMinimumInvocations: c.exceedsMinimum,
			}}
			r := newSamplingHandler(lookup, det)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
				"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/sampling", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var resp SamplingResponse
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp.WouldFireRecommendation != c.wantWouldFire {
				t.Errorf("WouldFireRecommendation = %v, want %v", resp.WouldFireRecommendation, c.wantWouldFire)
			}
			if resp.ExceedsFloor != c.exceedsFloor || resp.ExceedsMinimumInvocations != c.exceedsMinimum {
				t.Errorf("predicate booleans don't echo back: got (floor=%v min=%v) want (%v %v)",
					resp.ExceedsFloor, resp.ExceedsMinimumInvocations, c.exceedsFloor, c.exceedsMinimum)
			}
		})
	}
}
