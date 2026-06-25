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

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

func init() { gin.SetMode(gin.TestMode) }

// stubColdStartReader is a programmable in-memory implementation of
// ColdStartObservationReader for unit testing the per-resource
// endpoint. Each (resourceARN, windowHours) key carries a canned row
// + bool + error so each test can pin a specific edge case.
type stubColdStartReader struct {
	rows map[string]map[int]coldStartRowOutcome
}

type coldStartRowOutcome struct {
	row   sqlite.ColdStartObservationRow
	found bool
	err   error
}

func (s *stubColdStartReader) LatestColdStartObservation(_ context.Context, resourceARN string, windowHours int) (sqlite.ColdStartObservationRow, bool, error) {
	if s.rows == nil {
		return sqlite.ColdStartObservationRow{}, false, nil
	}
	if perResource, ok := s.rows[resourceARN]; ok {
		if oc, ok := perResource[windowHours]; ok {
			return oc.row, oc.found, oc.err
		}
	}
	return sqlite.ColdStartObservationRow{}, false, nil
}

func (s *stubColdStartReader) set(arn string, windowHours int, p95Ms float64, samples int, observedAt time.Time) {
	if s.rows == nil {
		s.rows = map[string]map[int]coldStartRowOutcome{}
	}
	if s.rows[arn] == nil {
		s.rows[arn] = map[int]coldStartRowOutcome{}
	}
	s.rows[arn][windowHours] = coldStartRowOutcome{
		row: sqlite.ColdStartObservationRow{
			ResourceARN: arn,
			WindowHours: windowHours,
			P95Ms:       p95Ms,
			SampleCount: samples,
			ObservedAt:  observedAt,
		},
		found: true,
	}
}

func newColdStartHandler(reader ColdStartObservationReader) *gin.Engine {
	h := NewDiscoveryServerlessColdStartHandlers(
		reader,
		NewStaticColdStartDetectionConstants(24, 168, 1.5, 500.0),
		nil,
	)
	r := gin.New()
	r.GET("/api/v1/discovery/:provider/inventory/serverless/:id/cold_start", h.HandleColdStart)
	return r
}

// TestColdStartEndpoint_ShapeMatches§6_1 — slice 1 chunk 2 acceptance
// test 11. The successful response carries the §6.1 shape verbatim:
// resource_arn, current_window {window_hours, p95_ms, sample_count,
// observed_at}, baseline_window {…}, ratio, exceeds_threshold,
// exceeds_floor_ms.
func TestColdStartEndpoint_ShapeMatches_Section6_1(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:order-processor"
	observed := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)
	reader := &stubColdStartReader{}
	reader.set(arn, 24, 4230.0, 142, observed)
	reader.set(arn, 168, 2820.0, 1086, observed)

	r := newColdStartHandler(reader)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/cold_start", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp ColdStartResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.ResourceARN != arn {
		t.Errorf("ResourceARN = %q, want %q", resp.ResourceARN, arn)
	}
	if resp.CurrentWindow.WindowHours != 24 {
		t.Errorf("CurrentWindow.WindowHours = %d, want 24", resp.CurrentWindow.WindowHours)
	}
	if resp.CurrentWindow.P95Ms != 4230.0 {
		t.Errorf("CurrentWindow.P95Ms = %v, want 4230", resp.CurrentWindow.P95Ms)
	}
	if resp.CurrentWindow.SampleCount != 142 {
		t.Errorf("CurrentWindow.SampleCount = %d, want 142", resp.CurrentWindow.SampleCount)
	}
	if resp.BaselineWindow.WindowHours != 168 {
		t.Errorf("BaselineWindow.WindowHours = %d, want 168", resp.BaselineWindow.WindowHours)
	}
	if resp.BaselineWindow.P95Ms != 2820.0 {
		t.Errorf("BaselineWindow.P95Ms = %v, want 2820", resp.BaselineWindow.P95Ms)
	}
	// 4230 / 2820 = ~1.5
	if resp.Ratio < 1.49 || resp.Ratio > 1.51 {
		t.Errorf("Ratio = %v, want ~1.5", resp.Ratio)
	}
	if !resp.ExceedsThreshold {
		t.Error("ExceedsThreshold = false, want true at ratio ~1.5")
	}
	if !resp.ExceedsFloorMs {
		t.Error("ExceedsFloorMs = false, want true (4230 > 500)")
	}
}

// TestColdStartEndpoint_404WhenNoObservations — when the store
// returns ok=false for either window, the endpoint surfaces 404.
// Matches the per-resource span-quality endpoint's cold-start
// posture so the UI's two drill-down endpoints share one
// "no data yet" rendering pathway.
func TestColdStartEndpoint_404WhenNoObservations(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:never-seen"
	reader := &stubColdStartReader{} // empty
	r := newColdStartHandler(reader)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/cold_start", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestColdStartEndpoint_404WhenOnlyCurrentWindow — half-observed
// state: current window has a row, baseline does not. Surfaces 404
// because the ratio comparison would be meaningless.
func TestColdStartEndpoint_404WhenOnlyCurrentWindow(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:partial"
	reader := &stubColdStartReader{}
	reader.set(arn, 24, 800, 50, time.Now().UTC())
	// baseline NOT set
	r := newColdStartHandler(reader)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/cold_start", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when baseline absent", w.Code)
	}
}

// TestColdStartEndpoint_NilStore_Returns404 — a handler built with
// a nil store (deployment hasn't wired chunk 1 yet) surfaces 404 on
// every call. Matches the trace-coverage handler's nil-tolerant
// posture.
func TestColdStartEndpoint_NilStore_Returns404(t *testing.T) {
	r := newColdStartHandler(nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/fn/cold_start", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (nil store)", w.Code)
	}
}

