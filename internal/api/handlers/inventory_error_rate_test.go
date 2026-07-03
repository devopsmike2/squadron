// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/proposer"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// inventory_error_rate_test.go — Error rate correlation slice 1
// chunk 3 (v0.89.129, #769 Stream 167). Pins
// AnnotateServerlessWithErrorRate against §11 acceptance test 13.

// stubErrorRateStore — programmable ErrorRateObservationStore.
type stubErrorRateStore struct {
	rows map[string]map[int]sqlite.ErrorRateObservationRow
	err  error
}

func (s *stubErrorRateStore) LatestErrorRateObservation(
	_ context.Context,
	_ string,
	resourceARN string,
	windowHours int,
) (sqlite.ErrorRateObservationRow, bool, error) {
	if s.err != nil {
		return sqlite.ErrorRateObservationRow{}, false, s.err
	}
	if m, ok := s.rows[resourceARN]; ok {
		if r, ok := m[windowHours]; ok {
			return r, true, nil
		}
	}
	return sqlite.ErrorRateObservationRow{}, false, nil
}

// TestInventoryAnnotation_AddsCurrentErrorRateToServerlessRow —
// acceptance test 13. A Lambda row with a fired detection result
// (current 3% error rate over 3000 invocations + 90 errors versus
// baseline 1% rate over 22400 invocations + 224 errors) gets
// CurrentErrorRate + ErrorRateExceedsThreshold stamped, with
// ErrorRateExceedsThreshold=true.
func TestInventoryAnnotation_AddsCurrentErrorRateToServerlessRow(t *testing.T) {
	arn := "arn:aws:lambda:us-east-1:123:function:order-processor"
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "lambda", ResourceARN: arn},
	}
	store := &stubErrorRateStore{
		rows: map[string]map[int]sqlite.ErrorRateObservationRow{
			arn: {
				proposer.ErrorRateCurrentWindowHours: {
					ResourceARN:     arn,
					WindowHours:     proposer.ErrorRateCurrentWindowHours,
					ErrorCount:      90,
					InvocationCount: 3000,
					ErrorRate:       0.03,
				},
				proposer.ErrorRateBaselineWindowHours: {
					ResourceARN:     arn,
					WindowHours:     proposer.ErrorRateBaselineWindowHours,
					ErrorCount:      224,
					InvocationCount: 22400,
					ErrorRate:       0.01,
				},
			},
		},
	}
	AnnotateServerlessWithErrorRate(context.Background(), store, snapshots, nil)
	if snapshots[0].CurrentErrorRate == nil {
		t.Fatalf("CurrentErrorRate is nil; want populated")
	}
	if got := *snapshots[0].CurrentErrorRate; got < 0.029 || got > 0.031 {
		t.Errorf("CurrentErrorRate = %v, want ~0.03", got)
	}
	if snapshots[0].ErrorRateExceedsThreshold == nil {
		t.Fatalf("ErrorRateExceedsThreshold is nil; want populated")
	}
	if !*snapshots[0].ErrorRateExceedsThreshold {
		t.Error("ErrorRateExceedsThreshold = false, want true (3.0x ratio > 2.0x)")
	}
}

// TestInventoryAnnotation_NullWhenNoObservation — no current row
// means both pointers stay nil (UI renders "—").
func TestInventoryAnnotation_NullWhenNoObservation(t *testing.T) {
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "lambda", ResourceARN: "arn:none"},
	}
	store := &stubErrorRateStore{rows: map[string]map[int]sqlite.ErrorRateObservationRow{}}
	AnnotateServerlessWithErrorRate(context.Background(), store, snapshots, nil)
	if snapshots[0].CurrentErrorRate != nil {
		t.Errorf("CurrentErrorRate = %v, want nil for no-observation row", *snapshots[0].CurrentErrorRate)
	}
	if snapshots[0].ErrorRateExceedsThreshold != nil {
		t.Errorf("ErrorRateExceedsThreshold = %v, want nil", *snapshots[0].ErrorRateExceedsThreshold)
	}
}

