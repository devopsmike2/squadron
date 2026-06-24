// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// ColdStartDetectionRatioThreshold is the multiplier above which
// Squadron considers the current-window P95 to be a regression
// versus the rolling 7-day baseline. Pinned to 1.5x to match the
// AWS slice 1 / GCP slice 2 chunk 1 / Azure slice 2 chunk 2
// constants — slice 2 §3.4 explicitly requires uniform detection
// thresholds across all 4 clouds.
//
// Pinned by cold_start_test.go::TestOCIColdStartThresholdsMatchAWS.
//
// See docs/proposals/cold-start-latency-slice2.md §3.4, §11
// acceptance test 11.
const ColdStartDetectionRatioThreshold = 1.5

// ColdStartDetectionFloorMs is the absolute current-window P95 floor
// in milliseconds. Pinned to 500.0 — matches AWS slice 1 / GCP slice
// 2 / Azure slice 2 per the uniform-thresholds rule.
//
// Pinned by cold_start_test.go::TestOCIColdStartThresholdsMatchAWS.
const ColdStartDetectionFloorMs = 500.0

// ColdStartCurrentWindowHours is the rolling 24-hour current
// observation window. Pinned by
// cold_start_test.go::TestOCIColdStartThresholdsMatchAWS.
const ColdStartCurrentWindowHours = 24

// ColdStartBaselineWindowHours is the rolling 7-day (168-hour)
// baseline observation window. Pinned by
// cold_start_test.go::TestOCIColdStartThresholdsMatchAWS.
const ColdStartBaselineWindowHours = 168

// ColdStartBaselineMinimumSamples is the floor on sample count
// required for the 7-day baseline to be considered trustworthy.
// Pinned to 50 to match the AWS slice 1 constant — slice 2 §3.4
// requires uniform thresholds.
const ColdStartBaselineMinimumSamples = 50

// ColdStartSurfaceOCIFunc is the Surface discriminator string the
// chunk-4 detection branch writes onto persisted cold-start
// observation rows. Matches the per-snapshot Surface discriminator
// scanner_functions.go::ocifuncSurface uses for the inventory
// projection — the proposer's recommendation-kind prefix routing
// switches on "ocifunc" → OCI.
const ColdStartSurfaceOCIFunc = "ocifunc"

// ColdStartProviderOCI is the Provider discriminator string the
// chunk-4 detection branch writes onto persisted cold-start
// observation rows. Matches the per-snapshot Provider field
// scanner_functions.go::providerOCI uses.
const ColdStartProviderOCI = "oci"

// ColdStartStore is the storage adapter the Scanner.coldStartStore
// field is typed against. Mirrors the AWS scanner's chunk-2 typedef
// — both SaveColdStartObservation and LatestColdStartObservation
// already live in the sqlite package from slice 1 chunk 1
// (cold_start_observation.go).
type ColdStartStore interface {
	SaveColdStartObservation(ctx context.Context, row sqlite.ColdStartObservationRow) error
	LatestColdStartObservation(ctx context.Context, resourceARN string, windowHours int) (sqlite.ColdStartObservationRow, bool, error)
}

// ColdStartDetectionResult captures the outcome of one detection
// comparison for a single OCI Function. Mirrors the AWS slice-1
// shape with two additions for the OCI-specific signal: a
// CurrentColdStartCount counter (from the cold_start_count metric)
// and a Skipped boolean signaling the §3.4 short-circuit (no cold
// starts in the window means no detection — return early without
// firing).
type ColdStartDetectionResult struct {
	// ResourceARN is the OCI Function OCID this result applies to.
	ResourceARN string

	// Surface is the discriminator string for the per-row surface.
	// Always "ocifunc" for the OCI cold-start detection branch.
	Surface string

	// CurrentP95Ms is the P95 function_duration over the rolling
	// 24-hour current window. Zero when the function had no
	// invocations in the current window — the substrate's
	// empty-result semantics surface as a zero value, not an error.
	CurrentP95Ms float64

	// BaselineP95Ms is the P95 function_duration over the rolling
	// 7-day baseline window. Zero on cold start (the function is
	// younger than 7 days) — Ratio is undefined in that case and
	// stays 0.
	BaselineP95Ms float64

	// Ratio is CurrentP95Ms / BaselineP95Ms. Zero when the baseline
	// is also zero; ShouldFireRecommendation is the canonical
	// predicate that knows how to interpret this.
	Ratio float64

	// ExceedsThreshold is Ratio >= ColdStartDetectionRatioThreshold.
	ExceedsThreshold bool

	// ExceedsFloor is CurrentP95Ms >= ColdStartDetectionFloorMs.
	ExceedsFloor bool

	// CurrentSampleCount + BaselineSampleCount are the underlying
	// OCI Monitoring sample counts (one per returned datapoint).
	// The ShouldFireRecommendation predicate uses
	// BaselineSampleCount to gate on
	// ColdStartBaselineMinimumSamples.
	CurrentSampleCount  int
	BaselineSampleCount int

	// CurrentColdStartCount is the SUM of cold_start_count over the
	// current window. Slice 2 §3.4 uses this as the short-circuit
	// gate: a function with zero cold starts in the window has no
	// cold-start signal to detect, so the detection skips
	// regardless of how the duration P95 compares to the baseline.
	CurrentColdStartCount int

	// Skipped signals that the detection short-circuited because
	// the current-window cold_start_count was zero. When true,
	// ShouldFireRecommendation always returns false and the
	// CurrentP95Ms / BaselineP95Ms / Ratio fields are left at
	// their zero values (the duration queries are not even
	// dispatched — saves the rate-limiter budget).
	Skipped bool

	// ObservedAt is the reference timestamp the detection ran.
	ObservedAt time.Time
}

