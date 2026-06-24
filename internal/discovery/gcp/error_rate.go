// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Error rate correlation slice 1 chunk 3 (v0.89.129, #769 Stream
// 167) — GCP-side per-resource error-rate detection for the Cloud
// Run + Cloud Functions surfaces. Mirrors the AWS slice 1 chunk 3
// detection branch on the thresholds (2.0x ratio + 1000 invocation
// minimum + 50 error minimum + 0.0001 baseline floor) byte-
// identically per design doc §11 acceptance test 11 (cross-cloud
// uniform detection logic); the per-surface variant just swaps
// the metric source.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

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
// error_rate_observation rows. Mirrors AWS scanner's ErrorRateStore.
type ErrorRateStore interface {
	SaveErrorRateObservation(ctx context.Context, row sqlite.ErrorRateObservationRow) error
}

// ErrorRateDetectionResult captures the per-resource error-rate
// comparison for a Cloud Run service or Cloud Function.
type ErrorRateDetectionResult struct {
	ResourceARN             string
	Surface                 string
	CurrentErrorCount       uint64
	CurrentInvocationCount  uint64
	CurrentErrorRate        float64
	BaselineErrorCount      uint64
	BaselineInvocationCount uint64
	BaselineErrorRate       float64
	ObservedAt              time.Time
}

// DetectErrorRate runs the per-resource comparison: 4 metric
// queries (current 24h invocations + errors, baseline 168h
// invocations + errors). Surface selects the per-cloud metric
// source.
func (s *Scanner) DetectErrorRate(
	ctx context.Context,
	resourceARN, surface string,
) (ErrorRateDetectionResult, error) {
	invMetric, errMetric, ok := errorRateMetricsFor(surface)
	if !ok {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: unsupported surface: %q", surface)
	}
	currentWindow := time.Duration(ErrorRateCurrentWindowHours) * time.Hour
	baselineWindow := time.Duration(ErrorRateBaselineWindowHours) * time.Hour

	currInv, err := s.QueryAggregate(ctx, resourceARN, invMetric, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current invocation count query: %w", err)
	}
	currErr, err := s.QueryAggregate(ctx, resourceARN, errMetric, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current error count query: %w", err)
	}
	baseInv, err := s.QueryAggregate(ctx, resourceARN, invMetric, baselineWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: baseline invocation count query: %w", err)
	}
	baseErr, err := s.QueryAggregate(ctx, resourceARN, errMetric, baselineWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: baseline error count query: %w", err)
	}

	result := ErrorRateDetectionResult{
		ResourceARN:             resourceARN,
		Surface:                 surface,
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

func errorRateMetricsFor(surface string) (inv, errMetric string, ok bool) {
	switch surface {
	case cloudRunServerlessSurface:
		return CloudRunRequestCountMetricType, CloudRunRequestCount5xxMetricType, true
	case cloudFuncServerlessSurface:
		return CloudFunctionsExecutionCountMetricType, CloudFunctionsExecutionCountErrorMetricType, true
	}
	return "", "", false
}

// runErrorRateDetectionForServerless walks the Cloud Run +
// Cloud Functions snapshots in result.Serverless, runs the
// error-rate detection per row, and persists both windows.
//
// Partial-scan posture: a per-resource detection failure is logged
// into FailedServices with the per-surface identifier
// ("cloudrun_error_rate" / "cloudfunc_error_rate") but does NOT
// halt the per-row loop.
func (s *Scanner) runErrorRateDetectionForServerless(ctx context.Context, result *scanner.Result) {
	if s.metricsClient == nil || s.errorRateStore == nil || s.connectionID == "" {
		return
	}
	for _, snap := range result.Serverless {
		if snap.Provider != ProviderGCP {
			continue
		}
		if snap.ResourceARN == "" {
			continue
		}
		var surface string
		switch snap.Surface {
		case cloudRunServerlessSurface:
			surface = cloudRunServerlessSurface
		case cloudFuncServerlessSurface:
			surface = cloudFuncServerlessSurface
		default:
			continue
		}
		detection, err := s.DetectErrorRate(ctx, snap.ResourceARN, surface)
		if err != nil {
			recordPartialFailure(result, surface+"_error_rate",
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
	snapshotJSON, err := json.Marshal(map[string]any{
		"resource_arn":     detection.ResourceARN,
		"surface":          detection.Surface,
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
		ConnectionID:    s.connectionID,
		Provider:        ProviderGCP,
		Surface:         detection.Surface,
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
	if err := s.errorRateStore.SaveErrorRateObservation(ctx, row); err != nil {
		recordPartialFailure(result, detection.Surface+"_error_rate",
			fmt.Sprintf("persist error-rate observation for %s (window=%dh): %s",
				snap.ResourceARN, windowHours, err.Error()))
	}
}
