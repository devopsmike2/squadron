// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/iac"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

const snsTopicARN = "arn:aws:sns:us-east-1:111122223333:orders"

// TestAppendAWSEventSourceRecs_SNS_Fires: an SNS topic scanned without
// the delivery-log axis becomes one deterministic recommendation with
// the sns-delivery-logging-enable kind + the picker's Terraform, mapped
// onto the wire envelope (renders + opens a PR like an LLM step). This
// is the reference proof that the previously-dormant
// PickSNSDeliveryLoggingPattern now has a production caller.
func TestAppendAWSEventSourceRecs_SNS_Fires(t *testing.T) {
	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-es-1",
		AccountID: "111122223333",
		Regions:   []string{"us-east-1"},
		EventSources: []eventSourceRow{{
			Provider:     "aws",
			Surface:      "sns",
			ResourceName: "orders",
			ResourceARN:  snsTopicARN,
			Region:       "us-east-1",
			HasLogAxis:   false, // no delivery-feedback role → fires
		}},
	}

	var recs []recommendations.Recommendation
	h.appendAWSEventSourceRecs(context.Background(), &recs, scan, time.Now().UTC())

	if len(recs) != 1 {
		t.Fatalf("want 1 event-source rec, got %d", len(recs))
	}
	got := recs[0]
	if got.ResourceKind != "sns-delivery-logging-enable" {
		t.Errorf("ResourceKind = %q, want sns-delivery-logging-enable", got.ResourceKind)
	}
	if got.ID != snsTopicARN {
		t.Errorf("ID = %q, want %q (stable across scans)", got.ID, snsTopicARN)
	}
	if got.IaC == nil || !strings.Contains(got.IaC.Source, "feedback_role_arn") {
		t.Error("expected the picker's per-protocol feedback-role Terraform")
	}
	// Disposition must resolve from the map, not fall to the unknown default.
	if got.Disposition != iac.DispositionPatchExisting {
		t.Errorf("Disposition = %q, want patch_existing", got.Disposition)
	}
	if got.Source == nil || got.Source.Kind != recommendations.SourceDiscoveryScan {
		t.Error("expected Source.Kind = discovery_scan")
	}
	if len(got.AffectedResources) != 1 || got.AffectedResources[0] != snsTopicARN {
		t.Errorf("AffectedResources = %v", got.AffectedResources)
	}
}

// TestAppendAWSEventSourceRecs_SNS_HasLogAxis_NoRec: a topic already
// wired with a delivery-feedback destination yields no recommendation.
func TestAppendAWSEventSourceRecs_SNS_HasLogAxis_NoRec(t *testing.T) {
	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-es-2",
		AccountID: "111122223333",
		Regions:   []string{"us-east-1"},
		EventSources: []eventSourceRow{{
			Provider: "aws", Surface: "sns", ResourceName: "orders",
			ResourceARN: snsTopicARN, Region: "us-east-1", HasLogAxis: true,
		}},
	}
	var recs []recommendations.Recommendation
	h.appendAWSEventSourceRecs(context.Background(), &recs, scan, time.Now().UTC())
	if len(recs) != 0 {
		t.Fatalf("want 0 recs when HasLogAxis=true, got %d", len(recs))
	}
}

// TestAppendAWSEventSourceRecs_SNS_Excluded: a prior decline recorded
// against the topic ARN suppresses the recommendation.
func TestAppendAWSEventSourceRecs_SNS_Excluded(t *testing.T) {
	excl := &fakeExclusionStore{seeded: []types.ExcludedRecommendation{{
		RecommendationID: snsTopicARN,
		ConnectionID:     "111122223333",
		AccountID:        "111122223333",
		Region:           "us-east-1",
	}}}
	h := &DiscoveryHandlers{exclusionStore: excl}
	scan := awsScanResponse{
		ScanID:    "scan-es-3",
		AccountID: "111122223333",
		Regions:   []string{"us-east-1"},
		EventSources: []eventSourceRow{{
			Provider: "aws", Surface: "sns", ResourceName: "orders",
			ResourceARN: snsTopicARN, Region: "us-east-1", HasLogAxis: false,
		}},
	}
	var recs []recommendations.Recommendation
	h.appendAWSEventSourceRecs(context.Background(), &recs, scan, time.Now().UTC())
	if len(recs) != 0 {
		t.Fatalf("want 0 recs when excluded, got %d", len(recs))
	}
}

