// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

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
// (v0.89.129, #769 Stream 167). Pins the OCI
// runErrorRateDetectionForServerless against per-resource
// persistence + non-fatal posture.

// recordingErrorRateStore captures persistence calls.
type recordingErrorRateStore struct {
	mu   sync.Mutex
	rows []sqlite.ErrorRateObservationRow
}

func (s *recordingErrorRateStore) SaveErrorRateObservation(_ context.Context, row sqlite.ErrorRateObservationRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
	return nil
}

// TestOCIScan_RunsErrorRateDetection_PersistsObservations — 1 OCI
// Functions row produces 2 persisted observations.
func TestOCIScan_RunsErrorRateDetection_PersistsObservations(t *testing.T) {
	now := time.Now().UTC()
	mf := &monitoringFake{
		respondWith: []ociMetricDataPoint{{Timestamp: now, Value: 500}},
	}
	s := newMetricsTestScanner(t, mf)
	store := &recordingErrorRateStore{}
	s.WithErrorRateStore(store)
	s.connectionID = "conn-test"
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    "oci",
			Surface:     ocifuncSurface,
			AccountID:   "ocid1.tenancy.oc1..aaa",
			Region:      "us-phoenix-1",
			ResourceARN: "ocid1.fnfunc.oc1..xxx",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if len(store.rows) != 2 {
		t.Fatalf("persisted rows = %d, want 2 (1 function x 2 windows)", len(store.rows))
	}
	for _, r := range store.rows {
		if r.ConnectionID != "conn-test" {
			t.Errorf("ConnectionID = %q, want conn-test", r.ConnectionID)
		}
		if r.Provider != "oci" || r.Surface != ocifuncSurface {
			t.Errorf("provider/surface = %q/%q", r.Provider, r.Surface)
		}
	}
}

// TestOCIScan_ErrorRateFailureIsNonFatal — metric query errors
// degrade to partial failures and the loop continues.
func TestOCIScan_ErrorRateFailureIsNonFatal(t *testing.T) {
	mf := &monitoringFake{
		respondErr: errors.New("oci monitoring quota exceeded"),
	}
	s := newMetricsTestScanner(t, mf)
	store := &recordingErrorRateStore{}
	s.WithErrorRateStore(store)
	s.connectionID = "conn-test"
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    "oci",
			Surface:     ocifuncSurface,
			ResourceARN: "ocid1.fnfunc.oc1..xxx",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if result.PartialReason == "" && len(result.FailedServices) == 0 {
		t.Error("expected partial-failure record after metric query error; got none")
	}
}

// TestOCIScan_ErrorRate_NilStore_NoOp — nil store short-circuits.
func TestOCIScan_ErrorRate_NilStore_NoOp(t *testing.T) {
	mf := &monitoringFake{}
	s := newMetricsTestScanner(t, mf)
	s.connectionID = "conn-test"
	// NO WithErrorRateStore.
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    "oci",
			Surface:     ocifuncSurface,
			ResourceARN: "ocid1.fnfunc.oc1..xxx",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if mf.calls != 0 {
		t.Errorf("Monitoring calls = %d, want 0 with nil store", mf.calls)
	}
}

// TestOCIErrorRateConstants_PinValues — pin design doc values.
func TestOCIErrorRateConstants_PinValues(t *testing.T) {
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
