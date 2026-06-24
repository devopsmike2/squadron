// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// perARNMetricsFake returns deterministic responses keyed by the
// (resource_arn, window_hours) pair so a single test can drive the
// 24h and 168h windows of a single resource through different
// canned values — needed for the boundary-case detection tests.
// Mirrors the AWS slice 1 cwPerARNFake shape.
type perARNMetricsFake struct {
	mu        sync.Mutex
	calls     int
	responses map[string]map[time.Duration][]TimeSeriesPoint
	errors    map[string]map[time.Duration]error
}

func (f *perARNMetricsFake) QueryTimeSeries(
	_ context.Context,
	_ string,
	filter string,
	startTime, endTime time.Time,
	_ string,
) ([]TimeSeriesPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	// Pull the resource name (service_name = "X" or function_name =
	// "Y") out of the filter so the per-ARN map keys match the
	// resource the caller asked for.
	name := nameFromFilter(filter)
	window := endTime.Sub(startTime).Round(time.Hour)
	if errMap, ok := f.errors[name]; ok {
		if e, ok := errMap[window]; ok && e != nil {
			return nil, e
		}
	}
	if outMap, ok := f.responses[name]; ok {
		if out, ok := outMap[window]; ok {
			return out, nil
		}
	}
	return nil, nil
}

// nameFromFilter extracts the resource name (either service_name or
// function_name) value out of a Cloud Monitoring filter expression.
// Matches the shape QueryAggregate produces:
//
//	... resource.labels.service_name = "X" ...
//	... resource.labels.function_name = "Y" ...
func nameFromFilter(filter string) string {
	for _, key := range []string{`service_name = "`, `function_name = "`} {
		if i := indexAll(filter, key); i >= 0 {
			rest := filter[i+len(key):]
			if j := indexAll(rest, `"`); j > 0 {
				return rest[:j]
			}
		}
	}
	return ""
}

// indexAll is strings.Index inlined so this file doesn't need to
// import strings just for one helper.
func indexAll(haystack, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func pointsWithSamples(p95 float64, samples int64) []TimeSeriesPoint {
	return []TimeSeriesPoint{{Value: p95, SampleCount: samples}}
}

func newColdStartTestScanner(t *testing.T, f *perARNMetricsFake) *Scanner {
	t.Helper()
	return (&Scanner{ProjectID: "test-project"}).
		WithMetricsClient(f).
		WithMetricsRateLimiter(rate.NewLimiter(rate.Inf, 1))
}

// TestGCPDetectColdStartRegression_CloudRun_ExceedsThreshold — slice 2
// detection happy path on the Cloud Run surface. Boundary case
// mirrors AWS slice 1 chunk 2 acceptance test 5: current = 750ms,
// baseline = 500ms, ratio = 1.5x exactly. All three predicates flip
// true; ShouldFireRecommendation returns true.
func TestGCPDetectColdStartRegression_CloudRun_ExceedsThreshold(t *testing.T) {
	const arn = "projects/test-project/locations/us-central1/services/hot-svc"
	f := &perARNMetricsFake{
		responses: map[string]map[time.Duration][]TimeSeriesPoint{
			"hot-svc": {
				24 * time.Hour:  pointsWithSamples(750.0, 200),
				168 * time.Hour: pointsWithSamples(500.0, 1400),
			},
		},
	}
	s := newColdStartTestScanner(t, f)
	r, err := s.DetectColdStartRegression(context.Background(), arn, SurfaceCloudRun)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Surface != SurfaceCloudRun {
		t.Errorf("Surface = %q, want %q", r.Surface, SurfaceCloudRun)
	}
	if r.Ratio != 1.5 {
		t.Errorf("Ratio = %v, want 1.5 (boundary)", r.Ratio)
	}
	if !r.ExceedsThreshold {
		t.Error("ExceedsThreshold = false, want true at ratio==1.5 (boundary)")
	}
	if !r.ExceedsFloor {
		t.Error("ExceedsFloor = false, want true (current 750ms > 500ms floor)")
	}
	if r.BaselineSampleCount < ColdStartBaselineMinimumSamples {
		t.Errorf("BaselineSampleCount = %d, want >= %d", r.BaselineSampleCount, ColdStartBaselineMinimumSamples)
	}
	if !r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = false, want true (all predicates hold)")
	}
}

