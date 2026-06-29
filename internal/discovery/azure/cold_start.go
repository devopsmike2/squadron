// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// ColdStartDetectionRatioThreshold is the multiplier above which the
// slice 2 substrate considers the current-window aggregate to be a
// regression versus the rolling 7-day baseline. Per the slice 2 §3
// design rule (which inherits slice 1's threshold for cross-cloud
// uniformity) the threshold is exactly 1.5x —
// current / baseline >= 1.5 is the "exceeds threshold" predicate.
//
// Pinned by cold_start_test.go::TestAzureColdStartThresholdsMatchAWS
// against the AWS slice 1 constant — any divergence breaks the
// cross-cloud "uniform detection logic" guarantee the design doc
// promises in §1, §11 acceptance test 11.
//
// See docs/proposals/cold-start-latency-slice2.md §1
// ("detection logic stays uniform (1.5x ratio + 500ms floor + 50
// baseline samples)"); §11 acceptance test 11.
const ColdStartDetectionRatioThreshold = 1.5

// ColdStartDetectionFloorMs is the absolute current-window aggregate
// floor in milliseconds. Even when the ratio threshold fires, the
// recommendation is suppressed unless the current value exceeds this
// floor. The rule filters well-tuned Function Apps that hit the
// ratio edge on small absolute numbers (baseline 200ms, current
// 320ms is a 1.6x ratio but the cold start is still fast enough to
// not affect the operator).
//
// Pinned by cold_start_test.go::TestAzureColdStartThresholdsMatchAWS.
//
// See docs/proposals/cold-start-latency-slice2.md §1.
const ColdStartDetectionFloorMs = 500.0

// ColdStartCurrentWindowHours is the rolling 24-hour current
// observation window. Matches AWS slice 1 for cross-cloud
// comparability.
const ColdStartCurrentWindowHours = 24

// ColdStartBaselineWindowHours is the rolling 7-day (168-hour)
// baseline observation window. Matches AWS slice 1 for cross-cloud
// comparability.
const ColdStartBaselineWindowHours = 168

// ColdStartBaselineMinimumSamples is the floor on sample count
// required for the 7-day baseline to be considered trustworthy. A
// function that's been live for less than a day or that runs at very
// low throughput won't have accumulated enough datapoints to make a
// baseline comparison meaningful — the rule skips the recommendation
// rather than firing noisy verdicts on thin data.
//
// On the Azure side, "sample count" is "count of 5-minute timeseries
// buckets that had a non-nil aggregate" (Azure Monitor doesn't
// surface a per-bucket sample count the way CloudWatch does). The
// gate's threshold matches AWS slice 1 (50) for cross-cloud
// uniformity; 50 buckets at 5-minute intervals is ~4 hours of
// continuous traffic, which is the substrate's "statistically
// trustworthy" floor.
//
// Pinned by cold_start_test.go::TestAzureColdStartThresholdsMatchAWS.
const ColdStartBaselineMinimumSamples = 50

// ColdStartDetectionResult captures the outcome of one detection
// comparison for a single Azure Function App. The shape mirrors the
// AWS slice 1 ColdStartDetectionResult (so the chunk-4 per-resource
// API endpoint can adapt either provider's result type with the
// same handler shape) and adds the Surface + UsedFallback fields
// the multi-cloud chunk-4 wiring keys on.
type ColdStartDetectionResult struct {
	// ResourceARN is the Function App's ARM resource id. The Azure
	// scanner persists this verbatim — operator-hostile but the
	// canonical identifier the proposer's evidence list and
	// recommendation envelope's AffectedResources field reference.
	ResourceARN string

	// Surface is the slice 2 multi-cloud surface discriminator. Set
	// to "azfunc" for the Azure Functions branch (mirrors
	// azureFunctionsServerlessSurface from functions_scanner.go).
	// The chunk-4 cold-start dispatcher reads this to route the
	// recommendation to the azfunc-cold-start-baseline kind.
	Surface string

	// CurrentP95Ms is the current 24-hour window aggregate in
	// milliseconds. Note: Azure Monitor doesn't natively support
	// percentile aggregations on FunctionExecutionDuration, so this
	// is the 5-minute Maximum rolled up across the window rather
	// than a true P95 — the field name preserves cross-cloud
	// symmetry with the AWS slice 1 result type; the operator
	// runbook documents the approximation. Zero when the function
	// had no invocations in the current window.
	CurrentP95Ms float64

	// BaselineP95Ms is the baseline 168-hour window aggregate in
	// milliseconds. Same approximation note as CurrentP95Ms. Zero
	// on cold start (the function is younger than 7 days) — Ratio
	// is undefined in that case and stays 0.
	BaselineP95Ms float64

	// Ratio is CurrentP95Ms / BaselineP95Ms. Zero when the baseline
	// is also zero (cold-start function); ShouldFireRecommendation
	// is the canonical predicate that knows how to interpret this.
	Ratio float64

	// ExceedsThreshold is Ratio >= ColdStartDetectionRatioThreshold.
	// Pre-computed so callers don't have to repeat the comparison.
	ExceedsThreshold bool

	// ExceedsFloor is CurrentP95Ms >= ColdStartDetectionFloorMs.
	// Pre-computed for the same reason.
	ExceedsFloor bool

	// CurrentSampleCount + BaselineSampleCount are the underlying
	// Azure Monitor 5-minute bucket counts. The
	// ShouldFireRecommendation predicate uses BaselineSampleCount
	// to gate on ColdStartBaselineMinimumSamples — too few buckets
	// in the baseline means the comparison isn't trustworthy.
	CurrentSampleCount  int
	BaselineSampleCount int

	// UsedFallback is true when the IsAfterColdStart=true dimension
	// filter was NOT honored by the Function App's runtime version
	// (2023+ runtimes emit the dimension; older runtimes don't).
	// The substrate falls back to an unfiltered query and surfaces
	// the signal here so the chunk-4 detection branch can record
	// an informational note in the snapshot detail per design doc
	// §3.3.
	//
	// True iff EITHER the current OR the baseline query fell back —
	// in practice the two windows query the same resource so the
	// signal is symmetric, but the OR semantics keep the field
	// honest when (e.g.) the operator upgrades the runtime mid-
	// baseline-window.
	UsedFallback bool

	// ObservedAt is the reference timestamp the detection ran. Set
	// to time.Now().UTC() at the top of DetectColdStartRegression;
	// round-tripped through the cold_start_observation table's
	// observed_at column.
	ObservedAt time.Time
}

