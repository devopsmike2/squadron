// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// error_rate.go — Error rate correlation slice 1 chunk 3
// (v0.89.129, #769 Stream 167). Sibling of cold_start.go: per-Lambda
// scan-time error-rate detection that mirrors the cold-start
// branch pattern verbatim.
//
// Mirrors proposer.error_rate.go's detection math byte-identically
// here in the AWS package to avoid the proposer → aws import cycle
// (the proposer already imports aws for the metric name constants;
// the scanner cannot import proposer back). Cross-package
// consistency is pinned by error_rate_test.go::
// TestAWSErrorRateConstantsMatchProposer.
//
// For each Lambda serverless inventory row, runs 4 metric queries
// (current 24h invocations + errors, baseline 168h invocations +
// errors) and persists both the 24h current-window and 168h
// baseline-window observations to error_rate_observation via
// Storage.SaveErrorRateObservation. Per-row failures degrade to a
// recordPartialFailure entry ("lambda_error_rate" sentinel) and
// continue — error-rate metric query failures do NOT break the
// scan, matching the cold-start branch posture.
//
// See docs/proposals/error-rate-correlation-slice1.md §6.2.

// ErrorRateRatioFloor mirrors proposer.ErrorRateRatioFloor.
const ErrorRateRatioFloor = 2.0

// ErrorRateMinInvocationCount mirrors proposer.ErrorRateMinInvocationCount.
const ErrorRateMinInvocationCount uint64 = 1000

// ErrorRateMinErrorCount mirrors proposer.ErrorRateMinErrorCount.
const ErrorRateMinErrorCount uint64 = 50

// ErrorRateBaselineFloor mirrors proposer.ErrorRateBaselineFloor.
const ErrorRateBaselineFloor = 0.0001

// ErrorRateCurrentWindowHours mirrors proposer.ErrorRateCurrentWindowHours.
const ErrorRateCurrentWindowHours = 24

// ErrorRateBaselineWindowHours mirrors proposer.ErrorRateBaselineWindowHours.
const ErrorRateBaselineWindowHours = 168

// ErrorRateStore is the storage adapter the Scanner.errorRateStore
// field is typed against. Mirrors ColdStartStore for symmetry —
// the production wiring path provides a real *sqlite.Storage that
// satisfies both interfaces.
type ErrorRateStore interface {
	SaveErrorRateObservation(ctx context.Context, row sqlite.ErrorRateObservationRow) error
}

// ErrorRateDetectionResult captures the per-Lambda error-rate
// comparison. Mirrors proposer.ErrorRateDetectionResult in shape.
type ErrorRateDetectionResult struct {
	ResourceARN             string
	CurrentErrorCount       uint64
	CurrentInvocationCount  uint64
	CurrentErrorRate        float64
	BaselineErrorCount      uint64
	BaselineInvocationCount uint64
	BaselineErrorRate       float64
	ObservedAt              time.Time
}

// DetectErrorRate runs the per-Lambda error-rate comparison: 4
// metric queries (current invocations + errors, baseline
// invocations + errors). Returns the populated result; per-cloud
// scan integration code persists both windows.
func (s *Scanner) DetectErrorRate(
	ctx context.Context,
	resourceARN string,
) (ErrorRateDetectionResult, error) {
	currentWindow := time.Duration(ErrorRateCurrentWindowHours) * time.Hour
	baselineWindow := time.Duration(ErrorRateBaselineWindowHours) * time.Hour

	currInv, err := s.QueryAggregate(ctx, resourceARN, LambdaInvocationsMetricName, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current invocation count query: %w", err)
	}
	currErr, err := s.QueryAggregate(ctx, resourceARN, LambdaErrorsMetricName, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current error count query: %w", err)
	}
	baseInv, err := s.QueryAggregate(ctx, resourceARN, LambdaInvocationsMetricName, baselineWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: baseline invocation count query: %w", err)
	}
	baseErr, err := s.QueryAggregate(ctx, resourceARN, LambdaErrorsMetricName, baselineWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: baseline error count query: %w", err)
	}

	result := ErrorRateDetectionResult{
		ResourceARN:             resourceARN,
		CurrentInvocationCount:  posErrorRateUint64(currInv.Value),
		CurrentErrorCount:       posErrorRateUint64(currErr.Value),
		BaselineInvocationCount: posErrorRateUint64(baseInv.Value),
		BaselineErrorCount:      posErrorRateUint64(baseErr.Value),
		ObservedAt:              time.Now().UTC(),
	}
	if result.CurrentInvocationCount > 0 {
		result.CurrentErrorRate = float64(result.CurrentErrorCount) / float64(result.CurrentInvocationCount)
	}
	if result.BaselineInvocationCount > 0 {
		result.BaselineErrorRate = float64(result.BaselineErrorCount) / float64(result.BaselineInvocationCount)
	}
	return result, nil
}

func posErrorRateUint64(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}

