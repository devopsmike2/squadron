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

// region_aware_query_test.go — #295 slice 3. Pins that a STANDALONE
// QueryAggregate (e.g. the sampling-rate annotation) binds the CloudWatch
// client to the queried Lambda's own region rather than whatever region
// the per-region scan walk last left bound — the correctness fix that
// lets sampling annotate multi-region scans accurately.

// regionTrackingFactory records the regions CloudWatch was asked to build
// a client for, so the test can assert region-aware selection.
type regionTrackingFactory struct {
	fakeFactory
	cw         CloudWatchClient
	gotRegions []string
}

func (f *regionTrackingFactory) CloudWatch(_ context.Context, region string) (CloudWatchClient, error) {
	f.gotRegions = append(f.gotRegions, region)
	return f.cw, nil
}

// TestQueryAggregate_RebindsToARNRegion verifies QueryAggregate rebinds
// the CloudWatch client to the ARN's region when a per-region factory is
// present (the production flag-on / commercial path).
func TestQueryAggregate_RebindsToARNRegion(t *testing.T) {
	cw := &cwSumFake{
		responses: map[string]map[string]map[time.Duration]float64{
			"fn": {LambdaInvocationsMetricName: {24 * time.Hour: 1500}},
		},
	}
	f := &regionTrackingFactory{cw: cw}
	s := newMetricsTestScanner().WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1))
	s.cwClient = cw // non-nil so QueryAggregate passes its nil gate and reaches the rebind
	s.factory = f

	res, err := s.QueryAggregate(context.Background(),
		"arn:aws:lambda:eu-west-2:123456789012:function:fn",
		LambdaInvocationsMetricName, 24*time.Hour, scanner.StatisticSum)
	if err != nil {
		t.Fatalf("QueryAggregate: %v", err)
	}
	if res.Value != 1500 {
		t.Errorf("Value = %v, want 1500", res.Value)
	}
	if len(f.gotRegions) == 0 || f.gotRegions[len(f.gotRegions)-1] != "eu-west-2" {
		t.Errorf("CloudWatch built for regions %v, want the ARN's region eu-west-2 to be selected", f.gotRegions)
	}
}

// TestQueryAggregate_NoFactoryLeavesInjectedClient confirms the OSS/test
// path (a directly-injected client, no factory) is untouched — no rebind,
// no attempt to build a per-region client.
func TestQueryAggregate_NoFactoryLeavesInjectedClient(t *testing.T) {
	cw := &cwSumFake{
		responses: map[string]map[string]map[time.Duration]float64{
			"fn": {LambdaInvocationsMetricName: {24 * time.Hour: 42}},
		},
	}
	s := newMetricsTestScanner().WithCloudWatchClient(cw).
		WithCloudWatchRateLimiter(rate.NewLimiter(rate.Inf, 1))
	// s.factory stays nil — the rebind must be skipped entirely.

	res, err := s.QueryAggregate(context.Background(),
		"arn:aws:lambda:eu-west-2:123456789012:function:fn",
		LambdaInvocationsMetricName, 24*time.Hour, scanner.StatisticSum)
	if err != nil {
		t.Fatalf("QueryAggregate: %v", err)
	}
	if res.Value != 42 {
		t.Errorf("Value = %v, want 42 (injected client used directly)", res.Value)
	}
}
