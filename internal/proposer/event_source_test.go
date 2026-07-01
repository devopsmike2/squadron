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
