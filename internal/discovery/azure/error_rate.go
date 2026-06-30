// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

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
// (v0.89.129, #769 Stream 167). Mirrors the AWS / GCP / OCI
// chunk-3 pattern for the Azure Functions surface. Threshold
// constants are byte-identical to the proposer package's per
// design doc §11 (cross-cloud uniform detection logic).
//
// Wire shape parity: per-row DetectErrorRate + the
// runErrorRateDetectionForServerless helper + the WithErrorRateStore
// setter mirror the cold-start chunk pattern. The Azure scanner's
// Scan() lifecycle does NOT yet call runColdStartDetectionForServerless
// (cold-start arc deferred this wiring); the error-rate branch
// matches that posture — the helper is callable directly by tests
// and by future scan wiring, but Scan() is not modified here so
// the cross-cloud cold-start vs error-rate symmetry holds.

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

// ErrorRateStore is the storage adapter for persisting
// error_rate_observation rows.
type ErrorRateStore interface {
	SaveErrorRateObservation(ctx context.Context, row sqlite.ErrorRateObservationRow) error
}

// ErrorRateDetectionResult captures the per-Function App error-rate
// comparison.
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

// DetectErrorRate runs 4 metric queries (current 24h invocations +
// errors, baseline 168h invocations + errors) against the Azure
// Monitor MetricQuerier substrate.
func (s *Scanner) DetectErrorRate(
	ctx context.Context,
	resourceARN string,
) (ErrorRateDetectionResult, error) {
	currentWindow := time.Duration(ErrorRateCurrentWindowHours) * time.Hour
	baselineWindow := time.Duration(ErrorRateBaselineWindowHours) * time.Hour

	// #153 enterprise-gate: OSS reads FunctionInvocations/FunctionErrors
	// (no native Functions error metric ⇒ empty ⇒ never fires); the
	// commercial gate reads App Insights requests/count + requests/failed.
	totalMetric := s.errorTotalMetric()
	failedMetric := s.errorFailedMetric()
	currInv, err := s.QueryAggregate(ctx, resourceARN, totalMetric, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current invocation count query: %w", err)
	}
	currErr, err := s.QueryAggregate(ctx, resourceARN, failedMetric, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current error count query: %w", err)
	}
	baseInv, err := s.QueryAggregate(ctx, resourceARN, totalMetric, baselineWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: baseline invocation count query: %w", err)
	}
	baseErr, err := s.QueryAggregate(ctx, resourceARN, failedMetric, baselineWindow, scanner.StatisticSum)
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

// RunErrorRateDetectionForServerless walks the Azure Functions
// snapshots in result.Serverless, runs the error-rate detection
// per row, and persists both windows to error_rate_observation.
// Designed to be called from the scanner's Scan() lifecycle when
// the cold-start wiring lands; currently exported (capital R) so
// tests can drive the integration directly without Scan().
//
// Partial-scan posture: per-row failures land in FailedServices
// under "azfunc_error_rate" and continue.
//
// errorRateStore + accessToken must both be wired (the accessToken
// gates Azure Monitor reachability — same posture as the
// QueryAggregate substrate). Empty connectionID is allowed here
// because the Azure scanner does not currently carry a connectionID
// field (the row's ConnectionID column stays empty for now —
// chunk 4 may align this once Azure scan integration lands).
func (s *Scanner) RunErrorRateDetectionForServerless(
	ctx context.Context,
	store ErrorRateStore,
	connectionID string,
	result *scanner.Result,
) {
	if store == nil || s.accessToken == "" {
		return
	}
	for _, snap := range result.Serverless {
		if snap.Surface != azureFunctionsServerlessSurface {
			continue
		}
		if snap.ResourceARN == "" {
			continue
		}
		detection, err := s.DetectErrorRate(ctx, snap.ResourceARN)
		if err != nil {
			recordPartialFailure(result, "azfunc_error_rate",
				fmt.Sprintf("error-rate detection failed for %s: %s",
					snap.ResourceARN, err.Error()))
			continue
		}
		persistAzureErrorRateObservation(ctx, store, connectionID, snap, detection,
			ErrorRateCurrentWindowHours, result)
		persistAzureErrorRateObservation(ctx, store, connectionID, snap, detection,
			ErrorRateBaselineWindowHours, result)
	}
}

func persistAzureErrorRateObservation(
	ctx context.Context,
	store ErrorRateStore,
	connectionID string,
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
	snapshotJSON, err := json.Marshal(map[string]any{
		"resource_arn":     detection.ResourceARN,
		"surface":          azureFunctionsServerlessSurface,
		"window_hours":     windowHours,
		"error_count":      errorCount,
		"invocation_count": invocationCount,
		"error_rate":       errorRate,
		"observed_at":      detection.ObservedAt,
	})
	if err != nil {
		snapshotJSON = []byte("{}")
	}
	row := sqlite.ErrorRateObservationRow{
		ID:              uuid.NewString(),
		ConnectionID:    connectionID,
		Provider:        "azure",
		Surface:         azureFunctionsServerlessSurface,
		AccountID:       snap.AccountID,
		Region:          snap.Region,
		ResourceARN:     snap.ResourceARN,
		ObservedAt:      detection.ObservedAt,
		WindowHours:     windowHours,
		ErrorCount:      errorCount,
		InvocationCount: invocationCount,
		ErrorRate:       errorRate,
		SnapshotJSON:    string(snapshotJSON),
	}
	if err := store.SaveErrorRateObservation(ctx, row); err != nil {
		recordPartialFailure(result, "azfunc_error_rate",
			fmt.Sprintf("persist error-rate observation for %s (window=%dh): %s",
				snap.ResourceARN, windowHours, err.Error()))
	}
}
