// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// cwPerARNFake returns deterministic responses keyed by the
// (resource_arn, window_hours) pair so a single test can drive the
// 24h and 168h windows of a single resource through different
// canned values — needed for the boundary-case detection tests.
type cwPerARNFake struct {
	mu        sync.Mutex
	responses map[string]map[time.Duration]*cloudwatch.GetMetricStatisticsOutput
	errors    map[string]map[time.Duration]error
	calls     int
}

func (f *cwPerARNFake) GetMetricStatistics(
	_ context.Context,
	in *cloudwatch.GetMetricStatisticsInput,
	_ ...func(*cloudwatch.Options),
) (*cloudwatch.GetMetricStatisticsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(in.Dimensions) == 0 ||
		in.Dimensions[0].Value == nil {
		return &cloudwatch.GetMetricStatisticsOutput{}, nil
	}
	fnName := *in.Dimensions[0].Value
	window := in.EndTime.Sub(*in.StartTime)
	// Window matching tolerates the ±1s clock drift between the
	// Scanner's time.Now() at request start and our retrieval of
	// the canned response. Round to the nearest hour.
	roundedWindow := time.Duration(window.Round(time.Hour))
	if errMap, ok := f.errors[fnName]; ok {
		if e, ok := errMap[roundedWindow]; ok && e != nil {
			return nil, e
		}
	}
	if outMap, ok := f.responses[fnName]; ok {
		if out, ok := outMap[roundedWindow]; ok {
			if out == nil {
				return &cloudwatch.GetMetricStatisticsOutput{}, nil
			}
			return out, nil
		}
	}
	return &cloudwatch.GetMetricStatisticsOutput{}, nil
}

func cwOutput(p95 float64, samples int) *cloudwatch.GetMetricStatisticsOutput {
	return &cloudwatch.GetMetricStatisticsOutput{
		Datapoints: []cwtypes.Datapoint{{
			ExtendedStatistics: map[string]float64{"p95": p95},
			SampleCount:        awssdk.Float64(float64(samples)),
			Unit:               cwtypes.StandardUnitMilliseconds,
		}},
	}
}

func newColdStartTestScanner(t *testing.T, cw *cwPerARNFake) *Scanner {
	t.Helper()
	return newMetricsTestScanner().
		WithCloudWatchClient(cw).
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1))
}

