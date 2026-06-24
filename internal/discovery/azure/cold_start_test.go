// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	awsm "github.com/devopsmike2/squadron/internal/discovery/aws"
)

// perWindowMetricsFake serves canned armMetricsResponse values keyed
// by the timespan length (rounded to the nearest hour) so a single
// test can drive the 24h current and 168h baseline windows through
// different responses. Optional perCallOverride flips specific
// calls to return errors (used for fallback path testing).
type perWindowMetricsFake struct {
	mu               sync.Mutex
	calls            int32
	responses        map[time.Duration]armMetricsResponse
	// perCallOverride lets a test return a non-200 status on
	// specific call indices — used by the fallback path test to
	// inject a 400 on call N then a 200 on call N+1.
	perCallOverride  func(callN int, r *http.Request) (status int, body interface{}, handled bool)
	filterSeen       []string
}

func (f *perWindowMetricsFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callN := int(atomic.AddInt32(&f.calls, 1))
		f.mu.Lock()
		f.filterSeen = append(f.filterSeen, r.URL.Query().Get("$filter"))
		override := f.perCallOverride
		f.mu.Unlock()
		if override != nil {
			if status, body, handled := override(callN, r); handled {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(body)
				return
			}
		}
		// Parse the timespan to bucket by window.
		timespan := r.URL.Query().Get("timespan")
		window := windowFromTimespan(timespan)
		f.mu.Lock()
		resp, ok := f.responses[window]
		f.mu.Unlock()
		if !ok {
			// Default: empty timeseries → zero result.
			resp = armMetricsResponse{Value: []armMetricsValue{{
				Unit:       "Milliseconds",
				Timeseries: []armMetricsTimeseries{},
			}}}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// windowFromTimespan parses the start/end timespan and returns the
// duration rounded to the nearest hour. Tolerates the ±1s drift
// between time.Now() at the call site and the timespan parsing
// here.
func windowFromTimespan(timespan string) time.Duration {
	parts := strings.SplitN(timespan, "/", 2)
	if len(parts) != 2 {
		return 0
	}
	startTime, err1 := time.Parse(time.RFC3339, parts[0])
	endTime, err2 := time.Parse(time.RFC3339, parts[1])
	if err1 != nil || err2 != nil {
		return 0
	}
	return endTime.Sub(startTime).Round(time.Hour)
}

// metricsResponseWithBuckets builds a response with N buckets each
// carrying the same per-bucket max value. The substrate counts
// every bucket toward SampleCount, so this is the simplest way to
// drive the BaselineMinimumSamples gate.
func metricsResponseWithBuckets(perBucketValue float64, n int) armMetricsResponse {
	dps := make([]armMetricsDatapoint, 0, n)
	for i := 0; i < n; i++ {
		dps = append(dps, armMetricsDatapoint{
			TimeStamp: fmt.Sprintf("2025-01-01T00:%02d:00Z", i),
			Maximum:   fpPtr(perBucketValue),
		})
	}
	return armMetricsResponse{
		Value: []armMetricsValue{{
			Unit:       "Milliseconds",
			Timeseries: []armMetricsTimeseries{{Data: dps}},
		}},
	}
}

func newColdStartTestScanner(t *testing.T, fake *perWindowMetricsFake) *Scanner {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return &Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("s"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		accessToken:    "fake-token",
		metricsLimiter: rate.NewLimiter(rate.Inf, 1),
	}
}

const coldStartTestARN = "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg/providers/Microsoft.Web/sites/hot-path"

// TestAzureDetectColdStartRegression_ExceedsThreshold — boundary case.
// Current = 750ms (max across buckets), baseline = 500ms (max across
// buckets). Ratio = 1.5x exactly. Current clears 500ms floor.
// Baseline has 60 buckets (above the 50-sample minimum). All three
// predicates flip true; ShouldFireRecommendation returns true.
func TestAzureDetectColdStartRegression_ExceedsThreshold(t *testing.T) {
	fake := &perWindowMetricsFake{
		responses: map[time.Duration]armMetricsResponse{
			24 * time.Hour:  metricsResponseWithBuckets(750.0, 100),
			168 * time.Hour: metricsResponseWithBuckets(500.0, 60),
		},
	}
	s := newColdStartTestScanner(t, fake)
	r, err := s.DetectColdStartRegression(context.Background(), coldStartTestARN)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Ratio != 1.5 {
		t.Errorf("Ratio = %v, want 1.5 (boundary case)", r.Ratio)
	}
	if !r.ExceedsThreshold {
		t.Error("ExceedsThreshold = false, want true at ratio==1.5")
	}
	if !r.ExceedsFloor {
		t.Error("ExceedsFloor = false, want true (current 750ms > 500ms floor)")
	}
	if r.BaselineSampleCount < ColdStartBaselineMinimumSamples {
		t.Errorf("BaselineSampleCount = %d, want >= %d",
			r.BaselineSampleCount, ColdStartBaselineMinimumSamples)
	}
	if !r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = false, want true")
	}
	if r.Surface != "azfunc" {
		t.Errorf("Surface = %q, want azfunc", r.Surface)
	}
	if r.UsedFallback {
		t.Error("UsedFallback = true unexpectedly; runtime supports the dimension here")
	}
}

// TestAzureDetectColdStartRegression_BelowFloor_DoesNotFire — even
// when the ratio exceeds 1.5x, the absolute floor gate suppresses
// the recommendation when the current value sits below 500ms.
// Current = 400ms (below the 500ms floor), baseline = 200ms (ratio
// = 2.0x). Floor gate blocks; ShouldFireRecommendation returns
// false.
func TestAzureDetectColdStartRegression_BelowFloor_DoesNotFire(t *testing.T) {
	fake := &perWindowMetricsFake{
		responses: map[time.Duration]armMetricsResponse{
			24 * time.Hour:  metricsResponseWithBuckets(400.0, 100),
			168 * time.Hour: metricsResponseWithBuckets(200.0, 60),
		},
	}
	s := newColdStartTestScanner(t, fake)
	r, err := s.DetectColdStartRegression(context.Background(), coldStartTestARN)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.ExceedsThreshold {
		t.Errorf("ExceedsThreshold = false, want true at ratio=%v", r.Ratio)
	}
	if r.ExceedsFloor {
		t.Error("ExceedsFloor = true, want false (current 400ms < 500ms floor)")
	}
	if r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (floor gate blocks)")
	}
}

