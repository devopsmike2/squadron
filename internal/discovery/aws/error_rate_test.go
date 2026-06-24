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

// error_rate_test.go — Error rate correlation slice 1 chunk 3
// (v0.89.129, #769 Stream 167). Pins
// runErrorRateDetectionForServerless against the §11 acceptance
// tests (per-cloud scan integration + non-fatal posture).

// cwSumFake returns deterministic Sum-statistic responses keyed by
// (resource_arn, metric_name, window_hours) so a single test can
// drive the 4-query sequence (current invocations + errors,
// baseline invocations + errors) of a single resource through
// different canned values.
type cwSumFake struct {
	mu        sync.Mutex
	responses map[string]map[string]map[time.Duration]float64
	errors    map[string]map[string]map[time.Duration]error
	calls     int
}

func (f *cwSumFake) GetMetricStatistics(
	_ context.Context,
	in *cloudwatch.GetMetricStatisticsInput,
	_ ...func(*cloudwatch.Options),
) (*cloudwatch.GetMetricStatisticsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if in.Dimensions == nil || len(in.Dimensions) == 0 || in.Dimensions[0].Value == nil {
		return &cloudwatch.GetMetricStatisticsOutput{}, nil
	}
	fnName := *in.Dimensions[0].Value
	metricName := ""
	if in.MetricName != nil {
		metricName = *in.MetricName
	}
	window := time.Duration(in.EndTime.Sub(*in.StartTime).Round(time.Hour))

	if errMap, ok := f.errors[fnName]; ok {
		if mm, ok := errMap[metricName]; ok {
			if e, ok := mm[window]; ok && e != nil {
				return nil, e
			}
		}
	}
	if m, ok := f.responses[fnName]; ok {
		if mm, ok := m[metricName]; ok {
			if v, ok := mm[window]; ok {
				return &cloudwatch.GetMetricStatisticsOutput{
					Datapoints: []cwtypes.Datapoint{{
						Sum:  awssdk.Float64(v),
						Unit: cwtypes.StandardUnitCount,
					}},
				}, nil
			}
		}
	}
	return &cloudwatch.GetMetricStatisticsOutput{}, nil
}

// recordingErrorRateStore captures persistence calls so the
// scan-integration tests can assert the rows produced.
type recordingErrorRateStore struct {
	mu   sync.Mutex
	rows []sqlite.ErrorRateObservationRow
	err  error
}

func (s *recordingErrorRateStore) SaveErrorRateObservation(_ context.Context, row sqlite.ErrorRateObservationRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.rows = append(s.rows, row)
	return nil
}

// TestAWSScan_RunsErrorRateDetection_PersistsObservations —
// integration-shaped: 1 Lambda gets 2 windows persisted (current +
// baseline).
func TestAWSScan_RunsErrorRateDetection_PersistsObservations(t *testing.T) {
	cw := &cwSumFake{
		responses: map[string]map[string]map[time.Duration]float64{
			"fn-fires": {
				LambdaInvocationsMetricName: {
					24 * time.Hour:  3000,
					168 * time.Hour: 22400,
				},
				LambdaErrorsMetricName: {
					24 * time.Hour:  90,
					168 * time.Hour: 224,
				},
			},
		},
	}
	store := &recordingErrorRateStore{}
	s := newMetricsTestScanner().
		WithCloudWatchClient(cw).
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithErrorRateStore(store).
		WithConnectionID("conn-test")
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    "aws",
			Surface:     "lambda",
			AccountID:   "123456789012",
			Region:      "us-east-1",
			ResourceARN: "arn:aws:lambda:us-east-1:123456789012:function:fn-fires",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if len(store.rows) != 2 {
		t.Fatalf("persisted rows = %d, want 2 (1 function x 2 windows)", len(store.rows))
	}
	// Verify one row per window with correct counts.
	var foundCurrent, foundBaseline bool
	for _, r := range store.rows {
		if r.ConnectionID != "conn-test" {
			t.Errorf("ConnectionID = %q, want conn-test", r.ConnectionID)
		}
		if r.Provider != "aws" || r.Surface != "lambda" {
			t.Errorf("provider/surface = %q/%q", r.Provider, r.Surface)
		}
		if r.SnapshotJSON == "" {
			t.Errorf("SnapshotJSON empty")
		}
		switch r.WindowHours {
		case ErrorRateCurrentWindowHours:
			foundCurrent = true
			if r.ErrorCount != 90 || r.InvocationCount != 3000 {
				t.Errorf("current window: errors=%d invs=%d, want 90/3000", r.ErrorCount, r.InvocationCount)
			}
		case ErrorRateBaselineWindowHours:
			foundBaseline = true
			if r.ErrorCount != 224 || r.InvocationCount != 22400 {
				t.Errorf("baseline window: errors=%d invs=%d, want 224/22400", r.ErrorCount, r.InvocationCount)
			}
		}
	}
	if !foundCurrent || !foundBaseline {
		t.Errorf("missing window rows: current=%v baseline=%v", foundCurrent, foundBaseline)
	}
}

