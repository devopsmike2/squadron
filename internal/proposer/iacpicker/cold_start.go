// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"fmt"
)

// Cold-start latency analysis slice 1 chunk 3 (v0.89.115, #753 Stream
// 151) — Terraform pattern emitter for the lambda-cold-start-baseline
// recommendation. Lives alongside the existing trace-emission +
// span-quality emitters so a single PickedPattern shape feeds the
// discovery proposer's draft pipeline regardless of which detection
// branch produced the draft.
//
// The pattern adds an aws_lambda_provisioned_concurrency_config
// resource pinned to the offending function with a baseline floor of
// 1 — operators tune the value to their traffic shape. Provisioned
// concurrency is the canonical fix for the "frequency increase" cause
// in the §8 three-failure-mode framing; the picker's reasoning calls
// out the other two causes (init-script regression / architecture
// change) explicitly so an operator whose actual cause is different
// can decline cleanly from the PR body alone.
//
// See docs/proposals/cold-start-latency-slice1.md §8.

// PickColdStartProvisionedConcurrencyPattern returns the Terraform
// snippet + operator-facing reasoning for a lambda-cold-start-baseline
// recommendation. The pattern adds an
// aws_lambda_provisioned_concurrency_config resource with a floor of
// 1 (operator tunes).
//
// Returns a PickedPattern so the proposer's draft pipeline consumes
// the same shape it consumes from PickOrphanTracePattern et al. The
// FallbackUsed bit stays false: there's only one Terraform path for
// this kind in slice 1.
//
// The function intentionally takes RecommendationContext rather than a
// narrower per-row struct so the future GCP / Azure / OCI cold-start
// kinds in slice 2 can re-use this dispatch boundary without churning
// the signature.
func PickColdStartProvisionedConcurrencyPattern(ctx RecommendationContext) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	terraform := fmt.Sprintf(`resource "aws_lambda_provisioned_concurrency_config" "%s" {
  function_name                     = aws_lambda_function.%s.function_name
  provisioned_concurrent_executions = 1  # operator tunes
  qualifier                         = aws_lambda_function.%s.version
}
`, name, name, name)

	reasoning := "Provisioned concurrency keeps the Lambda execution environment warm, eliminating cold-start latency for the configured concurrency floor. Tune the value based on your traffic pattern; 1 is the minimum to start. If your cause is actually init-script regression or architecture change, decline this PR and address the actual cause."

	return PickedPattern{
		PrimaryTerraform: terraform,
		Reasoning:        reasoning,
	}
}

// Cold-start latency analysis slice 2 chunk 4 (v0.89.119, #759 Stream
// 157) — four new per-cloud Terraform pattern emitters siblings to
// PickColdStartProvisionedConcurrencyPattern. Each per-cloud emitter
// returns the closest-equivalent warm-floor pattern for the surface:
//
//   - Cloud Run: google_cloud_run_service annotation
//     autoscaling.knative.dev/minScale = 1 (operator tunes).
//   - Cloud Functions (Gen 2): google_cloudfunctions2_function
//     service_config.min_instance_count = 1 (operator tunes).
//   - Azure Functions: two paths — Premium Plan migration
//     (azurerm_service_plan sku_name = "EP1") OR placeholder mode
//     disable (WEBSITE_USE_PLACEHOLDER = "0") — the operator picks by
//     cost tolerance.
//   - OCI Functions: WARMUP_DELAY config adjustment with the
//     provisioned_concurrent_executions GA note (currently in
//     preview).
//
// See docs/proposals/cold-start-latency-slice2.md §3 + §8.

// PickCloudRunColdStartPattern returns the Terraform snippet +
// reasoning for the cloudrun-cold-start-baseline kind. The pattern
// adds the autoscaling.knative.dev/minScale annotation set to 1
// (operator tunes the floor).
func PickCloudRunColdStartPattern(ctx RecommendationContext) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	terraform := fmt.Sprintf(`resource "google_cloud_run_service" "%s" {
  # ... existing fields ...
  metadata {
    annotations = {
      "autoscaling.knative.dev/minScale" = "1"  # operator tunes; pin baseline warm instances
    }
  }
}
`, name)

	reasoning := "Cloud Run minScale annotation pins a minimum of 1 warm instance, eliminating cold-start for the configured floor. Tune based on your traffic; 1 is the minimum to start. Note: Cloud Run's request_latencies metric includes warm-path invocations, so permanently-warm services may show false positives. If the cause is init script regression or architecture change, decline this PR."

	return PickedPattern{
		PrimaryTerraform: terraform,
		Reasoning:        reasoning,
	}
}