// TestAzureDetectColdStartRegression_FallbackPathRecorded — when the
// IsAfterColdStart dimension isn't available on the runtime, the
// substrate falls back to an unfiltered query and surfaces the
// signal via UsedFallback=true. The current+baseline windows both
// fall back here (same runtime); the OR semantics flip UsedFallback
// true for the whole result.
func TestAzureDetectColdStartRegression_FallbackPathRecorded(t *testing.T) {
	fake := &perWindowMetricsFake{
		responses: map[time.Duration]armMetricsResponse{
			24 * time.Hour:  metricsResponseWithBuckets(900.0, 100),
			168 * time.Hour: metricsResponseWithBuckets(500.0, 60),
		},
		// First call (filtered) for each window returns the 400
		// dimension-not-found. The unfiltered retry succeeds.
		perCallOverride: func(_ int, r *http.Request) (int, interface{}, bool) {
			if r.URL.Query().Get("$filter") != "" {
				// Pretend the runtime doesn't emit IsAfterColdStart.
				return http.StatusBadRequest, armErrorResponse{
					Error: armErrorBody{
						Code: "BadRequest",
						Message: fmt.Sprintf(
							"dimension %s not found on resource",
							AzureFunctionsIsAfterColdStartDimension),
					},
				}, true
			}
			return 0, nil, false
		},
	}
	s := newColdStartTestScanner(t, fake)
	r, err := s.DetectColdStartRegression(context.Background(), coldStartTestARN)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.UsedFallback {
		t.Error("UsedFallback = false, want true (filter rejected → fellBack)")
	}
	// The fallback path STILL fires the recommendation per design
	// doc §3.3 — the regression signal is actionable even without
	// dimension isolation.
	if !r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = false on fallback; recommendation must still fire")
	}
	if r.Surface != "azfunc" {
		t.Errorf("Surface = %q, want azfunc", r.Surface)
	}
}

// TestAzureDetectColdStartRegression_NotEnoughBaselineSamples_DoesNotFire
// — when the baseline window has fewer than 50 buckets, the rule
// skips the recommendation. Mirrors the AWS slice 1 behavior on
// thin-data functions.
func TestAzureDetectColdStartRegression_NotEnoughBaselineSamples_DoesNotFire(t *testing.T) {
	fake := &perWindowMetricsFake{
		responses: map[time.Duration]armMetricsResponse{
			24 * time.Hour:  metricsResponseWithBuckets(900.0, 50),
			168 * time.Hour: metricsResponseWithBuckets(500.0, 10), // below minimum
		},
	}
	s := newColdStartTestScanner(t, fake)
	r, err := s.DetectColdStartRegression(context.Background(), coldStartTestARN)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BaselineSampleCount >= ColdStartBaselineMinimumSamples {
		t.Errorf("BaselineSampleCount = %d, expected to be below %d for this case",
			r.BaselineSampleCount, ColdStartBaselineMinimumSamples)
	}
	if r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (baseline too thin)")
	}
}

