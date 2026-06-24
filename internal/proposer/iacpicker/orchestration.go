// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import "fmt"

// PickResourceManagerLoggingPattern emits the Terraform snippet for a
// resmgr-logging-enable recommendation per orchestration tier slice 2
// chunk 2 (v0.89.136, #776 Stream 174). Configures an OCI Logging
// log group + log resource with Resource Manager as the source
// service per §8 of the design doc
// (docs/proposals/orchestration-tier-slice2.md).
//
// Slice 2 closes the qualification on the orchestration tier's
// universal claim. After this arc, every tier (compute / database /
// kubernetes / serverless / orchestration / event sources) is
// cleanly 4-cloud without an asterisk. Resource Manager is OCI's
// infrastructure orchestration primitive (Terraform-as-a-service);
// Process Automation (BPMN) is the semantically closer match to
// Step Functions / Workflows / Logic Apps but deferred to slice 3
// because of smaller adoption.
//
// The emitted Terraform configures:
//   - oci_logging_log_group at compartment scope (display name
//     "resmgr-stack-logs")
//   - oci_logging_log with log_type=SERVICE and
//     configuration.source.service="resourcemanager",
//     source_type="OCISERVICE", pointing at the Stack resource
//
// The detection rule fires when the OCI scanner's
// listResourceManagerLogSources walk returns zero log resources
// in the Stack's compartment whose Configuration.Source.Service
// matches OCIResourceManagerLogSourceService ("resourcemanager"
// from internal/discovery/oci/scanner_resmgr.go). The slice 2
// chunk 1 scanner sets HasLogAxis=false in that case; the
// proposer routes the resmgr- prefix to OCI via the chunk 2
// webhook extension in internal/api/handlers/iac_github_webhook.go.
//
// row.ResourceTFName is the best-effort Terraform resource name
// the proposer extracted from the operator's repo. When empty,
// the snippet falls back to "<name>" so the operator can
// substitute the real Stack name during review. The reasoning
// text reuses the slice 1 honest-framing pattern from
// stepfunc-/workflows-/logicapps-: the operator may have
// intentionally chosen a non-Logging destination, and the
// verdict learning loop records the decline.
func PickResourceManagerLoggingPattern(row RecommendationContext) (terraform, reasoning string) {
	name := row.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	terraform = fmt.Sprintf(`resource "oci_logging_log_group" "resmgr_%s" {
  compartment_id = var.compartment_ocid
  display_name   = "resmgr-stack-logs"
}

resource "oci_logging_log" "resmgr_%s" {
  log_group_id = oci_logging_log_group.resmgr_%s.id
  display_name = "stack-events"
  log_type     = "SERVICE"
  is_enabled   = true

  configuration {
    source {
      category    = "all"
      resource    = oci_resourcemanager_stack.%s.id
      service     = "resourcemanager"
      source_type = "OCISERVICE"
    }
    compartment_id = var.compartment_ocid
  }
}
`, name, name, name, name)

	reasoning = "OCI Resource Manager Stacks emit Job (apply/destroy) events to OCI Logging when a log group is configured with service=resourcemanager. Without Logging, failed Job operations leave no audit trail beyond the OCI console. If your team uses a non-OCI-Logging observability destination, decline this PR — the verdict learning loop records."

	return
}