// TestAppendAWSEventSourceRecs_NonSNSSurface_NoRec: a non-SNS
// event-source row (EventBridge) is not this slice's concern.
func TestAppendAWSEventSourceRecs_NonSNSSurface_NoRec(t *testing.T) {
	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-es-4",
		AccountID: "111122223333",
		Regions:   []string{"us-east-1"},
		EventSources: []eventSourceRow{{
			Provider: "aws", Surface: "eventbridge", ResourceName: "bus",
			ResourceARN: "arn:aws:events:us-east-1:111122223333:event-bus/default",
			Region:      "us-east-1", HasLogAxis: false,
		}},
	}
	var recs []recommendations.Recommendation
	h.appendAWSEventSourceRecs(context.Background(), &recs, scan, time.Now().UTC())
	if len(recs) != 0 {
		t.Fatalf("want 0 recs for non-sns surface, got %d", len(recs))
	}
}

const sqsQueueARN = "arn:aws:sqs:us-east-1:111122223333:orders"

// TestAppendAWSEventSourceRecs_SQS_Fires: an SQS queue scanned without a
// redrive policy (HasTraceAxis=false) becomes an sqs-redrive-policy-enable
// recommendation with the picker's Terraform, proving the multi-check
// dispatch reaches the second AWS surface. HasTraceAxis is the fire gate
// (redrive policy present), not HasLogAxis (DLQ reachable).
func TestAppendAWSEventSourceRecs_SQS_Fires(t *testing.T) {
	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-sqs-1",
		AccountID: "111122223333",
		Regions:   []string{"us-east-1"},
		EventSources: []eventSourceRow{{
			Provider:     "aws",
			Surface:      "sqs",
			ResourceName: "orders",
			ResourceARN:  sqsQueueARN,
			Region:       "us-east-1",
			HasTraceAxis: false, // no redrive policy → fires
		}},
	}
	var recs []recommendations.Recommendation
	h.appendAWSEventSourceRecs(context.Background(), &recs, scan, time.Now().UTC())

	if len(recs) != 1 {
		t.Fatalf("want 1 SQS event-source rec, got %d", len(recs))
	}
	got := recs[0]
	if got.ResourceKind != "sqs-redrive-policy-enable" {
		t.Errorf("ResourceKind = %q, want sqs-redrive-policy-enable", got.ResourceKind)
	}
	if got.IaC == nil || !strings.Contains(got.IaC.Source, "redrive_policy") {
		t.Error("expected the picker's redrive_policy Terraform")
	}
	if got.Disposition != iac.DispositionPatchExisting {
		t.Errorf("Disposition = %q, want patch_existing", got.Disposition)
	}
}

// TestAppendAWSEventSourceRecs_SQS_HasRedrive_NoRec: a queue that already
// has a redrive policy (HasTraceAxis=true) yields no recommendation.
func TestAppendAWSEventSourceRecs_SQS_HasRedrive_NoRec(t *testing.T) {
	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-sqs-2",
		AccountID: "111122223333",
		Regions:   []string{"us-east-1"},
		EventSources: []eventSourceRow{{
			Provider: "aws", Surface: "sqs", ResourceName: "orders",
			ResourceARN: sqsQueueARN, Region: "us-east-1", HasTraceAxis: true,
		}},
	}
	var recs []recommendations.Recommendation
	h.appendAWSEventSourceRecs(context.Background(), &recs, scan, time.Now().UTC())
	if len(recs) != 0 {
		t.Fatalf("want 0 recs when redrive policy present, got %d", len(recs))
	}
}

// TestAppendAWSEventSourceRecs_MixedSurfaces: an SNS topic (no log axis)
// + an SQS queue (no redrive) in one scan produce exactly two recs, one
// per surface — confirms the registry dispatch fans over surfaces.
func TestAppendAWSEventSourceRecs_MixedSurfaces(t *testing.T) {
	h := &DiscoveryHandlers{exclusionStore: &fakeExclusionStore{}}
	scan := awsScanResponse{
		ScanID:    "scan-mixed",
		AccountID: "111122223333",
		Regions:   []string{"us-east-1"},
		EventSources: []eventSourceRow{
			{Provider: "aws", Surface: "sns", ResourceName: "orders",
				ResourceARN: snsTopicARN, Region: "us-east-1", HasLogAxis: false},
			{Provider: "aws", Surface: "sqs", ResourceName: "orders-q",
				ResourceARN: sqsQueueARN, Region: "us-east-1", HasTraceAxis: false},
		},
	}
	var recs []recommendations.Recommendation
	h.appendAWSEventSourceRecs(context.Background(), &recs, scan, time.Now().UTC())
	if len(recs) != 2 {
		t.Fatalf("want 2 recs (one SNS, one SQS), got %d", len(recs))
	}
	kinds := map[string]bool{}
	for _, r := range recs {
		kinds[r.ResourceKind] = true
	}
	if !kinds["sns-delivery-logging-enable"] || !kinds["sqs-redrive-policy-enable"] {
		t.Errorf("expected both kinds, got %v", kinds)
	}
}