// TestColdStartEndpoint_StoreErrorReturns500 — a store error that
// isn't NotFound surfaces 500. Matches the discovery-summary
// handler's posture for storage-layer failures.
func TestColdStartEndpoint_StoreErrorReturns500(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:err"
	reader := &stubColdStartReader{
		rows: map[string]map[int]coldStartRowOutcome{
			arn: {
				24: {err: errors.New("simulated sqlite failure")},
			},
		},
	}
	r := newColdStartHandler(reader)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/cold_start", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestColdStartEndpoint_RatioCalculatedFromLatestObservations —
// verifies the ratio math: current/baseline rounded as the JSON
// response carries it. Distinct from the boundary-case tests on
// the detection branch — this one pins the handler's computation
// independent of the scanner-side detection logic.
func TestColdStartEndpoint_RatioCalculatedFromLatestObservations(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:ratio-test"
	now := time.Now().UTC()
	cases := []struct {
		name      string
		curr      float64
		baseline  float64
		wantRatio float64
		wantExc   bool
	}{
		{"2x ratio fires", 1000.0, 500.0, 2.0, true},
		{"1.49x ratio does not fire", 745.0, 500.0, 1.49, false},
		{"3x ratio fires", 1500.0, 500.0, 3.0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubColdStartReader{}
			reader.set(arn, 24, tc.curr, 200, now)
			reader.set(arn, 168, tc.baseline, 1400, now)
			r := newColdStartHandler(reader)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/cold_start", nil)
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var resp ColdStartResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Ratio < tc.wantRatio-0.01 || resp.Ratio > tc.wantRatio+0.01 {
				t.Errorf("Ratio = %v, want %v", resp.Ratio, tc.wantRatio)
			}
			if resp.ExceedsThreshold != tc.wantExc {
				t.Errorf("ExceedsThreshold = %v, want %v", resp.ExceedsThreshold, tc.wantExc)
			}
		})
	}
}

// TestColdStartEndpoint_ExceedsThresholdReflectsDetectionLogic —
// pins the two-predicate logic the handler applies: ratio >= 1.5 AND
// the current value gate. The ExceedsFloorMs field is independent
// from ExceedsThreshold; their combination is what the chunk-3 UI
// renders amber. The detection branch's ShouldFireRecommendation
// adds the BaselineSampleCount gate, but THIS endpoint surfaces
// the two raw predicates so the UI can render the partial state
// (e.g. "ratio fires but absolute value is fine — no amber yet").
func TestColdStartEndpoint_ExceedsThresholdReflectsDetectionLogic(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:gate-test"
	now := time.Now().UTC()
	cases := []struct {
		name             string
		curr             float64
		baseline         float64
		wantExceedsRatio bool
		wantExceedsFloor bool
	}{
		{"ratio yes / floor yes", 750, 500, true, true},
		{"ratio yes / floor no (well-tuned)", 499, 100, true, false},
		{"ratio no / floor yes", 700, 500, false, true}, // ratio 1.4
		{"ratio no / floor no", 300, 250, false, false}, // ratio 1.2
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubColdStartReader{}
			reader.set(arn, 24, tc.curr, 200, now)
			reader.set(arn, 168, tc.baseline, 1400, now)
			r := newColdStartHandler(reader)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/cold_start", nil)
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var resp ColdStartResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.ExceedsThreshold != tc.wantExceedsRatio {
				t.Errorf("ExceedsThreshold = %v, want %v", resp.ExceedsThreshold, tc.wantExceedsRatio)
			}
			if resp.ExceedsFloorMs != tc.wantExceedsFloor {
				t.Errorf("ExceedsFloorMs = %v, want %v", resp.ExceedsFloorMs, tc.wantExceedsFloor)
			}
		})
	}
}

// TestColdStartEndpoint_EmptyIDReturns400 — gin's path matching
// already requires :id be present, so a missing-id call lands on a
// different route. But a request that URL-encodes whitespace as id
// (i.e. operator-supplied empty string) lands here; the handler
// surfaces 400. Documents the path validation that lives inside the
// handler rather than relying on the router.
func TestColdStartEndpoint_EmptyIDReturns400(t *testing.T) {
	r := newColdStartHandler(&stubColdStartReader{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/%20/cold_start", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for whitespace id", w.Code)
	}
}

// TestColdStartEndpoint_DefaultConstantsApplied — when the
// constructor is passed nil constants, the endpoint defaults to
// 24h / 168h / 1.5x / 500ms (the slice 1 substrate values). Pins
// the chunk-2 nil-tolerance contract.
func TestColdStartEndpoint_DefaultConstantsApplied(t *testing.T) {
	h := NewDiscoveryServerlessColdStartHandlers(&stubColdStartReader{}, nil, nil)
	if h.constants == nil {
		t.Fatal("constants should default to a non-nil value")
	}
	if h.constants.CurrentWindowHours() != 24 {
		t.Errorf("CurrentWindowHours = %d, want 24", h.constants.CurrentWindowHours())
	}
	if h.constants.BaselineWindowHours() != 168 {
		t.Errorf("BaselineWindowHours = %d, want 168", h.constants.BaselineWindowHours())
	}
	if h.constants.RatioThreshold() != 1.5 {
		t.Errorf("RatioThreshold = %v, want 1.5", h.constants.RatioThreshold())
	}
	if h.constants.FloorMs() != 500.0 {
		t.Errorf("FloorMs = %v, want 500", h.constants.FloorMs())
	}
}
