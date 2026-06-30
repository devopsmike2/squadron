// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/proposer"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestAppendAWSColdStartRegressionRecs_Fires is the reference happy path:
// a Lambda row whose cold-start detector fired (ColdStartExceedsThreshold)
// becomes one deterministic recommendation with the canonical kind + a
// Terraform IaC snippet, mapped onto the wire envelope so it renders +
// opens a PR like any LLM-proposed step.
func TestAppendAWSColdStartRegressionRecs_Fires(t *testing.T) {
	exceeds := true
	p95 := 620.0
	const arn = "arn:aws:lambda:us-east-1:111122223333:function:checkout"

	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-1",
		AccountID: "111122223333",
		Regions:   []string{"us-east-1"},
		Serverless: []awsServerlessRow{{
			Provider:                  "aws",
			Surface:                   "lambda",
			ResourceName:              "checkout",
			ResourceARN:               arn,
			Region:                    "us-east-1",
			ColdStartP95Ms:            &p95,
			ColdStartExceedsThreshold: &exceeds,
		}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSColdStartRegressionRecs(context.Background(), &recs, scan, time.Now().UTC())

	if len(recs) != 1 {
		t.Fatalf("want 1 cold-start regression rec, got %d", len(recs))
	}
	got := recs[0]
	if got.ResourceKind != proposer.ColdStartRecommendationKind {
		t.Errorf("ResourceKind = %q, want %q", got.ResourceKind, proposer.ColdStartRecommendationKind)
	}
	if got.ID != arn+".cold_start" {
		t.Errorf("ID = %q, want %q (stable across scans)", got.ID, arn+".cold_start")
	}
	if got.IaC == nil || got.IaC.Source == "" {
		t.Error("expected a Terraform IaC snippet on the regression rec")
	}
	if got.Source == nil || got.Source.Kind != recommendations.SourceDiscoveryScan {
		t.Error("expected Source.Kind = discovery_scan")
	}
	if len(got.AffectedResources) != 1 || got.AffectedResources[0] != arn {
		t.Errorf("AffectedResources = %v, want [%s]", got.AffectedResources, arn)
	}
}

// TestAppendAWSColdStartRegressionRecs_NoFlag_NoRec confirms the gate: a
// Lambda whose cold-start detector did NOT fire (nil exceeds flag — the OSS
// default, or a function within threshold) yields no recommendation.
func TestAppendAWSColdStartRegressionRecs_NoFlag_NoRec(t *testing.T) {
	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-1",
		AccountID: "acc",
		Regions:   []string{"us-east-1"},
		Serverless: []awsServerlessRow{{
			Surface:     "lambda",
			ResourceARN: "arn:aws:lambda:us-east-1:acc:function:fn",
			// ColdStartExceedsThreshold left nil → detector did not fire.
		}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSColdStartRegressionRecs(context.Background(), &recs, scan, time.Now().UTC())

	if len(recs) != 0 {
		t.Fatalf("no detection flag: want 0 recs, got %d", len(recs))
	}
}

// TestAppendAWSColdStartRegressionRecs_Excluded confirms operator exclusions
// suppress the regression rec — the same verdict-learning posture the rest of
// the discovery recs honor (the builder consults the exclusion store keyed by
// the stable recommendation_id).
func TestAppendAWSColdStartRegressionRecs_Excluded(t *testing.T) {
	exceeds := true
	p95 := 620.0
	const arn = "arn:aws:lambda:us-east-1:acc:function:fn"

	excl := &fakeExclusionStore{seeded: []types.ExcludedRecommendation{{
		RecommendationID: arn + ".cold_start",
		ConnectionID:     "acc",
		AccountID:        "acc",
		Region:           "us-east-1",
	}}}
	h := &DiscoveryHandlers{exclusionStore: excl}
	scan := awsScanResponse{
		ScanID:    "scan-1",
		AccountID: "acc",
		Regions:   []string{"us-east-1"},
		Serverless: []awsServerlessRow{{
			Surface:                   "lambda",
			ResourceName:              "fn",
			ResourceARN:               arn,
			Region:                    "us-east-1",
			ColdStartP95Ms:            &p95,
			ColdStartExceedsThreshold: &exceeds,
		}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSColdStartRegressionRecs(context.Background(), &recs, scan, time.Now().UTC())

	if len(recs) != 0 {
		t.Fatalf("excluded rec: want 0 recs, got %d", len(recs))
	}
}

// TestAppendAWSErrorRateRegressionRecs_Fires reconstructs the detection result
// from the error-rate observation store (24h current + 168h baseline), re-gates
// it via the shared FinalizeErrorRateGates, and confirms a Lambda clearing all
// three gates yields one error-rate-spike recommendation.
func TestAppendAWSErrorRateRegressionRecs_Fires(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:acc:function:orders"
	now := time.Now().UTC()

	store := &stubErrorRateReader{}
	// current: 2000 errors / 5000 inv = 0.40; baseline: 100 / 10000 = 0.01.
	// ratio 40x > 2.0, inv 5000 >= 1000, errors 2000 >= 50 → fires.
	store.set(arn, regressionCurrentWindowHours, 2000, 5000, 0.40, now)
	store.set(arn, regressionBaselineWindowHours, 100, 10000, 0.01, now)

	h := &DiscoveryHandlers{errorRateStore: store, exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-1",
		AccountID: "acc",
		Regions:   []string{"us-east-1"},
		Serverless: []awsServerlessRow{{
			Surface:      "lambda",
			ResourceName: "orders",
			ResourceARN:  arn,
			Region:       "us-east-1",
		}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSErrorRateRegressionRecs(context.Background(), &recs, scan, now)

	if len(recs) != 1 {
		t.Fatalf("want 1 error-rate rec, got %d", len(recs))
	}
	got := recs[0]
	if got.ResourceKind != proposer.ErrorRateRecommendationKind {
		t.Errorf("ResourceKind = %q, want %q", got.ResourceKind, proposer.ErrorRateRecommendationKind)
	}
	if got.IaC == nil || got.IaC.Source == "" {
		t.Error("expected a Terraform IaC snippet on the error-rate rec")
	}
	if got.ID != arn+".error_rate_spike" {
		t.Errorf("ID = %q, want %q", got.ID, arn+".error_rate_spike")
	}
}

// TestAppendAWSErrorRateRegressionRecs_LowVolume_NoRec confirms the noise floor:
// a high rate on too few invocations does not clear ExceedsMinimumInvocations.
func TestAppendAWSErrorRateRegressionRecs_LowVolume_NoRec(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:acc:function:rare"
	now := time.Now().UTC()

	store := &stubErrorRateReader{}
	// 30 errors / 100 inv = 0.30 — high rate, but only 100 invocations (< 1000).
	store.set(arn, regressionCurrentWindowHours, 30, 100, 0.30, now)
	store.set(arn, regressionBaselineWindowHours, 1, 1000, 0.001, now)

	h := &DiscoveryHandlers{errorRateStore: store, exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:     "scan-1",
		AccountID:  "acc",
		Regions:    []string{"us-east-1"},
		Serverless: []awsServerlessRow{{Surface: "lambda", ResourceName: "rare", ResourceARN: arn, Region: "us-east-1"}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSErrorRateRegressionRecs(context.Background(), &recs, scan, now)

	if len(recs) != 0 {
		t.Fatalf("low volume: want 0 recs, got %d", len(recs))
	}
}
