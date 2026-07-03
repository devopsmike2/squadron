// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// TestRegionFromARN covers the per-region routing the commercial detectors
// use to bind a CloudWatch client to each function's own region.
func TestRegionFromARN(t *testing.T) {
	cases := []struct {
		name string
		arn  string
		want string
	}{
		{"lambda us-east-1", "arn:aws:lambda:us-east-1:123456789012:function:checkout", "us-east-1"},
		{"lambda eu-west-2", "arn:aws:lambda:eu-west-2:123456789012:function:orders", "eu-west-2"},
		{"region-less (iam) → empty", "arn:aws:iam::123456789012:role/r", ""},
		{"malformed → empty", "not-an-arn", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := regionFromARN(tc.arn); got != tc.want {
				t.Errorf("regionFromARN(%q) = %q, want %q", tc.arn, got, tc.want)
			}
		})
	}
}

// fakeObsStore satisfies ColdStartStore + ErrorRateStore for the activation test.
type fakeObsStore struct{}

func (fakeObsStore) SaveColdStartObservation(_ context.Context, _ sqlite.ColdStartObservationRow) error {
	return nil
}
func (fakeObsStore) LatestColdStartObservation(_ context.Context, _ string, _ string, _ int) (sqlite.ColdStartObservationRow, bool, error) {
	return sqlite.ColdStartObservationRow{}, false, nil
}
func (fakeObsStore) SaveErrorRateObservation(_ context.Context, _ sqlite.ErrorRateObservationRow) error {
	return nil
}

// TestEnableCommercialDetectors covers that the activation method flips the
// gate on and wires both observation stores — the contract the scan
// orchestrator relies on.
func TestEnableCommercialDetectors(t *testing.T) {
	s := (&Scanner{}).EnableCommercialDetectors(fakeObsStore{}, fakeObsStore{})
	if !s.commercialDetectors {
		t.Error("commercialDetectors should be true after EnableCommercialDetectors")
	}
	if s.coldStartStore == nil {
		t.Error("coldStartStore should be wired")
	}
	if s.errorRateStore == nil {
		t.Error("errorRateStore should be wired")
	}
}
