// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// stubEventSourceExclusions is a minimal EventSourceExclusionStore for
// the detection-branch tests.
type stubEventSourceExclusions struct {
	excluded []applicationstore.ExcludedRecommendation
	err      error
}

func (s stubEventSourceExclusions) ListExcludedRecommendations(
	_ context.Context, _, _, _ string, _ int,
) ([]applicationstore.ExcludedRecommendation, error) {
	return s.excluded, s.err
}

func snsRow(hasLog bool) EventSourceInventoryRow {
	return EventSourceInventoryRow{
		RecommendationID: "arn:aws:sns:us-east-1:111122223333:orders",
		Provider:         "aws",
		Surface:          "sns",
		ResourceTFName:   "orders",
		ResourceID:       "arn:aws:sns:us-east-1:111122223333:orders",
		Region:           "us-east-1",
		HasLogAxis:       hasLog,
	}
}

// TestCheckSNSDeliveryLogging_Fires: an SNS topic with no delivery-log
// axis yields a draft with the picker's Terraform + the canonical kind.
func TestCheckSNSDeliveryLogging_Fires(t *testing.T) {
	draft, err := CheckSNSDeliveryLogging(context.Background(), snsRow(false),
		EventSourceScope{ConnectionID: "111122223333", ScopeID: "111122223333", Region: "us-east-1"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draft == nil {
		t.Fatal("expected a draft, got nil")
	}
	if draft.Kind != SNSDeliveryLoggingRecommendationKind {
		t.Errorf("kind = %q, want %q", draft.Kind, SNSDeliveryLoggingRecommendationKind)
	}
	if draft.ResourceID != "arn:aws:sns:us-east-1:111122223333:orders" {
		t.Errorf("resourceID = %q", draft.ResourceID)
	}
	// The picker interpolates the TF name into the topic resource block.
	if !strings.Contains(draft.Terraform, `aws_sns_topic" "orders"`) {
		t.Errorf("terraform missing topic block for resource name:\n%s", draft.Terraform)
	}
	if !strings.Contains(draft.Terraform, "feedback_role_arn") {
		t.Errorf("terraform missing per-protocol feedback role attachments:\n%s", draft.Terraform)
	}
	if draft.Reasoning == "" {
		t.Error("expected non-empty reasoning")
	}
}

// TestCheckSNSDeliveryLogging_SkipsWhenLogAxisPresent: a topic that
// already wires a delivery-feedback destination is not recommended.
func TestCheckSNSDeliveryLogging_SkipsWhenLogAxisPresent(t *testing.T) {
	draft, err := CheckSNSDeliveryLogging(context.Background(), snsRow(true),
		EventSourceScope{ConnectionID: "c", ScopeID: "c"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draft != nil {
		t.Errorf("expected nil draft when HasLogAxis=true, got %+v", draft)
	}
}

// TestCheckSNSDeliveryLogging_SkipsNonSNSSurface: an EventBridge/SQS row
// is not this branch's concern.
func TestCheckSNSDeliveryLogging_SkipsNonSNSSurface(t *testing.T) {
	row := snsRow(false)
	row.Surface = "eventbridge"
	draft, err := CheckSNSDeliveryLogging(context.Background(), row,
		EventSourceScope{ConnectionID: "c", ScopeID: "c"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draft != nil {
		t.Errorf("expected nil draft for non-sns surface, got %+v", draft)
	}
}

// TestCheckSNSDeliveryLogging_ExcludedByID: a prior decline recorded
// against this recommendation ID suppresses the recommendation.
func TestCheckSNSDeliveryLogging_ExcludedByID(t *testing.T) {
	ex := stubEventSourceExclusions{excluded: []applicationstore.ExcludedRecommendation{
		{RecommendationID: "arn:aws:sns:us-east-1:111122223333:orders"},
	}}
	draft, err := CheckSNSDeliveryLogging(context.Background(), snsRow(false),
		EventSourceScope{ConnectionID: "111122223333", ScopeID: "111122223333", Region: "us-east-1"}, ex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draft != nil {
		t.Errorf("expected nil draft when excluded by ID, got %+v", draft)
	}
}

// TestCheckSNSDeliveryLogging_ExcludedByKind: a kind-level exclusion
// (RecommendationID empty) suppresses the whole kind.
func TestCheckSNSDeliveryLogging_ExcludedByKind(t *testing.T) {
	ex := stubEventSourceExclusions{excluded: []applicationstore.ExcludedRecommendation{
		{RecommendationKind: SNSDeliveryLoggingRecommendationKind},
	}}
	draft, err := CheckSNSDeliveryLogging(context.Background(), snsRow(false),
		EventSourceScope{ConnectionID: "111122223333", ScopeID: "111122223333", Region: "us-east-1"}, ex)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draft != nil {
		t.Errorf("expected nil draft when excluded by kind, got %+v", draft)
	}
}

// TestCheckSNSDeliveryLogging_ExclusionStoreError propagates.
func TestCheckSNSDeliveryLogging_ExclusionStoreError(t *testing.T) {
	ex := stubEventSourceExclusions{err: errors.New("boom")}
	_, err := CheckSNSDeliveryLogging(context.Background(), snsRow(false),
		EventSourceScope{ConnectionID: "c", ScopeID: "c"}, ex)
	if err == nil {
		t.Fatal("expected error from exclusion store to propagate")
	}
}

func sqsRow(hasRedrive bool) EventSourceInventoryRow {
	return EventSourceInventoryRow{
		RecommendationID: "arn:aws:sqs:us-east-1:111122223333:orders",
		Provider:         "aws",
		Surface:          "sqs",
		ResourceTFName:   "orders",
		ResourceID:       "arn:aws:sqs:us-east-1:111122223333:orders",
		Region:           "us-east-1",
		HasTraceAxis:     hasRedrive, // HasTraceAxis = redrive policy present
	}
}

// TestCheckSQSRedrive_Fires: an SQS queue with no redrive policy
// (HasTraceAxis==false) yields a draft with the picker's redrive+DLQ
// Terraform and the canonical kind.
func TestCheckSQSRedrive_Fires(t *testing.T) {
	draft, err := CheckSQSRedrive(context.Background(), sqsRow(false),
		EventSourceScope{ConnectionID: "111122223333", ScopeID: "111122223333", Region: "us-east-1"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draft == nil {
		t.Fatal("expected a draft, got nil")
	}
	if draft.Kind != SQSRedrivePolicyRecommendationKind {
		t.Errorf("kind = %q, want %q", draft.Kind, SQSRedrivePolicyRecommendationKind)
	}
	if !strings.Contains(draft.Terraform, "redrive_policy") {
		t.Errorf("terraform missing redrive_policy:\n%s", draft.Terraform)
	}
	if !strings.Contains(draft.Terraform, "aws_sqs_queue") {
		t.Errorf("terraform missing DLQ queue resource:\n%s", draft.Terraform)
	}
}

// TestCheckSQSRedrive_SkipsWhenRedrivePresent: a queue that already has
// a redrive policy (HasTraceAxis==true) is not recommended.
func TestCheckSQSRedrive_SkipsWhenRedrivePresent(t *testing.T) {
	draft, err := CheckSQSRedrive(context.Background(), sqsRow(true),
		EventSourceScope{ConnectionID: "c", ScopeID: "c"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draft != nil {
		t.Errorf("expected nil draft when redrive policy present, got %+v", draft)
	}
}

// TestCheckSQSRedrive_SkipsNonSQS: an SNS row is not this branch's concern.
func TestCheckSQSRedrive_SkipsNonSQS(t *testing.T) {
	row := sqsRow(false)
	row.Surface = "sns"
	draft, err := CheckSQSRedrive(context.Background(), row,
		EventSourceScope{ConnectionID: "c", ScopeID: "c"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if draft != nil {
		t.Errorf("expected nil draft for non-sqs surface, got %+v", draft)
	}
}

// TestEventSourceChecks_RegistryCoversSNSAndSQS: guards the dispatch
// registry so a new Check must be registered (not just defined) to reach
// production. SNS + SQS are the batch-1 members.
func TestEventSourceChecks_RegistryCoversSNSAndSQS(t *testing.T) {
	if len(EventSourceChecks) < 2 {
		t.Fatalf("expected at least 2 registered checks, got %d", len(EventSourceChecks))
	}
	// SNS row fires exactly one registered check (the SNS one); SQS row
	// fires exactly the SQS one. Confirms the registry dispatches by
	// surface without cross-firing.
	countFires := func(row EventSourceInventoryRow) int {
		n := 0
		for _, c := range EventSourceChecks {
			d, err := c(context.Background(), row, EventSourceScope{}, nil)
			if err != nil {
				t.Fatalf("check error: %v", err)
			}
			if d != nil {
				n++
			}
		}
		return n
	}
	if got := countFires(snsRow(false)); got != 1 {
		t.Errorf("SNS row fired %d checks, want 1", got)
	}
	if got := countFires(sqsRow(false)); got != 1 {
		t.Errorf("SQS row fired %d checks, want 1", got)
	}
}

// --- GCP event-source checks (batch 2) ---

func gcpRow(surface string) EventSourceInventoryRow {
	return EventSourceInventoryRow{
		RecommendationID: "projects/p/locations/us-central1/" + surface + "/q1",
		Provider:         "gcp",
		Surface:          surface,
		ResourceTFName:   "q1",
		ResourceID:       "projects/p/locations/us-central1/" + surface + "/q1",
		Region:           "us-central1",
	}
}

func TestCheckCloudTasksRetryPolicy_FiresAndSkips(t *testing.T) {
	// Fires when no retry policy (HasTraceAxis false).
	d, err := CheckCloudTasksRetryPolicy(context.Background(), gcpRow("cloudtasks"),
		EventSourceScope{ConnectionID: "c", ScopeID: "p"}, nil)
	if err != nil || d == nil {
		t.Fatalf("expected fire, got draft=%v err=%v", d, err)
	}
	if d.Kind != CloudTasksRetryPolicyRecommendationKind {
		t.Errorf("kind = %q", d.Kind)
	}
	// Skips when retry policy present.
	row := gcpRow("cloudtasks")
	row.HasTraceAxis = true
	d2, _ := CheckCloudTasksRetryPolicy(context.Background(), row, EventSourceScope{}, nil)
	if d2 != nil {
		t.Error("expected nil when retry policy present")
	}
}

func TestCheckCloudTasksLogging_FiresAndSkips(t *testing.T) {
	d, err := CheckCloudTasksLogging(context.Background(), gcpRow("cloudtasks"),
		EventSourceScope{ConnectionID: "c", ScopeID: "p"}, nil)
	if err != nil || d == nil || d.Kind != CloudTasksLoggingRecommendationKind {
		t.Fatalf("expected cloudtasks-logging fire, got %v err %v", d, err)
	}
	row := gcpRow("cloudtasks")
	row.HasLogAxis = true
	if d2, _ := CheckCloudTasksLogging(context.Background(), row, EventSourceScope{}, nil); d2 != nil {
		t.Error("expected nil when logging present")
	}
}

func TestCheckPubSubLiteLogging_FiresAndSkips(t *testing.T) {
	d, err := CheckPubSubLiteLogging(context.Background(), gcpRow("pubsublite"),
		EventSourceScope{ConnectionID: "c", ScopeID: "p"}, nil)
	if err != nil || d == nil || d.Kind != PubSubLiteLoggingRecommendationKind {
		t.Fatalf("expected pubsublite-logging fire, got %v err %v", d, err)
	}
	row := gcpRow("pubsublite")
	row.HasLogAxis = true
	if d2, _ := CheckPubSubLiteLogging(context.Background(), row, EventSourceScope{}, nil); d2 != nil {
		t.Error("expected nil when logging present")
	}
}

// TestCheckPubSubLiteReservation_ReadsDetailSignal: reservation fires on
// !HasReservation (the Detail["has_reservation"] signal), NOT on an axis.
func TestCheckPubSubLiteReservation_ReadsDetailSignal(t *testing.T) {
	d, err := CheckPubSubLiteReservation(context.Background(), gcpRow("pubsublite"),
		EventSourceScope{ConnectionID: "c", ScopeID: "p"}, nil)
	if err != nil || d == nil || d.Kind != PubSubLiteReservationRecommendationKind {
		t.Fatalf("expected pubsublite-reservation fire, got %v err %v", d, err)
	}
	row := gcpRow("pubsublite")
	row.HasReservation = true
	if d2, _ := CheckPubSubLiteReservation(context.Background(), row, EventSourceScope{}, nil); d2 != nil {
		t.Error("expected nil when reservation attached")
	}
}

// TestEventSourceChecks_CloudTasksFiresBoth: a Cloud Tasks queue lacking
// both retry policy AND logging fires exactly the two Cloud Tasks checks
// (multi-signal surface), confirming the registry dispatch.
func TestEventSourceChecks_CloudTasksFiresBoth(t *testing.T) {
	row := gcpRow("cloudtasks") // both axes false
	n := 0
	for _, c := range EventSourceChecks {
		if d, _ := c(context.Background(), row, EventSourceScope{}, nil); d != nil {
			n++
		}
	}
	if n != 2 {
		t.Errorf("cloud tasks row fired %d checks, want 2 (retry + logging)", n)
	}
}

// --- Azure event-source checks (batch 3) ---

func azureRow(surface string) EventSourceInventoryRow {
	return EventSourceInventoryRow{
		RecommendationID: "/subscriptions/s/rg/providers/Microsoft.EventGrid/" + surface + "/x",
		Provider:         "azure",
		Surface:          surface,
		ResourceTFName:   "x",
		ResourceID:       "/subscriptions/s/rg/providers/Microsoft.EventGrid/" + surface + "/x",
		Region:           "eastus",
	}
}

func TestCheckEventGridDiagnostics_FiresAndSkips(t *testing.T) {
	d, err := CheckEventGridDiagnostics(context.Background(), azureRow("eventgrid"),
		EventSourceScope{ConnectionID: "c", ScopeID: "s"}, nil)
	if err != nil || d == nil || d.Kind != EventGridDiagnosticsRecommendationKind {
		t.Fatalf("expected eventgrid-diagnostics fire, got %v err %v", d, err)
	}
	row := azureRow("eventgrid")
	row.HasLogAxis = true
	if d2, _ := CheckEventGridDiagnostics(context.Background(), row, EventSourceScope{}, nil); d2 != nil {
		t.Error("expected nil when diagnostics present")
	}
}

func TestCheckEventGridCloudEventSchema_FiresAndSkips(t *testing.T) {
	// Fires when proprietary schema (HasTraceAxis false).
	d, err := CheckEventGridCloudEventSchema(context.Background(), azureRow("eventgrid"),
		EventSourceScope{ConnectionID: "c", ScopeID: "s"}, nil)
	if err != nil || d == nil || d.Kind != EventGridCloudEventRecommendationKind {
		t.Fatalf("expected eventgrid-cloudevent fire, got %v err %v", d, err)
	}
	row := azureRow("eventgrid")
	row.HasTraceAxis = true // already CloudEventSchemaV1_0
	if d2, _ := CheckEventGridCloudEventSchema(context.Background(), row, EventSourceScope{}, nil); d2 != nil {
		t.Error("expected nil when already CloudEvents schema")
	}
}

func TestCheckEventHubsDiagnostics_FiresAndSkips(t *testing.T) {
	d, err := CheckEventHubsDiagnostics(context.Background(), azureRow("eventhubs"),
		EventSourceScope{ConnectionID: "c", ScopeID: "s"}, nil)
	if err != nil || d == nil || d.Kind != EventHubsDiagnosticsRecommendationKind {
		t.Fatalf("expected eventhubs-diagnostics fire, got %v err %v", d, err)
	}
	row := azureRow("eventhubs")
	row.HasLogAxis = true
	if d2, _ := CheckEventHubsDiagnostics(context.Background(), row, EventSourceScope{}, nil); d2 != nil {
		t.Error("expected nil when diagnostics present")
	}
}

// TestCheckEventHubsCapture_ReadsDetailSignal: capture fires on
// !HasCapture (the Detail["has_capture"] signal), NOT on an axis.
func TestCheckEventHubsCapture_ReadsDetailSignal(t *testing.T) {
	d, err := CheckEventHubsCapture(context.Background(), azureRow("eventhubs"),
		EventSourceScope{ConnectionID: "c", ScopeID: "s"}, nil)
	if err != nil || d == nil || d.Kind != EventHubsCaptureRecommendationKind {
		t.Fatalf("expected eventhubs-capture fire, got %v err %v", d, err)
	}
	row := azureRow("eventhubs")
	row.HasCapture = true
	if d2, _ := CheckEventHubsCapture(context.Background(), row, EventSourceScope{}, nil); d2 != nil {
		t.Error("expected nil when a hub has Capture enabled")
	}
}

// TestEventSourceChecks_EventGridFiresBoth: an Event Grid topic lacking
// diagnostics AND using a proprietary schema fires exactly the two Event
// Grid checks.
func TestEventSourceChecks_EventGridFiresBoth(t *testing.T) {
	row := azureRow("eventgrid") // both axes false
	n := 0
	for _, c := range EventSourceChecks {
		if d, _ := c(context.Background(), row, EventSourceScope{}, nil); d != nil {
			n++
		}
	}
	if n != 2 {
		t.Errorf("event grid row fired %d checks, want 2 (diagnostics + schema)", n)
	}
}

// --- OCI ONS logging check (batch 4) ---

func TestCheckONSLogging_FiresAndSkips(t *testing.T) {
	row := EventSourceInventoryRow{
		RecommendationID: "ocid1.onstopic.oc1..abc",
		Provider:         "oci",
		Surface:          "notifications",
		ResourceTFName:   "t1",
		ResourceID:       "ocid1.onstopic.oc1..abc",
		Region:           "us-ashburn-1",
	}
	d, err := CheckONSLogging(context.Background(), row,
		EventSourceScope{ConnectionID: "c", ScopeID: "ten"}, nil)
	if err != nil || d == nil || d.Kind != ONSLoggingRecommendationKind {
		t.Fatalf("expected ons-logging fire, got %v err %v", d, err)
	}
	if !strings.Contains(d.Terraform, "oci_logging_log") {
		t.Errorf("terraform missing oci_logging_log:\n%s", d.Terraform)
	}
	row.HasLogAxis = true
	if d2, _ := CheckONSLogging(context.Background(), row, EventSourceScope{}, nil); d2 != nil {
		t.Error("expected nil when logging present")
	}
	row.HasLogAxis = false
	row.Surface = "streaming"
	if d3, _ := CheckONSLogging(context.Background(), row, EventSourceScope{}, nil); d3 != nil {
		t.Error("expected nil for non-notifications surface")
	}
}
