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

// discovery_serverless_error_rate_test.go — Error rate correlation
// slice 1 chunk 2 (v0.89.128, #768 Stream 166). Pins the §6.1
// response shape + §11 acceptance test 12 + the §12 near-zero
// baseline guard surfacing via the baseline_adjusted field.

// stubErrorRateReader is the in-memory ErrorRateObservationReader
// the handler tests substitute for the production sqlite store.
// Mirrors stubColdStartReader's shape.
type stubErrorRateReader struct {
	rows map[string]map[int]errorRateRowOutcome
}

type errorRateRowOutcome struct {
	row   sqlite.ErrorRateObservationRow
	found bool
	err   error
}

func (s *stubErrorRateReader) LatestErrorRateObservation(_ context.Context, resourceARN string, windowHours int) (sqlite.ErrorRateObservationRow, bool, error) {
	if s.rows == nil {
		return sqlite.ErrorRateObservationRow{}, false, nil
	}
	if perResource, ok := s.rows[resourceARN]; ok {
		if oc, ok := perResource[windowHours]; ok {
			return oc.row, oc.found, oc.err
		}
	}
	return sqlite.ErrorRateObservationRow{}, false, nil
}

func (s *stubErrorRateReader) set(arn string, windowHours int, errorCount, invocationCount int, errorRate float64, observedAt time.Time) {
	if s.rows == nil {
		s.rows = map[string]map[int]errorRateRowOutcome{}
	}
	if s.rows[arn] == nil {
		s.rows[arn] = map[int]errorRateRowOutcome{}
	}
	s.rows[arn][windowHours] = errorRateRowOutcome{
		row: sqlite.ErrorRateObservationRow{
			ResourceARN:     arn,
			WindowHours:     windowHours,
			ErrorCount:      errorCount,
			InvocationCount: invocationCount,
			ErrorRate:       errorRate,
			ObservedAt:      observedAt,
		},
		found: true,
	}
}

func newErrorRateHandler(reader ErrorRateObservationReader) *gin.Engine {
	h := NewDiscoveryServerlessErrorRateHandlers(reader, nil)
	r := gin.New()
	r.GET("/api/v1/discovery/:provider/inventory/serverless/:id/error_rate", h.HandleErrorRate)
	return r
}

// TestErrorRateEndpoint_ShapeMatches_Section6_1 — slice 1 chunk 2
// acceptance test 12. The successful response carries the §6.1
// shape verbatim: resource_arn, current_window {window_hours,
// error_count, invocation_count, error_rate, observed_at},
// baseline_window {…}, rate_ratio, baseline_adjusted,
// exceeds_rate_ratio_floor, exceeds_minimum_invocations,
// exceeds_minimum_errors, would_fire_recommendation.
func TestErrorRateEndpoint_ShapeMatches_Section6_1(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:order-processor"
	observed := time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)
	reader := &stubErrorRateReader{}
	// current 87/3200 = 2.719%, baseline 192/22400 = 0.857%, ratio ~3.17
	reader.set(arn, 24, 87, 3200, 0.02719, observed)
	reader.set(arn, 168, 192, 22400, 0.00857, observed)

	r := newErrorRateHandler(reader)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/error_rate", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Decode into a generic map so we can pin every JSON field name
	// per §6.1. Pinning via a typed struct alone would let an
	// accidental field rename slip past — the map decode catches it.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"resource_arn", "current_window", "baseline_window",
		"rate_ratio", "baseline_adjusted",
		"exceeds_rate_ratio_floor", "exceeds_minimum_invocations",
		"exceeds_minimum_errors", "would_fire_recommendation",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response missing §6.1 field %q; body=%s", k, w.Body.String())
		}
	}
	cw, _ := raw["current_window"].(map[string]any)
	for _, k := range []string{"window_hours", "error_count", "invocation_count", "error_rate", "observed_at"} {
		if _, ok := cw[k]; !ok {
			t.Errorf("current_window missing §6.1 field %q; got %v", k, cw)
		}
	}

	var resp ErrorRateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal typed: %v", err)
	}
	if resp.ResourceARN != arn {
		t.Errorf("ResourceARN = %q, want %q", resp.ResourceARN, arn)
	}
	if resp.CurrentWindow.WindowHours != 24 {
		t.Errorf("CurrentWindow.WindowHours = %d, want 24", resp.CurrentWindow.WindowHours)
	}
	if resp.CurrentWindow.ErrorCount != 87 {
		t.Errorf("CurrentWindow.ErrorCount = %d, want 87", resp.CurrentWindow.ErrorCount)
	}
	if resp.CurrentWindow.InvocationCount != 3200 {
		t.Errorf("CurrentWindow.InvocationCount = %d, want 3200", resp.CurrentWindow.InvocationCount)
	}
	if resp.BaselineWindow.WindowHours != 168 {
		t.Errorf("BaselineWindow.WindowHours = %d, want 168", resp.BaselineWindow.WindowHours)
	}
	if resp.RateRatio < 3.10 || resp.RateRatio > 3.25 {
		t.Errorf("RateRatio = %v, want ~3.17", resp.RateRatio)
	}
	if !resp.ExceedsRateRatioFloor {
		t.Error("ExceedsRateRatioFloor = false at ratio ~3.17, want true")
	}
	if !resp.ExceedsMinimumInvocations {
		t.Error("ExceedsMinimumInvocations = false at 3200 invocations, want true")
	}
	if !resp.ExceedsMinimumErrors {
		t.Error("ExceedsMinimumErrors = false at 87 errors, want true")
	}
	if !resp.WouldFireRecommendation {
		t.Error("WouldFireRecommendation = false, want true")
	}
	if resp.BaselineAdjusted {
		t.Error("BaselineAdjusted = true; baseline 0.86% well above 0.01% floor")
	}
}