// ShouldFireRecommendation is the canonical predicate the chunk-4
// scan integration uses to decide whether to record an
// ocifunc-cold-start-baseline recommendation. Four sub-rules must
// all hold:
//
//  1. Skipped is false — cold_start_count > 0 in the window,
//  2. ExceedsThreshold — current is at least 1.5x baseline,
//  3. ExceedsFloor — current is at least 500ms (the absolute floor),
//  4. BaselineSampleCount >= ColdStartBaselineMinimumSamples — the
//     baseline is statistically trustworthy.
//
// See docs/proposals/cold-start-latency-slice2.md §3.4 + §11
// acceptance tests 8-9.
func (r ColdStartDetectionResult) ShouldFireRecommendation() bool {
	if r.Skipped {
		return false
	}
	if !r.ExceedsThreshold {
		return false
	}
	if !r.ExceedsFloor {
		return false
	}
	if r.BaselineSampleCount < ColdStartBaselineMinimumSamples {
		return false
	}
	return true
}

// DetectColdStartRegression runs the per-function cold-start
// regression comparison for a single OCI Function. Per §3.4 the
// detection is a three-step query:
//
//  1. Query cold_start_count over the current window. If the SUM
//     is zero, return immediately with Skipped=true — no cold
//     starts means no signal to detect, regardless of duration.
//  2. Query function_duration P95 over the current window.
//  3. Query function_duration P95 over the baseline window.
//
// Returns the ColdStartDetectionResult unconditionally — the caller
// decides whether to fire a recommendation via the result's
// ShouldFireRecommendation predicate.
//
// Per the MetricQuerier contract, a function that has emitted no
// datapoints in a window returns a zero Value + zero SampleCount
// with no error. The detection threads that through cleanly:
// zero baseline value short-circuits the ratio computation to zero
// (Ratio stays 0, ExceedsThreshold stays false), and
// ShouldFireRecommendation returns false because
// BaselineSampleCount is below the minimum.
//
// See docs/proposals/cold-start-latency-slice2.md §3.4 + §11.
func (s *Scanner) DetectColdStartRegression(
	ctx context.Context,
	resourceARN string,
) (ColdStartDetectionResult, error) {
	observedAt := time.Now().UTC()

	// Step 1: cold_start_count over the current window. The §3.4
	// short-circuit: zero cold starts means no signal — return
	// Skipped=true without dispatching the duration queries.
	csCount, err := s.QueryAggregate(
		ctx, resourceARN, OCIFunctionsColdStartCountMetric,
		time.Duration(ColdStartCurrentWindowHours)*time.Hour,
		scanner.StatisticSum,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("cold-start count query: %w", err)
	}

	result := ColdStartDetectionResult{
		ResourceARN:           resourceARN,
		Surface:               ColdStartSurfaceOCIFunc,
		CurrentColdStartCount: int(csCount.Value),
		ObservedAt:            observedAt,
	}
	if csCount.Value <= 0 {
		// §3.4 short-circuit: no cold starts in the current
		// window. The duration queries would tell us nothing
		// about cold-start latency (since no cold starts
		// happened), so we skip them entirely.
		result.Skipped = true
		return result, nil
	}

	// Step 2: function_duration P95 over the current window.
	current, err := s.QueryAggregate(
		ctx, resourceARN, OCIFunctionsFunctionDurationMetric,
		time.Duration(ColdStartCurrentWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("current window query: %w", err)
	}

	// Step 3: function_duration P95 over the baseline window.
	baseline, err := s.QueryAggregate(
		ctx, resourceARN, OCIFunctionsFunctionDurationMetric,
		time.Duration(ColdStartBaselineWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("baseline window query: %w", err)
	}

	result.CurrentP95Ms = current.Value
	result.BaselineP95Ms = baseline.Value
	result.CurrentSampleCount = current.SampleCount
	result.BaselineSampleCount = baseline.SampleCount

	if baseline.Value > 0 {
		result.Ratio = current.Value / baseline.Value
		result.ExceedsThreshold = result.Ratio >= ColdStartDetectionRatioThreshold
	}
	result.ExceedsFloor = current.Value >= ColdStartDetectionFloorMs

	return result, nil
}

// runColdStartDetectionForServerless walks the OCI Functions-surface
// serverless snapshots in result.Serverless, runs the cold-start
// detection per row, and persists both the 24h current-window and
// 168h baseline-window observations to the cold_start_observation
// table. v0.89.118 (slice 2 chunk 3). Called from the chunk-4
// detection branch wiring (currently lives behind the
// monitoringClient + coldStartStore + connectionID gate; chunk-4
// will wire the call into Scan).
//
// Partial-scan posture mirrors the AWS scanner: a per-function
// detection failure is logged into result.FailedServices but does
// NOT halt the per-row loop. Skipped detections (cold_start_count=0)
// are NOT persisted — there's nothing meaningful to record about a
// window with no cold starts.
func (s *Scanner) runColdStartDetectionForServerless(ctx context.Context, result *scanner.Result) {
	if s.monitoringClient == nil || s.coldStartStore == nil || s.connectionID == "" {
		return
	}
	for _, snap := range result.Serverless {
		if snap.Surface != ColdStartSurfaceOCIFunc {
			continue
		}
		if snap.ResourceARN == "" {
			continue
		}
		detection, err := s.DetectColdStartRegression(ctx, snap.ResourceARN)
		if err != nil {
			recordPartialFailure(result, "ocifunc_cold_start",
				fmt.Sprintf("cold-start detection failed for %s: %s",
					snap.ResourceARN, err.Error()))
			continue
		}
		if detection.Skipped {
			// §3.4: no cold starts in window — nothing to
			// persist, nothing to recommend on.
			continue
		}
		s.persistColdStartObservation(ctx, snap, detection,
			ColdStartCurrentWindowHours,
			detection.CurrentP95Ms, detection.CurrentSampleCount, result)
		s.persistColdStartObservation(ctx, snap, detection,
			ColdStartBaselineWindowHours,
			detection.BaselineP95Ms, detection.BaselineSampleCount, result)
	}
}

// persistColdStartObservation marshals the detection result into a
// ColdStartObservationRow for the supplied window and persists it
// via the slice-1-chunk-1 storage adapter. Mirrors the AWS scanner's
// chunk-2 helper of the same name.
func (s *Scanner) persistColdStartObservation(
	ctx context.Context,
	snap scanner.ServerlessInstanceSnapshot,
	detection ColdStartDetectionResult,
	windowHours int,
	p95Ms float64,
	sampleCount int,
	result *scanner.Result,
) {
	snapshotJSON, err := json.Marshal(map[string]any{
		"resource_arn":            snap.ResourceARN,
		"metric_name":             OCIFunctionsFunctionDurationMetric,
		"window_hours":            windowHours,
		"statistic":               string(scanner.StatisticP95),
		"value":                   p95Ms,
		"sample_count":            sampleCount,
		"observed_at":             detection.ObservedAt,
		"current_cold_start_count": detection.CurrentColdStartCount,
	})
	if err != nil {
		snapshotJSON = []byte("{}")
	}
	row := sqlite.ColdStartObservationRow{
		ID:           uuid.NewString(),
		ConnectionID: s.connectionID,
		Provider:     ColdStartProviderOCI,
		Surface:      ColdStartSurfaceOCIFunc,
		AccountID:    snap.AccountID,
		Region:       snap.Region,
		ResourceARN:  snap.ResourceARN,
		ObservedAt:   detection.ObservedAt,
		WindowHours:  windowHours,
		P95Ms:        p95Ms,
		SampleCount:  sampleCount,
		SnapshotJSON: string(snapshotJSON),
	}
	if err := s.coldStartStore.SaveColdStartObservation(ctx, row); err != nil {
		recordPartialFailure(result, "ocifunc_cold_start",
			fmt.Sprintf("persist cold-start observation for %s (window=%dh): %s",
				snap.ResourceARN, windowHours, err.Error()))
	}
}