// TestAWSScan_ErrorRateFailureIsNonFatal — when a CloudWatch
// metric query fails, the scan continues and records a partial
// failure (does NOT panic, does NOT halt).
func TestAWSScan_ErrorRateFailureIsNonFatal(t *testing.T) {
	cw := &cwSumFake{
		errors: map[string]map[string]map[time.Duration]error{
			"err-fn": {
				LambdaInvocationsMetricName: {
					24 * time.Hour: errors.New("cloudwatch quota exceeded"),
				},
			},
		},
	}
	store := &recordingErrorRateStore{}
	s := newMetricsTestScanner().
		WithCloudWatchClient(cw).
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithErrorRateStore(store).
		WithConnectionID("conn-test")
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{
			{Provider: "aws", Surface: "lambda", ResourceARN: "arn:aws:lambda:us-east-1:123:function:err-fn"},
			{Provider: "aws", Surface: "lambda", ResourceARN: "arn:aws:lambda:us-east-1:123:function:ok-fn"},
		},
	}
	// ok-fn returns zero (default) — that's fine, the loop continues
	// past it.
	s.runErrorRateDetectionForServerless(context.Background(), result)
	// Result.PartialReason / FailedServices should mention the
	// per-row failure.
	if result.PartialReason == "" && len(result.FailedServices) == 0 {
		t.Error("expected partial-failure record after metric query error; got none")
	}
}

// TestAWSScan_ErrorRate_NilStore_NoOp — when the store is nil, no
// CloudWatch calls are made.
func TestAWSScan_ErrorRate_NilStore_NoOp(t *testing.T) {
	cw := &cwSumFake{}
	s := newMetricsTestScanner().
		WithCloudWatchClient(cw).
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithConnectionID("conn-test")
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider: "aws", Surface: "lambda",
			ResourceARN: "arn:aws:lambda:us-east-1:123:function:fn",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if cw.calls != 0 {
		t.Errorf("CloudWatch calls = %d, want 0 with nil store", cw.calls)
	}
}

// TestAWSErrorRateConstants_PinValues — the AWS-package constants
// are pinned to the design doc §3 / §12 values: 2.0x ratio,
// 1000 invocations, 50 errors, 0.0001 baseline floor, 24h/168h
// windows. The proposer package owns the canonical values; this
// per-cloud package mirrors them so the cross-cloud uniform
// detection logic claim from design doc §11 holds without forcing
// an import cycle between the proposer and the scanner packages.
func TestAWSErrorRateConstants_PinValues(t *testing.T) {
	if ErrorRateRatioFloor != 2.0 {
		t.Errorf("ErrorRateRatioFloor = %v, want 2.0", ErrorRateRatioFloor)
	}
	if ErrorRateMinInvocationCount != 1000 {
		t.Errorf("ErrorRateMinInvocationCount = %v, want 1000", ErrorRateMinInvocationCount)
	}
	if ErrorRateMinErrorCount != 50 {
		t.Errorf("ErrorRateMinErrorCount = %v, want 50", ErrorRateMinErrorCount)
	}
	if ErrorRateBaselineFloor != 0.0001 {
		t.Errorf("ErrorRateBaselineFloor = %v, want 0.0001", ErrorRateBaselineFloor)
	}
	if ErrorRateCurrentWindowHours != 24 {
		t.Errorf("ErrorRateCurrentWindowHours = %v, want 24", ErrorRateCurrentWindowHours)
	}
	if ErrorRateBaselineWindowHours != 168 {
		t.Errorf("ErrorRateBaselineWindowHours = %v, want 168", ErrorRateBaselineWindowHours)
	}
}
