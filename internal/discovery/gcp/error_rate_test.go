// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// error_rate_test.go — Error rate correlation slice 1 chunk 3
// (v0.89.129, #769 Stream 167). Pins the GCP
// runErrorRateDetectionForServerless against the per-cloud scan
// integration acceptance test (per-resource persistence + non-fatal
// posture).

// recordingErrorRateStore captures persistence calls.
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

// TestGCPScan_RunsErrorRateDetection_PersistsObservations —
// per-cloud scan integration: 1 Cloud Run row produces 2 persisted
// observations (current + baseline). Non-zero responses come from
// the metricsFake's respondWith slice; both invocations + errors
// flow through the same fake.
func TestGCPScan_RunsErrorRateDetection_PersistsObservations(t *testing.T) {
	now := time.Now().UTC()
	// Each QueryAggregate call returns a single-point response
	// summing to non-zero — that's enough for the persistence row
	// to carry through. The fake doesn't discriminate per-call so
	// all 4 queries (current/baseline x inv/err) get the same
	// canned response, and the resulting row counts are equal but
	// the row still proves the persistence wiring works.
	f := &metricsFake{
		respondWith: []TimeSeriesPoint{
			{Value: 1500.0, SampleCount: 12, StartTime: now.Add(-30 * time.Minute), EndTime: now},
		},
	}
	s := newMetricsTestScannerWithFake(t, f)
	store := &recordingErrorRateStore{}
	s.WithErrorRateStore(store)
	s.connectionID = "conn-test"
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    ProviderGCP,
			Surface:     cloudRunServerlessSurface,
			AccountID:   "test-project",
			Region:      "us-central1",
			ResourceARN: "projects/test-project/locations/us-central1/services/svc",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if len(store.rows) != 2 {
		t.Fatalf("persisted rows = %d, want 2 (1 service x 2 windows)", len(store.rows))
	}
	for _, r := range store.rows {
		if r.ConnectionID != "conn-test" {
			t.Errorf("ConnectionID = %q, want conn-test", r.ConnectionID)
		}
		if r.Provider != ProviderGCP {
			t.Errorf("provider = %q, want %q", r.Provider, ProviderGCP)
		}
		if r.SnapshotJSON == "" {
			t.Errorf("SnapshotJSON empty")
		}
	}
}

// TestGCPScan_ErrorRateFailureIsNonFatal — when QueryAggregate
// errors, the scan continues and a partial-failure record is
// captured.
func TestGCPScan_ErrorRateFailureIsNonFatal(t *testing.T) {
	f := &metricsFake{
		respondErr: errors.New("cloud monitoring quota exceeded"),
	}
	s := newMetricsTestScannerWithFake(t, f)
	store := &recordingErrorRateStore{}
	s.WithErrorRateStore(store)
	s.connectionID = "conn-test"
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    ProviderGCP,
			Surface:     cloudRunServerlessSurface,
			ResourceARN: "projects/test-project/locations/us-central1/services/svc",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if result.PartialReason == "" && len(result.FailedServices) == 0 {
		t.Error("expected partial-failure record after metric query error; got none")
	}
	if len(store.rows) != 0 {
		t.Errorf("persisted rows = %d, want 0 when detection failed", len(store.rows))
	}
}

// TestGCPScan_ErrorRate_NilStore_NoOp — nil store short-circuits.
func TestGCPScan_ErrorRate_NilStore_NoOp(t *testing.T) {
	f := &metricsFake{respondWith: []TimeSeriesPoint{{Value: 100}}}
	s := newMetricsTestScannerWithFake(t, f)
	s.connectionID = "conn-test"
	// NO WithErrorRateStore.
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    ProviderGCP,
			Surface:     cloudRunServerlessSurface,
			ResourceARN: "projects/p/locations/us-central1/services/svc",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if f.calls != 0 {
		t.Errorf("Cloud Monitoring calls = %d, want 0 with nil store", f.calls)
	}
}

// TestGCPErrorRateConstants_PinValues — design doc §3 + §12
// constants.
func TestGCPErrorRateConstants_PinValues(t *testing.T) {
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
}
