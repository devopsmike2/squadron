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
		// v0.89.355: the qualifier is now a loud placeholder, not a
		// live .version reference (which resolves to $LATEST and hard-
		// fails apply when the function isn't publishing versions).
		`qualifier                         = "REPLACE_WITH_PUBLISHED_VERSION_OR_ALIAS"`,
	} {
		if !strings.Contains(tf, want) {
			t.Errorf("Terraform missing %q; got:\n%s", want, tf)
		}
	}
	// Guard against regressing to the apply-breaking form: the snippet
	// must NOT emit a bare `.version` qualifier expression (provisioned
	// concurrency cannot target $LATEST). The precondition must be a
	// substitute-before-apply placeholder + a reasoning caveat instead.
	if strings.Contains(tf, "qualifier                         = aws_lambda_function.order_processor.version") {
		t.Errorf("snippet emits a bare .version qualifier — resolves to $LATEST and fails apply; got:\n%s", tf)
	}
	for _, want := range []string{"$LATEST", "published version", "publish = true"} {
		if !strings.Contains(picked.Reasoning, want) {
			t.Errorf("reasoning missing precondition %q; got: %s", want, picked.Reasoning)
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

// Cold-start latency analysis slice 2 chunk 4 (v0.89.119, #759 Stream
// 157) — per-cloud Terraform pattern emitter tests. One per emitter,
// pinning the shape against the §8 spec wording.

// TestPickCloudRunColdStartPattern_IncludesMinScaleAnnotation — pins
// the Cloud Run emitter's Terraform snippet against the §3.1 +
// §8 reference shape. The minScale annotation appears with the
// operator-tunes comment.
func TestPickCloudRunColdStartPattern_IncludesMinScaleAnnotation(t *testing.T) {
	picked := PickCloudRunColdStartPattern(RecommendationContext{
		Provider:       "gcp",
		Tier:           "serverless",
		ResourceTFName: "checkout_svc",
	})
	if picked.PrimaryTerraform == "" {
		t.Fatalf("PrimaryTerraform empty; want a snippet")
	}
	for _, want := range []string{
		`google_cloud_run_service" "checkout_svc"`,
		`"autoscaling.knative.dev/minScale" = "1"`,
		"# operator tunes",
	} {
		if !strings.Contains(picked.PrimaryTerraform, want) {
			t.Errorf("Terraform missing %q; got:\n%s", want, picked.PrimaryTerraform)
		}
	}
	for _, want := range []string{
		"Cloud Run minScale",
		"warm",
		"decline",
		"init script regression",
	} {
		if !strings.Contains(picked.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, picked.Reasoning)
		}
	}
	if picked.FallbackUsed {
		t.Errorf("FallbackUsed = true; want false")
	}
}

// TestPickCloudFunctionsColdStartPattern_IncludesMinInstanceCount —
// pins the Cloud Functions Gen 2 emitter's Terraform snippet against
// the §3.2 + §8 reference shape.
func TestPickCloudFunctionsColdStartPattern_IncludesMinInstanceCount(t *testing.T) {
	picked := PickCloudFunctionsColdStartPattern(RecommendationContext{
		Provider:       "gcp",
		Tier:           "serverless",
		ResourceTFName: "image_resize",
	})
	if picked.PrimaryTerraform == "" {
		t.Fatalf("PrimaryTerraform empty; want a snippet")
	}
	for _, want := range []string{
		`google_cloudfunctions2_function" "image_resize"`,
		"min_instance_count = 1",
		"# operator tunes",
	} {
		if !strings.Contains(picked.PrimaryTerraform, want) {
			t.Errorf("Terraform missing %q; got:\n%s", want, picked.PrimaryTerraform)
		}
	}
	for _, want := range []string{
		"min_instance_count",
		"warm-path",
		"Decline",
	} {
		if !strings.Contains(picked.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, picked.Reasoning)
		}
	}
}

