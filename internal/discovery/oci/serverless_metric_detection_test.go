// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"sync"
	"testing"

	"golang.org/x/time/rate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// serverless_metric_detection_test.go — option 2 (#300 resolution),
// OCI slice. Pins scanServerlessTier: the Scan-folded OCI Functions
// walk + native-metric cold-start / error-rate detection passes, gated
// on the monitoring client being wired (config.ServerlessMetricDetection
// .Enabled, which OCIFactory honors).

// recordingColdStartStore captures cold-start persistence calls.
type recordingColdStartStore struct {
	mu   sync.Mutex
	rows []sqlite.ColdStartObservationRow
}

func (s *recordingColdStartStore) SaveColdStartObservation(_ context.Context, row sqlite.ColdStartObservationRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
	return nil
}

func (s *recordingColdStartStore) LatestColdStartObservation(_ context.Context, _ string, _ int) (sqlite.ColdStartObservationRow, bool, error) {
	return sqlite.ColdStartObservationRow{}, false, nil
}

// TestScanServerlessTier_DiscoversAndDetects proves the slice-2 fold:
// with the monitoring client wired (the flag-on state), Scan's
// serverless tier walks Functions (populating result.Serverless) AND
// runs the cold-start detector, persisting observations scoped to the
// connection.
func TestScanServerlessTier_DiscoversAndDetects(t *testing.T) {
	const rootCompartment = "ocid1.tenancy.oc1..aaa"

	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment[rootCompartment] = []ociApplication{
		makeOCIApplication("prod-app"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...prod-app"] = []ociFunction{
		makeOCIFunction("checkout", "iad.ocir.io/team/checkout:v1", nil),
	}

	// Monitoring fake: baseline (168h) + current (24h) duration data
	// with enough samples to clear ColdStartBaselineMinimumSamples (50)
	// so the detection produces a persistable observation.
	mon := newDetectionMockMonitoring()
	mon.setDuration("168h", 120.0, 60)
	mon.setDuration("24h", 360.0, 60)

	coldStore := &recordingColdStartStore{}
	errStore := &recordingErrorRateStore{}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1").
		WithMonitoringClient(mon).
		WithMonitoringRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		WithColdStartStore(coldStore).
		WithErrorRateStore(errStore).
		WithConnectionID("conn-oci-1")

	result := &scanner.Result{}
	s.scanServerlessTier(context.Background(),
		[]ociCompartment{{ID: rootCompartment, Name: "root", LifecycleState: "ACTIVE"}},
		result)

	require.Len(t, result.Serverless, 1, "the folded ScanServerless walk should discover the one Function")
	assert.Equal(t, "checkout", result.Serverless[0].ResourceName)

	coldStore.mu.Lock()
	gotRows := len(coldStore.rows)
	coldStore.mu.Unlock()
	assert.GreaterOrEqual(t, gotRows, 1,
		"cold-start detector should have run over the discovered Function and persisted an observation")
	if gotRows > 0 {
		assert.Equal(t, "conn-oci-1", coldStore.rows[0].ConnectionID,
			"persisted observation must be scoped to the connection id")
	}
}

// TestScanServerlessTier_DiscoversWithoutMonitoringClient pins #306:
// Functions inventory discovery is UNCONDITIONAL (an inventory tier like
// compute / database / OKE), so result.Serverless is populated even with
// the flag off (no monitoring client). Only the metric DETECTION passes
// are gated on the monitoring client — with it nil, the walk still runs
// but no per-resource metric reads happen.
func TestScanServerlessTier_DiscoversWithoutMonitoringClient(t *testing.T) {
	const rootCompartment = "ocid1.tenancy.oc1..aaa"

	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment[rootCompartment] = []ociApplication{
		makeOCIApplication("prod-app"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...prod-app"] = []ociFunction{
		makeOCIFunction("checkout", "iad.ocir.io/team/checkout:v1", nil),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	// No WithMonitoringClient — monitoringClient stays nil (flag off).

	result := &scanner.Result{}
	s.scanServerlessTier(context.Background(),
		[]ociCompartment{{ID: rootCompartment, Name: "root", LifecycleState: "ACTIVE"}},
		result)

	require.Len(t, result.Serverless, 1,
		"Functions inventory discovery must run even with the metric flag off (#306)")
	assert.Equal(t, "checkout", result.Serverless[0].ResourceName)
	assert.Greater(t, fake.ApplicationsCalls, 0,
		"the Applications walk should run as part of unconditional discovery")
}
