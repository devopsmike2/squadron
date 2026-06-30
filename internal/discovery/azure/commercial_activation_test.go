// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

func TestInstrumentationKeyFromConnString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"full conn string", "InstrumentationKey=abc-123;IngestionEndpoint=https://x/;ApplicationId=def", "abc-123"},
		{"ik only", "InstrumentationKey=abc-123", "abc-123"},
		{"ik not first", "IngestionEndpoint=https://x/;InstrumentationKey=k9", "k9"},
		{"absent", "IngestionEndpoint=https://x/", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := instrumentationKeyFromConnString(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestErrorRateExceeds(t *testing.T) {
	cases := []struct {
		name string
		er   ErrorRateDetectionResult
		want bool
	}{
		{
			name: "fires: 3x ratio, high volume",
			er: ErrorRateDetectionResult{
				CurrentErrorRate: 0.30, BaselineErrorRate: 0.10,
				CurrentInvocationCount: 5000, CurrentErrorCount: 1500,
			},
			want: true,
		},
		{
			name: "no fire: ratio below floor",
			er: ErrorRateDetectionResult{
				CurrentErrorRate: 0.12, BaselineErrorRate: 0.10,
				CurrentInvocationCount: 5000, CurrentErrorCount: 600,
			},
			want: false,
		},
		{
			name: "no fire: low volume",
			er: ErrorRateDetectionResult{
				CurrentErrorRate: 0.30, BaselineErrorRate: 0.10,
				CurrentInvocationCount: 100, CurrentErrorCount: 30,
			},
			want: false,
		},
		{
			name: "no fire: baseline below noise floor",
			er: ErrorRateDetectionResult{
				CurrentErrorRate: 0.30, BaselineErrorRate: 0.00001,
				CurrentInvocationCount: 5000, CurrentErrorCount: 1500,
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorRateExceeds(tc.er); got != tc.want {
				t.Errorf("errorRateExceeds = %v, want %v", got, tc.want)
			}
		})
	}
}

const (
	testFunctionARN  = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Web/sites/fn1"
	testComponentARN = "/subscriptions/sub/resourceGroups/rg/providers/microsoft.insights/components/ai1"
	testIK           = "ik-fn1"
)

// armCommercialFake serves the Microsoft.Insights/components LIST + the
// metrics endpoint so the enrichment pass can be exercised end-to-end.
func armCommercialFake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Check the metric endpoint FIRST: a metric query targets
		// {componentARN}/providers/microsoft.insights/metrics, and the
		// component ARN itself contains "/providers/microsoft.insights/
		// components/…", so a components-first match would misroute it.
		path := strings.ToLower(r.URL.Path)
		switch {
		case strings.Contains(path, "/providers/microsoft.insights/metrics"):
			metric := r.URL.Query().Get("metricnames")
			dp := armMetricsDatapoint{TimeStamp: "2026-01-01T00:00:00Z"}
			switch {
			case strings.Contains(metric, "duration"):
				dp.Maximum = fpPtr(200.0)
			case strings.Contains(metric, "failed"):
				dp.Total = fpPtr(300.0)
			default: // requests/count
				dp.Total = fpPtr(5000.0)
			}
			_ = json.NewEncoder(w).Encode(armMetricsResponse{
				Value: []armMetricsValue{{Unit: "Milliseconds", Timeseries: []armMetricsTimeseries{{Data: []armMetricsDatapoint{dp}}}}},
			})
		case strings.Contains(path, "/providers/microsoft.insights/components"):
			_ = json.NewEncoder(w).Encode(armAppInsightsComponentList{
				Value: []armAppInsightsComponent{{
					ID: testComponentARN,
					Properties: struct {
						InstrumentationKey string `json:"InstrumentationKey"`
					}{InstrumentationKey: testIK},
				}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unhandled " + path})
		}
	}))
}

// TestRunAzureServerlessCommercialDetection_Annotates exercises the whole
// gated enrichment: resolve the App Insights component by IK, run both
// detectors against it, and annotate the Function App's serverless row.
func TestRunAzureServerlessCommercialDetection_Annotates(t *testing.T) {
	srv := armCommercialFake(t)
	t.Cleanup(srv.Close)

	s := &Scanner{
		SubscriptionID:  "sub",
		httpClient:      srv.Client(),
		armEndpoint:     srv.URL,
		metricsLimiter:  rate.NewLimiter(rate.Inf, 1),
		ikByFunctionARN: map[string]string{testFunctionARN: testIK},
	}
	s = s.WithCommercialDetectors(true)

	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Provider:    "azure",
			Surface:     azureFunctionsServerlessSurface,
			ResourceARN: testFunctionARN,
		}},
	}

	s.runAzureServerlessCommercialDetection(context.Background(), "tok", result)

	row := result.Serverless[0]
	if row.ColdStartP95Ms == nil {
		t.Fatal("expected ColdStartP95Ms to be set from App Insights requests/duration")
	}
	if *row.ColdStartP95Ms != 200.0 {
		t.Errorf("ColdStartP95Ms = %v, want 200", *row.ColdStartP95Ms)
	}
	if row.CurrentErrorRate == nil {
		t.Fatal("expected CurrentErrorRate to be set from App Insights requests/count+failed")
	}
	// 300 failed / 5000 count = 0.06.
	if got := *row.CurrentErrorRate; got < 0.059 || got > 0.061 {
		t.Errorf("CurrentErrorRate = %v, want ~0.06", got)
	}
	if row.Detail["appinsights_component_id"] != testComponentARN {
		t.Errorf("Detail[appinsights_component_id] = %v, want %s", row.Detail["appinsights_component_id"], testComponentARN)
	}
}

// TestRunAzureServerlessCommercialDetection_GateOff confirms the OSS posture:
// with the gate off the pass is a complete no-op (no annotation, no ARM call).
func TestRunAzureServerlessCommercialDetection_GateOff(t *testing.T) {
	s := &Scanner{SubscriptionID: "sub"} // commercialDetectors=false
	result := &scanner.Result{
		Serverless: []scanner.ServerlessInstanceSnapshot{{
			Surface: azureFunctionsServerlessSurface, ResourceARN: testFunctionARN,
		}},
	}
	s.runAzureServerlessCommercialDetection(context.Background(), "tok", result)
	if result.Serverless[0].ColdStartP95Ms != nil || result.Serverless[0].CurrentErrorRate != nil {
		t.Error("gate off should annotate nothing")
	}
}