// TestPickAzureFunctionsColdStartPattern_OffersBothPremiumAndPlaceholder
// — pins the Azure Functions emitter's two-path Terraform snippet
// against §3.3 + §8. Both Premium Plan (EP1) and placeholder mode
// (WEBSITE_USE_PLACEHOLDER) appear so the operator can pick by cost
// tolerance.
func TestPickAzureFunctionsColdStartPattern_OffersBothPremiumAndPlaceholder(t *testing.T) {
	picked := PickAzureFunctionsColdStartPattern(RecommendationContext{
		Provider:       "azure",
		Tier:           "serverless",
		ResourceTFName: "payments_func",
	})
	if picked.PrimaryTerraform == "" {
		t.Fatalf("PrimaryTerraform empty; want a snippet")
	}
	for _, want := range []string{
		`azurerm_service_plan" "payments_func"`,
		`sku_name = "EP1"`,
		`azurerm_linux_function_app" "payments_func"`,
		`WEBSITE_USE_PLACEHOLDER = "0"`,
		"OR (lighter-weight)",
	} {
		if !strings.Contains(picked.PrimaryTerraform, want) {
			t.Errorf("Terraform missing %q; got:\n%s", want, picked.PrimaryTerraform)
		}
	}
	for _, want := range []string{
		"Premium Plan",
		"WEBSITE_USE_PLACEHOLDER",
		"cost tolerance",
		"Decline",
	} {
		if !strings.Contains(picked.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, picked.Reasoning)
		}
	}
}

// TestPickOCIFunctionsColdStartPattern_NotesProvisionedConcurrencyPreview
// — pins the OCI Functions emitter's WARMUP_DELAY snippet + the
// reasoning's preview note for the not-yet-GA
// provisioned_concurrent_executions field, per §3.4.
func TestPickOCIFunctionsColdStartPattern_NotesProvisionedConcurrencyPreview(t *testing.T) {
	picked := PickOCIFunctionsColdStartPattern(RecommendationContext{
		Provider:       "oci",
		Tier:           "serverless",
		ResourceTFName: "ingest_worker",
	})
	if picked.PrimaryTerraform == "" {
		t.Fatalf("PrimaryTerraform empty; want a snippet")
	}
	for _, want := range []string{
		`oci_functions_function" "ingest_worker"`,
		`"WARMUP_DELAY" = "100"`,
		"# operator tunes",
	} {
		if !strings.Contains(picked.PrimaryTerraform, want) {
			t.Errorf("Terraform missing %q; got:\n%s", want, picked.PrimaryTerraform)
		}
	}
	for _, want := range []string{
		"provisioned concurrency",
		"GA",
		"WARMUP_DELAY",
		"preview",
		"Decline",
	} {
		if !strings.Contains(picked.Reasoning, want) {
			t.Errorf("Reasoning missing %q; got: %s", want, picked.Reasoning)
		}
	}
}

// TestAllFourPerCloudPickers_FallBackToNamePlaceholder — when the
// operator's IaC repo introspection didn't classify a TF name, each
// of the four new pickers falls back to "<name>" so the snippet
// still parses and the operator sees an obvious placeholder.
func TestAllFourPerCloudPickers_FallBackToNamePlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(RecommendationContext) PickedPattern
	}{
		{"cloudrun", PickCloudRunColdStartPattern},
		{"cloudfunc", PickCloudFunctionsColdStartPattern},
		{"azfunc", PickAzureFunctionsColdStartPattern},
		{"ocifunc", PickOCIFunctionsColdStartPattern},
	} {
		t.Run(tc.name, func(t *testing.T) {
			picked := tc.fn(RecommendationContext{Tier: "serverless"})
			if !strings.Contains(picked.PrimaryTerraform, `"<name>"`) {
				t.Errorf("expected <name> placeholder for %s; got:\n%s", tc.name, picked.PrimaryTerraform)
			}
		})
	}
}
