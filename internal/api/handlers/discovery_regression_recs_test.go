// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
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

// TestAppendAWSSamplingRegressionRecs_Fires exercises the live-join path: the
// scan-response annotation pass has cached a below-floor sampling result for a
// Lambda (10 spans / 2000 invocations = 0.5% < 5% floor, >= 1000 invocations),
// so the sampling regression pass reads it back and emits one
// span-quality-sampling-too-aggressive recommendation with a Terraform snippet.
func TestAppendAWSSamplingRegressionRecs_Fires(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:acc:function:ingest"
	now := time.Now().UTC()
	exceeds := true

	cache := NewSamplingObservationCache()
	cache.record(arn, "lambda", arn, proposer.SamplingRateDetectionResult{
		ResourceARN:               arn,
		Surface:                   "lambda",
		ObservedSpanCount:         10,
		ExpectedInvocationCount:   2000,
		Ratio:                     0.005,
		ExceedsFloor:              true,
		ExceedsMinimumInvocations: true,
		ObservedAt:                now,
	})

	h := &DiscoveryHandlers{samplingSink: cache, exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-1",
		AccountID: "acc",
		Regions:   []string{"us-east-1"},
		Serverless: []awsServerlessRow{{
			Provider:             "aws",
			Surface:              "lambda",
			ResourceName:         "ingest",
			ResourceARN:          arn,
			Region:               "us-east-1",
			SamplingExceedsFloor: &exceeds,
		}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSSamplingRegressionRecs(context.Background(), &recs, scan, now)

	if len(recs) != 1 {
		t.Fatalf("want 1 sampling regression rec, got %d", len(recs))
	}
	got := recs[0]
	if got.ResourceKind != proposer.SamplingRateRecommendationKind {
		t.Errorf("ResourceKind = %q, want %q", got.ResourceKind, proposer.SamplingRateRecommendationKind)
	}
	if got.ID != arn+".sampling_too_aggressive" {
		t.Errorf("ID = %q, want %q (stable across scans)", got.ID, arn+".sampling_too_aggressive")
	}
	if got.IaC == nil || got.IaC.Source == "" {
		t.Error("expected a Terraform IaC snippet on the sampling rec")
	}
	if got.Source == nil || got.Source.Kind != recommendations.SourceDiscoveryScan {
		t.Error("expected Source.Kind = discovery_scan")
	}
}

// TestAppendAWSSamplingRegressionRecs_AcceptableRatio_NoRec confirms the gate:
// a cached result whose ratio clears the 5% floor (200 spans / 2000 = 10%) does
// NOT fire, even though the row carries the annotation flag — the builder's
// ShouldFireRecommendation is the real gate.
func TestAppendAWSSamplingRegressionRecs_AcceptableRatio_NoRec(t *testing.T) {
	const arn = "arn:aws:lambda:us-east-1:acc:function:healthy"
	now := time.Now().UTC()
	// SamplingExceedsFloor is false → the row is skipped before the lookup.
	notExceeds := false

	cache := NewSamplingObservationCache()
	cache.record(arn, "lambda", arn, proposer.SamplingRateDetectionResult{
		ResourceARN:               arn,
		Surface:                   "lambda",
		ObservedSpanCount:         200,
		ExpectedInvocationCount:   2000,
		Ratio:                     0.10,
		ExceedsFloor:              false,
		ExceedsMinimumInvocations: true,
		ObservedAt:                now,
	})

	h := &DiscoveryHandlers{samplingSink: cache, exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-1",
		AccountID: "acc",
		Regions:   []string{"us-east-1"},
		Serverless: []awsServerlessRow{{
			Surface:              "lambda",
			ResourceName:         "healthy",
			ResourceARN:          arn,
			Region:               "us-east-1",
			SamplingExceedsFloor: &notExceeds,
		}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSSamplingRegressionRecs(context.Background(), &recs, scan, now)

	if len(recs) != 0 {
		t.Fatalf("acceptable ratio: want 0 recs, got %d", len(recs))
	}
}

// TestAppendAWSSamplingRegressionRecs_NilSink_NoRec confirms the nil-safe skip:
// a deployment with no sampling cache wired (serverless_metric_detection off)
// emits no sampling recs and never panics.
func TestAppendAWSSamplingRegressionRecs_NilSink_NoRec(t *testing.T) {
	exceeds := true
	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}} // samplingSink nil
	scan := awsScanResponse{
		ScanID:    "scan-1",
		AccountID: "acc",
		Regions:   []string{"us-east-1"},
		Serverless: []awsServerlessRow{{
			Surface:              "lambda",
			ResourceARN:          "arn:aws:lambda:us-east-1:acc:function:fn",
			SamplingExceedsFloor: &exceeds,
		}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSSamplingRegressionRecs(context.Background(), &recs, scan, time.Now().UTC())

	if len(recs) != 0 {
		t.Fatalf("nil sink: want 0 recs, got %d", len(recs))
	}
}

// TestAppendColdStartRegressionRecs_PerCloudSurfaces confirms the surface
// dispatch fires the right per-cloud builder + kind for the GCP/Azure/OCI
// serverless surfaces, gating purely on the snapshot's exceeds flag (no store).
func TestAppendColdStartRegressionRecs_PerCloudSurfaces(t *testing.T) {
	exceeds := true
	p95 := 700.0
	now := time.Now().UTC()
	scope := proposer.ColdStartScope{ConnectionID: "conn", ScopeID: "scope", Region: "r1"}

	cases := []struct {
		surface  string
		wantKind string
	}{
		{"cloudrun", proposer.ColdStartRecommendationKindCloudRun},
		{"cloudfunc", proposer.ColdStartRecommendationKindCloudFunc},
		{"azfunc", proposer.ColdStartRecommendationKindAzureFunc},
		{"ocifunc", proposer.ColdStartRecommendationKindOCIFunc},
	}
	for _, tc := range cases {
		t.Run(tc.surface, func(t *testing.T) {
			snaps := []scanner.ServerlessInstanceSnapshot{{
				Provider: "x", Surface: tc.surface, ResourceName: "fn",
				ResourceARN: "res-" + tc.surface, Region: "r1",
				ColdStartP95Ms: &p95, ColdStartExceedsThreshold: &exceeds,
			}}
			var recs []recommendations.Recommendation
			appendColdStartRegressionRecs(context.Background(), &recs, snaps, nil, nil, scope, "scan", now, nil)
			if len(recs) != 1 {
				t.Fatalf("%s: want 1 rec, got %d", tc.surface, len(recs))
			}
			if recs[0].ResourceKind != tc.wantKind {
				t.Errorf("%s: kind = %q, want %q", tc.surface, recs[0].ResourceKind, tc.wantKind)
			}
			if recs[0].IaC == nil || recs[0].IaC.Source == "" {
				t.Errorf("%s: expected Terraform snippet", tc.surface)
			}
		})
	}

	// An unknown surface is skipped (no mis-shaped rec).
	t.Run("unknown-surface-skipped", func(t *testing.T) {
		snaps := []scanner.ServerlessInstanceSnapshot{{
			Surface: "appservice", ResourceARN: "res-x",
			ColdStartP95Ms: &p95, ColdStartExceedsThreshold: &exceeds,
		}}
		var recs []recommendations.Recommendation
		appendColdStartRegressionRecs(context.Background(), &recs, snaps, nil, nil, scope, "scan", now, nil)
		if len(recs) != 0 {
			t.Fatalf("unknown surface: want 0 recs, got %d", len(recs))
		}
	})
}

// TestAppendErrorRateRegressionRecs_PerCloudSurface confirms the error-rate
// path reconstructs the result from the store and fires the per-cloud builder
// for a non-AWS surface (Cloud Run).
func TestAppendErrorRateRegressionRecs_PerCloudSurface(t *testing.T) {
	const arn = "res-cloudrun-err"
	now := time.Now().UTC()
	store := &stubErrorRateReader{}
	store.set(arn, regressionCurrentWindowHours, 2000, 5000, 0.40, now)
	store.set(arn, regressionBaselineWindowHours, 100, 10000, 0.01, now)

	snaps := []scanner.ServerlessInstanceSnapshot{{
		Provider: "gcp", Surface: "cloudrun", ResourceName: "svc", ResourceARN: arn, Region: "r1",
	}}
	scope := proposer.ErrorRateScope{ConnectionID: "conn", ScopeID: "scope", Region: "r1"}

	var recs []recommendations.Recommendation
	appendErrorRateRegressionRecs(context.Background(), &recs, snaps, store, nil, scope, "scan", now, nil)

	if len(recs) != 1 {
		t.Fatalf("want 1 error-rate rec, got %d", len(recs))
	}
	if recs[0].ResourceKind != proposer.ErrorRateRecommendationKind {
		t.Errorf("kind = %q, want %q", recs[0].ResourceKind, proposer.ErrorRateRecommendationKind)
	}
}
