// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
)

// Quality-specific Terraform pattern emitters per
// docs/proposals/span-quality-slice1.md §4. Each function returns a
// PickedPattern (Terraform + reasoning) the chunk-2 detection branch
// threads into the recommendation draft. The reasoning is operator-
// facing — paired with the per-kind reasoning template from
// CheckSpanQualityIssues, NOT a duplicate of it; the picker tells the
// operator WHICH terraform path Squadron chose, the detection branch
// tells the operator WHY the recommendation fired.
//
// Three kinds, one emitter each. Each emitter is per-cloud-aware: the
// AWS / GCP / Azure / OCI Terraform shapes differ enough that a single
// generic pattern would either over-document or under-document the
// specific knobs the operator needs to turn. The cloud is sourced
// from ctx.Provider, the tier from ctx.Tier (compute / db / k8s),
// matching the existing trace-emission picker's dispatch signature.

// --- 4.1 orphan trace ---------------------------------------------------

// PickOrphanTracePattern emits the §4.1 Terraform pattern for the
// span-quality-orphan-trace kind. Cause: broken W3C trace context
// propagation across HTTP or queue boundaries; remedy: enable the
// tracecontext + baggage propagators on the SDK config.
//
// Per-tier dispatch:
//   - compute: ADOT collector config edit (the collector emits a
//     YAML config; Squadron adds the propagators block via a
//     templatefile + cloud-init / user-data update).
//   - k8s: Instrumentation CRD edit (the OpenTelemetry Operator's
//     CRD carries the propagators in spec.propagators).
//   - db: redirect; databases don't carry an SDK directly, so the
//     orphan signal here means the application connecting to them
//     isn't propagating. The picker returns a reasoning-only result
//     pointing the operator at the application-side recommendation.
func PickOrphanTracePattern(ctx RecommendationContext) PickedPattern {
	switch ctx.Tier {
	case "k8s":
		return PickedPattern{
			PrimaryTerraform: `resource "kubernetes_manifest" "otel_instrumentation" {
  manifest = {
    apiVersion = "opentelemetry.io/v1alpha1"
    kind       = "Instrumentation"
    metadata = {
      name      = "squadron-trace-context"
      namespace = "observability"
    }
    spec = {
      propagators = ["tracecontext", "baggage"]
      sampler = {
        type     = "parentbased_traceidratio"
        argument = "1.0"
      }
    }
  }
}
`,
			Reasoning: "span-quality-orphan-trace (k8s): enabling tracecontext + baggage propagators on the OpenTelemetry Operator Instrumentation CRD per §4.1.",
		}
	case "db":
		return PickedPattern{
			PrimaryTerraform: "",
			Reasoning:        "span-quality-orphan-trace (db): databases don't carry an OTel SDK directly; the orphan signal indicates the connecting application isn't propagating context. Apply the corresponding application-side recommendation rather than patching the database resource.",
			FallbackUsed:     true,
		}
	default: // compute
		return PickedPattern{
			PrimaryTerraform: orphanComputeTerraformFor(ctx.Provider),
			Reasoning:        "span-quality-orphan-trace (compute): injecting OTEL_PROPAGATORS=tracecontext,baggage on the resource's runtime environment per §4.1.",
		}
	}
}

// orphanComputeTerraformFor selects the per-cloud compute Terraform
// shape for the orphan-trace propagator injection. Provider-specific
// because the env-var carrier differs: EC2 uses user-data, ECS uses
// task-definition env, GCE uses metadata, Azure VMs use extension,
// OCI uses user-data. The shapes here are the smallest patch the
// operator can review in one read.
func orphanComputeTerraformFor(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "aws":
		return `resource "aws_ssm_parameter" "otel_propagators" {
  name  = "/squadron/otel/propagators"
  type  = "String"
  value = "tracecontext,baggage"
  tags = {
    "managed-by" = "squadron"
  }
}
`
	case "gcp":
		return `resource "google_compute_project_metadata_item" "otel_propagators" {
  key   = "OTEL_PROPAGATORS"
  value = "tracecontext,baggage"
}
`
	case "azure":
		return `resource "azurerm_virtual_machine_extension" "otel_propagators" {
  name                 = "squadron-otel-propagators"
  virtual_machine_id   = azurerm_linux_virtual_machine.target.id
  publisher            = "Microsoft.Azure.Extensions"
  type                 = "CustomScript"
  type_handler_version = "2.1"
  settings = jsonencode({
    commandToExecute = "echo 'OTEL_PROPAGATORS=tracecontext,baggage' >> /etc/environment"
  })
}
`
	case "oci":
		return `resource "oci_core_instance_configuration" "otel_propagators" {
  compartment_id = var.compartment_ocid
  display_name   = "squadron-otel-propagators"
  instance_details {
    instance_type = "compute"
    launch_details {
      metadata = {
        "user_data" = base64encode("#!/bin/bash\necho OTEL_PROPAGATORS=tracecontext,baggage >> /etc/environment")
      }
    }
  }
}
`
	}
	return ""
}

