// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// -- v0.89.118 slice 2 chunk 3 OCI cold-start detection tests --------

// detectionMockMonitoring is a programmable MonitoringClient that
// returns canned datapoints keyed on the MQL query string. Lets each
// test deterministically drive the (cold_start_count, current
// duration, baseline duration) trio that
// DetectColdStartRegression sequences.
type detectionMockMonitoring struct {
	// byMetric maps the metric name embedded in the query
	// (function_duration / cold_start_count) → a per-window
	// response. The cold_start_count entry has a single window key
	// "current". The function_duration entries are keyed by
	// window-hours-suffix ("24h" / "168h") so the test can
	// distinguish current from baseline.
	byMetric map[string]map[string][]ociMetricDataPoint

	// errOn is a metric-name → error injector. When set, the
	// monitoring client returns the error for queries embedding the
	// named metric.
	errOn map[string]error
}

func newDetectionMockMonitoring() *detectionMockMonitoring {
	return &detectionMockMonitoring{
		byMetric: map[string]map[string][]ociMetricDataPoint{},
		errOn:    map[string]error{},
	}
}

func (m *detectionMockMonitoring) setDuration(window string, p95Ms float64, sampleCount int) {
	if _, ok := m.byMetric[OCIFunctionsFunctionDurationMetric]; !ok {
		m.byMetric[OCIFunctionsFunctionDurationMetric] = map[string][]ociMetricDataPoint{}
	}
	if p95Ms == 0 && sampleCount == 0 {
		m.byMetric[OCIFunctionsFunctionDurationMetric][window] = []ociMetricDataPoint{}
		return
	}
	pts := make([]ociMetricDataPoint, 0, sampleCount)
	per := 1
	if sampleCount > 0 {
		per = 1
	}
	for i := 0; i < sampleCount; i++ {
		pts = append(pts, ociMetricDataPoint{
			Timestamp:   time.Now().Add(-time.Duration(i) * time.Minute),
			Value:       p95Ms,
			SampleCount: per,
		})
	}
	m.byMetric[OCIFunctionsFunctionDurationMetric][window] = pts
}

func (m *detectionMockMonitoring) SummarizeMetricsData(
	ctx context.Context,
	compartmentID, namespace, query string,
	startTime, endTime time.Time,
) ([]ociMetricDataPoint, error) {
	// Determine which metric the query is asking about.
	var metricName string
	if containsSubstring(query, OCIFunctionsFunctionDurationMetric) {
		metricName = OCIFunctionsFunctionDurationMetric
	}
	if err, ok := m.errOn[metricName]; ok {
		return nil, err
	}
	// Determine the window from the query suffix.
	var window string
	switch {
	case containsSubstring(query, "[24h]"):
		window = "24h"
	case containsSubstring(query, "[168h]"):
		window = "168h"
	default:
		window = "current"
	}
	if perMetric, ok := m.byMetric[metricName]; ok {
		if pts, ok2 := perMetric[window]; ok2 {
			return pts, nil
		}
	}
	return nil, nil
}

func containsSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	// strings.Contains import would be cleaner but to keep imports
	// minimal, use a basic loop.
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// newColdStartTestScanner wires a Scanner with a mock monitoring
// client and an infinite-rate limiter for fast tests.
func newColdStartTestScanner(t *testing.T, mc MonitoringClient) *Scanner {
	t.Helper()
	s := &Scanner{
		TenancyOCID: "ocid1.tenancy.oc1..aaa",
		UserOCID:    "ocid1.user.oc1..bbb",
		Fingerprint: "aa:bb:cc:dd",
		Region:      "us-phoenix-1",
	}
	s.WithMonitoringClient(mc)
	s.WithMonitoringRateLimiter(rate.NewLimiter(rate.Inf, 1))
	return s
}

// TestOCIColdStartThresholdsMatchAWS — slice 2 chunk 3 acceptance
// test 11. Pin the per-cloud detection constants to the AWS slice 1
// canonical values (1.5x ratio + 500ms floor + 50 baseline samples +
// 24h current + 168h baseline windows). Slice 2 §3.4 explicitly
// requires uniform thresholds across all 4 clouds.
func TestOCIColdStartThresholdsMatchAWS(t *testing.T) {
	if ColdStartDetectionRatioThreshold != 1.5 {
		t.Errorf("ColdStartDetectionRatioThreshold = %v, want 1.5",
			ColdStartDetectionRatioThreshold)
	}
	if ColdStartDetectionFloorMs != 500.0 {
		t.Errorf("ColdStartDetectionFloorMs = %v, want 500.0",
			ColdStartDetectionFloorMs)
	}
	if ColdStartBaselineMinimumSamples != 50 {
		t.Errorf("ColdStartBaselineMinimumSamples = %d, want 50",
			ColdStartBaselineMinimumSamples)
	}
	if ColdStartCurrentWindowHours != 24 {
		t.Errorf("ColdStartCurrentWindowHours = %d, want 24",
			ColdStartCurrentWindowHours)
	}
	if ColdStartBaselineWindowHours != 168 {
		t.Errorf("ColdStartBaselineWindowHours = %d, want 168",
			ColdStartBaselineWindowHours)
	}
}

