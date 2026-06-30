// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"golang.org/x/time/rate"
)

// TestLive_AWSColdStartCommercial exercises the REAL #152 commercial-tier
// cold-start path end-to-end against live AWS: it builds a production
// CloudWatch client, flips the commercial gate on, and runs
// DetectColdStartRegression against a function that has the paid Lambda
// Insights add-on enabled. It proves the gate re-points the query at the
// LambdaInsights namespace AND that the regression detector reads real
// init_duration telemetry — not a fake.
//
// Skipped unless SQUADRON_LIVE_AWS_FN_ARN is set (a Lambda ARN whose
// function has Lambda Insights enabled and recent cold-start data). Run:
//
//	AWS_PROFILE=… SQUADRON_LIVE_AWS_FN_ARN=arn:aws:lambda:us-east-1:…:function:… \
//	  go test ./internal/discovery/aws/ -run TestLive_AWSColdStartCommercial -v
//
// AWS credentials come from the default chain (profile / env).
func TestLive_AWSColdStartCommercial(t *testing.T) {
	fnARN := os.Getenv("SQUADRON_LIVE_AWS_FN_ARN")
	if fnARN == "" {
		t.Skip("set SQUADRON_LIVE_AWS_FN_ARN to run the live Lambda Insights verification")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	cw := cloudwatch.NewFromConfig(cfg)

	// Build a Scanner wired with the real CloudWatch client and the
	// commercial gate ON — exactly the production commercial-tier shape.
	s := (&Scanner{}).
		WithCloudWatchClient(cw).
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Limit(AWSCloudWatchRateLimitRPS), 1)).
		WithCommercialDetectors(true)

	res, err := s.DetectColdStartRegression(ctx, fnARN)
	if err != nil {
		t.Fatalf("DetectColdStartRegression (live): %v", err)
	}
	t.Logf("LIVE cold-start result: currentP95=%.1fms baselineP95=%.1fms ratio=%.2f currentSamples=%d baselineSamples=%d",
		res.CurrentP95Ms, res.BaselineP95Ms, res.Ratio, res.CurrentSampleCount, res.BaselineSampleCount)

	// The decisive assertion: with the gate on the detector queried the
	// LambdaInsights namespace and got REAL init_duration datapoints.
	// (Sample/value > 0 ⇒ data flowed; OSS gate-off would read the empty
	// AWS/Lambda namespace and return zero.)
	if res.CurrentP95Ms <= 0 && res.CurrentSampleCount <= 0 {
		t.Fatalf("expected real Lambda Insights init_duration data; got empty result (gate not reading LambdaInsights, or no cold-start data yet)")
	}
}
