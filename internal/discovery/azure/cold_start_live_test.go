// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// TestLive_AzureCommercialDetectors exercises the REAL #153 commercial-tier
// path end-to-end against live Azure: with the commercial gate ON it runs
// DetectColdStartRegression + DetectErrorRate against an Application Insights
// component resource that has real request telemetry, proving the gate
// re-points the queries at the App Insights metrics (requests/duration,
// requests/count, requests/failed) and reads real data — not a fake.
//
// Skipped unless both env vars are set:
//   - SQUADRON_LIVE_AZURE_AI_ARN: the App Insights component ARM resource ID
//     (…/providers/microsoft.insights/components/<name>)
//   - SQUADRON_LIVE_AZURE_TOKEN:  an ARM bearer token
//     (az account get-access-token --resource https://management.azure.com)
//
// Run:
//
//	SQUADRON_LIVE_AZURE_AI_ARN=… SQUADRON_LIVE_AZURE_TOKEN=… \
//	  go test ./internal/discovery/azure/ -run TestLive_AzureCommercialDetectors -v
func TestLive_AzureCommercialDetectors(t *testing.T) {
	arn := os.Getenv("SQUADRON_LIVE_AZURE_AI_ARN")
	token := os.Getenv("SQUADRON_LIVE_AZURE_TOKEN")
	if arn == "" || token == "" {
		t.Skip("set SQUADRON_LIVE_AZURE_AI_ARN and SQUADRON_LIVE_AZURE_TOKEN to run the live App Insights verification")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Production-shaped Scanner: real ARM endpoint + bearer token, commercial
	// gate ON so the detectors request the App Insights metric names.
	s := (&Scanner{
		armEndpoint:    armManagementEndpoint,
		accessToken:    token,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		metricsLimiter: rate.NewLimiter(rate.Inf, 1),
	}).WithCommercialDetectors(true)

	// 1) Cold-start (requests/duration P95).
	cs, err := s.DetectColdStartRegression(ctx, arn)
	if err != nil {
		t.Fatalf("DetectColdStartRegression (live): %v", err)
	}
	t.Logf("LIVE cold-start: currentP95=%.1fms baselineP95=%.1fms ratio=%.2f currentSamples=%d",
		cs.CurrentP95Ms, cs.BaselineP95Ms, cs.Ratio, cs.CurrentSampleCount)
	if cs.CurrentP95Ms <= 0 && cs.CurrentSampleCount <= 0 {
		t.Errorf("expected real App Insights requests/duration data; got empty cold-start result")
	}

	// 2) Error rate (requests/count denominator + requests/failed numerator).
	er, err := s.DetectErrorRate(ctx, arn)
	if err != nil {
		t.Fatalf("DetectErrorRate (live): %v", err)
	}
	t.Logf("LIVE error-rate: currentInvocations=%d currentErrors=%d currentRate=%.3f",
		er.CurrentInvocationCount, er.CurrentErrorCount, er.CurrentErrorRate)
	if er.CurrentInvocationCount == 0 {
		t.Errorf("expected real App Insights requests/count data; got zero invocations")
	}
}