// TestGCPDetectColdStartRegression_CloudFunctions_ExceedsThreshold —
// same detection logic on the Cloud Functions surface. Mirrors the
// Cloud Run case but routes through the execution_times metric.
func TestGCPDetectColdStartRegression_CloudFunctions_ExceedsThreshold(t *testing.T) {
	const arn = "projects/test-project/locations/us-east1/functions/hot-fn"
	f := &perARNMetricsFake{
		responses: map[string]map[time.Duration][]TimeSeriesPoint{
			"hot-fn": {
				24 * time.Hour:  pointsWithSamples(2000.0, 200),
				168 * time.Hour: pointsWithSamples(900.0, 1400),
			},
		},
	}
	s := newColdStartTestScanner(t, f)
	r, err := s.DetectColdStartRegression(context.Background(), arn, SurfaceCloudFunc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Surface != SurfaceCloudFunc {
		t.Errorf("Surface = %q, want %q", r.Surface, SurfaceCloudFunc)
	}
	if r.Ratio < 2.2 || r.Ratio > 2.3 {
		t.Errorf("Ratio = %v, want ~2.22", r.Ratio)
	}
	if !r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = false, want true")
	}
}

// TestGCPDetectColdStartRegression_BelowFloor_DoesNotFire — current
// = 499ms (just below the 500ms floor), baseline = 100ms (5x ratio,
// well over threshold). ExceedsFloor is false;
// ShouldFireRecommendation returns false because the absolute floor
// gate blocks. Mirrors AWS slice 1 chunk 2 acceptance test 7.
func TestGCPDetectColdStartRegression_BelowFloor_DoesNotFire(t *testing.T) {
	const arn = "projects/test-project/locations/us-central1/services/fast-svc"
	f := &perARNMetricsFake{
		responses: map[string]map[time.Duration][]TimeSeriesPoint{
			"fast-svc": {
				24 * time.Hour:  pointsWithSamples(499.0, 200),
				168 * time.Hour: pointsWithSamples(100.0, 1400),
			},
		},
	}
	s := newColdStartTestScanner(t, f)
	r, err := s.DetectColdStartRegression(context.Background(), arn, SurfaceCloudRun)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.ExceedsThreshold {
		t.Error("ExceedsThreshold = false, want true (ratio ~4.99 > 1.5)")
	}
	if r.ExceedsFloor {
		t.Error("ExceedsFloor = true, want false (499ms < 500ms floor)")
	}
	if r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (under absolute floor)")
	}
}

// TestGCPDetectColdStartRegression_BaselineSampleTooSmall_DoesNotFire
// — current and baseline values exceed both the ratio and floor
// thresholds, but the baseline has too few samples (< 50). The
// ShouldFireRecommendation predicate blocks because the baseline
// isn't statistically trustworthy.
func TestGCPDetectColdStartRegression_BaselineSampleTooSmall_DoesNotFire(t *testing.T) {
	const arn = "projects/test-project/locations/us-central1/services/new-svc"
	f := &perARNMetricsFake{
		responses: map[string]map[time.Duration][]TimeSeriesPoint{
			"new-svc": {
				24 * time.Hour:  pointsWithSamples(1000.0, 200),
				168 * time.Hour: pointsWithSamples(500.0, 30), // < 50
			},
		},
	}
	s := newColdStartTestScanner(t, f)
	r, err := s.DetectColdStartRegression(context.Background(), arn, SurfaceCloudRun)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.ExceedsThreshold {
		t.Error("ExceedsThreshold = false, want true (ratio 2.0 > 1.5)")
	}
	if !r.ExceedsFloor {
		t.Error("ExceedsFloor = false, want true (current 1000ms > 500ms)")
	}
	if r.BaselineSampleCount >= ColdStartBaselineMinimumSamples {
		t.Errorf("BaselineSampleCount = %d, want < %d", r.BaselineSampleCount, ColdStartBaselineMinimumSamples)
	}
	if r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (baseline samples too few)")
	}
}

