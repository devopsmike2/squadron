// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/time/rate"
)

// metricNameCapture is a minimal Azure Monitor /metrics fake that
// records the metricnames query parameter of every call and always
// returns an empty timeseries. It lets the #153 gate test assert which
// metric the detector asked for without caring about the result.
type metricNameCapture struct {
	mu      sync.Mutex
	seen    []string
	filters []string
}

func (c *metricNameCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, r.URL.Query().Get("metricnames"))
		c.filters = append(c.filters, r.URL.Query().Get("$filter"))
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(armMetricsResponse{
			Value: []armMetricsValue{{Unit: "Milliseconds", Timeseries: []armMetricsTimeseries{}}},
		})
	})
}

func newGateTestScanner(t *testing.T, c *metricNameCapture, commercial bool) *Scanner {
	t.Helper()
	srv := httptest.NewServer(c.handler())
	t.Cleanup(srv.Close)
	return (&Scanner{
		TenantID:       "11111111-1111-1111-1111-111111111111",
		SubscriptionID: "22222222-2222-2222-2222-222222222222",
		ClientID:       "33333333-3333-3333-3333-333333333333",
		ClientSecret:   []byte("s"),
		httpClient:     srv.Client(),
		armEndpoint:    srv.URL,
		accessToken:    "fake-token",
		metricsLimiter: rate.NewLimiter(rate.Inf, 1),
	}).WithCommercialDetectors(commercial)
}

const gateTestARN = "/subscriptions/22222222-2222-2222-2222-222222222222/resourceGroups/rg/providers/microsoft.insights/components/ai-comp"

// TestAzureColdStartFilterGate locks the #153 live-caught fix: the App
// Insights requests/duration metric has no IsAfterColdStart dimension, so the
// commercial cold-start query must NOT send the $filter — while the OSS path
// (FunctionExecutionDuration) still does.
func TestAzureColdStartFilterGate(t *testing.T) {
	t.Run("commercial requests/duration sends no IsAfterColdStart filter", func(t *testing.T) {
		cap := &metricNameCapture{}
		s := newGateTestScanner(t, cap, true)
		if _, err := s.DetectColdStartRegression(context.Background(), gateTestARN); err != nil {
			t.Fatalf("DetectColdStartRegression: %v", err)
		}
		for i, f := range cap.filters {
			if strings.Contains(strings.ToLower(f), "isaftercoldstart") {
				t.Errorf("query[%d] sent an IsAfterColdStart filter for requests/duration: %q", i, f)
			}
		}
	})
	t.Run("OSS FunctionExecutionDuration sends the IsAfterColdStart filter", func(t *testing.T) {
		cap := &metricNameCapture{}
		s := newGateTestScanner(t, cap, false)
		if _, err := s.DetectColdStartRegression(context.Background(), gateTestARN); err != nil {
			t.Fatalf("DetectColdStartRegression: %v", err)
		}
		found := false
		for _, f := range cap.filters {
			if strings.Contains(strings.ToLower(f), "isaftercoldstart") {
				found = true
			}
		}
		if !found {
			t.Errorf("OSS cold-start path should send the IsAfterColdStart filter; filters=%v", cap.filters)
		}
	})
}

// TestIsAzureDimensionNotFoundError_CaseInsensitive locks the #153 fix to the
// fallback matcher: Azure echoes the dimension lowercased, so the match must
// be case-insensitive.
func TestIsAzureDimensionNotFoundError_CaseInsensitive(t *testing.T) {
	err := &armCallError{
		StatusCode: 400,
		Code:       "BadRequest",
		Message:    "Metric: requests/duration does not support requested dimension combination: isaftercoldstart, supported ones are: request/resultCode",
	}
	if !isAzureDimensionNotFoundError(err, AzureFunctionsIsAfterColdStartDimension) {
		t.Errorf("expected lowercased 'isaftercoldstart' in message to match dimension %q", AzureFunctionsIsAfterColdStartDimension)
	}
}

// TestAzureColdStartMetricGate covers the #153 enterprise-gate: the
// cold-start detector queries the inert FunctionExecutionDuration metric
// in OSS and the Application Insights requests/duration metric when the
// commercial tier is on.
func TestAzureColdStartMetricGate(t *testing.T) {
	cases := []struct {
		name       string
		commercial bool
		wantMetric string
	}{
		{"OSS queries FunctionExecutionDuration (inert)", false, "FunctionExecutionDuration"},
		{"commercial queries App Insights requests/duration", true, "requests/duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &metricNameCapture{}
			s := newGateTestScanner(t, cap, tc.commercial)
			if _, err := s.DetectColdStartRegression(context.Background(), gateTestARN); err != nil {
				t.Fatalf("DetectColdStartRegression: %v", err)
			}
			if len(cap.seen) == 0 {
				t.Fatal("no metrics query issued")
			}
			for i, got := range cap.seen {
				if got != tc.wantMetric {
					t.Errorf("query[%d] metricnames = %q, want %q", i, got, tc.wantMetric)
				}
			}
		})
	}
}

// TestAzureErrorRateMetricGate covers the #153 gate for the error-rate
// detector: FunctionInvocations/FunctionErrors in OSS, App Insights
// requests/count + requests/failed when commercial.
func TestAzureErrorRateMetricGate(t *testing.T) {
	cases := []struct {
		name       string
		commercial bool
		wantTotal  string
		wantFailed string
	}{
		{"OSS", false, "FunctionInvocations", "FunctionErrors"},
		{"commercial", true, "requests/count", "requests/failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &metricNameCapture{}
			s := newGateTestScanner(t, cap, tc.commercial)
			if _, err := s.DetectErrorRate(context.Background(), gateTestARN); err != nil {
				t.Fatalf("DetectErrorRate: %v", err)
			}
			sawTotal, sawFailed := false, false
			for _, got := range cap.seen {
				if got == tc.wantTotal {
					sawTotal = true
				}
				if got == tc.wantFailed {
					sawFailed = true
				}
			}
			if !sawTotal {
				t.Errorf("never queried total metric %q; saw %v", tc.wantTotal, cap.seen)
			}
			if !sawFailed {
				t.Errorf("never queried failed metric %q; saw %v", tc.wantFailed, cap.seen)
			}
		})
	}
}
