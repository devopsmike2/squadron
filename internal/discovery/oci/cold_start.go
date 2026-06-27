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

// ColdStartDetectionResult captures the outcome of one detection comparison
// for a single OCI Function.
//
// NOTE: OCI's oci_faas namespace has no cold-start counter metric, so OCI
// cold-start detection is a FunctionExecutionDuration P95-regression heuristic
// (current vs 7-day baseline). It cannot isolate cold-start latency
// specifically — a duration spike may be a cold start OR a slow dependency.
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

	// ObservedAt is the reference timestamp the detection ran.
	ObservedAt time.Time
}

// ShouldFireRecommendation is the canonical predicate the chunk-4
// scan integration uses to decide whether to record an
// ocifunc-cold-start-baseline recommendation. Four sub-rules must
// all hold:
//
//  1. ExceedsThreshold — current is at least 1.5x baseline,
//  2. ExceedsFloor — current is at least 500ms (the absolute floor),
//  3. BaselineSampleCount >= ColdStartBaselineMinimumSamples — the
//     baseline is statistically trustworthy.
//
// See docs/proposals/cold-start-latency-slice2.md §3.4 + §11
// acceptance tests 8-9.
func (r ColdStartDetectionResult) ShouldFireRecommendation() bool {
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

// DetectColdStartRegression runs the per-function cold-start regression
// comparison for a single OCI Function as a FunctionExecutionDuration
// P95-regression heuristic (oci_faas has no cold-start counter metric).
// Two queries: current 24h P95 vs 7-day baseline P95.
//
// Returns the ColdStartDetectionResult unconditionally — the caller decides
// whether to fire via ShouldFireRecommendation. A zero baseline (function
// younger than 7 days / no datapoints) short-circuits the ratio to zero, and
// ShouldFireRecommendation returns false because BaselineSampleCount is below
// the minimum.
//
// NOTE: duration-regression, not cold-start-isolated — a duration spike may be
// a cold start OR a slow downstream dependency. See the type doc.
func (s *Scanner) DetectColdStartRegression(
	ctx context.Context,
	resourceARN string,
) (ColdStartDetectionResult, error) {
	observedAt := time.Now().UTC()

	result := ColdStartDetectionResult{
		ResourceARN: resourceARN,
		Surface:     ColdStartSurfaceOCIFunc,
		ObservedAt:  observedAt,
	}

	// Step 1: FunctionExecutionDuration P95 over the current window.
	current, err := s.QueryAggregate(
		ctx, resourceARN, OCIFunctionsFunctionDurationMetric,
		time.Duration(ColdStartCurrentWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("current window query: %w", err)
	}

	// Step 2: FunctionExecutionDuration P95 over the baseline window.
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
		"resource_arn": snap.ResourceARN,
		"metric_name":  OCIFunctionsFunctionDurationMetric,
		"window_hours": windowHours,
		"statistic":    string(scanner.StatisticP95),
		"value":        p95Ms,
		"sample_count": sampleCount,
		"observed_at":  detection.ObservedAt,
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