// TestGCPDetectColdStartRegression_UnsupportedSurface_ReturnsError —
// surface values outside the slice 2 substrate (cloudrun /
// cloudfunc) surface an error so callers can log + skip the row.
func TestGCPDetectColdStartRegression_UnsupportedSurface_ReturnsError(t *testing.T) {
	f := &perARNMetricsFake{}
	s := newColdStartTestScanner(t, f)
	_, err := s.DetectColdStartRegression(
		context.Background(),
		"projects/test-project/locations/us-central1/services/svc",
		"azfunc",
	)
	if err == nil {
		t.Fatal("expected error for unsupported surface")
	}
}

// TestGCPColdStartThresholdsMatchAWS — slice 2 §11 acceptance test
// 11. The detection thresholds (1.5x ratio + 500ms floor + 50
// baseline samples) MUST be byte-identical to slice 1's AWS values.
// The cross-cloud claim that "the detection logic stays uniform"
// rests on these constants being pinned to identical values.
//
// We pin them to the literal expected values rather than importing
// from the AWS package — the AWS package depends on AWS SDK
// modules, and importing it into the GCP test introduces a
// directional dependency we don't want (the substrate is a peer-of-
// peers; both implementations satisfy a shared scanner.MetricQuerier
// interface). The pin doubles as a tripwire if a future slice tries
// to drift the GCP thresholds — slice 2's contract requires updating
// both clouds together.
func TestGCPColdStartThresholdsMatchAWS(t *testing.T) {
	if ColdStartDetectionRatioThreshold != 1.5 {
		t.Errorf("ColdStartDetectionRatioThreshold = %v, want 1.5 (must match AWS slice 1)", ColdStartDetectionRatioThreshold)
	}
	if ColdStartDetectionFloorMs != 500.0 {
		t.Errorf("ColdStartDetectionFloorMs = %v, want 500.0 (must match AWS slice 1)", ColdStartDetectionFloorMs)
	}
	if ColdStartBaselineMinimumSamples != 50 {
		t.Errorf("ColdStartBaselineMinimumSamples = %d, want 50 (must match AWS slice 1)", ColdStartBaselineMinimumSamples)
	}
	if ColdStartCurrentWindowHours != 24 {
		t.Errorf("ColdStartCurrentWindowHours = %d, want 24 (must match AWS slice 1)", ColdStartCurrentWindowHours)
	}
	if ColdStartBaselineWindowHours != 168 {
		t.Errorf("ColdStartBaselineWindowHours = %d, want 168 (must match AWS slice 1)", ColdStartBaselineWindowHours)
	}
}

// recordingColdStartStore is the in-memory ColdStartStore fake the
// runColdStartDetectionForServerless test exercises. Mirrors the
// AWS slice 1 chunk 2 recordingColdStartStore shape.
type recordingColdStartStore struct {
	mu      sync.Mutex
	rows    []sqlite.ColdStartObservationRow
	saveErr error
}

func (s *recordingColdStartStore) SaveColdStartObservation(_ context.Context, row sqlite.ColdStartObservationRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.rows = append(s.rows, row)
	return nil
}

func (s *recordingColdStartStore) LatestColdStartObservation(_ context.Context, resourceARN string, windowHours int) (sqlite.ColdStartObservationRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.rows) - 1; i >= 0; i-- {
		r := s.rows[i]
		if r.ResourceARN == resourceARN && r.WindowHours == windowHours {
			return r, true, nil
		}
	}
	return sqlite.ColdStartObservationRow{}, false, nil
}

