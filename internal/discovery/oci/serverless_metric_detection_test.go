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

// TestScanServerlessTier_SkippedWithoutMonitoringClient confirms the
// OSS default: with no monitoring client wired (flag off), the
// serverless tier is a no-op — no Functions walk, no detection, no
// metric reads — so a stock scan is unchanged.
func TestScanServerlessTier_SkippedWithoutMonitoringClient(t *testing.T) {
	const rootCompartment = "ocid1.tenancy.oc1..aaa"

	fake := newFakeOCIFunctions()
	fake.ApplicationsByCompartment[rootCompartment] = []ociApplication{
		makeOCIApplication("prod-app"),
	}
	fake.FunctionsByApplication["ocid1.fnapp.oc1...prod-app"] = []ociFunction{
		makeOCIFunction("checkout", "iad.ocir.io/team/checkout:v1", nil),
	}

	s := newFunctionsScannerWithFake(t, fake, "us-phoenix-1")
	// No WithMonitoringClient — monitoringClient stays nil.

	result := &scanner.Result{}
	s.scanServerlessTier(context.Background(),
		[]ociCompartment{{ID: rootCompartment, Name: "root", LifecycleState: "ACTIVE"}},
		result)

	assert.Empty(t, result.Serverless,
		"serverless tier must be skipped when no monitoring client is wired (flag off)")
	assert.Equal(t, 0, fake.ApplicationsCalls,
		"no Applications walk should happen when the tier is gated off")
}
