// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// serverless_metric_detection_test.go — option 2 (#300 resolution):
// the native-metric serverless detection gate that runs Lambda
// error-rate against the native AWS/Lambda Errors + Invocations
// metrics WITHOUT the commercial (Lambda Insights) add-on. Pins the
// EnableServerlessMetricDetection wiring contract + the decoupled
// run-loop gate.

// cwBuildFactory embeds the test fakeFactory and adds the CloudWatch
// builder method so it satisfies cloudWatchBuilder — letting the
// per-region client-build branch in runErrorRateDetectionForServerless
// resolve a client the same way the commercial path does, but driven
// by the serverless-metric-detection flag instead.
type cwBuildFactory struct {
	fakeFactory
	cw CloudWatchClient
}

func (f *cwBuildFactory) CloudWatch(_ context.Context, _ string) (CloudWatchClient, error) {
	return f.cw, nil
}

// TestEnableServerlessMetricDetection pins the activation contract the
// scan orchestrator relies on: the native-metric gate flips on and the
// error-rate store is wired, but the cold-start store stays nil — so
// cold-start (which needs the Lambda Insights add-on) cannot fire under
// this flag.
func TestEnableServerlessMetricDetection(t *testing.T) {
	s := (&Scanner{}).EnableServerlessMetricDetection(fakeObsStore{})
	if !s.serverlessMetricDetection {
		t.Error("serverlessMetricDetection should be true after EnableServerlessMetricDetection")
	}
	if s.errorRateStore == nil {
		t.Error("errorRateStore should be wired")
	}
	if s.coldStartStore != nil {
		t.Error("coldStartStore must stay nil — cold-start needs the commercial add-on, not this flag")
	}
	if s.commercialDetectors {
		t.Error("commercialDetectors must stay false — the native-metric flag is independent")
	}
}

// TestServerlessMetricDetection_RunsErrorRateWithoutCommercial proves
// the decouple: with the native-metric flag on (and NO commercial
// gate), the run loop builds a per-region CloudWatch client via the
// factory and persists both error-rate windows from the native
// AWS/Lambda metrics.
func TestServerlessMetricDetection_RunsErrorRateWithoutCommercial(t *testing.T) {
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
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1)).
		EnableServerlessMetricDetection(store).
		WithConnectionID("conn-test")
	// Per-region build seam — no commercial gate, native metric.
	s.factory = &cwBuildFactory{cw: cw}
	if s.commercialDetectors {
		t.Fatal("precondition: commercialDetectors must be false for this test")
	}

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
		t.Fatalf("persisted rows = %d, want 2 (1 function x 2 windows) — native error-rate should run without the commercial gate", len(store.rows))
	}
	// Both windows must be attributed to the connection + the native
	// Lambda surface, with the current-window counts carried through.
	var foundCurrent bool
	for _, row := range store.rows {
		if row.ConnectionID != "conn-test" || row.Provider != "aws" {
			t.Errorf("row mis-scoped: conn=%q provider=%q", row.ConnectionID, row.Provider)
		}
		if row.WindowHours == ErrorRateCurrentWindowHours {
			foundCurrent = true
			if row.ErrorCount != 90 || row.InvocationCount != 3000 {
				t.Errorf("current window counts = (err %d, inv %d), want (90, 3000)", row.ErrorCount, row.InvocationCount)
			}
		}
	}
	if !foundCurrent {
		t.Error("no current-window (24h) error-rate row persisted")
	}
}

// TestServerlessMetricDetection_DormantWhenOff confirms the OSS default
// stays dormant: with neither gate set and no injected client, the
// run loop early-returns and persists nothing (zero metric reads).
func TestServerlessMetricDetection_DormantWhenOff(t *testing.T) {
	store := &recordingErrorRateStore{}
	s := newMetricsTestScanner().
		WithErrorRateStore(store).
		WithConnectionID("conn-test")
	// No commercial gate, no serverless-metric gate, no cwClient.
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    "aws",
			Surface:     "lambda",
			AccountID:   "123456789012",
			Region:      "us-east-1",
			ResourceARN: "arn:aws:lambda:us-east-1:123456789012:function:fn-x",
		}},
	}
	s.runErrorRateDetectionForServerless(context.Background(), result)
	if len(store.rows) != 0 {
		t.Fatalf("persisted rows = %d, want 0 — detector must stay dormant with no gate enabled", len(store.rows))
	}
}
