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

const resmgrStackOCID = "ocid1.ormstack.oc1.iad.prod"

// TestAppendOCIOrchestrationRecs_Fires: an RM Stack scanned with HasLogAxis=false
// (no RM-source Logging in its compartment) yields one resmgr-logging-enable
// recommendation carrying the picker's Terraform, mapped onto the wire envelope
// so it renders + opens a PR like any step.
func TestAppendOCIOrchestrationRecs_Fires(t *testing.T) {
	h := &DiscoveryOCIHandlers{exclusionStore: &fakeExclusionStore{}}
	rows := []awsOrchestrationRow{{
		Provider:     "oci",
		Surface:      "resmgr",
		ResourceName: "prod_stack",
		ResourceARN:  resmgrStackOCID,
		Region:       "us-ashburn-1",
		WorkflowType: "Stack",
		HasLogAxis:   false,
	}}

	var recs []recommendations.Recommendation
	h.appendOCIOrchestrationRecs(context.Background(), &recs, rows,
		"conn-1", "ocid1.tenancy", "us-ashburn-1", "scan-1", time.Now().UTC())

	if len(recs) != 1 {
		t.Fatalf("want 1 orchestration rec, got %d", len(recs))
	}
	got := recs[0]
	if got.ResourceKind != proposer.ResourceManagerLoggingRecommendationKind {
		t.Errorf("ResourceKind = %q, want %q", got.ResourceKind, proposer.ResourceManagerLoggingRecommendationKind)
	}
	if got.ID != resmgrStackOCID {
		t.Errorf("ID = %q, want %q (stable across scans)", got.ID, resmgrStackOCID)
	}
	if got.IaC == nil || got.IaC.Source == "" {
		t.Error("expected a Terraform IaC snippet on the orchestration rec")
	}
	if len(got.AffectedResources) != 1 || got.AffectedResources[0] != resmgrStackOCID {
		t.Errorf("AffectedResources = %v, want [%s]", got.AffectedResources, resmgrStackOCID)
	}
}

// TestAppendOCIOrchestrationRecs_HasLogAxis_NoRec: a Stack that already has
// RM-source Logging yields no recommendation.
func TestAppendOCIOrchestrationRecs_HasLogAxis_NoRec(t *testing.T) {
	h := &DiscoveryOCIHandlers{exclusionStore: &fakeExclusionStore{}}
	rows := []awsOrchestrationRow{{
		Provider: "oci", Surface: "resmgr", ResourceName: "logged_stack",
		ResourceARN: resmgrStackOCID, Region: "us-ashburn-1", HasLogAxis: true,
	}}

	var recs []recommendations.Recommendation
	h.appendOCIOrchestrationRecs(context.Background(), &recs, rows,
		"conn-1", "ocid1.tenancy", "us-ashburn-1", "scan-1", time.Now().UTC())

	if len(recs) != 0 {
		t.Fatalf("logging present: want 0 recs, got %d", len(recs))
	}
}

// TestAppendOCIOrchestrationRecs_Excluded: an operator decline (by stable ID)
// suppresses the recommendation — the verdict-learning posture the rest of the
// discovery recs honor.
func TestAppendOCIOrchestrationRecs_Excluded(t *testing.T) {
	excl := &fakeExclusionStore{seeded: []types.ExcludedRecommendation{{
		RecommendationID: resmgrStackOCID,
		ConnectionID:     "conn-1",
		AccountID:        "ocid1.tenancy",
		Region:           "us-ashburn-1",
	}}}
	h := &DiscoveryOCIHandlers{exclusionStore: excl}
	rows := []awsOrchestrationRow{{
		Provider: "oci", Surface: "resmgr", ResourceName: "prod_stack",
		ResourceARN: resmgrStackOCID, Region: "us-ashburn-1", HasLogAxis: false,
	}}

	var recs []recommendations.Recommendation
	h.appendOCIOrchestrationRecs(context.Background(), &recs, rows,
		"conn-1", "ocid1.tenancy", "us-ashburn-1", "scan-1", time.Now().UTC())

	if len(recs) != 0 {
		t.Fatalf("excluded rec: want 0 recs, got %d", len(recs))
	}
}