// TestOCIDetectColdStartRegression_ExceedsThreshold — a function
// with non-zero cold starts whose current P95 is well above the
// 1.5x baseline ratio and above the 500ms floor + has enough
// baseline samples should fire.
func TestOCIDetectColdStartRegression_ExceedsThreshold(t *testing.T) {
	mock := newDetectionMockMonitoring()
	mock.setDuration("24h", 2400.0, 60)  // current p95 high
	mock.setDuration("168h", 800.0, 100) // baseline p95 lower
	s := newColdStartTestScanner(t, mock)

	res, err := s.DetectColdStartRegression(context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.CurrentP95Ms != 2400.0 {
		t.Errorf("CurrentP95Ms = %v, want 2400.0", res.CurrentP95Ms)
	}
	if res.BaselineP95Ms != 800.0 {
		t.Errorf("BaselineP95Ms = %v, want 800.0", res.BaselineP95Ms)
	}
	if res.Ratio != 3.0 {
		t.Errorf("Ratio = %v, want 3.0 (2400/800)", res.Ratio)
	}
	if !res.ExceedsThreshold {
		t.Error("ExceedsThreshold = false, want true (3.0 >= 1.5)")
	}
	if !res.ExceedsFloor {
		t.Error("ExceedsFloor = false, want true (2400 >= 500)")
	}
	if res.BaselineSampleCount < ColdStartBaselineMinimumSamples {
		t.Errorf("BaselineSampleCount = %d, want >= %d",
			res.BaselineSampleCount, ColdStartBaselineMinimumSamples)
	}
	if !res.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = false, want true")
	}
	if res.Surface != ColdStartSurfaceOCIFunc {
		t.Errorf("Surface = %q, want %q", res.Surface, ColdStartSurfaceOCIFunc)
	}
}

// TestOCIDetectColdStartRegression_BelowFloor_DoesNotFire — even
// when the ratio exceeds 1.5x, a current P95 below the 500ms floor
// must NOT fire a recommendation. Pinned by the absolute floor
// rationale (well-tuned small-numbers function shouldn't bother the
// operator).
func TestOCIDetectColdStartRegression_BelowFloor_DoesNotFire(t *testing.T) {
	mock := newDetectionMockMonitoring()
	mock.setDuration("24h", 320.0, 60) // 1.6x ratio but < 500ms
	mock.setDuration("168h", 200.0, 100)
	s := newColdStartTestScanner(t, mock)

	res, err := s.DetectColdStartRegression(context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ExceedsThreshold {
		t.Errorf("ExceedsThreshold = false; ratio %v should exceed %v",
			res.Ratio, ColdStartDetectionRatioThreshold)
	}
	if res.ExceedsFloor {
		t.Errorf("ExceedsFloor = true, want false (320 < 500)")
	}
	if res.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (below floor)")
	}
}

// TestOCIDetectColdStartRegression_InsufficientBaselineSamples —
// when the baseline window has fewer than 50 samples, the
// recommendation must NOT fire even when ratio + floor both pass.
// Mirrors the AWS BaselineSampleBelowMinimum case.
func TestOCIDetectColdStartRegression_InsufficientBaselineSamples(t *testing.T) {
	mock := newDetectionMockMonitoring()
	mock.setDuration("24h", 1800.0, 30)
	mock.setDuration("168h", 600.0, 20) // < 50 samples
	s := newColdStartTestScanner(t, mock)

	res, err := s.DetectColdStartRegression(context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ExceedsThreshold {
		t.Error("ExceedsThreshold = false, want true (3.0 ratio)")
	}
	if !res.ExceedsFloor {
		t.Error("ExceedsFloor = false, want true (1800 >= 500)")
	}
	if res.BaselineSampleCount >= ColdStartBaselineMinimumSamples {
		t.Errorf("BaselineSampleCount = %d, want < %d",
			res.BaselineSampleCount, ColdStartBaselineMinimumSamples)
	}
	if res.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (insufficient baseline)")
	}
}

// TestOCIDetectColdStartRegression_QueryError surfaces the
// monitoring client's error wrapped under one of the three
// per-step prefixes.
func TestOCIDetectColdStartRegression_QueryError(t *testing.T) {
	mock := newDetectionMockMonitoring()
	mock.errOn[OCIFunctionsFunctionDurationMetric] = errors.New("HTTP 500")
	s := newColdStartTestScanner(t, mock)
	_, err := s.DetectColdStartRegression(context.Background(),
		"ocid1.fnfunc.oc1.phx.xxx")
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestOCIColdStartConstants_StableStrings pins the per-row provider
// + surface strings the chunk-4 persistence will write.
func TestOCIColdStartConstants_StableStrings(t *testing.T) {
	if ColdStartProviderOCI != "oci" {
		t.Errorf("ColdStartProviderOCI = %q, want %q", ColdStartProviderOCI, "oci")
	}
	if ColdStartSurfaceOCIFunc != "ocifunc" {
		t.Errorf("ColdStartSurfaceOCIFunc = %q, want %q", ColdStartSurfaceOCIFunc, "ocifunc")
	}
}

// Compile-time check the storage interface stays narrow.
var _ scanner.MetricQuerier = (*Scanner)(nil)
