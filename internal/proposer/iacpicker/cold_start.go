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