// --- 4.2 missing resource attributes ------------------------------------

// PickMissingAttrsPattern emits the §4.2 Terraform pattern for the
// span-quality-missing-resource-attrs kind. Cause: the OTel SDK's
// resource detector ran with insufficient permissions or before the
// cloud metadata service was reachable; remedy: IAM permission
// adjustments + env-var wait-for-metadata.
//
// Per-cloud dispatch — the IAM block differs sharply across the four
// clouds. The picker always returns a patch the operator can review
// in one read; the slice-2 "actually parse the existing role" deep
// dive is out of scope for slice 1.
func PickMissingAttrsPattern(ctx RecommendationContext) PickedPattern {
	switch strings.ToLower(strings.TrimSpace(ctx.Provider)) {
	case "aws":
		return PickedPattern{
			PrimaryTerraform: `resource "aws_iam_policy" "squadron_otel_resource_detector" {
  name = "squadron-otel-resource-detector"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ec2:DescribeInstances", "ec2:DescribeTags"]
      Resource = "*"
    }]
  })
}
`,
			Reasoning: "span-quality-missing-resource-attrs (aws): adding ec2:DescribeInstances + ec2:DescribeTags so the OTel resource detector can populate host.id / cloud.account.id / cloud.region per §4.2.",
		}
	case "gcp":
		return PickedPattern{
			PrimaryTerraform: `resource "google_project_iam_member" "squadron_otel_metadata_reader" {
  project = var.project_id
  role    = "roles/compute.viewer"
  member  = "serviceAccount:${google_service_account.workload.email}"
}
`,
			Reasoning: "span-quality-missing-resource-attrs (gcp): granting roles/compute.viewer so the workload service account can reach the metadata server for cloud.account.id / cloud.region detection per §4.2.",
		}
	case "azure":
		return PickedPattern{
			PrimaryTerraform: `resource "azurerm_user_assigned_identity" "squadron_otel" {
  name                = "squadron-otel-resource-detector"
  resource_group_name = var.resource_group
  location            = var.location
}

resource "azurerm_role_assignment" "squadron_otel_reader" {
  scope                = data.azurerm_subscription.current.id
  role_definition_name = "Reader"
  principal_id         = azurerm_user_assigned_identity.squadron_otel.principal_id
}
`,
			Reasoning: "span-quality-missing-resource-attrs (azure): provisioning a managed identity with subscription Reader so the OTel azure_detector can populate cloud.account.id / cloud.region per §4.2.",
		}
	case "oci":
		return PickedPattern{
			PrimaryTerraform: `resource "oci_identity_dynamic_group" "squadron_otel_instances" {
  compartment_id = var.tenancy_ocid
  description    = "instances that may auth as themselves for OTel resource detection"
  matching_rule  = "ALL {instance.compartment.id = '${var.compartment_ocid}'}"
  name           = "squadron-otel-instance-principals"
}

resource "oci_identity_policy" "squadron_otel_metadata" {
  compartment_id = var.tenancy_ocid
  description    = "allow squadron-otel-instance-principals to read instance metadata"
  name           = "squadron-otel-metadata-read"
  statements = [
    "allow dynamic-group squadron-otel-instance-principals to inspect instances in tenancy",
  ]
}
`,
			Reasoning: "span-quality-missing-resource-attrs (oci): configuring instance-principal auth + a policy so the OTel oci_detector can populate cloud.account.id / cloud.region per §4.2.",
		}
	}
	return PickedPattern{
		PrimaryTerraform: "",
		Reasoning:        "span-quality-missing-resource-attrs: unrecognized provider; no per-cloud IAM pattern available.",
		FallbackUsed:     true,
	}
}

// --- 4.3 attribute mismatch / placeholder values ------------------------

