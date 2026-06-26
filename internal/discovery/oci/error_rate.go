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

// error_rate.go — Error rate correlation slice 1 chunk 3
// (v0.89.129, #769 Stream 167). Sibling of cold_start.go for the
// OCI Functions surface. Mirrors AWS / GCP / Azure chunk-3
// pattern; thresholds byte-identical to the proposer constants
// per design doc §11 cross-cloud uniform detection logic.
//
// Like the OCI cold_start chunk, runErrorRateDetectionForServerless
// is exposed as a method on Scanner and ready for invocation by a
// future Scan() lifecycle wiring (cold-start arc deferred the
// Scan() integration here too).

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

// ErrorRateDetectionResult captures the per-OCI Functions
// error-rate comparison.
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

// DetectErrorRate runs 4 metric queries against the OCI Monitoring
// substrate — current 24h invocations + errors, baseline 168h
// invocations + errors.
func (s *Scanner) DetectErrorRate(
	ctx context.Context,
	resourceARN string,
) (ErrorRateDetectionResult, error) {
	currentWindow := time.Duration(ErrorRateCurrentWindowHours) * time.Hour
	baselineWindow := time.Duration(ErrorRateBaselineWindowHours) * time.Hour

	currInv, err := s.QueryAggregate(ctx, resourceARN, OCIFunctionsInvocationCountMetric, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current invocation count query: %w", err)
	}
	currErr, err := s.QueryAggregate(ctx, resourceARN, OCIFunctionsErrorResponseCountMetric, currentWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: current error count query: %w", err)
	}
	baseInv, err := s.QueryAggregate(ctx, resourceARN, OCIFunctionsInvocationCountMetric, baselineWindow, scanner.StatisticSum)
	if err != nil {
		return ErrorRateDetectionResult{}, fmt.Errorf("error rate: baseline invocation count query: %w", err)
	}
	baseErr, err := s.QueryAggregate(ctx, resourceARN, OCIFunctionsErrorResponseCountMetric, baselineWindow, scanner.StatisticSum)
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

// runErrorRateDetectionForServerless walks the OCI Functions-
// surface serverless snapshots in result.Serverless, runs the
// error-rate detection per row, and persists both windows.
//
// Partial-scan posture: per-row failures land in FailedServices
// under "ocifunc_error_rate" and continue.
//
// Skipped when monitoringClient / errorRateStore / connectionID
// is unset — same nil-tolerant gates as
// runColdStartDetectionForServerless.
func (s *Scanner) runErrorRateDetectionForServerless(ctx context.Context, result *scanner.Result) {
	if s.monitoringClient == nil || s.errorRateStore == nil || s.connectionID == "" {
		return
	}
	for _, snap := range result.Serverless {
		if snap.Surface != ocifuncSurface {
			continue
		}
		if snap.ResourceARN == "" {
			continue
		}
		detection, err := s.DetectErrorRate(ctx, snap.ResourceARN)
		if err != nil {
			recordPartialFailure(result, "ocifunc_error_rate",
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
		"surface":          ocifuncSurface,
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
		Provider:        "oci",
		Surface:         ocifuncSurface,
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
		recordPartialFailure(result, "ocifunc_error_rate",
			fmt.Sprintf("persist error-rate observation for %s (window=%dh): %s",
				snap.ResourceARN, windowHours, err.Error()))
	}
}

// WithErrorRateStore wires the error-rate observation storage
// adapter into the Scanner. v0.89.129. Mirrors WithColdStartStore.
func (s *Scanner) WithErrorRateStore(store ErrorRateStore) *Scanner {
	s.errorRateStore = store
	return s
}