// TestGCPRunColdStartDetectionForServerless_PersistsBothWindows —
// integration test against the runColdStartDetectionForServerless
// helper that the Scan() loop calls. Mixed Cloud Run + Cloud
// Functions snapshots; both land 2 rows each (current + baseline) at
// the storage layer. The provider/surface stamps on the rows are
// what the chunk 4 proposer routes on.
func TestGCPRunColdStartDetectionForServerless_PersistsBothWindows(t *testing.T) {
	f := &perARNMetricsFake{
		responses: map[string]map[time.Duration][]TimeSeriesPoint{
			"hot-svc": {
				24 * time.Hour:  pointsWithSamples(2000, 200),
				168 * time.Hour: pointsWithSamples(500, 1400),
			},
			"warm-fn": {
				24 * time.Hour:  pointsWithSamples(150, 200),
				168 * time.Hour: pointsWithSamples(100, 1400),
			},
		},
	}
	store := &recordingColdStartStore{}
	s := (&Scanner{ProjectID: "test-project"}).
		WithMetricsClient(f).
		WithMetricsRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithColdStartStore(store).
		WithConnectionID("conn-test")
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{
			{
				Provider:    ProviderGCP,
				Surface:     SurfaceCloudRun,
				AccountID:   "test-project",
				Region:      "us-central1",
				ResourceARN: "projects/test-project/locations/us-central1/services/hot-svc",
			},
			{
				Provider:    ProviderGCP,
				Surface:     SurfaceCloudFunc,
				AccountID:   "test-project",
				Region:      "us-east1",
				ResourceARN: "projects/test-project/locations/us-east1/functions/warm-fn",
			},
		},
	}
	s.runColdStartDetectionForServerless(context.Background(), result)
	if len(store.rows) != 4 {
		t.Fatalf("persisted rows = %d, want 4 (2 resources x 2 windows)", len(store.rows))
	}
	for i, r := range store.rows {
		if r.ConnectionID != "conn-test" {
			t.Errorf("row[%d].ConnectionID = %q, want conn-test", i, r.ConnectionID)
		}
		if r.SnapshotJSON == "" {
			t.Errorf("row[%d].SnapshotJSON empty", i)
		}
		if r.Provider != ProviderGCP {
			t.Errorf("row[%d].Provider = %q, want gcp", i, r.Provider)
		}
		if r.Surface != SurfaceCloudRun && r.Surface != SurfaceCloudFunc {
			t.Errorf("row[%d].Surface = %q, want cloudrun or cloudfunc", i, r.Surface)
		}
	}
}

// TestGCPRunColdStartDetectionForServerless_NilStore_NoOp — when
// the store is nil, the helper does not invoke any Cloud Monitoring
// calls and the result.Serverless slice is unaffected. Same
// nil-tolerant posture as the rest of the chunk 1 wiring.
func TestGCPRunColdStartDetectionForServerless_NilStore_NoOp(t *testing.T) {
	f := &perARNMetricsFake{}
	s := (&Scanner{ProjectID: "test-project"}).
		WithMetricsClient(f).
		WithMetricsRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithConnectionID("conn-test")
	// Note: NO WithColdStartStore.
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    ProviderGCP,
			Surface:     SurfaceCloudRun,
			ResourceARN: "projects/test-project/locations/us-central1/services/svc",
		}},
	}
	s.runColdStartDetectionForServerless(context.Background(), result)
	if f.calls != 0 {
		t.Errorf("Cloud Monitoring calls = %d, want 0 with nil store", f.calls)
	}
}

// TestGCPRunColdStartDetectionForServerless_SkipsNonGCPRows —
// defensive filter: a Result populated by a multi-provider scan
// loop is not contaminated by the GCP detection branch running
// against non-GCP rows.
func TestGCPRunColdStartDetectionForServerless_SkipsNonGCPRows(t *testing.T) {
	f := &perARNMetricsFake{}
	store := &recordingColdStartStore{}
	s := (&Scanner{ProjectID: "test-project"}).
		WithMetricsClient(f).
		WithMetricsRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithColdStartStore(store).
		WithConnectionID("conn-test")
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{
			{
				Provider:    "aws",
				Surface:     "lambda",
				ResourceARN: "arn:aws:lambda:us-east-1:123:function:not-gcp",
			},
			{
				Provider:    ProviderGCP,
				Surface:     "unknown-surface",
				ResourceARN: "projects/test-project/locations/us-central1/services/svc",
			},
		},
	}
	s.runColdStartDetectionForServerless(context.Background(), result)
	if f.calls != 0 {
		t.Errorf("Cloud Monitoring calls = %d, want 0 (no GCP cloudrun/cloudfunc rows)", f.calls)
	}
	if len(store.rows) != 0 {
		t.Errorf("persisted rows = %d, want 0", len(store.rows))
	}
}
