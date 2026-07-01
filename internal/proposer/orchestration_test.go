// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package proposer

import (
	"context"
	"strings"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

func resmgrRow(hasLog bool) OrchestrationInventoryRow {
	return OrchestrationInventoryRow{
		RecommendationID: "ocid1.ormstack.oc1.iad.prod",
		Provider:         "oci",
		Surface:          "resmgr",
		ResourceTFName:   "prod_stack",
		ResourceID:       "ocid1.ormstack.oc1.iad.prod",
		Region:           "us-ashburn-1",
		HasLogAxis:       hasLog,
	}
}

// TestCheckResourceManagerLogging_Fires: an RM Stack with no RM-source Logging
// in its compartment (HasLogAxis=false) yields a resmgr-logging-enable draft
// whose Terraform is the picker's log-group + SERVICE-log pattern.
func TestCheckResourceManagerLogging_Fires(t *testing.T) {
	draft, err := CheckResourceManagerLogging(context.Background(), resmgrRow(false),
		EventSourceScope{ConnectionID: "conn-1", ScopeID: "ocid1.tenancy", Region: "us-ashburn-1"}, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if draft == nil {
		t.Fatal("draft is nil; want a resmgr-logging-enable recommendation")
	}
	if draft.Kind != ResourceManagerLoggingRecommendationKind {
		t.Errorf("Kind = %q, want %q", draft.Kind, ResourceManagerLoggingRecommendationKind)
	}
	if draft.RecommendationID != "ocid1.ormstack.oc1.iad.prod" {
		t.Errorf("RecommendationID = %q, want the Stack OCID (stable across scans)", draft.RecommendationID)
	}
	// The picker snippet must configure the RM-source SERVICE log.
	for _, want := range []string{"oci_logging_log_group", `service     = "resourcemanager"`, "prod_stack"} {
		if !strings.Contains(draft.Terraform, want) {
			t.Errorf("Terraform missing %q; got:\n%s", want, draft.Terraform)
		}
	}
}

// TestCheckResourceManagerLogging_HasLogAxis_NoFire: a Stack that already has
// RM-source Logging (HasLogAxis=true) produces no recommendation.
func TestCheckResourceManagerLogging_HasLogAxis_NoFire(t *testing.T) {
	draft, err := CheckResourceManagerLogging(context.Background(), resmgrRow(true),
		EventSourceScope{ConnectionID: "c", ScopeID: "t"}, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if draft != nil {
		t.Errorf("draft = %+v, want nil (logging already present)", draft)
	}
}

// TestCheckResourceManagerLogging_WrongSurface_NoFire: the check is surface-
// gated so a non-resmgr row is silently skipped (safe to run over every row).
func TestCheckResourceManagerLogging_WrongSurface_NoFire(t *testing.T) {
	row := resmgrRow(false)
	row.Surface = "notifications"
	draft, err := CheckResourceManagerLogging(context.Background(), row,
		EventSourceScope{ConnectionID: "c", ScopeID: "t"}, nil)
	if err != nil || draft != nil {
		t.Errorf("draft/err = %+v/%v, want nil/nil for a non-resmgr surface", draft, err)
	}
}

// TestCheckResourceManagerLogging_Excluded_NoFire: an operator decline (by
// stable ID) suppresses the recommendation — verdict-learning parity with the
// event-source branches.
func TestCheckResourceManagerLogging_Excluded_NoFire(t *testing.T) {
	ex := stubEventSourceExclusions{excluded: []applicationstore.ExcludedRecommendation{
		{RecommendationID: "ocid1.ormstack.oc1.iad.prod"},
	}}
	draft, err := CheckResourceManagerLogging(context.Background(), resmgrRow(false),
		EventSourceScope{ConnectionID: "conn-1", ScopeID: "ocid1.tenancy", Region: "us-ashburn-1"}, ex)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if draft != nil {
		t.Errorf("draft = %+v, want nil (excluded by operator decline)", draft)
	}
}
