// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

// TestColdStartNamespaceGate covers the #152 enterprise-gate: the
// Lambda cold-start InitDuration query targets the AWS/Lambda namespace
// in OSS (commercialDetectors=false) — where the metric does not exist,
// so the detector never fires — and is re-pointed at the Lambda Insights
// namespace (LambdaInsights/init_duration, dimension function_name) when
// the commercial tier is enabled.
func TestColdStartNamespaceGate(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:123456789012:function:checkout"

	cases := []struct {
		name          string
		commercial    bool
		wantNamespace string
		wantMetric    string
		wantDimName   string
	}{
		{
			name:          "OSS (gate off) queries AWS/Lambda InitDuration (inert)",
			commercial:    false,
			wantNamespace: "AWS/Lambda",
			wantMetric:    "InitDuration",
			wantDimName:   "FunctionName",
		},
		{
			name:          "commercial tier queries LambdaInsights/init_duration",
			commercial:    true,
			wantNamespace: "LambdaInsights",
			wantMetric:    "init_duration",
			wantDimName:   "function_name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cw := &cwFake{
				// Empty datapoints — we only assert the query target,
				// not the result. (No add-on data in either mode here.)
				respondWith: &cloudwatch.GetMetricStatisticsOutput{},
			}
			s := newMetricsTestScannerWithCW(t, cw).WithCommercialDetectors(tc.commercial)

			if _, err := s.DetectColdStartRegression(context.Background(), arn); err != nil {
				t.Fatalf("DetectColdStartRegression: %v", err)
			}
			if len(cw.receivedInputs) == 0 {
				t.Fatal("no CloudWatch query was issued")
			}
			in := cw.receivedInputs[0]
			if got := awsStr(in.Namespace); got != tc.wantNamespace {
				t.Errorf("Namespace = %q, want %q", got, tc.wantNamespace)
			}
			if got := awsStr(in.MetricName); got != tc.wantMetric {
				t.Errorf("MetricName = %q, want %q", got, tc.wantMetric)
			}
			if len(in.Dimensions) != 1 {
				t.Fatalf("Dimensions len = %d, want 1", len(in.Dimensions))
			}
			if got := awsStr(in.Dimensions[0].Name); got != tc.wantDimName {
				t.Errorf("Dimension name = %q, want %q", got, tc.wantDimName)
			}
			// Either way the function name resolves identically from the ARN.
			if got := awsStr(in.Dimensions[0].Value); got != "checkout" {
				t.Errorf("Dimension value = %q, want %q", got, "checkout")
			}
		})
	}
}

// TestCloudWatchPeriodForWindow locks the #152 live-discovered fix: the
// period must keep every window's datapoint count within CloudWatch's 1440
// cap, while leaving the 24h current window at the tuned 300s.
func TestCloudWatchPeriodForWindow(t *testing.T) {
	cases := []struct {
		name       string
		window     time.Duration
		wantPeriod int32
	}{
		{"1h SQS window stays 300", time.Hour, 300},
		{"24h current window stays 300", 24 * time.Hour, 300},
		{"168h baseline bumps above 300", 168 * time.Hour, 480},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cloudWatchPeriodForWindow(tc.window)
			if got != tc.wantPeriod {
				t.Errorf("period = %d, want %d", got, tc.wantPeriod)
			}
			// Invariant: datapoints must stay within the CloudWatch cap.
			datapoints := int(tc.window/time.Second) / int(got)
			if datapoints > cloudWatchMaxDatapoints {
				t.Errorf("datapoints = %d exceeds CloudWatch cap %d (window=%s period=%d)",
					datapoints, cloudWatchMaxDatapoints, tc.window, got)
			}
		})
	}
}

// awsStr safely derefs an aws *string for assertions.
func awsStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
