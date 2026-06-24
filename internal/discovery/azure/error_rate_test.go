// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"sync"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// error_rate_test.go — Error rate correlation slice 1 chunk 3
// (v0.89.129, #769 Stream 167). Lightweight pin against the Azure
// runErrorRateDetectionForServerless nil-gate posture and
// constants. The full per-window metric integration test mirrors
// the cold-start chunk's perWindowMetricsFake harness; given that
// the Azure scan() lifecycle does not yet call
// runColdStartDetectionForServerless either, the slice 1 chunk 3
// test surface here covers the wiring symmetry only.

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

// TestAzureScan_ErrorRate_NoOpWhenTokenMissing — the nil-token
// guard short-circuits the helper without any Azure Monitor
// calls. The Azure scanner's scan() lifecycle does not yet
// integrate this branch; this test pins the safe nil-tolerant
// posture so a future scan-integration patch can land without
// surprising the slice 1 contract.
func TestAzureScan_ErrorRate_NoOpWhenTokenMissing(t *testing.T) {
	s := &Scanner{}
	store := &recordingErrorRateStore{}
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider: "azure", Surface: azureFunctionsServerlessSurface,
			ResourceARN: "/subscriptions/x/resourceGroups/rg/providers/Microsoft.Web/sites/site",
		}},
	}
	s.RunErrorRateDetectionForServerless(context.Background(), store, "conn-test", result)
	if len(store.rows) != 0 {
		t.Errorf("persisted rows = %d, want 0 when accessToken is empty", len(store.rows))
	}
}

// TestAzureScan_ErrorRate_NilStore_NoOp — nil store short-circuits.
func TestAzureScan_ErrorRate_NilStore_NoOp(t *testing.T) {
	s := &Scanner{accessToken: "fake-token"}
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider: "azure", Surface: azureFunctionsServerlessSurface,
			ResourceARN: "/subscriptions/x/resourceGroups/rg/providers/Microsoft.Web/sites/site",
		}},
	}
	s.RunErrorRateDetectionForServerless(context.Background(), nil, "conn-test", result)
	// Reach this point without panic; nothing to assert beyond non-crash.
	_ = result
}

// TestAzureErrorRateConstants_PinValues — pin the design doc
// constants.
func TestAzureErrorRateConstants_PinValues(t *testing.T) {
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