// TestDetectColdStartRegression_ExceedsRatioAndFloor_ReturnsTrue —
// slice 1 chunk 2 acceptance test 5. Boundary case: current = 750ms,
// baseline = 500ms, ratio = 1.5x exactly. Floor = 500ms (current
// just clears it). Baseline sample count above the minimum. All
// three predicates flip true; ShouldFireRecommendation returns true.
func TestDetectColdStartRegression_ExceedsRatioAndFloor_ReturnsTrue(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:123456789012:function:hot-path"
	cw := &cwPerARNFake{
		responses: map[string]map[time.Duration]*cloudwatch.GetMetricStatisticsOutput{
			"hot-path": {
				24 * time.Hour:  cwOutput(750.0, 200),
				168 * time.Hour: cwOutput(500.0, 1400),
			},
		},
	}
	s := newColdStartTestScanner(t, cw)
	r, err := s.DetectColdStartRegression(context.Background(), arn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Ratio != 1.5 {
		t.Errorf("Ratio = %v, want 1.5 (boundary case)", r.Ratio)
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

// TestDetectColdStartRegression_Ratio1Pt4_DoesNotExceed — slice 1
// chunk 2 acceptance test 6. Current = 700ms, baseline = 500ms,
// ratio = 1.4x. Below the 1.5x threshold; ExceedsThreshold stays
// false even though ExceedsFloor would fire (700ms > 500ms).
// ShouldFireRecommendation returns false because the ratio gate
// blocks.
func TestDetectColdStartRegression_Ratio1Pt4_DoesNotExceed(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:123456789012:function:warm"
	cw := &cwPerARNFake{
		responses: map[string]map[time.Duration]*cloudwatch.GetMetricStatisticsOutput{
			"warm": {
				24 * time.Hour:  cwOutput(700.0, 200),
				168 * time.Hour: cwOutput(500.0, 1400),
			},
		},
	}
	s := newColdStartTestScanner(t, cw)
	r, err := s.DetectColdStartRegression(context.Background(), arn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Ratio < 1.39 || r.Ratio > 1.41 {
		t.Errorf("Ratio = %v, want ~1.4", r.Ratio)
	}
	if r.ExceedsThreshold {
		t.Error("ExceedsThreshold = true, want false (1.4 < 1.5)")
	}
	if !r.ExceedsFloor {
		t.Error("ExceedsFloor = false, want true (700ms > 500ms)")
	}
	if r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (ratio under threshold)")
	}
}

// TestDetectColdStartRegression_Floor499ms_DoesNotExceedFloor —
// slice 1 chunk 2 acceptance test 7. Current = 499ms (just below
// the floor); baseline = 100ms (5x ratio, well over threshold).
// ExceedsFloor is false; ShouldFireRecommendation returns false
// because the absolute floor gate blocks. Real-world meaning:
// well-tuned function with a 5x cold-start ratio but the cold start
// is still well under a half-second — not operator-actionable.
func TestDetectColdStartRegression_Floor499ms_DoesNotExceedFloor(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:123456789012:function:fast"
	cw := &cwPerARNFake{
		responses: map[string]map[time.Duration]*cloudwatch.GetMetricStatisticsOutput{
			"fast": {
				24 * time.Hour:  cwOutput(499.0, 200),
				168 * time.Hour: cwOutput(100.0, 1400),
			},
		},
	}
	s := newColdStartTestScanner(t, cw)
	r, err := s.DetectColdStartRegression(context.Background(), arn)
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

// TestDetectColdStartRegression_NoBaseline_ShouldNotFire — slice 1
// chunk 2 acceptance test 8. Baseline value = 0 (cold-start
// function, less than 7 days old or zero baseline invocations).
// Ratio stays at 0, ExceedsThreshold stays false, and
// ShouldFireRecommendation returns false. Operator-visible posture:
// new functions don't get noisy recommendations on their first
// observation window.
func TestDetectColdStartRegression_NoBaseline_ShouldNotFire(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:123456789012:function:newly-deployed"
	cw := &cwPerARNFake{
		responses: map[string]map[time.Duration]*cloudwatch.GetMetricStatisticsOutput{
			"newly-deployed": {
				24 * time.Hour:  cwOutput(2000.0, 100),
				168 * time.Hour: nil, // empty datapoints
			},
		},
	}
	s := newColdStartTestScanner(t, cw)
	r, err := s.DetectColdStartRegression(context.Background(), arn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BaselineP95Ms != 0 {
		t.Errorf("BaselineP95Ms = %v, want 0", r.BaselineP95Ms)
	}
	if r.Ratio != 0 {
		t.Errorf("Ratio = %v, want 0 when baseline is 0", r.Ratio)
	}
	if r.ExceedsThreshold {
		t.Error("ExceedsThreshold = true, want false when baseline is 0")
	}
	if r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (no baseline)")
	}
}

// TestDetectColdStartRegression_BaselineSampleBelowMinimum_ShouldNotFire
// — baseline has a real P95 value but the sample count is below
// ColdStartBaselineMinimumSamples (50). The ratio + floor predicates
// would otherwise fire, but the sample-count gate blocks
// ShouldFireRecommendation. Slice 1 design choice: thin baseline data
// suppresses the recommendation rather than risk a noisy verdict.
func TestDetectColdStartRegression_BaselineSampleBelowMinimum_ShouldNotFire(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:123456789012:function:rare-invokes"
	cw := &cwPerARNFake{
		responses: map[string]map[time.Duration]*cloudwatch.GetMetricStatisticsOutput{
			"rare-invokes": {
				24 * time.Hour:  cwOutput(1500.0, 30),
				168 * time.Hour: cwOutput(500.0, 10), // below minimum
			},
		},
	}
	s := newColdStartTestScanner(t, cw)
	r, err := s.DetectColdStartRegression(context.Background(), arn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.ExceedsThreshold {
		t.Error("ExceedsThreshold = false, want true (3x ratio > 1.5)")
	}
	if !r.ExceedsFloor {
		t.Error("ExceedsFloor = false, want true (1500 > 500)")
	}
	if r.ShouldFireRecommendation() {
		t.Error("ShouldFireRecommendation = true, want false (baseline samples < minimum)")
	}
}

// TestColdStartDetectionResult_ShouldFireRecommendation_RequiresAllConditions —
// exercises the predicate's per-gate logic with crafted result
// structures (no CloudWatch involvement). Each sub-case flips
// exactly one gate off and verifies the predicate returns false.
func TestColdStartDetectionResult_ShouldFireRecommendation_RequiresAllConditions(t *testing.T) {
	// Base: all three predicates hold.
	base := ColdStartDetectionResult{
		ExceedsThreshold:    true,
		ExceedsFloor:        true,
		BaselineSampleCount: 100,
	}
	if !base.ShouldFireRecommendation() {
		t.Fatal("base case should fire (sanity)")
	}
	cases := []struct {
		name   string
		mutate func(*ColdStartDetectionResult)
	}{
		{"no threshold", func(r *ColdStartDetectionResult) { r.ExceedsThreshold = false }},
		{"no floor", func(r *ColdStartDetectionResult) { r.ExceedsFloor = false }},
		{"thin baseline", func(r *ColdStartDetectionResult) { r.BaselineSampleCount = 10 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if r.ShouldFireRecommendation() {
				t.Errorf("%s: ShouldFireRecommendation = true, want false", tc.name)
			}
		})
	}
}

// recordingColdStartStore captures persistence calls so the
// scan-integration tests can pin both windows landed at the storage
// layer.
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

func (s *recordingColdStartStore) LatestColdStartObservation(_ context.Context, _ string, resourceARN string, windowHours int) (sqlite.ColdStartObservationRow, bool, error) {
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

// TestRunColdStartDetectionForServerless_PersistsBothWindows —
// integration-shaped test against the runColdStartDetectionForServerless
// helper that the Scan() loop calls. Two Lambda snapshots; one
// produces a fire-worthy result, the other produces a quiet one.
// Both should land 2 rows (current + baseline) at the storage layer.
func TestRunColdStartDetectionForServerless_PersistsBothWindows(t *testing.T) {
	cw := &cwPerARNFake{
		responses: map[string]map[time.Duration]*cloudwatch.GetMetricStatisticsOutput{
			"fn-fires": {
				24 * time.Hour:  cwOutput(2000, 200),
				168 * time.Hour: cwOutput(500, 1400),
			},
			"fn-quiet": {
				24 * time.Hour:  cwOutput(150, 200),
				168 * time.Hour: cwOutput(100, 1400),
			},
		},
	}
	store := &recordingColdStartStore{}
	s := newMetricsTestScanner().
		WithCloudWatchClient(cw).
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithColdStartStore(store).
		WithConnectionID("conn-test")
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{
			{
				Provider:    "aws",
				Surface:     "lambda",
				AccountID:   "123456789012",
				Region:      "us-east-1",
				ResourceARN: "arn:aws:lambda:us-east-1:123456789012:function:fn-fires",
			},
			{
				Provider:    "aws",
				Surface:     "lambda",
				AccountID:   "123456789012",
				Region:      "us-east-1",
				ResourceARN: "arn:aws:lambda:us-east-1:123456789012:function:fn-quiet",
			},
		},
	}
	s.runColdStartDetectionForServerless(context.Background(), result)
	if len(store.rows) != 4 {
		t.Fatalf("persisted rows = %d, want 4 (2 fns x 2 windows)", len(store.rows))
	}
	// Each row should carry the connection ID and a non-empty
	// snapshot JSON.
	for i, r := range store.rows {
		if r.ConnectionID != "conn-test" {
			t.Errorf("row[%d].ConnectionID = %q, want conn-test", i, r.ConnectionID)
		}
		if r.SnapshotJSON == "" {
			t.Errorf("row[%d].SnapshotJSON empty", i)
		}
		if r.Provider != "aws" || r.Surface != "lambda" {
			t.Errorf("row[%d] provider/surface = %q/%q", i, r.Provider, r.Surface)
		}
	}
}

// TestRunColdStartDetectionForServerless_NilStore_NoOp — when the
// store is nil, the helper does not invoke any CloudWatch calls and
// the result.Serverless slice is unaffected. Same nil-tolerant
// posture as the rest of the chunk-2 wiring.
func TestRunColdStartDetectionForServerless_NilStore_NoOp(t *testing.T) {
	cw := &cwPerARNFake{}
	s := newMetricsTestScanner().
		WithCloudWatchClient(cw).
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithConnectionID("conn-test")
	// Note: NO WithColdStartStore.
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    "aws",
			Surface:     "lambda",
			ResourceARN: "arn:aws:lambda:us-east-1:123456789012:function:fn",
		}},
	}
	s.runColdStartDetectionForServerless(context.Background(), result)
	if cw.calls != 0 {
		t.Errorf("CloudWatch calls = %d, want 0 with nil store", cw.calls)
	}
}

// TestExtractLambdaFunctionName_Variants — table-driven coverage of
// the ARN parser. Documents the supported inputs and rejected
// shapes so future chunks (slice 2 GCP/Azure cold-start surfaces)
// know what the substrate accepts.
func TestExtractLambdaFunctionName_Variants(t *testing.T) {
	cases := []struct {
		arn    string
		want   string
		wantOK bool
	}{
		{"arn:aws:lambda:us-east-1:123456789012:function:order", "order", true},
		{"arn:aws:lambda:us-west-2:111:function:checkout:$LATEST", "checkout", true},
		{"arn:aws:ec2:us-east-1:123:instance/i-abc", "", false},
		{"", "", false},
		{"arn:aws:lambda:us-east-1:123:function:", "", false},
		{"not-an-arn", "", false},
	}
	for _, tc := range cases {
		got, err := extractLambdaFunctionName(tc.arn)
		if tc.wantOK && err != nil {
			t.Errorf("arn=%q: unexpected error %v", tc.arn, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("arn=%q: expected error, got %q", tc.arn, got)
		}
		if got != tc.want {
			t.Errorf("arn=%q: got %q, want %q", tc.arn, got, tc.want)
		}
	}
}

// TestIsCloudWatchThrottleError_NotThrottle — non-throttle SDK errors
// (e.g. AccessDenied) should NOT flip the retry path on. The error
// goes straight back to the caller.
func TestIsCloudWatchThrottleError_NotThrottle(t *testing.T) {
	if isCloudWatchThrottleError(nil) {
		t.Error("nil error must not be classified as throttle")
	}
	if isCloudWatchThrottleError(errors.New("AccessDenied")) {
		t.Error("plain text error must not be classified as throttle")
	}
}

// TestColdStartConstants_Values pins the four detection constants
// to the slice 1 design doc §3 values. Changing any of these
// requires updating the runbook in chunk 4 and the proposer prompt
// in chunk 3 — pinning here surfaces accidental drift early.
func TestColdStartConstants_Values(t *testing.T) {
	if ColdStartDetectionRatioThreshold != 1.5 {
		t.Errorf("ColdStartDetectionRatioThreshold = %v, want 1.5", ColdStartDetectionRatioThreshold)
	}
	if ColdStartDetectionFloorMs != 500.0 {
		t.Errorf("ColdStartDetectionFloorMs = %v, want 500.0", ColdStartDetectionFloorMs)
	}
	if ColdStartCurrentWindowHours != 24 {
		t.Errorf("ColdStartCurrentWindowHours = %d, want 24", ColdStartCurrentWindowHours)
	}
	if ColdStartBaselineWindowHours != 168 {
		t.Errorf("ColdStartBaselineWindowHours = %d, want 168", ColdStartBaselineWindowHours)
	}
	if ColdStartBaselineMinimumSamples != 50 {
		t.Errorf("ColdStartBaselineMinimumSamples = %d, want 50", ColdStartBaselineMinimumSamples)
	}
}
