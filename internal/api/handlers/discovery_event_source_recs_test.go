// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

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

// TestAppendGCPEventSourceRecs_Fires: a Cloud Tasks queue lacking retry
// policy + logging and a Pub/Sub Lite topic lacking logging + reservation
// produce four recs (2 + 2). Exercises the GCP adapter + the Detail-bag
// reservation read (has_reservation absent → fires).
func TestAppendGCPEventSourceRecs_Fires(t *testing.T) {
	h := &DiscoveryGCPHandlers{exclusionStore: &fakeExclusionStore{}, logger: zap.NewNop()}
	rows := []eventSourceRow{
		{Provider: "gcp", Surface: "cloudtasks", ResourceName: "q1",
			ResourceARN: "projects/p/locations/us-central1/queues/q1", Region: "us-central1"},
		{Provider: "gcp", Surface: "pubsublite", ResourceName: "t1",
			ResourceARN: "projects/p/locations/us-central1/topics/t1", Region: "us-central1"},
	}
	var recs []recommendations.Recommendation
	h.appendGCPEventSourceRecs(context.Background(), &recs, rows, "conn-1", "p", "us-central1", "scan-g", time.Now().UTC())

	if len(recs) != 4 {
		t.Fatalf("want 4 GCP event-source recs, got %d", len(recs))
	}
	kinds := map[string]bool{}
	for _, r := range recs {
		kinds[r.ResourceKind] = true
		if r.IaC == nil || r.IaC.Source == "" {
			t.Errorf("rec %q missing Terraform", r.ResourceKind)
		}
	}
	for _, want := range []string{
		"cloudtasks-retry-policy-enable", "cloudtasks-logging-enable",
		"pubsublite-logging-enable", "pubsublite-reservation-attach",
	} {
		if !kinds[want] {
			t.Errorf("missing kind %q (got %v)", want, kinds)
		}
	}
}

// TestAppendGCPEventSourceRecs_ReservationHasDetail: when the scanned
// Detail bag reports has_reservation=true, the reservation rec is
// suppressed but logging still fires (one rec, not two).
func TestAppendGCPEventSourceRecs_ReservationHasDetail(t *testing.T) {
	h := &DiscoveryGCPHandlers{exclusionStore: &fakeExclusionStore{}, logger: zap.NewNop()}
	rows := []eventSourceRow{{
		Provider: "gcp", Surface: "pubsublite", ResourceName: "t1",
		ResourceARN: "projects/p/locations/us-central1/topics/t1", Region: "us-central1",
		HasLogAxis: true, // logging present → no logging rec
		Detail:     map[string]any{"has_reservation": true},
	}}
	var recs []recommendations.Recommendation
	h.appendGCPEventSourceRecs(context.Background(), &recs, rows, "conn-1", "p", "us-central1", "scan-g2", time.Now().UTC())
	if len(recs) != 0 {
		t.Fatalf("want 0 recs when logging present + reservation attached, got %d", len(recs))
	}
}

// TestAppendAzureEventSourceRecs_Fires: an Event Grid topic lacking
// diagnostics + proprietary schema and an Event Hubs namespace lacking
// diagnostics + Capture produce four recs (2 + 2). Exercises the Azure
// adapter + the Detail-bag has_capture read.
func TestAppendAzureEventSourceRecs_Fires(t *testing.T) {
	h := &DiscoveryAzureHandlers{exclusionStore: &fakeExclusionStore{}, logger: zap.NewNop()}
	rows := []eventSourceRow{
		{Provider: "azure", Surface: "eventgrid", ResourceName: "eg1",
			ResourceARN: "/subscriptions/s/rg/providers/Microsoft.EventGrid/topics/eg1", Region: "eastus"},
		{Provider: "azure", Surface: "eventhubs", ResourceName: "eh1",
			ResourceARN: "/subscriptions/s/rg/providers/Microsoft.EventHub/namespaces/eh1", Region: "eastus"},
	}
	var recs []recommendations.Recommendation
	h.appendAzureEventSourceRecs(context.Background(), &recs, rows, "conn-1", "sub-1", "eastus", "scan-a", time.Now().UTC())

	if len(recs) != 4 {
		t.Fatalf("want 4 Azure event-source recs, got %d", len(recs))
	}
	kinds := map[string]bool{}
	for _, r := range recs {
		kinds[r.ResourceKind] = true
	}
	for _, want := range []string{
		"eventgrid-diagnostics-enable", "eventgrid-cloudevent-schema-enforce",
		"eventhubs-diagnostics-enable", "eventhubs-capture-enable",
	} {
		if !kinds[want] {
			t.Errorf("missing kind %q (got %v)", want, kinds)
		}
	}
}

// TestAppendOCIEventSourceRecs_Fires: an ONS topic scanned without OCI
// Logging becomes an ons-logging-enable recommendation. Exercises the
// OCI adapter (the fourth and final cloud in the picker-activation arc).
func TestAppendOCIEventSourceRecs_Fires(t *testing.T) {
	h := &DiscoveryOCIHandlers{exclusionStore: &fakeExclusionStore{}, logger: zap.NewNop()}
	rows := []eventSourceRow{{
		Provider: "oci", Surface: "notifications", ResourceName: "t1",
		ResourceARN: "ocid1.onstopic.oc1..abc", Region: "us-ashburn-1", HasLogAxis: false,
	}}
	var recs []recommendations.Recommendation
	h.appendOCIEventSourceRecs(context.Background(), &recs, rows, "conn-1", "ocid1.tenancy.oc1..t", "us-ashburn-1", "scan-o", time.Now().UTC())

	if len(recs) != 1 {
		t.Fatalf("want 1 OCI event-source rec, got %d", len(recs))
	}
	if recs[0].ResourceKind != "ons-logging-enable" {
		t.Errorf("ResourceKind = %q, want ons-logging-enable", recs[0].ResourceKind)
	}
	if recs[0].Disposition != iac.DispositionNewFile {
		t.Errorf("Disposition = %q, want new_file", recs[0].Disposition)
	}
}

// TestAppendOCIEventSourceRecs_QueuesFires: an OCI queue scanned without
// Logging produces a queues-logging-enable rec (second OCI surface).
func TestAppendOCIEventSourceRecs_QueuesFires(t *testing.T) {
	h := &DiscoveryOCIHandlers{exclusionStore: &fakeExclusionStore{}, logger: zap.NewNop()}
	rows := []eventSourceRow{{
		Provider: "oci", Surface: "queues", ResourceName: "q1",
		ResourceARN: "ocid1.queue.oc1..abc", Region: "us-ashburn-1", HasLogAxis: false,
	}}
	var recs []recommendations.Recommendation
	h.appendOCIEventSourceRecs(context.Background(), &recs, rows, "conn-1", "ocid1.tenancy.oc1..t", "us-ashburn-1", "scan-oq", time.Now().UTC())
	if len(recs) != 1 {
		t.Fatalf("want 1 OCI queues rec, got %d", len(recs))
	}
	if recs[0].ResourceKind != "queues-logging-enable" {
		t.Errorf("ResourceKind = %q, want queues-logging-enable", recs[0].ResourceKind)
	}
	if recs[0].Disposition != iac.DispositionNewFile {
		t.Errorf("Disposition = %q, want new_file", recs[0].Disposition)
	}
}
