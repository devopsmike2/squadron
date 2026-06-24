// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
	"testing"
)

// TestPickResourceManagerLoggingPattern_IncludesLogGroupResource —
// orchestration tier slice 2 chunk 2 (v0.89.136, #776 Stream 174).
// The Terraform snippet must include the oci_logging_log_group
// resource with a compartment_id reference, since the log resource
// the picker emits depends on the log_group_id from this block.
func TestPickResourceManagerLoggingPattern_IncludesLogGroupResource(t *testing.T) {
	tf, _ := PickResourceManagerLoggingPattern(RecommendationContext{
		Provider:       "oci",
		ResourceTFName: "production_stack",
	})
	if !strings.Contains(tf, `resource "oci_logging_log_group" "resmgr_production_stack"`) {
		t.Errorf("expected oci_logging_log_group resource block for production_stack, got:\n%s", tf)
	}
	if !strings.Contains(tf, "compartment_id = var.compartment_ocid") {
		t.Errorf("expected compartment_id = var.compartment_ocid reference, got:\n%s", tf)
	}
	if !strings.Contains(tf, `display_name   = "resmgr-stack-logs"`) {
		t.Errorf("expected display_name = \"resmgr-stack-logs\", got:\n%s", tf)
	}
}

// TestPickResourceManagerLoggingPattern_IncludesSourceServiceResourcemanager
// — the configuration.source.service value MUST be "resourcemanager".
// The OCI scanner from chunk 1 (internal/discovery/oci/scanner_resmgr.go)
// detects has_log_axis=true ONLY when a log resource carries this
// exact service identifier per §3.4 of the design doc; the proposer's
// emitted Terraform has to match.
func TestPickResourceManagerLoggingPattern_IncludesSourceServiceResourcemanager(t *testing.T) {
	tf, _ := PickResourceManagerLoggingPattern(RecommendationContext{
		Provider:       "oci",
		ResourceTFName: "infra",
	})
	if !strings.Contains(tf, `service     = "resourcemanager"`) {
		t.Errorf("expected configuration.source.service = \"resourcemanager\", got:\n%s", tf)
	}
	if !strings.Contains(tf, `source_type = "OCISERVICE"`) {
		t.Errorf("expected configuration.source.source_type = \"OCISERVICE\", got:\n%s", tf)
	}
	if !strings.Contains(tf, `log_type     = "SERVICE"`) {
		t.Errorf("expected log_type = \"SERVICE\", got:\n%s", tf)
	}
	if !strings.Contains(tf, "oci_resourcemanager_stack.infra.id") {
		t.Errorf("expected source.resource pointing at the Stack resource, got:\n%s", tf)
	}
}

// TestPickResourceManagerLoggingPattern_ReasoningMentionsDeclinePath —
// the reasoning string must surface the slice 1 honest-framing
// pattern: operators using non-OCI-Logging observability destinations
// should decline. The verdict learning loop records.
func TestPickResourceManagerLoggingPattern_ReasoningMentionsDeclinePath(t *testing.T) {
	_, reasoning := PickResourceManagerLoggingPattern(RecommendationContext{
		Provider:       "oci",
		ResourceTFName: "stack_a",
	})
	for _, token := range []string{
		"decline this PR",
		"non-OCI-Logging observability destination",
		"verdict learning loop",
		"Resource Manager Stacks",
	} {
		if !strings.Contains(reasoning, token) {
			t.Errorf("reasoning missing %q, got: %s", token, reasoning)
		}
	}
}

// TestPickResourceManagerLoggingPattern_EmptyResourceName_FallsBack
// — when the proposer cannot recover the Terraform resource name
// from the operator's repo, the snippet falls back to "<name>" so the
// operator can substitute it during review. Mirrors pickAWSDB /
// pickAWSK8s in picker.go.
func TestPickResourceManagerLoggingPattern_EmptyResourceName_FallsBack(t *testing.T) {
	tf, _ := PickResourceManagerLoggingPattern(RecommendationContext{
		Provider:       "oci",
		ResourceTFName: "",
	})
	if !strings.Contains(tf, "resmgr_<name>") {
		t.Errorf("expected fallback resmgr_<name> label, got:\n%s", tf)
	}
}
