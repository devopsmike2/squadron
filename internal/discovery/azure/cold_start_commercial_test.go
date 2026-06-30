// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"golang.org/x/time/rate"
)

// metricNameCapture is a minimal Azure Monitor /metrics fake that
// records the metricnames query parameter of every call and always
// returns an empty timeseries. It lets the #153 gate test assert which
// metric the detector asked for without caring about the result.
type metricNameCapture struct {
	mu   sync.Mutex
	seen []string
}

func (c *metricNameCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, r.URL.Query().Get("metricnames"))
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
