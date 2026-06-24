// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// ColdStartDetectionRatioThreshold is the multiplier above which
// Squadron considers the current-window P95 to be a regression
// versus the rolling 7-day baseline. Per the slice 1 §3 design rule
// the threshold is exactly 1.5x — current_p95 / baseline_p95 >= 1.5
// is the "exceeds threshold" predicate.
//
// Acceptance test 5 pins the boundary case (1.5x exactly triggers)
// and acceptance test 6 pins the just-below case (1.4x does not).
//
// See docs/proposals/cold-start-latency-slice1.md §3, §3.1.
const ColdStartDetectionRatioThreshold = 1.5

// ColdStartDetectionFloorMs is the absolute current-window P95 floor
// in milliseconds. Even when the ratio threshold fires, the
// recommendation is suppressed unless the current P95 exceeds this
// floor. The rule filters well-tuned functions that hit the ratio
// edge on small absolute numbers (baseline 200ms, current 320ms is a
// 1.6x ratio but the cold start is still fast enough to not affect
// the operator).
//
// Acceptance test 7 pins the just-below case (499ms does not fire).
//
// See docs/proposals/cold-start-latency-slice1.md §3 (the absolute
// floor rationale).
const ColdStartDetectionFloorMs = 500.0

// ColdStartCurrentWindowHours is the rolling 24-hour current
// observation window. Pinned by acceptance test 11 (the per-resource
// API endpoint reports current_window.window_hours = 24).
const ColdStartCurrentWindowHours = 24

// ColdStartBaselineWindowHours is the rolling 7-day (168-hour)
// baseline observation window. Pinned by acceptance test 11.
const ColdStartBaselineWindowHours = 168

// ColdStartBaselineMinimumSamples is the floor on sample count
// required for the 7-day baseline to be considered trustworthy. A
// function that's been live for less than a day or that runs at very
// low throughput won't have accumulated enough InitDuration
// datapoints to make a baseline comparison meaningful — the rule
// skips the recommendation rather than firing noisy verdicts on thin
// data. Pinned at 50 samples to match the §3 §3.1 design discussion
// ("the variance is unlikely to be statistical noise" requires a
// baseline with enough points to be statistical).
//
// Acceptance test 8 covers the no-baseline case (baseline value=0);
// the BaselineSampleBelowMinimum case is exercised by the
// ShouldFireRecommendation suite.
const ColdStartBaselineMinimumSamples = 50

// ColdStartStore is the storage adapter the Scanner.coldStartStore
// field is typed against. v0.89.114 — extracted from the concrete
// *sqlite.Storage type so the production scanner can take a real
// store, the chunk-2 tests can substitute an in-memory fake, and the
// chunk-3 cold-start detection unit tests can swap in a recording
// stub without dragging in the sqlite migration machinery.
//
// The interface is a strict subset of *sqlite.Storage — both
// SaveColdStartObservation and LatestColdStartObservation already
// live there from chunk 1 (cold_start_observation.go).
type ColdStartStore interface {
	SaveColdStartObservation(ctx context.Context, row sqlite.ColdStartObservationRow) error
	LatestColdStartObservation(ctx context.Context, resourceARN string, windowHours int) (sqlite.ColdStartObservationRow, bool, error)
}

// ColdStartDetectionResult captures the outcome of one detection
// comparison for a single Lambda function. The shape mirrors the
// per-resource API endpoint response (§6.1) so the chunk-2 handler
// can adapt it directly. The bool predicates (ExceedsThreshold /
// ExceedsFloor) are pre-computed so the caller doesn't have to
// re-apply the ratio / floor rule.
type ColdStartDetectionResult struct {
	// ResourceARN is the Lambda function ARN this result applies to.
	ResourceARN string

	// CurrentP95Ms is the P95 InitDuration over the rolling 24-hour
	// current window. Zero when the function had no invocations in
	// the current window — the substrate's empty-result semantics
	// surface as a zero value, not an error.
	CurrentP95Ms float64

	// BaselineP95Ms is the P95 InitDuration over the rolling 7-day
	// baseline window. Zero on cold start (the function is younger
	// than 7 days) — Ratio is undefined in that case and stays 0.
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
	// CloudWatch sample counts. The ShouldFireRecommendation
	// predicate uses BaselineSampleCount to gate on
	// ColdStartBaselineMinimumSamples — too few samples in the
	// baseline means the comparison isn't trustworthy.
	CurrentSampleCount  int
	BaselineSampleCount int

	// ObservedAt is the reference timestamp the detection ran. Set
	// to time.Now().UTC() at the top of DetectColdStartRegression;
	// round-tripped through the cold_start_observation table's
	// observed_at column.
	ObservedAt time.Time
}