// ShouldFireRecommendation is the canonical predicate the chunk-4
// scan integration uses to decide whether to record an
// azfunc-cold-start-baseline recommendation. All three sub-rules
// must hold:
//
//  1. ExceedsThreshold — current is at least 1.5x baseline,
//  2. ExceedsFloor — current is at least 500ms (the absolute floor),
//  3. BaselineSampleCount >= ColdStartBaselineMinimumSamples — the
//     baseline is statistically trustworthy.
//
// Mirrors the AWS slice 1 predicate of the same name (cross-cloud
// uniform detection logic per design doc §1).
//
// Note: UsedFallback does NOT gate firing. The slice 2 contract is
// that the recommendation fires for older-runtime functions too —
// the informational note in the snapshot detail tells the operator
// the IsAfterColdStart dimension wasn't available, but the
// regression signal is still actionable (the Function App's overall
// execution duration spiked, even if Squadron can't isolate cold-
// start from warm).
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

// DetectColdStartRegression runs the per-function cold-start
// regression comparison for a single Azure Function App. Performs
// two QueryAggregate calls — one for the 24-hour current window,
// one for the 168-hour baseline — and computes the ratio + threshold
// predicates against the ColdStartDetectionRatioThreshold (1.5x)
// and ColdStartDetectionFloorMs (500ms) constants.
//
// Returns the ColdStartDetectionResult unconditionally — the caller
// decides whether to fire a recommendation via the result's
// ShouldFireRecommendation predicate. The detection itself does
// NOT decide; that policy choice lives on the result type so future
// slices can tune the predicate without touching the detection
// math.
//
// Per the MetricQuerier contract, a function that has emitted no
// datapoints in the requested window returns a zero Value + zero
// SampleCount with no error. The detection threads that through
// cleanly: zero baseline value short-circuits the ratio computation
// to zero (Ratio stays 0, ExceedsThreshold stays false), and
// ShouldFireRecommendation returns false because
// BaselineSampleCount is below the minimum.
//
// The UsedFallback bool is populated from the Unit field's
// " (fallback)" suffix the QueryAggregate path appends when the
// IsAfterColdStart=true dimension filter wasn't honored by the
// runtime version. The signal flows through the snapshot detail per
// design doc §3.3.
//
// See docs/proposals/cold-start-latency-slice2.md §3.3 + §11
// acceptance tests 5-7.
// COMMERCIAL-TIER (#153, enterprise-gate decision): Azure Functions
// cold-start + error-rate detection needs Application Insights and is part of
// the future commercial tier, not OSS. This detector is not invoked by the
// Azure scan path; OSS surfaces the gap via the proposer's
// azfunc-appinsights-enable recommendation.
func (s *Scanner) DetectColdStartRegression(
	ctx context.Context,
	resourceARN string,
) (ColdStartDetectionResult, error) {
	current, err := s.QueryAggregate(
		ctx, resourceARN, AzureFunctionsExecutionDurationMetric,
		time.Duration(ColdStartCurrentWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("current window query: %w", err)
	}

	baseline, err := s.QueryAggregate(
		ctx, resourceARN, AzureFunctionsExecutionDurationMetric,
		time.Duration(ColdStartBaselineWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("baseline window query: %w", err)
	}

	result := ColdStartDetectionResult{
		ResourceARN:         resourceARN,
		Surface:             azureFunctionsServerlessSurface,
		CurrentP95Ms:        current.Value,
		BaselineP95Ms:       baseline.Value,
		CurrentSampleCount:  current.SampleCount,
		BaselineSampleCount: baseline.SampleCount,
		ObservedAt:          time.Now().UTC(),
		UsedFallback: aggregateUsedFallback(current.Unit) ||
			aggregateUsedFallback(baseline.Unit),
	}
	if baseline.Value > 0 {
		result.Ratio = current.Value / baseline.Value
		result.ExceedsThreshold = result.Ratio >= ColdStartDetectionRatioThreshold
	}
	result.ExceedsFloor = current.Value >= ColdStartDetectionFloorMs

	return result, nil
}

// aggregateUsedFallback inspects an AggregateMetricResult.Unit
// string for the " (fallback)" suffix the QueryAggregate path
// appends when the IsAfterColdStart=true dimension filter wasn't
// honored. The substrate keeps the fallback signal in-band on the
// Unit field to avoid growing the MetricQuerier interface's return
// shape for the Azure-only fallback case; this helper unpacks it.
//
// Returns true iff the Unit ends with the fallback suffix. A
// caller that checks both the current and baseline window's results
// (DetectColdStartRegression does this) gets the OR semantics
// described in ColdStartDetectionResult.UsedFallback godoc.
func aggregateUsedFallback(unit string) bool {
	return strings.HasSuffix(unit, azureMonitorFallbackUnitSuffix)
}
