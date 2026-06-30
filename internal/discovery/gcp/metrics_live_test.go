// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// metrics_live_test.go — the live-verification harness for the GCP
// Cloud Monitoring V3 adapter (metrics_sdk.go). The adapter's parsing
// contract is unit-tested against canned JSON in metrics_sdk_test.go;
// THIS test exercises the SAME adapter against a REAL Cloud Monitoring
// backend, so the two semantics flagged in the metrics_sdk.go header —
// the SampleCount proxy that feeds the cold-start baseline-minimum-
// samples gate, and TypedValue field selection across aligners — can be
// confirmed on real data.
//
// SKIPPED BY DEFAULT. It runs only when SQUADRON_GCP_LIVE=1, so it never
// touches the network in CI. Run it from any machine with GCP access:
//
//	SQUADRON_GCP_LIVE=1 \
//	SQUADRON_GCP_SA_JSON=/path/to/sa.json \
//	SQUADRON_GCP_PROJECT=my-project \
//	SQUADRON_GCP_LOCATION=us-central1 \
//	SQUADRON_GCP_SERVICE=my-cloud-run-service \
//	go test ./internal/discovery/gcp/ -run TestGCPLiveMonitoring -v
//
// The Service Account (or ADC principal) needs roles/monitoring.viewer
// on the project, and the named Cloud Run service must have received
// traffic in the last 24h for the latency series to be non-empty.
//
// What to eyeball in the -v output:
//   - "raw QueryTimeSeries returned N points" with N > 0 → the request
//     reached Cloud Monitoring and parsed.
//   - the per-point Value samples are plausible latencies (ms).
//   - "SampleCount sum = S" → S is the count of populated 5m periods
//     (the proxy). For a 24h window S should be roughly the number of
//     5m buckets that had traffic (<= 288). Over a 168h baseline this
//     must clear ColdStartBaselineMinimumSamples (50) for the cold-start
//     detector to fire — confirm the busy-service case clears it.
//   - the QueryAggregate P95 value + SampleCount match expectations.
func TestGCPLiveMonitoring_Verify(t *testing.T) {
	if os.Getenv("SQUADRON_GCP_LIVE") != "1" {
		t.Skip("set SQUADRON_GCP_LIVE=1 (+ SQUADRON_GCP_SA_JSON/PROJECT/LOCATION/SERVICE) to run the GCP live verification")
	}

	saPath := mustEnv(t, "SQUADRON_GCP_SA_JSON")
	project := mustEnv(t, "SQUADRON_GCP_PROJECT")

	saJSON, err := os.ReadFile(saPath)
	if err != nil {
		t.Fatalf("read SA JSON %q: %v", saPath, err)
	}

	// Production-path scanner: metric detection on so buildOAuthHTTPClient
	// requests the monitoring.read scope, no test httpClient/endpoint so
	// buildMonitoringClient takes the real-oauth branch.
	s := (&Scanner{ProjectID: project, SAJSON: saJSON}).WithServerlessMetricDetection(true)

	ctx := context.Background()
	oauthClient, err := s.buildOAuthHTTPClient(ctx)
	if err != nil {
		t.Fatalf("buildOAuthHTTPClient: %v", err)
	}
	mc, err := s.buildMonitoringClient(ctx, oauthClient)
	if err != nil {
		t.Fatalf("buildMonitoringClient: %v", err)
	}

	end := time.Now().UTC()
	start := end.Add(-24 * time.Hour)

	// GENERIC MODE: when SQUADRON_GCP_FILTER is set, query that metric
	// directly. This validates the adapter's auth + request + response
	// parsing + pagination + SampleCount proxy against ANY metric with
	// data (e.g. serviceruntime.googleapis.com/api/request_count), so the
	// adapter can be confirmed on real Cloud Monitoring responses without
	// a deployed serverless resource. The serverless-specific filters are
	// just strings over the same response envelope, so a passing generic
	// run de-risks them too.
	if filter := os.Getenv("SQUADRON_GCP_FILTER"); filter != "" {
		aligner := os.Getenv("SQUADRON_GCP_ALIGNER")
		if aligner == "" {
			aligner = "ALIGN_RATE"
		}
		points, qerr := mc.QueryTimeSeries(ctx, fmt.Sprintf("projects/%s", project), filter, start, end, aligner)
		if qerr != nil {
			t.Fatalf("generic QueryTimeSeries (%s / %s): %v", filter, aligner, qerr)
		}
		var sampleSum int64
		var maxVal float64
		for i, p := range points {
			sampleSum += p.SampleCount
			if p.Value > maxVal {
				maxVal = p.Value
			}
			if i < 5 {
				t.Logf("  point[%d]: Value=%g SampleCount=%d [%s .. %s]",
					i, p.Value, p.SampleCount, p.StartTime.Format(time.RFC3339), p.EndTime.Format(time.RFC3339))
			}
		}
		t.Logf("GENERIC %s / %s → %d points; SampleCount sum = %d; max Value = %g",
			filter, aligner, len(points), sampleSum, maxVal)
		if len(points) == 0 {
			t.Fatal("generic filter returned zero points — pick a metric with recent data so the parser is actually exercised")
		}
		return
	}

	// SERVERLESS MODE: requires LOCATION + SERVICE.
	location := mustEnv(t, "SQUADRON_GCP_LOCATION")
	service := mustEnv(t, "SQUADRON_GCP_SERVICE")

	// 1) Raw adapter call: Cloud Run request_latencies, percentile-aligned.
	filter := fmt.Sprintf(
		`metric.type = %q AND resource.labels.service_name = %q AND metric.labels.response_code_class = "2xx"`,
		CloudRunRequestLatenciesMetricType, service)
	points, err := mc.QueryTimeSeries(ctx, fmt.Sprintf("projects/%s", project),
		filter, start, end, "ALIGN_PERCENTILE_95")
	if err != nil {
		t.Fatalf("raw QueryTimeSeries (request_latencies): %v", err)
	}
	var sampleSum int64
	var maxVal float64
	for i, p := range points {
		sampleSum += p.SampleCount
		if p.Value > maxVal {
			maxVal = p.Value
		}
		if i < 5 {
			t.Logf("  point[%d]: Value=%.2fms SampleCount=%d [%s .. %s]",
				i, p.Value, p.SampleCount, p.StartTime.Format(time.RFC3339), p.EndTime.Format(time.RFC3339))
		}
	}
	t.Logf("raw QueryTimeSeries returned %d points; SampleCount sum = %d; max P95 = %.2fms",
		len(points), sampleSum, maxVal)
	if len(points) == 0 {
		t.Log("WARNING: zero points — the service may have had no 2xx traffic in the last 24h; pick a busier service/window")
	}

	// 2) End-to-end QueryAggregate over the same surface (validates the
	//    filter build + rollup the detection path actually uses).
	arn := fmt.Sprintf("projects/%s/locations/%s/services/%s", project, location, service)
	agg, err := s.QueryAggregate(ctx, arn, CloudRunRequestLatenciesMetricType, 24*time.Hour, scanner.StatisticP95)
	if err != nil {
		t.Fatalf("QueryAggregate (request_latencies): %v", err)
	}
	t.Logf("QueryAggregate P95: Value=%.2f%s SampleCount=%d", agg.Value, agg.Unit, agg.SampleCount)

	// 3) Count surface: request_count via ALIGN_DELTA (the error-rate path).
	countFilter := fmt.Sprintf(
		`metric.type = %q AND resource.labels.service_name = %q AND metric.labels.response_code_class != "5xx"`,
		CloudRunRequestCountMetricType, service)
	countPoints, err := mc.QueryTimeSeries(ctx, fmt.Sprintf("projects/%s", project),
		countFilter, start, end, "ALIGN_DELTA")
	if err != nil {
		t.Fatalf("raw QueryTimeSeries (request_count): %v", err)
	}
	var countSum float64
	for _, p := range countPoints {
		countSum += p.Value
	}
	t.Logf("request_count (non-5xx) over 24h: %d points, summed count = %.0f", len(countPoints), countSum)
}

func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s is required when SQUADRON_GCP_LIVE=1", key)
	}
	return v
}