// runErrorRateDetectionForServerless walks the Lambda-surface
// serverless snapshots in result.Serverless, runs the error-rate
// detection per row, and persists both the 24h current-window and
// 168h baseline-window observations to error_rate_observation.
// Called from Scan after scanRegionLambdaServerless has populated
// result.Serverless for the region (alongside the cold-start
// runColdStartDetectionForServerless call).
//
// Partial-scan posture: a per-function detection failure is
// logged into result.FailedServices (via recordPartialFailure
// with the sentinel "lambda_error_rate") but does NOT halt the
// per-row loop. Same posture scanRegionLambdaServerless +
// runColdStartDetectionForServerless use.
//
// Skips the entire branch when either s.cwClient or
// s.errorRateStore is nil, or when connectionID is empty.
func (s *Scanner) runErrorRateDetectionForServerless(ctx context.Context, result *scanner.Result) {
	if s.errorRateStore == nil || s.connectionID == "" {
		return
	}
	// Dormant path: no activation gate AND no directly-injected client ⇒
	// nothing to do. Two gates activate this native-metric detector: the
	// commercial gate (#152 productization, bundled with cold-start) and the
	// standalone serverless-metric-detection gate
	// (config.ServerlessMetricDetection.Enabled) — the latter exists because
	// Lambda Errors/Invocations are native AWS/Lambda metrics needing no paid
	// add-on, so error-rate can run without the commercial tier. Either gate
	// builds a per-region CloudWatch client on demand below; it still issues a
	// per-Lambda GetMetricStatistics call, which is why it is opt-in.
	if !s.commercialDetectors && !s.serverlessMetricDetection && s.cwClient == nil {
		return
	}
	for _, snap := range result.Serverless {
		if snap.Surface != lambdaServerlessSurface {
			continue
		}
		if snap.ResourceARN == "" {
			continue
		}
		if s.commercialDetectors || s.serverlessMetricDetection {
			cw, err := s.cloudWatchForRegion(ctx, regionFromARN(snap.ResourceARN))
			if err != nil {
				recordPartialFailure(result, "lambda_error_rate",
					fmt.Sprintf("error-rate CloudWatch client build failed for %s: %s",
						snap.ResourceARN, err.Error()))
				continue
			}
			s.cwClient = cw
		}
		detection, err := s.DetectErrorRate(ctx, snap.ResourceARN)
		if err != nil {
			recordPartialFailure(result, "lambda_error_rate",
				fmt.Sprintf("error-rate detection failed for %s: %s",
					snap.ResourceARN, err.Error()))
			continue
		}
		s.persistErrorRateObservation(ctx, snap, detection,
			ErrorRateCurrentWindowHours, result)
		s.persistErrorRateObservation(ctx, snap, detection,
			ErrorRateBaselineWindowHours, result)
	}
}

// persistErrorRateObservation marshals the detection result for
// the supplied window into an ErrorRateObservationRow and
// persists it via the chunk-1 storage adapter.
func (s *Scanner) persistErrorRateObservation(
	ctx context.Context,
	snap scanner.ServerlessInstanceSnapshot,
	detection ErrorRateDetectionResult,
	windowHours int,
	result *scanner.Result,
) {
	var errorCount, invocationCount int
	var errorRate float64
	if windowHours == ErrorRateCurrentWindowHours {
		errorCount = int(detection.CurrentErrorCount)
		invocationCount = int(detection.CurrentInvocationCount)
		errorRate = detection.CurrentErrorRate
	} else {
		errorCount = int(detection.BaselineErrorCount)
		invocationCount = int(detection.BaselineInvocationCount)
		errorRate = detection.BaselineErrorRate
	}
	snapshotJSON := marshalErrorRateSnapshot(detection, snap.Surface, windowHours,
		errorCount, invocationCount, errorRate)
	row := sqlite.ErrorRateObservationRow{
		ID:              uuid.NewString(),
		ConnectionID:    s.connectionID,
		Provider:        "aws",
		Surface:         lambdaServerlessSurface,
		AccountID:       snap.AccountID,
		Region:          snap.Region,
		ResourceARN:     snap.ResourceARN,
		ObservedAt:      detection.ObservedAt,
		WindowHours:     windowHours,
		ErrorCount:      errorCount,
		InvocationCount: invocationCount,
		ErrorRate:       errorRate,
		SnapshotJSON:    snapshotJSON,
	}
	if err := s.errorRateStore.SaveErrorRateObservation(ctx, row); err != nil {
		recordPartialFailure(result, "lambda_error_rate",
			fmt.Sprintf("persist error-rate observation for %s (window=%dh): %s",
				snap.ResourceARN, windowHours, err.Error()))
	}
}

// marshalErrorRateSnapshot serializes the per-window slice of the
// detection result for the snapshot_json column. The chunk-2
// per-resource error_rate API endpoint can return the raw shape
// without re-querying CloudWatch.
func marshalErrorRateSnapshot(
	detection ErrorRateDetectionResult,
	surface string,
	windowHours, errorCount, invocationCount int,
	errorRate float64,
) string {
	b, err := json.Marshal(map[string]any{
		"resource_arn":     detection.ResourceARN,
		"surface":          surface,
		"window_hours":     windowHours,
		"error_count":      errorCount,
		"invocation_count": invocationCount,
		"error_rate":       errorRate,
		"observed_at":      detection.ObservedAt,
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}

// WithErrorRateStore wires the error-rate observation storage
// adapter into the Scanner. v0.89.129. Mirrors WithColdStartStore.
func (s *Scanner) WithErrorRateStore(store ErrorRateStore) *Scanner {
	s.errorRateStore = store
	return s
}
