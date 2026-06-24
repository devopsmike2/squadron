// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"fmt"
)

// error_rate.go — Error rate correlation slice 1 chunk 2
// (v0.89.128, #768 Stream 166). Terraform pattern emitter for the
// span-quality-error-rate-spike recommendation. Lives alongside the
// cold-start + sampling-rate + trace-emission + span-quality
// emitters so the proposer's draft pipeline consumes one shape
// regardless of which detection branch produced the draft.
//
// The pattern targets the §8 case (3) failure mode — resource
// exhaustion under load. The Terraform raises memory + concurrency
// limits to give the function headroom against throttling, memory
// pressure, and connection-pool exhaustion. The reasoning text
// emitted by the proposer's per-cloud Check helper explicitly
// frames cases (1) recent deploy regression and (2) downstream
// dependency failure as the MORE COMMON causes; operators whose
// actual cause is (1) or (2) decline the PR and the verdict
// learning loop records.
//
// The numeric raise targets (1024 MB memory / 100 concurrency etc.)
// are OPERATOR-TUNABLE STARTING POINTS — the recommendation
// suggests them as a baseline, not a target.
//
// See docs/proposals/error-rate-correlation-slice1.md §8.

// PickErrorRateTerraform returns the Terraform snippet for the
// resource-exhaustion mitigation on the per-cloud serverless
// resource. AWS Lambda raises memory + reserved concurrency. GCP
// Cloud Run raises memory + container_concurrency. GCP Cloud
// Functions Gen 2 raises service_config.available_memory. Azure
// Functions bumps the Premium Plan tier (EP1 → EP2). OCI Functions
// raises memory_in_mbs.
//
// surface is needed for GCP because Cloud Run and Cloud Functions
// use different Terraform resource types; AWS / Azure / OCI each
// have one shape per provider.
//
// When the provider isn't in the slice 1 set, returns "" — the
// caller treats an empty snippet as "no pattern" and skips emitting
// the draft.
func PickErrorRateTerraform(provider, surface, resourceTFName string) string {
	name := resourceTFName
	if name == "" {
		name = "<name>"
	}
	switch provider {
	case "aws":
		// AWS Lambda — memory_size raise + reserved_concurrent_executions
		// bump. memory_size in MB; 1024 MB is the §8 starting point
		// (operator tunes). reserved_concurrent_executions = 100 caps
		// the per-function concurrency so Lambda doesn't throttle
		// silently when invocations spike against an exhausted
		// account-level reserve.
		return fmt.Sprintf(`resource "aws_lambda_function" "%s" {
  # ... existing fields ...
  memory_size                    = 1024  # operator tunes; starting point per Squadron's error-rate analysis (case 3 — resource exhaustion)
  reserved_concurrent_executions = 100   # operator tunes; cap per-function concurrency to absorb load spikes
}
`, name)
	case "gcp":
		if surface == "cloudrun" {
			// GCP Cloud Run — container_concurrency on the template
			// + resources.limits.memory raise. container_concurrency
			// = 80 is the GCP-recommended baseline for stateless
			// workloads; memory = 1Gi gives headroom for connection
			// pools + per-request allocation. Operator tunes both.
			return fmt.Sprintf(`resource "google_cloud_run_service" "%s" {
  # ... existing fields ...
  template {
    spec {
      container_concurrency = 80  # operator tunes; starting point per Squadron's error-rate analysis
      containers {
        resources {
          limits = {
            memory = "1Gi"  # operator tunes; starting point per Squadron's error-rate analysis (case 3)
          }
        }
      }
    }
  }
}
`, name)
		}
		// GCP Cloud Functions Gen 2 — service_config.available_memory
		// raise. 1Gi is the §8 starting point (operator tunes). Same
		// resource type as the sampling-rate emitter; the config
		// surface differs.
		return fmt.Sprintf(`resource "google_cloudfunctions2_function" "%s" {
  # ... existing fields ...
  service_config {
    available_memory = "1Gi"  # operator tunes; starting point per Squadron's error-rate analysis (case 3 — resource exhaustion)
  }
}
`, name)
	case "azure":
		// Azure Functions — bump the Premium Plan tier. EP1 → EP2
		// doubles vCPU + RAM; the snippet's comment calls out that
		// the operator should change EP1 (if currently on EP1) to
		// EP2 here. Premium plan migration from Consumption is a
		// distinct migration path (Cold-start slice 2's Azure
		// emitter covers that surface); the error-rate emitter
		// assumes Premium already.
		return fmt.Sprintf(`resource "azurerm_service_plan" "%s" {
  # ... existing fields ...
  # Premium plan tier bump: EP1 → EP2 doubles vCPU + RAM, giving
  # the function app headroom against throttling + memory pressure
  # (Squadron error-rate analysis case 3).
  sku_name = "EP2"  # operator tunes; raises from EP1 baseline
}
`, name)
	case "oci":
		// OCI Functions — memory_in_mbs raise on oci_functions_function.
		// 1024 MB is the §8 starting point (operator tunes). The
		// OCI Functions runtime allocates per-invocation containers
		// at this memory ceiling; raising it absorbs load spikes
		// that would otherwise OOM-kill in-flight invocations.
		return fmt.Sprintf(`resource "oci_functions_function" "%s" {
  # ... existing fields ...
  memory_in_mbs = 1024  # operator tunes; starting point per Squadron's error-rate analysis (case 3 — resource exhaustion)
}
`, name)
	}
	return ""
}
