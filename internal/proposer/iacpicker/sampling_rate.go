// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"fmt"
)

// sampling_rate.go — Sampling rate analysis slice 1 chunk 2
// (v0.89.123, #763 Stream 161). Terraform pattern emitter for the
// span-quality-sampling-too-aggressive recommendation. Lives
// alongside the cold-start + trace-emission + span-quality emitters
// so the proposer's draft pipeline consumes one shape regardless of
// which detection branch produced the draft.
//
// The pattern injects OTEL_TRACES_SAMPLER_ARG=0.5 env var into the
// per-cloud serverless resource. 0.5 (50%) is the §8 OPERATOR-
// TUNABLE STARTING POINT — the recommendation suggests it as a
// baseline, not a target; operators tune from there based on cost
// tolerance + signal-to-noise. The picker's comments call this out
// explicitly so the operator reading the PR knows the value is a
// suggestion not a prescription.
//
// See docs/proposals/sampling-rate-analysis-slice1.md §8 +
// Terraform pattern per cloud.

// PickSamplingRateTerraform returns the Terraform snippet for
// raising the OTEL_TRACES_SAMPLER_ARG env var on the per-cloud
// serverless resource. The default raise target is 0.5 (50%) —
// the §8 operator-tunable starting point.
//
// Per-cloud branches mirror the cold-start per-cloud emitter
// pattern: each cloud's env var injection mechanism gets a
// distinct snippet, but the env var name is uniform across all 5
// surfaces (the OpenTelemetry SDK convention).
//
// surface is needed for GCP because Cloud Run and Cloud Functions
// use different Terraform resource types
// (google_cloud_run_service vs google_cloudfunctions2_function);
// AWS / Azure / OCI each have one shape per provider.
//
// When the provider isn't in the slice 1 set, returns "" — the
// caller treats an empty snippet as "no pattern" and skips
// emitting the draft. The per-cloud Check helpers in
// internal/proposer/sampling_rate.go gate on surface before
// calling this, so empty isn't reachable from production code; the
// defensive fall-through keeps the func total.
func PickSamplingRateTerraform(provider, surface, resourceTFName string) string {
	name := resourceTFName
	if name == "" {
		name = "<name>"
	}
	switch provider {
	case "aws":
		// AWS Lambda — env var injection on aws_lambda_function.
		// The environment.variables block is the canonical OTel
		// env var attachment surface. Operator tunes 0.5 to
		// match their cost / signal trade-off.
		return fmt.Sprintf(`resource "aws_lambda_function" "%s" {
  # ... existing fields ...
  environment {
    variables = {
      OTEL_TRACES_SAMPLER     = "parentbased_traceidratio"  # ratio sampler — REQUIRED for the ARG below to take effect (default sampler ignores it)
      OTEL_TRACES_SAMPLER_ARG = "0.5"  # operator tunes; starting point per Squadron\'s sampling-rate analysis
    }
  }
}
`, name)
	case "gcp":
		if surface == "cloudrun" {
			// GCP Cloud Run — env injection on the container
			// block. The template / spec / containers / env
			// nesting mirrors the v1 Cloud Run Terraform shape.
			return fmt.Sprintf(`resource "google_cloud_run_service" "%s" {
  # ... existing fields ...
  template {
    spec {
      containers {
        env {
          name  = "OTEL_TRACES_SAMPLER"
          value = "parentbased_traceidratio"  # ratio sampler — REQUIRED for the ARG below to take effect (default sampler ignores it)
        }
        env {
          name  = "OTEL_TRACES_SAMPLER_ARG"
          value = "0.5"  # operator tunes; starting point per Squadron's sampling-rate analysis
        }
      }
    }
  }
}
`, name)
		}
		// GCP Cloud Functions (Gen 2) — env injection on the
		// service_config block. Gen 1 uses a different shape
		// (environment_variables top-level); slice 1 targets
		// Gen 2 because the Gen 1 surface is end-of-life.
		return fmt.Sprintf(`resource "google_cloudfunctions2_function" "%s" {
  # ... existing fields ...
  service_config {
    environment_variables = {
      OTEL_TRACES_SAMPLER     = "parentbased_traceidratio"  # ratio sampler — REQUIRED for the ARG below to take effect (default sampler ignores it)
      OTEL_TRACES_SAMPLER_ARG = "0.5"  # operator tunes; starting point per Squadron\'s sampling-rate analysis
    }
  }
}
`, name)
	case "azure":
		// Azure Functions — app_settings map on
		// azurerm_linux_function_app. The Windows variant uses
		// azurerm_windows_function_app with the same app_settings
		// surface; slice 1 emits the linux shape since the OTel
		// auto-instrumentation story is most mature on linux.
		return fmt.Sprintf(`resource "azurerm_linux_function_app" "%s" {
  # ... existing fields ...
  app_settings = {
    OTEL_TRACES_SAMPLER     = "parentbased_traceidratio"  # ratio sampler — REQUIRED for the ARG below to take effect (default sampler ignores it)
    OTEL_TRACES_SAMPLER_ARG = "0.5"  # operator tunes; starting point per Squadron\'s sampling-rate analysis
  }
}
`, name)
	case "oci":
		// OCI Functions — config map on oci_functions_function.
		// OCI's config block is the OTel-equivalent of AWS's
		// environment.variables — the function runtime reads
		// these as env vars at invocation time.
		return fmt.Sprintf(`resource "oci_functions_function" "%s" {
  # ... existing fields ...
  config = {
    OTEL_TRACES_SAMPLER     = "parentbased_traceidratio"  # ratio sampler — REQUIRED for the ARG below to take effect (default sampler ignores it)
    OTEL_TRACES_SAMPLER_ARG = "0.5"  # operator tunes; starting point per Squadron\'s sampling-rate analysis
  }
}
`, name)
	}
	return ""
}
