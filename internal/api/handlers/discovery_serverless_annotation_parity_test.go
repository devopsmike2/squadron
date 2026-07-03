// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// discovery_serverless_annotation_parity_test.go — serverless-annotation-
// parity arc. Pins that the GCP / OCI / Azure scan handlers carry the
// cold-start + error-rate observation stores (+ cold-start thresholds)
// the per-row annotation passes need, so their serverless inventory rows
// populate cold_start_p95_ms + current_error_rate like AWS does instead
// of rendering "—". The annotation helpers themselves are provider-
// agnostic and tested in inventory_{cold_start,error_rate}_test.go; this
// pins the handler wiring that was previously AWS-only.

// parityErrorRateStore is a minimal ErrorRateObservationStore returning a
// canned current-window observation so the annotation has data to project.
type parityErrorRateStore struct{}

func (parityErrorRateStore) LatestErrorRateObservation(
	_ context.Context, _ string, resourceARN string, _ int,
) (sqlite.ErrorRateObservationRow, bool, error) {
	return sqlite.ErrorRateObservationRow{
		ResourceARN:     resourceARN,
		ErrorCount:      60,
		InvocationCount: 2000,
		ErrorRate:       0.03, // 60/2000 — the annotation stamps this verbatim
	}, true, nil
}

func TestGCPHandler_CarriesColdStartAndErrorRateStores(t *testing.T) {
	h := (&DiscoveryGCPHandlers{}).
		WithGCPRegressionStores(nil, parityErrorRateStore{}, nil).
		WithGCPColdStartConstants(NewStaticColdStartDetectionConstants(24, 168, 1.5, 500.0))

	if h.errorRateStore == nil {
		t.Error("GCP handler must carry the error-rate store for the serverless annotation pass")
	}
	if h.coldStartConstants == nil {
		t.Error("GCP handler must carry cold-start thresholds so AnnotateServerlessWithColdStart can run")
	}

	// The error-rate store + a Cloud Functions row should annotate to a
	// non-nil CurrentErrorRate (3% > floor) — proving the wired store is
	// the same one the annotation reads.
	rows := []scanner.ServerlessInstanceSnapshot{{
		Surface:     "cloudfunc",
		ResourceARN: "projects/p/locations/us-central1/functions/checkout",
	}}
	AnnotateServerlessWithErrorRate(context.Background(), h.errorRateStore, rows, nil)
	if rows[0].CurrentErrorRate == nil {
		t.Fatal("a wired error-rate store must populate CurrentErrorRate on a cloudfunc row")
	}
	if *rows[0].CurrentErrorRate <= 0 {
		t.Errorf("CurrentErrorRate = %v, want the 60/2000 = 0.03 observation", *rows[0].CurrentErrorRate)
	}
}

func TestOCIHandler_CarriesColdStartAndErrorRateStores(t *testing.T) {
	h := (&DiscoveryOCIHandlers{}).
		WithOCIRegressionStores(nil, parityErrorRateStore{}, nil).
		WithOCIColdStartConstants(NewStaticColdStartDetectionConstants(24, 168, 1.5, 500.0))
	if h.errorRateStore == nil || h.coldStartConstants == nil {
		t.Fatal("OCI handler must carry the error-rate store + cold-start thresholds for the annotation pass")
	}
	rows := []scanner.ServerlessInstanceSnapshot{{Surface: "ocifunc", ResourceARN: "ocid1.fnfunc.oc1..checkout"}}
	AnnotateServerlessWithErrorRate(context.Background(), h.errorRateStore, rows, nil)
	if rows[0].CurrentErrorRate == nil {
		t.Error("a wired OCI error-rate store must populate CurrentErrorRate on an ocifunc row")
	}
}

func TestAzureHandler_CarriesColdStartAndErrorRateStores(t *testing.T) {
	h := (&DiscoveryAzureHandlers{}).
		WithAzureRegressionStores(nil, parityErrorRateStore{}, nil).
		WithAzureColdStartConstants(NewStaticColdStartDetectionConstants(24, 168, 1.5, 500.0))
	if h.errorRateStore == nil || h.coldStartConstants == nil {
		t.Fatal("Azure handler must carry the error-rate store + cold-start thresholds for the annotation pass")
	}
	rows := []scanner.ServerlessInstanceSnapshot{{Surface: "azfunc", ResourceARN: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/sites/checkout"}}
	AnnotateServerlessWithErrorRate(context.Background(), h.errorRateStore, rows, nil)
	if rows[0].CurrentErrorRate == nil {
		t.Error("a wired Azure error-rate store must populate CurrentErrorRate on an azfunc row")
	}
}
