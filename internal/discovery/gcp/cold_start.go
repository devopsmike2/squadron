// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Cold-start latency slice 2 chunk 1 (v0.89.118, #756 Stream 154) —
// GCP-side cold-start regression detection for the Cloud Run +
// Cloud Functions surfaces. Mirrors the AWS slice 1 chunk 2
// detection branch verbatim on the thresholds (1.5x ratio + 500ms
// floor + 50 baseline samples) per design doc §11 acceptance test
// 11; the per-surface variant just swaps the metric source.
//
// The thresholds being byte-identical across all four clouds is
// pinned by cold_start_test.go::TestGCPColdStartThresholdsMatchAWS
// — slice 2 §11 test 11 names this explicitly so the cross-cloud
// claim ("the detection logic stays uniform") stays honest.

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
// versus the rolling 7-day baseline. Per slice 2 §11 acceptance
// test 11 the threshold MUST match slice 1's AWS implementation
// byte-identically (1.5x); the cross-cloud claim that "the
// detection logic stays uniform" rests on the thresholds being
// pinned to identical values across AWS / GCP / Azure / OCI.
//
// Pinned by cold_start_test.go::TestGCPColdStartThresholdsMatchAWS.
//
// See docs/proposals/cold-start-latency-slice2.md §11 + slice 1
// §3.1 for the threshold rationale.
const ColdStartDetectionRatioThreshold = 1.5

// ColdStartDetectionFloorMs is the absolute current-window P95
// floor in milliseconds. Even when the ratio threshold fires, the
// recommendation is suppressed unless the current P95 exceeds this
// floor. Mirrors slice 1's AWS value byte-identically per §11
// acceptance test 11.
const ColdStartDetectionFloorMs = 500.0

// ColdStartCurrentWindowHours is the rolling 24-hour current
// observation window. Mirrors slice 1.
const ColdStartCurrentWindowHours = 24

// ColdStartBaselineWindowHours is the rolling 7-day (168-hour)
// baseline observation window. Mirrors slice 1.
const ColdStartBaselineWindowHours = 168

// ColdStartBaselineMinimumSamples is the floor on baseline sample
// count required for the comparison to be considered trustworthy.
// Mirrors slice 1's AWS value byte-identically per §11 acceptance
// test 11.
const ColdStartBaselineMinimumSamples = 50

// SurfaceCloudRun + SurfaceCloudFunc are the surface identifier
// strings the slice 2 detection branch routes on. They match the
// per-surface discriminator strings the serverless tier slice 1
// chunk 2 scanner stamps on ServerlessInstanceSnapshot.Surface
// (cloudrun / cloudfunc). Keeping the constant pinned in this file
// means the detection branch + the per-surface constructor calls
// share a single source of truth for the surface string.
const (
	SurfaceCloudRun  = cloudRunServerlessSurface
	SurfaceCloudFunc = cloudFuncServerlessSurface
)

// ColdStartStore is the storage adapter the Scanner.coldStartStore
// field is typed against. v0.89.118 — mirrors the AWS slice 1 chunk
// 2 ColdStartStore interface so the production wiring path can use
// a single concrete *sqlite.Storage that satisfies both surfaces'
// detection branches; tests substitute a recording fake without
// dragging in the sqlite migration machinery.
//
// The interface is a strict subset of *sqlite.Storage — both
// SaveColdStartObservation and LatestColdStartObservation already
// live there from slice 1.
type ColdStartStore interface {
	SaveColdStartObservation(ctx context.Context, row sqlite.ColdStartObservationRow) error
	LatestColdStartObservation(ctx context.Context, resourceARN string, windowHours int) (sqlite.ColdStartObservationRow, bool, error)
}