// ShouldFireRecommendation is the canonical predicate the chunk-2
// scan integration uses to decide whether to record a
// lambda-cold-start-baseline recommendation. All three sub-rules
// must hold:
//
//  1. ExceedsThreshold — current is at least 1.5x baseline,
//  2. ExceedsFloor — current is at least 500ms (the absolute floor),
//  3. BaselineSampleCount >= ColdStartBaselineMinimumSamples — the
//     baseline is statistically trustworthy.
//
// Acceptance tests 5 / 6 / 7 / 8 + the BaselineSampleBelowMinimum
// case pin this predicate.
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
// regression comparison for a single Lambda. Performs two
// QueryAggregate calls — one for the 24-hour current window, one
// for the 168-hour baseline — and computes the ratio + threshold
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
// InitDuration datapoints in the requested window returns a zero
// Value + zero SampleCount with no error. The detection threads
// that through cleanly: zero baseline value short-circuits the
// ratio computation to zero (Ratio stays 0, ExceedsThreshold stays
// false), and ShouldFireRecommendation returns false because
// BaselineSampleCount is below the minimum.
//
// See docs/proposals/cold-start-latency-slice1.md §3 + §11
// acceptance tests 5-8.
func (s *Scanner) DetectColdStartRegression(
	ctx context.Context,
	resourceARN string,
) (ColdStartDetectionResult, error) {
	current, err := s.QueryAggregate(
		ctx, resourceARN, LambdaInitDurationMetricName,
		time.Duration(ColdStartCurrentWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("current window query: %w", err)
	}

	baseline, err := s.QueryAggregate(
		ctx, resourceARN, LambdaInitDurationMetricName,
		time.Duration(ColdStartBaselineWindowHours)*time.Hour,
		scanner.StatisticP95,
	)
	if err != nil {
		return ColdStartDetectionResult{}, fmt.Errorf("baseline window query: %w", err)
	}

	result := ColdStartDetectionResult{
		ResourceARN:         resourceARN,
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

// runColdStartDetectionForServerless walks the Lambda-surface
// serverless snapshots in result.Serverless, runs the cold-start
// detection per row, and persists both the 24h current-window and
// 168h baseline-window observations to the cold_start_observation
// table. v0.89.114 (slice 1 chunk 2). Called from Scan after
// scanRegionLambdaServerless has populated result.Serverless for
// the region.
//
// Partial-scan posture: a per-function detection failure is logged
// into result.FailedServices (via recordPartialFailure with the
// sentinel "lambda_cold_start") but does NOT halt the per-row loop —
// one function with a transient CloudWatch error doesn't sink the
// observations for the rest of the region. Same posture
// scanRegionLambdaServerless uses.
//
// Skips persistence (and therefore the whole detection branch) when
// either s.cwClient or s.coldStartStore is nil. The cwClient gate
// keeps the validation-only path from making CloudWatch calls; the
// coldStartStore gate keeps the test-only Scanner constructions
// (which don't wire a store) from failing on the nil receiver.
//
// Also skips when connectionID is empty — same rationale as the
// chunk-1 store contract: rows attributed to no owner would leak
// across operators in a multi-tenant deployment.
func (s *Scanner) runColdStartDetectionForServerless(ctx context.Context, result *scanner.Result) {
	if s.cwClient == nil || s.coldStartStore == nil || s.connectionID == "" {
		return
	}
	for _, snap := range result.Serverless {
		if snap.Surface != lambdaServerlessSurface {
			continue
		}
		if snap.ResourceARN == "" {
			continue
		}
		detection, err := s.DetectColdStartRegression(ctx, snap.ResourceARN)
		if err != nil {
			recordPartialFailure(result, "lambda_cold_start",
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
// scanner.AggregateMetricResult-equivalent shape so the chunk-2
// per-resource API endpoint can return the raw window data without
// re-querying CloudWatch on every dashboard click.
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
		"metric_name":  LambdaInitDurationMetricName,
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
		Provider:     "aws",
		Surface:      lambdaServerlessSurface,
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
		recordPartialFailure(result, "lambda_cold_start",
			fmt.Sprintf("persist cold-start observation for %s (window=%dh): %s",
				snap.ResourceARN, windowHours, err.Error()))
	}
}

// WithCloudWatchClient wires the CloudWatch SDK client (or a test
// fake satisfying CloudWatchClient) into the Scanner. v0.89.114.
// Returns the Scanner so the constructor chain composes:
//
//	s := NewScannerForValidation(creds, accountID).WithCloudWatchClient(cw)
//
// Nil clients are accepted — the QueryAggregate path treats a nil
// cwClient as the chunk-1 skeleton (returns
// scanner.ErrMetricNotImplemented), preserving the v0.89.113 surface
// when callers explicitly want to opt out.
func (s *Scanner) WithCloudWatchClient(client CloudWatchClient) *Scanner {
	s.cwClient = client
	return s
}

// WithColdStartStore wires the cold-start observation storage
// adapter into the Scanner. v0.89.114. Same setter-only extension
// pattern as the trace-coverage handler family — production wires
// the real *sqlite.Storage; tests substitute an in-memory fake.
func (s *Scanner) WithColdStartStore(store ColdStartStore) *Scanner {
	s.coldStartStore = store
	return s
}

// WithConnectionID overrides the connection identifier used to scope
// persisted cold-start observations. v0.89.114. Production carries
// the value through NewScannerFromConnection; the
// validation-constructor path leaves it empty, so tests that want to
// exercise the persistence branch via NewScannerForValidation set it
// here explicitly.
func (s *Scanner) WithConnectionID(id string) *Scanner {
	s.connectionID = id
	return s
}

// WithCloudWatchRateLimiter overrides the per-Scanner rate limiter.
// v0.89.114. Reserved for tests that need to pin the limiter's
// burst to a specific value to deterministically time the
// 10-RPS pin (TestQueryAggregate_RateLimiterCapsAt10RPS), or to
// disable it entirely (a nil limiter short-circuits the Wait
// call). Production never calls this — the constructors pre-arm the
// limiter at the substrate-default RPS.
func (s *Scanner) WithCloudWatchRateLimiter(limiter *rate.Limiter) *Scanner {
	s.cwRateLimiter = limiter
	return s
}