// TestAzureColdStartThresholdsMatchAWS — pin to 1.5 + 500.0 + 50.
// The cross-cloud uniformity guarantee from the design doc §1
// requires every cloud's detection thresholds match. A drift here
// breaks acceptance test 11 ("Per-cloud detection thresholds match
// slice 1: pin to identical values across AWS / GCP / Azure / OCI").
//
// Compares directly to the AWS package constants (imported as
// awsm) so the test catches drift on either side — if AWS changes
// the AWS constant first, this test fails until Azure follows; if
// Azure drifts, the test fails until AWS is also bumped (which
// would itself trigger the chunk-5 runbook update).
func TestAzureColdStartThresholdsMatchAWS(t *testing.T) {
	if ColdStartDetectionRatioThreshold != 1.5 {
		t.Errorf("Azure ratio threshold = %v, want 1.5",
			ColdStartDetectionRatioThreshold)
	}
	if ColdStartDetectionFloorMs != 500.0 {
		t.Errorf("Azure floor = %v, want 500.0", ColdStartDetectionFloorMs)
	}
	if ColdStartBaselineMinimumSamples != 50 {
		t.Errorf("Azure baseline min samples = %d, want 50",
			ColdStartBaselineMinimumSamples)
	}
	// Cross-package pin: Azure constants must match AWS.
	if ColdStartDetectionRatioThreshold != awsm.ColdStartDetectionRatioThreshold {
		t.Errorf("Azure ratio (%v) != AWS ratio (%v) — cross-cloud uniformity broken",
			ColdStartDetectionRatioThreshold, awsm.ColdStartDetectionRatioThreshold)
	}
	if ColdStartDetectionFloorMs != awsm.ColdStartDetectionFloorMs {
		t.Errorf("Azure floor (%v) != AWS floor (%v) — cross-cloud uniformity broken",
			ColdStartDetectionFloorMs, awsm.ColdStartDetectionFloorMs)
	}
	if ColdStartBaselineMinimumSamples != awsm.ColdStartBaselineMinimumSamples {
		t.Errorf("Azure baseline min (%d) != AWS baseline min (%d) — cross-cloud uniformity broken",
			ColdStartBaselineMinimumSamples, awsm.ColdStartBaselineMinimumSamples)
	}
	if ColdStartCurrentWindowHours != awsm.ColdStartCurrentWindowHours {
		t.Errorf("Azure current window (%d) != AWS current window (%d)",
			ColdStartCurrentWindowHours, awsm.ColdStartCurrentWindowHours)
	}
	if ColdStartBaselineWindowHours != awsm.ColdStartBaselineWindowHours {
		t.Errorf("Azure baseline window (%d) != AWS baseline window (%d)",
			ColdStartBaselineWindowHours, awsm.ColdStartBaselineWindowHours)
	}
}

// TestAggregateUsedFallback_DetectsSuffix — the helper unpacks the
// fallback signal from the Unit field's " (fallback)" suffix. Pins
// the in-band signalling contract documented in
// queryAzureMetricWithFallback.
func TestAggregateUsedFallback_DetectsSuffix(t *testing.T) {
	if !aggregateUsedFallback("Milliseconds (fallback)") {
		t.Error("aggregateUsedFallback returned false for fallback-marked unit")
	}
	if aggregateUsedFallback("Milliseconds") {
		t.Error("aggregateUsedFallback returned true for plain unit")
	}
	if aggregateUsedFallback("") {
		t.Error("aggregateUsedFallback returned true for empty unit")
	}
}

// TestShouldFireRecommendation_AllPredicatesRequired — directly
// exercise the predicate matrix so a refactor can't silently
// loosen one gate.
func TestShouldFireRecommendation_AllPredicatesRequired(t *testing.T) {
	type tc struct {
		name             string
		exceedsThreshold bool
		exceedsFloor     bool
		baselineSamples  int
		want             bool
	}
	cases := []tc{
		{"all-on", true, true, 100, true},
		{"threshold-off", false, true, 100, false},
		{"floor-off", true, false, 100, false},
		{"baseline-thin", true, true, 10, false},
		{"baseline-exact-minimum", true, true, ColdStartBaselineMinimumSamples, true},
		{"baseline-just-below", true, true, ColdStartBaselineMinimumSamples - 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ColdStartDetectionResult{
				ExceedsThreshold:    c.exceedsThreshold,
				ExceedsFloor:        c.exceedsFloor,
				BaselineSampleCount: c.baselineSamples,
			}
			if got := r.ShouldFireRecommendation(); got != c.want {
				t.Errorf("ShouldFireRecommendation = %v, want %v", got, c.want)
			}
		})
	}
}
