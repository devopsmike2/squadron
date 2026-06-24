// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
	"testing"
)

// cold_start_test.go — Cold-start latency analysis slice 1 chunk 3
// (v0.89.115, #753 Stream 151). Pins the
// PickColdStartProvisionedConcurrencyPattern emitter against the §8
// spec wording (provisioned_concurrent_executions = 1, qualifier
// pinned to the function's version) and the operator-facing reasoning
// (decline path mentioned for the init-script and architecture
// causes).

// TestPickColdStartProvisionedConcurrency_PatternMatchesSpec — pins the
// emitted Terraform snippet against the §8 reference shape. The block
// names the aws_lambda_provisioned_concurrency_config resource type;
// includes provisioned_concurrent_executions = 1 with the operator-
// tunes comment; and references function_name + version on the
// aws_lambda_function block keyed by the operator's TF name.
func TestPickColdStartProvisionedConcurrency_PatternMatchesSpec(t *testing.T) {
	picked := PickColdStartProvisionedConcurrencyPattern(RecommendationContext{
		Provider:       "aws",
		Tier:           "compute",
		ResourceTFName: "order_processor",
	})
	if picked.PrimaryTerraform == "" {
		t.Fatalf("PrimaryTerraform empty; want a snippet")
	}
	tf := picked.PrimaryTerraform
	for _, want := range []string{
		`aws_lambda_provisioned_concurrency_config" "order_processor"`,
		"function_name                     = aws_lambda_function.order_processor.function_name",
		"provisioned_concurrent_executions = 1",
		"# operator tunes",
		"qualifier                         = aws_lambda_function.order_processor.version",
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("Terraform missing %q; got:\n%s", want, tf)
		}
	}
	if picked.FallbackUsed {
		t.Errorf("FallbackUsed = true; want false (the single slice 1 path)")
	}
}

// TestPickColdStartProvisionedConcurrency_FallsBackToNamePlaceholder —
// when the operator's IaC repo introspection didn't classify a TF name
// for the row, the picker falls back to "<name>" so the snippet still
// parses + the operator sees an obvious placeholder to replace.
func TestPickColdStartProvisionedConcurrency_FallsBackToNamePlaceholder(t *testing.T) {
	picked := PickColdStartProvisionedConcurrencyPattern(RecommendationContext{
		Provider: "aws",
		Tier:     "compute",
		// ResourceTFName intentionally empty.
	})
	if !strings.Contains(picked.PrimaryTerraform, `"<name>"`) {
		t.Errorf("expected <name> placeholder; got:\n%s", picked.PrimaryTerraform)
	}
}

// TestPickColdStartProvisionedConcurrency_ReasoningMentionsDeclinePath
// — the picker's reasoning must surface the "decline if the actual
// cause is init-script regression / architecture change" hedge so the
// operator sees the alternative-cause framing without drilling into
// the runbook. Mirrors the §8 design doc.
func TestPickColdStartProvisionedConcurrency_ReasoningMentionsDeclinePath(t *testing.T) {
	picked := PickColdStartProvisionedConcurrencyPattern(RecommendationContext{
		Provider:       "aws",
		Tier:           "compute",
		ResourceTFName: "order_processor",
	})
	for _, want := range []string{
		"Provisioned concurrency",
		"decline",
		"init-script regression",
		"architecture change",
	} {
		if !strings.Contains(picked.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, picked.Reasoning)
		}
	}
}