// PickMismatchPattern emits the §4.3 Terraform pattern for the
// span-quality-attribute-mismatch kind. Cause: the OTel SDK fell back
// to default values when the resource detector failed silently;
// remedy: explicit OTEL_RESOURCE_ATTRIBUTES env-var injection
// hardcoding the correct values from the inventory row Squadron
// already has.
//
// Per-tier dispatch:
//   - compute: EC2 user-data / ECS task-def env / Azure VM extension
//     / OCI cloud-init.
//   - k8s: Deployment env block (the most reliable injection point;
//     pod restart picks it up immediately).
//   - db: not directly applicable — same redirect posture as
//     PickOrphanTracePattern's db case.
//
// The {RESOURCE_ID_PLACEHOLDER} token in the snippet is replaced by
// the caller at draft time with the inventory row's canonical ID;
// the picker can't see the row's ID directly via RecommendationContext
// in slice 1 (the context carries the Terraform name, not the cloud
// ID — adding it would expand the slice-1 surface). Slice 2 candidate.
func PickMismatchPattern(ctx RecommendationContext) PickedPattern {
	switch ctx.Tier {
	case "k8s":
		return PickedPattern{
			PrimaryTerraform: `resource "kubernetes_manifest" "otel_resource_attrs" {
  manifest = {
    apiVersion = "v1"
    kind       = "ConfigMap"
    metadata = {
      name      = "squadron-otel-resource-attrs"
      namespace = "observability"
    }
    data = {
      "OTEL_RESOURCE_ATTRIBUTES" = "cloud.provider=${var.cloud_provider},cloud.account.id=${var.account_id},cloud.region=${var.region},service.name=${var.service_name}"
    }
  }
}
`,
			Reasoning: "span-quality-attribute-mismatch (k8s): injecting OTEL_RESOURCE_ATTRIBUTES via ConfigMap so workloads override the SDK's silent placeholder fallback per §4.3.",
		}
	case "db":
		return PickedPattern{
			PrimaryTerraform: "",
			Reasoning:        "span-quality-attribute-mismatch (db): databases don't emit OTel spans directly; the mismatch signal indicates the connecting application's SDK is emitting placeholders. Apply the corresponding application-side recommendation.",
			FallbackUsed:     true,
		}
	default: // compute
		return PickedPattern{
			PrimaryTerraform: mismatchComputeTerraformFor(ctx.Provider, ctx.ResourceTFName),
			Reasoning:        "span-quality-attribute-mismatch (compute): injecting explicit OTEL_RESOURCE_ATTRIBUTES on the resource's runtime environment per §4.3.",
		}
	}
}

// mismatchComputeTerraformFor selects the per-cloud compute Terraform
// shape for OTEL_RESOURCE_ATTRIBUTES injection. The shapes here
// follow §4.3's "from the inventory row" stance — variables stand in
// for the values Squadron threads in at draft time.
func mismatchComputeTerraformFor(provider, tfName string) string {
	if tfName == "" {
		tfName = "<name>"
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "aws":
		return `resource "aws_ecs_task_definition" "` + tfName + `" {
  # ... existing fields ...
  container_definitions = jsonencode([{
    name = "app"
    environment = [
      { name = "OTEL_RESOURCE_ATTRIBUTES",
        value = "cloud.provider=aws,cloud.account.id=${var.account_id},cloud.region=${var.region},service.name=${var.service_name}" }
    ]
  }])
}
`
	case "gcp":
		return `resource "google_compute_instance" "` + tfName + `" {
  # ... existing fields ...
  metadata = {
    "OTEL_RESOURCE_ATTRIBUTES" = "cloud.provider=gcp,cloud.account.id=${var.project_id},cloud.region=${var.region},service.name=${var.service_name}"
  }
}
`
	case "azure":
		return `resource "azurerm_linux_virtual_machine" "` + tfName + `" {
  # ... existing fields ...
  custom_data = base64encode(<<-EOT
    #!/bin/bash
    echo 'OTEL_RESOURCE_ATTRIBUTES=cloud.provider=azure,cloud.account.id=${var.subscription_id},cloud.region=${var.location},service.name=${var.service_name}' >> /etc/environment
  EOT
  )
}
`
	case "oci":
		return `resource "oci_core_instance" "` + tfName + `" {
  # ... existing fields ...
  metadata = {
    "user_data" = base64encode(<<-EOT
      #!/bin/bash
      echo 'OTEL_RESOURCE_ATTRIBUTES=cloud.provider=oci,cloud.account.id=${var.tenancy_ocid},cloud.region=${var.region},service.name=${var.service_name}' >> /etc/environment
    EOT
    )
  }
}
`
	}
	return ""
}