// ColdStartDetectionResult captures the outcome of one detection
// comparison for a single GCP serverless resource (Cloud Run or
// Cloud Function). Same shape as the AWS slice 1 result type. The
// Surface field distinguishes the two GCP variants for the
// proposer's recommendation-kind routing (cloudrun-cold-start-
// baseline vs. cloudfunc-cold-start-baseline per design doc §8).
type ColdStartDetectionResult struct {
	// ResourceARN is the GCP fully-qualified resource path —
	// projects/{p}/locations/{loc}/services/{n} (Cloud Run) or
	// projects/{p}/locations/{loc}/functions/{n} (Cloud Functions).
	ResourceARN string

	// Surface is "cloudrun" or "cloudfunc".
	Surface string

	// CurrentP95Ms / BaselineP95Ms are the rolling 24h / 168h
	// window P95 latencies. Zero when the resource emitted no
	// datapoints in the window (substrate empty-result contract).
	CurrentP95Ms  float64
	BaselineP95Ms float64

	// Ratio is CurrentP95Ms / BaselineP95Ms. Zero when the baseline
	// is also zero (cold-start resource); ShouldFireRecommendation
	// is the canonical predicate that knows how to interpret this.
	Ratio float64

	// ExceedsThreshold / ExceedsFloor are pre-computed against
	// ColdStartDetectionRatioThreshold / ColdStartDetectionFloorMs.
	ExceedsThreshold bool
	ExceedsFloor     bool

	// CurrentSampleCount / BaselineSampleCount are the underlying
	// Cloud Monitoring sample counts. ShouldFireRecommendation
	// gates BaselineSampleCount on ColdStartBaselineMinimumSamples.
	CurrentSampleCount  int
	BaselineSampleCount int

	// ObservedAt is the detection's reference timestamp; round-
	// tripped through the cold_start_observation.observed_at
	// column.
	ObservedAt time.Time
}

// ShouldFireRecommendation is the canonical predicate the chunk-4
// scan integration uses to decide whether to record a
// (cloudrun|cloudfunc)-cold-start-baseline recommendation. All three
// sub-rules must hold:
//
//  1. ExceedsThreshold — current is at least 1.5x baseline,
//  2. ExceedsFloor — current is at least 500ms (the absolute floor),
//  3. BaselineSampleCount >= ColdStartBaselineMinimumSamples — the
//     baseline is statistically trustworthy.
//
// Mirrors the AWS slice 1 predicate verbatim — slice 2 §11
// acceptance test 11 pins the cross-cloud uniformity.
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