// TestErrorRateEndpoint_404WhenNoData — store returns ok=false for
// both windows → 404. Matches the cold-start "no observations
// recorded for resource" posture.
func TestErrorRateEndpoint_404WhenNoData(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:never-seen"
	reader := &stubErrorRateReader{} // empty
	r := newErrorRateHandler(reader)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/error_rate", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestErrorRateEndpoint_404WhenNilStore — handler with nil store
// returns 404 on every call. Matches the cold-start nil-tolerant
// posture for deployments that haven't wired chunk 1.
func TestErrorRateEndpoint_404WhenNilStore(t *testing.T) {
	r := newErrorRateHandler(nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/fn/error_rate", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (nil store)", w.Code)
	}
}

// TestErrorRateEndpoint_StoreErrorReturns500 — non-NotFound store
// error surfaces 500.
func TestErrorRateEndpoint_StoreErrorReturns500(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:err"
	reader := &stubErrorRateReader{
		rows: map[string]map[int]errorRateRowOutcome{
			arn: {
				24: {err: errors.New("simulated sqlite failure")},
			},
		},
	}
	r := newErrorRateHandler(reader)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/error_rate", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestErrorRateEndpoint_WouldFireReflectsAllGates — table-driven
// pin over the three boolean gate combinations to confirm
// would_fire_recommendation is exactly the AND of the three
// Exceeds* booleans.
func TestErrorRateEndpoint_WouldFireReflectsAllGates(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:gate-test"
	now := time.Now().UTC()
	cases := []struct {
		name        string
		errCurr     int
		invCurr     int
		errCurrRate float64
		errBase     int
		invBase     int
		errBaseRate float64
		wantFire    bool
	}{
		// 3x ratio, 3000 inv, 90 err → all three gates pass → fire.
		{"all three gates pass", 90, 3000, 0.030, 60, 6000, 0.010, true},
		// 1.9x ratio, 3000 inv, 57 err → ratio fails → no fire.
		{"ratio gate fails", 57, 3000, 0.019, 60, 6000, 0.010, false},
		// 3x ratio, 500 inv, 15 err → inv gate fails → no fire.
		{"invocation gate fails", 15, 500, 0.030, 30, 3000, 0.010, false},
		// 3x ratio, 3000 inv, 30 err → error gate fails → no fire.
		{"error gate fails", 30, 3000, 0.010, 30, 9000, 0.00333, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubErrorRateReader{}
			reader.set(arn, 24, tc.errCurr, tc.invCurr, tc.errCurrRate, now)
			reader.set(arn, 168, tc.errBase, tc.invBase, tc.errBaseRate, now)
			r := newErrorRateHandler(reader)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/error_rate", nil)
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			var resp ErrorRateResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.WouldFireRecommendation != tc.wantFire {
				t.Errorf("WouldFireRecommendation = %v, want %v (rate=%v inv=%v err=%v)",
					resp.WouldFireRecommendation, tc.wantFire,
					resp.ExceedsRateRatioFloor, resp.ExceedsMinimumInvocations, resp.ExceedsMinimumErrors)
			}
		})
	}
}

// TestErrorRateEndpoint_NearZeroBaseline_SurfacesBaselineAdjusted —
// the §12 near-zero baseline guard. Baseline rate = 0 (no errors in
// the 168h window), current = 3.33%. The response substitutes the
// floor as the comparison denominator and surfaces the substitution
// via baseline_adjusted = true.
func TestErrorRateEndpoint_NearZeroBaseline_SurfacesBaselineAdjusted(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123456789012:function:zero-base"
	now := time.Now().UTC()
	reader := &stubErrorRateReader{}
	reader.set(arn, 24, 100, 3000, 0.0333, now)
	reader.set(arn, 168, 0, 8000, 0.0, now)

	r := newErrorRateHandler(reader)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/discovery/aws/inventory/serverless/"+url.PathEscape(arn)+"/error_rate", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp ErrorRateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.BaselineAdjusted {
		t.Error("BaselineAdjusted = false at zero-baseline, want true")
	}
	// 0.0333 / 0.0001 = 333.
	if resp.RateRatio < 300 || resp.RateRatio > 400 {
		t.Errorf("RateRatio = %v, want ~333 (current / floor)", resp.RateRatio)
	}
	if !resp.WouldFireRecommendation {
		t.Error("WouldFireRecommendation = false, want true with floor-adjusted ratio")
	}
}