// PickCloudFunctionsColdStartPattern returns the Terraform snippet +
// reasoning for the cloudfunc-cold-start-baseline kind. Gen 2 Cloud
// Functions support min_instance_count on the service_config block;
// the picker pins it at 1 (operator tunes).
func PickCloudFunctionsColdStartPattern(ctx RecommendationContext) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	terraform := fmt.Sprintf(`resource "google_cloudfunctions2_function" "%s" {
  # ... existing fields ...
  service_config {
    min_instance_count = 1  # operator tunes; pin baseline warm instances
  }
}
`, name)

	reasoning := "Cloud Functions Gen 2 min_instance_count pins warm instances, eliminating cold-start for the configured floor. Tune based on your traffic; 1 is the minimum to start. Note: execution_times includes warm-path invocations, so permanently-warm functions may show false positives. Decline if the cause is init script regression or architecture change."

	return PickedPattern{
		PrimaryTerraform: terraform,
		Reasoning:        reasoning,
	}
}

// PickAzureFunctionsColdStartPattern returns the Terraform snippet +
// reasoning for the azfunc-cold-start-baseline kind. Two paths are
// offered in a single snippet — the operator picks by cost tolerance:
//
//   - Premium Plan migration (azurerm_service_plan EP1) eliminates
//     cold-start by maintaining warm instances.
//   - Placeholder mode disable (WEBSITE_USE_PLACEHOLDER = "0") is the
//     lighter-weight path: trades startup speed for predictable
//     first-request latency.
//
// Both patterns sit in PrimaryTerraform so the operator sees both
// affordances in one PR body. The reasoning makes the trade-off
// explicit.
func PickAzureFunctionsColdStartPattern(ctx RecommendationContext) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	terraform := fmt.Sprintf(`# Premium Plan migration for cold-start elimination
resource "azurerm_service_plan" "%s" {
  # ... existing fields ...
  sku_name = "EP1"  # Premium plan eliminates cold start
}

# OR (lighter-weight): disable placeholder mode
resource "azurerm_linux_function_app" "%s" {
  # ... existing fields ...
  app_settings = {
    WEBSITE_USE_PLACEHOLDER = "0"
  }
}
`, name, name)

	reasoning := "Azure Functions Premium Plan (EP1+) eliminates cold-start by maintaining warm instances. Lighter-weight alternative: WEBSITE_USE_PLACEHOLDER=0 trades startup speed for predictable first-request latency. Pick based on cost tolerance. Decline if the cause is init script regression or architecture change."

	return PickedPattern{
		PrimaryTerraform: terraform,
		Reasoning:        reasoning,
	}
}

// PickOCIFunctionsColdStartPattern returns the Terraform snippet +
// reasoning for the ocifunc-cold-start-baseline kind. OCI Functions
// doesn't currently expose provisioned concurrency (slated for GA);
// the picker emits a WARMUP_DELAY config tuning pattern with the
// preview note in the reasoning so the operator knows the
// provisioned_concurrent_executions field will be the recommended
// long-term fix.
func PickOCIFunctionsColdStartPattern(ctx RecommendationContext) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	terraform := fmt.Sprintf(`resource "oci_functions_function" "%s" {
  # ... existing fields ...
  config = {
    "WARMUP_DELAY" = "100"  # operator tunes; helps shape init behavior
  }
}
`, name)

	reasoning := "OCI Functions doesn't currently expose provisioned concurrency (slated for GA). WARMUP_DELAY adjustment helps tune init behavior. When provisioned_concurrent_executions exits preview, the recommendation will shift to that. Decline if the cause is init script regression or architecture change."

	return PickedPattern{
		PrimaryTerraform: terraform,
		Reasoning:        reasoning,
	}
}