// TestInventoryAnnotation_ExceedsThresholdReflectsAllGates —
// table-driven coverage of the three gates the threshold
// predicate enforces.
func TestInventoryAnnotation_ExceedsThresholdReflectsAllGates(t *testing.T) {
	arn := "arn:test"
	cases := []struct {
		name         string
		currentErr   int
		currentInv   int
		currentRate  float64
		baselineRate float64
		baselineOK   bool
		want         bool
	}{
		{"3x_ratio_3000_invocations_90_errors_FIRES", 90, 3000, 0.03, 0.01, true, true},
		{"1.9x_ratio_does_not_fire", 60, 3000, 0.02, 0.0106, true, false},
		{"3x_ratio_500_invocations_does_not_fire", 15, 500, 0.03, 0.01, true, false},
		{"3x_ratio_30_errors_does_not_fire", 30, 1000, 0.03, 0.01, true, false},
		{"near_zero_baseline_uses_floor", 60, 2000, 0.03, 0.000001, true, true},
		{"missing_baseline_uses_floor", 60, 2000, 0.03, 0.0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshots := []scanner.ServerlessInstanceSnapshot{
				{Provider: "aws", Surface: "lambda", ResourceARN: arn},
			}
			rows := map[int]sqlite.ErrorRateObservationRow{
				proposer.ErrorRateCurrentWindowHours: {
					ResourceARN: arn, WindowHours: proposer.ErrorRateCurrentWindowHours,
					ErrorCount: tc.currentErr, InvocationCount: tc.currentInv,
					ErrorRate: tc.currentRate,
				},
			}
			if tc.baselineOK {
				rows[proposer.ErrorRateBaselineWindowHours] = sqlite.ErrorRateObservationRow{
					ResourceARN: arn, WindowHours: proposer.ErrorRateBaselineWindowHours,
					ErrorCount: 100, InvocationCount: 10000, ErrorRate: tc.baselineRate,
				}
			}
			store := &stubErrorRateStore{rows: map[string]map[int]sqlite.ErrorRateObservationRow{arn: rows}}
			AnnotateServerlessWithErrorRate(context.Background(), store, snapshots, nil)
			if snapshots[0].ErrorRateExceedsThreshold == nil {
				t.Fatalf("ErrorRateExceedsThreshold nil; want %v", tc.want)
			}
			if got := *snapshots[0].ErrorRateExceedsThreshold; got != tc.want {
				t.Errorf("ErrorRateExceedsThreshold = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAnnotateServerlessWithErrorRate_HandlesAllFourProviders —
// all 5 surfaces (lambda / cloudrun / cloudfunc / azfunc / ocifunc)
// participate. The annotator should populate the fields for each.
func TestAnnotateServerlessWithErrorRate_HandlesAllFourProviders(t *testing.T) {
	surfaces := []struct {
		provider, surface, arn string
	}{
		{"aws", "lambda", "arn:aws:lambda:us-east-1:123:function:fn"},
		{"gcp", "cloudrun", "projects/p/locations/us-central1/services/svc"},
		{"gcp", "cloudfunc", "projects/p/locations/us-central1/functions/fn"},
		{"azure", "azfunc", "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/sites/site"},
		{"oci", "ocifunc", "ocid1.fnfunc.oc1..xxx"},
	}
	snapshots := make([]scanner.ServerlessInstanceSnapshot, 0, len(surfaces))
	rows := make(map[string]map[int]sqlite.ErrorRateObservationRow, len(surfaces))
	for _, s := range surfaces {
		snapshots = append(snapshots, scanner.ServerlessInstanceSnapshot{
			Provider: s.provider, Surface: s.surface, ResourceARN: s.arn,
		})
		rows[s.arn] = map[int]sqlite.ErrorRateObservationRow{
			proposer.ErrorRateCurrentWindowHours: {
				ResourceARN: s.arn, WindowHours: proposer.ErrorRateCurrentWindowHours,
				ErrorCount: 100, InvocationCount: 5000, ErrorRate: 0.02,
			},
			proposer.ErrorRateBaselineWindowHours: {
				ResourceARN: s.arn, WindowHours: proposer.ErrorRateBaselineWindowHours,
				ErrorCount: 100, InvocationCount: 20000, ErrorRate: 0.005,
			},
		}
	}
	store := &stubErrorRateStore{rows: rows}
	AnnotateServerlessWithErrorRate(context.Background(), store, snapshots, nil)
	for i, snap := range snapshots {
		if snap.CurrentErrorRate == nil {
			t.Errorf("snapshots[%d] (%s/%s): CurrentErrorRate nil; want populated",
				i, snap.Provider, snap.Surface)
			continue
		}
		if snap.ErrorRateExceedsThreshold == nil {
			t.Errorf("snapshots[%d] (%s/%s): ErrorRateExceedsThreshold nil",
				i, snap.Provider, snap.Surface)
		}
	}
}

// TestInventoryAnnotation_ErrorRate_NilStore_NoOp — nil store
// short-circuits.
func TestInventoryAnnotation_ErrorRate_NilStore_NoOp(t *testing.T) {
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "lambda", ResourceARN: "arn:x"},
	}
	AnnotateServerlessWithErrorRate(context.Background(), nil, snapshots, nil)
	if snapshots[0].CurrentErrorRate != nil {
		t.Error("CurrentErrorRate populated on nil store path; want nil")
	}
}

// TestInventoryAnnotation_ErrorRate_NonAnnotatableSurface_Skips —
// non-serverless surfaces skip.
func TestInventoryAnnotation_ErrorRate_NonAnnotatableSurface_Skips(t *testing.T) {
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "ec2", ResourceARN: "arn:x"},
	}
	store := &stubErrorRateStore{rows: map[string]map[int]sqlite.ErrorRateObservationRow{
		"arn:x": {
			proposer.ErrorRateCurrentWindowHours: {ErrorRate: 0.5},
		},
	}}
	AnnotateServerlessWithErrorRate(context.Background(), store, snapshots, nil)
	if snapshots[0].CurrentErrorRate != nil {
		t.Error("CurrentErrorRate populated on non-annotatable surface; want nil")
	}
}

// TestInventoryAnnotation_ErrorRate_StoreError_DegradesRowAndContinues
// — a store error on the current lookup degrades the row to nil
// pointers, but subsequent rows continue to be annotated.
func TestInventoryAnnotation_ErrorRate_StoreError_DegradesRowAndContinues(t *testing.T) {
	snapshots := []scanner.ServerlessInstanceSnapshot{
		{Provider: "aws", Surface: "lambda", ResourceARN: "arn:err"},
	}
	store := &stubErrorRateStore{err: errors.New("boom")}
	AnnotateServerlessWithErrorRate(context.Background(), store, snapshots, nil)
	if snapshots[0].CurrentErrorRate != nil {
		t.Error("CurrentErrorRate populated on store error; want nil")
	}
}

// TestIsErrorRateAnnotatableSurface — pin the 5-surface set.
func TestIsErrorRateAnnotatableSurface(t *testing.T) {
	for _, s := range []string{"lambda", "cloudrun", "cloudfunc", "azfunc", "ocifunc"} {
		if !isErrorRateAnnotatableSurface(s) {
			t.Errorf("isErrorRateAnnotatableSurface(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "ec2", "stepfunc", "unknown"} {
		if isErrorRateAnnotatableSurface(s) {
			t.Errorf("isErrorRateAnnotatableSurface(%q) = true, want false", s)
		}
	}
}
