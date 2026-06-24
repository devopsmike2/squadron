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

// --- slice 2 (v0.89.110) traceparent patterns ---------------------------
//
// Two new pickers for the slice 2 W3C trace context kinds:
//   - PickMalformedTraceparentPattern: SDK version pin per deployment
//     shape. The shape varies sharply across deployment forms (Lambda
//     layer ARN vs Kubernetes image tag vs Azure app_setting), so the
//     picker emits a generic placeholder that operators tune to their
//     deployment. The reasoning enumerates the per-cloud knobs the
//     operator can swap in.
//   - PickMissingTraceparentPattern: same propagator-injection shape as
//     PickOrphanTracePattern's compute path — both come down to
//     OTEL_PROPAGATORS=tracecontext,baggage on the runtime env. We
//     reuse orphanComputeTerraformFor / the k8s Instrumentation CRD
//     directly so the two kinds emit byte-identical Terraform when the
//     same provider+tier combination fires.
//
// COLD-START PARITY (v0.89.110): when the detection branch doesn't fire
// these kinds (no observation crossed the §3 traceparent thresholds),
// the pickers below are never invoked and no Terraform / reasoning
// appears in the prompt. Mirrors the slice 1 picker posture.

// PickMalformedTraceparentPattern emits the SDK version pin pattern
// for the span-quality-traceparent-malformed kind. The specific
// Terraform shape depends on the deployment form (Lambda layer ARN,
// Kubernetes image tag, Azure app_setting, OCI Functions image tag);
// slice 2 chunk 2 ships a generic placeholder + per-cloud reasoning
// the operator tunes to their deployment.
func PickMalformedTraceparentPattern(ctx RecommendationContext) PickedPattern {
	switch ctx.Tier {
	case "k8s":
		return PickedPattern{
			PrimaryTerraform: `# Pin the OpenTelemetry SDK image to the latest W3C-compliant release.
# Replace <image>:<tag> with the operator's actual workload image
# and the desired SDK / OTel distro version per the upstream changelog.
resource "kubernetes_manifest" "otel_sdk_version_pin" {
  manifest = {
    apiVersion = "apps/v1"
    kind       = "Deployment"
    metadata = {
      name      = "squadron-target-workload"
      namespace = "default"
    }
    spec = {
      template = {
        spec = {
          containers = [{
            name  = "app"
            image = "<image>:<tag>"
          }]
        }
      }
    }
  }
}
`,
			Reasoning: "span-quality-traceparent-malformed (k8s): pin the Deployment's image tag to the SDK release that's W3C-compliant for the runtime in use. The operator swaps <image>:<tag> for their actual workload image; the picker can't see the image directly in slice 2 chunk 2.",
		}
	case "db":
		return PickedPattern{
			PrimaryTerraform: "",
			Reasoning:        "span-quality-traceparent-malformed (db): databases don't carry an OTel SDK directly; the malformed-traceparent signal indicates the connecting application's SDK emits non-W3C trace IDs. Apply the corresponding application-side recommendation.",
			FallbackUsed:     true,
		}
	default: // compute
		return PickedPattern{
			PrimaryTerraform: malformedTraceparentComputeTerraformFor(ctx.Provider),
			Reasoning:        "span-quality-traceparent-malformed (compute): pin the runtime's SDK version (Lambda layer ARN / Azure app_setting / etc.) to the latest W3C-compliant release. Operator swaps the version literal for their target release.",
		}
	}
}

// malformedTraceparentComputeTerraformFor selects the per-cloud SDK
// version-pin shape. The slice 2 chunk 2 patterns are generic
// placeholders — operators tune the version literal to the upstream
// changelog for their runtime + distro combination.
func malformedTraceparentComputeTerraformFor(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "aws":
		return `# Pin the AWS Distro for OpenTelemetry Lambda layer to the latest
# W3C-compliant release. The operator picks the layer ARN matching
# the function's runtime; the example below uses the nodejs amd64
# layer at a placeholder version.
resource "aws_lambda_function" "target" {
  # ... existing fields ...
  layers = [
    "arn:aws:lambda:${var.region}:901920570463:layer:aws-otel-nodejs-amd64-ver-1-32-0:1",
  ]
}
`
	case "gcp":
		return `# Pin the OpenTelemetry GCE metadata key carrying the SDK image / distro
# version so a fresh boot picks up the W3C-compliant release. The operator
# tunes the version literal to the upstream changelog.
resource "google_compute_project_metadata_item" "otel_sdk_version" {
  key   = "OTEL_SDK_VERSION"
  value = "1.32.0"
}
`
	case "azure":
		return `# Pin the OpenTelemetry distro version on the Azure VM extension /
# App Service app_setting. The operator chooses the carrier per
# their deployment shape and tunes the version literal.
resource "azurerm_linux_web_app" "target" {
  # ... existing fields ...
  app_settings = {
    "OTEL_DOTNET_AUTO_HOME"   = "/otel-auto-instrumentation"
    "OTEL_DOTNET_AUTO_VERSION" = "1.7.0"
  }
}
`
	case "oci":
		return `# Pin the OpenTelemetry distro version on the OCI Functions image tag.
# The operator chooses the image registry + tag matching the W3C-
# compliant release for their runtime.
resource "oci_functions_function" "target" {
  # ... existing fields ...
  image = "<registry>/squadron-otel-runtime:1.32.0"
}
`
	}
	return ""
}

// PickMissingTraceparentPattern emits the OTEL_PROPAGATORS injection
// pattern for the span-quality-traceparent-missing kind. Same env-var
// shape as PickOrphanTracePattern's compute path — both kinds come
// down to enabling tracecontext + baggage on the SDK config. Per-tier
// dispatch mirrors PickOrphanTracePattern so the two kinds emit
// byte-identical Terraform when the same provider+tier combination
// fires (the operator merging one is automatically protected from
// having to merge a near-duplicate for the other).
func PickMissingTraceparentPattern(ctx RecommendationContext) PickedPattern {
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
			Reasoning: "span-quality-traceparent-missing (k8s): enabling tracecontext + baggage propagators on the OpenTelemetry Operator Instrumentation CRD so child spans inherit the upstream traceparent.",
		}
	case "db":
		return PickedPattern{
			PrimaryTerraform: "",
			Reasoning:        "span-quality-traceparent-missing (db): databases don't carry an OTel SDK directly; the missing-on-child signal indicates the connecting application isn't propagating. Apply the corresponding application-side recommendation.",
			FallbackUsed:     true,
		}
	default: // compute
		return PickedPattern{
			PrimaryTerraform: orphanComputeTerraformFor(ctx.Provider),
			Reasoning:        "span-quality-traceparent-missing (compute): injecting OTEL_PROPAGATORS=tracecontext,baggage on the resource's runtime environment so the SDK's HTTP server instrumentation extracts the W3C context propagator on the inbound request.",
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