// DetectColdStartRegression runs the per-resource cold-start
// regression comparison for a single Cloud Run service or Cloud
// Function. Surface selects the metric source ("cloudrun" →
// request_latencies, "cloudfunc" → execution_times).
//
// Performs two QueryAggregate calls (24h current + 168h baseline)
// and computes the ratio + threshold predicates against the
// ColdStartDetectionRatioThreshold (1.5x) and
// ColdStartDetectionFloorMs (500ms) constants. Returns the result
// unconditionally — the caller decides whether to fire a
// recommendation via ShouldFireRecommendation; the detection
// itself does NOT decide so future slices can tune the predicate
// without touching the math.
//
// Empty-result handling: zero baseline value short-circuits the
// ratio to zero; ShouldFireRecommendation returns false because
// BaselineSampleCount is below the minimum.
//
// See docs/proposals/cold-start-latency-slice2.md §3.1 + §3.2 +
// §11.
func (s *Scanner) DetectColdStartRegression(
	ctx context.Context,
	resourceARN, surface string,
) (ColdStartDetectionResult, error) {
	var metricName string
	switch surface {
	case SurfaceCloudRun:
		metricName = CloudRunRequestLatenciesMetricType
	case SurfaceCloudFunc:
		metricName = CloudFunctionsExecutionTimesMetricType
	default:
		return ColdStartDetectionResult{}, fmt.Errorf("unsupported surface: %q", surface)
	}

	current, err := s.QueryAggregate(
		ctx, resourceARN, metricName,
		time.Duration(ColdStartCurrentWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("current window query: %w", err)
	}

	baseline, err := s.QueryAggregate(
		ctx, resourceARN, metricName,
		time.Duration(ColdStartBaselineWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("baseline window query: %w", err)
	}

	result := ColdStartDetectionResult{
		ResourceARN:         resourceARN,
		Surface:             surface,
		CurrentP95Ms:        current.Value,
		BaselineP95Ms:       baseline.Value,
		CurrentSampleCount:  current.SampleCount,
		BaselineSampleCount: baseline.SampleCount,
		ObservedAt:          time.Now().UTC(),
	}
	if baseline.Value > 0 {
		result.Ratio = current.Value / baseline.Value
		result.ExceedsThreshold = result.Ratio >= ColdStartDetectionRatioThreshold
	}
	result.ExceedsFloor = current.Value >= ColdStartDetectionFloorMs

	return result, nil
}

// runColdStartDetectionForServerless walks the Cloud Run + Cloud
// Functions snapshots in result.Serverless, runs the cold-start
// detection per row, and persists both the 24h and 168h
// observations to cold_start_observation. Called from Scan after
// ScanServerless has populated result.Serverless.
//
// Partial-scan posture: a per-resource detection failure is logged
// into result.FailedServices with the surface-specific identifier
// ("cloudrun_cold_start" / "cloudfunc_cold_start") but does NOT
// halt the per-row loop. Same posture ScanServerless uses.
//
// Skips the whole detection branch when any of metricsClient /
// coldStartStore / connectionID is zero — the metricsClient gate
// keeps the validation-only path from making Cloud Monitoring
// calls; the coldStartStore gate keeps test-only Scanner
// constructions from failing on a nil receiver; the connectionID
// gate keeps rows attributed to no owner from leaking across
// operators.
func (s *Scanner) runColdStartDetectionForServerless(ctx context.Context, result *scanner.Result) {
	if s.metricsClient == nil || s.coldStartStore == nil || s.connectionID == "" {
		return
	}
	for _, snap := range result.Serverless {
		if snap.Provider != ProviderGCP {
			// The same Result can in principle carry Serverless
			// rows from a multi-provider scan — defensive filter
			// keeps the GCP detection branch off non-GCP rows.
			continue
		}
		if snap.ResourceARN == "" {
			continue
		}
		var surface string
		switch snap.Surface {
		case SurfaceCloudRun:
			surface = SurfaceCloudRun
		case SurfaceCloudFunc:
			surface = SurfaceCloudFunc
		default:
			// Future surfaces land in slice 3+; for slice 2 we
			// only detect on Cloud Run + Cloud Functions.
			continue
		}
		detection, err := s.DetectColdStartRegression(ctx, snap.ResourceARN, surface)
		if err != nil {
			recordPartialFailure(result, surface+"_cold_start",
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
// via the chunk-1 storage adapter. Partial-scan posture: a save
// failure is logged into FailedServices and the loop continues.
//
// The snapshot JSON carries the canonical
// scanner.AggregateMetricResult-equivalent shape so the chunk-4
// per-resource API endpoint can return the raw window data without
// re-querying Cloud Monitoring on every dashboard click.
func (s *Scanner) persistColdStartObservation(
	ctx context.Context,
	snap scanner.ServerlessInstanceSnapshot,
	detection ColdStartDetectionResult,
	windowHours int,
	p95Ms float64,
	sampleCount int,
	result *scanner.Result,
) {
	metricName := CloudRunRequestLatenciesMetricType
	if detection.Surface == SurfaceCloudFunc {
		metricName = CloudFunctionsExecutionTimesMetricType
	}
	snapshotJSON, err := json.Marshal(map[string]any{
		"resource_arn": snap.ResourceARN,
		"surface":      detection.Surface,
		"metric_name":  metricName,
		"window_hours": windowHours,
		"statistic":    string(scanner.StatisticP95),
		"value":        p95Ms,
		"sample_count": sampleCount,
		"observed_at":  detection.ObservedAt,
	})
	if err != nil {
		// JSON marshalling the result map of primitives is not
		// expected to fail — guard for the static analysis path
		// without bothering the partial-failure recorder.
		snapshotJSON = []byte("{}")
	}
	row := sqlite.ColdStartObservationRow{
		ID:           uuid.NewString(),
		ConnectionID: s.connectionID,
		Provider:     ProviderGCP,
		Surface:      detection.Surface,
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
		recordPartialFailure(result, detection.Surface+"_cold_start",
			fmt.Sprintf("persist cold-start observation for %s (window=%dh): %s",
				snap.ResourceARN, windowHours, err.Error()))
	}
}
