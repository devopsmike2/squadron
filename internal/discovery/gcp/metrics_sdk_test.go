// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// metrics_sdk_test.go — unit tests for the production Cloud Monitoring V3
// adapter (metrics_sdk.go). These exercise request routing, response
// marshaling, pagination, and the SampleCount proxy against an httptest
// server returning canned Cloud Monitoring timeSeries.list JSON. They do
// NOT validate against a real Cloud Monitoring backend (see the
// metrics_sdk.go header's live-verification note); they pin the
// adapter's parsing contract so a live run only has to confirm the
// upstream JSON shape + the SampleCount semantics.

// TestCloudMonitoringClient_QueryTimeSeries_RollupAndPagination verifies
// the adapter pages through every result and flattens each point of each
// returned series into a TimeSeriesPoint, with the SampleCount = 1
// per-populated-period proxy.
func TestCloudMonitoringClient_QueryTimeSeries_RollupAndPagination(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/timeSeries") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("pageToken") {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"timeSeries": []any{
					map[string]any{"points": []any{
						map[string]any{
							"interval": map[string]any{"startTime": "2024-01-01T00:00:00Z", "endTime": "2024-01-01T00:05:00Z"},
							"value":    map[string]any{"doubleValue": 120.0},
						},
						map[string]any{
							"interval": map[string]any{"startTime": "2024-01-01T00:05:00Z", "endTime": "2024-01-01T00:10:00Z"},
							"value":    map[string]any{"doubleValue": 200.0},
						},
					}},
				},
				"nextPageToken": "page2",
			})
		case "page2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"timeSeries": []any{
					map[string]any{"points": []any{
						map[string]any{"value": map[string]any{"doubleValue": 350.0}},
					}},
				},
			})
		}
	}))
	defer srv.Close()

	s := &Scanner{httpClient: srv.Client(), endpoint: srv.URL}
	mc, err := s.buildMonitoringClient(context.Background(), nil)
	if err != nil {
		t.Fatalf("buildMonitoringClient: %v", err)
	}

	pts, err := mc.QueryTimeSeries(context.Background(), "projects/test-project",
		`metric.type = "run.googleapis.com/request_latencies"`,
		time.Now().Add(-time.Hour), time.Now(), "ALIGN_PERCENTILE_95")
	if err != nil {
		t.Fatalf("QueryTimeSeries: %v", err)
	}

	if len(pts) != 3 {
		t.Fatalf("got %d points across pages, want 3", len(pts))
	}
	if calls != 2 {
		t.Errorf("expected 2 paged requests, got %d", calls)
	}

	var totalSamples int64
	var maxVal float64
	for _, p := range pts {
		totalSamples += p.SampleCount
		if p.Value > maxVal {
			maxVal = p.Value
		}
	}
	if totalSamples != 3 {
		t.Errorf("SampleCount sum = %d, want 3 (1 per populated period proxy)", totalSamples)
	}
	if maxVal != 350.0 {
		t.Errorf("max parsed value = %v, want 350.0", maxVal)
	}
	if pts[0].StartTime.IsZero() {
		t.Error("first point StartTime should have been parsed from the interval")
	}
}

// TestCloudMonitoringClient_QueryTimeSeries_Empty confirms an empty
// timeSeries response is a clean "no datapoints" (nil slice, no error),
// which QueryAggregate maps to Value=0/SampleCount=0.
func TestCloudMonitoringClient_QueryTimeSeries_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()

	s := &Scanner{httpClient: srv.Client(), endpoint: srv.URL}
	mc, err := s.buildMonitoringClient(context.Background(), nil)
	if err != nil {
		t.Fatalf("buildMonitoringClient: %v", err)
	}

	pts, err := mc.QueryTimeSeries(context.Background(), "projects/p",
		`metric.type = "x"`, time.Now().Add(-time.Hour), time.Now(), "ALIGN_PERCENTILE_95")
	if err != nil {
		t.Fatalf("QueryTimeSeries empty: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("empty response should yield 0 points, got %d", len(pts))
	}
}

// TestWithServerlessMetricDetection pins the gate setter the factory uses.
func TestWithServerlessMetricDetection(t *testing.T) {
	s := (&Scanner{}).WithServerlessMetricDetection(true)
	if !s.metricDetection {
		t.Error("metricDetection should be true after WithServerlessMetricDetection(true)")
	}
}
