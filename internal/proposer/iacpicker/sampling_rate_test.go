// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
	"testing"
)

// sampling_rate_test.go — Sampling rate analysis slice 1 chunk 2
// (v0.89.123, #763 Stream 161). Pins the per-cloud
// PickSamplingRateTerraform emitter for AWS Lambda / GCP Cloud Run /
// GCP Cloud Functions / Azure Functions / OCI Functions per §8.

// TestPickSamplingRateTerraform_AWS_LambdaEnvVar pins the AWS Lambda
// pattern shape: env var injection into aws_lambda_function with
// OTEL_TRACES_SAMPLER_ARG = "0.5".
func TestPickSamplingRateTerraform_AWS_LambdaEnvVar(t *testing.T) {
	tf := PickSamplingRateTerraform("aws", "lambda", "order_processor")
	if tf == "" {
		t.Fatalf("PickSamplingRateTerraform aws: returned empty snippet")
	}
	if !strings.Contains(tf, `resource "aws_lambda_function" "order_processor"`) {
		t.Errorf("snippet missing aws_lambda_function block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "environment {") {
		t.Errorf("snippet missing environment block; got:\n%s", tf)
	}
	if !strings.Contains(tf, `OTEL_TRACES_SAMPLER_ARG = "0.5"`) {
		t.Errorf("snippet missing OTEL_TRACES_SAMPLER_ARG=\"0.5\"; got:\n%s", tf)
	}
	if !strings.Contains(tf, "operator tunes") {
		t.Errorf("snippet missing operator-tunable comment; got:\n%s", tf)
	}
}

// TestPickSamplingRateTerraform_GCPCloudRun_TemplateContainerEnv pins
// the Cloud Run pattern: template.spec.containers.env shape with
// OTEL_TRACES_SAMPLER_ARG.
func TestPickSamplingRateTerraform_GCPCloudRun_TemplateContainerEnv(t *testing.T) {
	tf := PickSamplingRateTerraform("gcp", "cloudrun", "hello_run")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "google_cloud_run_service" "hello_run"`) {
		t.Errorf("snippet missing google_cloud_run_service block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "template {") || !strings.Contains(tf, "containers {") || !strings.Contains(tf, "env {") {
		t.Errorf("snippet missing template/containers/env nesting; got:\n%s", tf)
	}
	if !strings.Contains(tf, `name  = "OTEL_TRACES_SAMPLER_ARG"`) {
		t.Errorf("snippet missing OTEL_TRACES_SAMPLER_ARG env name; got:\n%s", tf)
	}
	if !strings.Contains(tf, `value = "0.5"`) {
		t.Errorf("snippet missing 0.5 value; got:\n%s", tf)
	}
}

// TestPickSamplingRateTerraform_GCPCloudFunctions_ServiceConfigEnv pins
// the Cloud Functions Gen 2 pattern: service_config.environment_variables.
func TestPickSamplingRateTerraform_GCPCloudFunctions_ServiceConfigEnv(t *testing.T) {
	tf := PickSamplingRateTerraform("gcp", "cloudfunc", "hello_func")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "google_cloudfunctions2_function" "hello_func"`) {
		t.Errorf("snippet missing google_cloudfunctions2_function block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "service_config {") {
		t.Errorf("snippet missing service_config block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "environment_variables = {") {
		t.Errorf("snippet missing environment_variables map; got:\n%s", tf)
	}
	if !strings.Contains(tf, `OTEL_TRACES_SAMPLER_ARG = "0.5"`) {
		t.Errorf("snippet missing OTEL_TRACES_SAMPLER_ARG=\"0.5\"; got:\n%s", tf)
	}
}

// TestPickSamplingRateTerraform_Azure_AppSettings pins the Azure Functions
// pattern: azurerm_linux_function_app app_settings map.
func TestPickSamplingRateTerraform_Azure_AppSettings(t *testing.T) {
	tf := PickSamplingRateTerraform("azure", "azfunc", "hello_azure")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "azurerm_linux_function_app" "hello_azure"`) {
		t.Errorf("snippet missing azurerm_linux_function_app block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "app_settings = {") {
		t.Errorf("snippet missing app_settings map; got:\n%s", tf)
	}
	if !strings.Contains(tf, `OTEL_TRACES_SAMPLER_ARG = "0.5"`) {
		t.Errorf("snippet missing OTEL_TRACES_SAMPLER_ARG=\"0.5\"; got:\n%s", tf)
	}
}

// TestPickSamplingRateTerraform_OCI_Config pins the OCI Functions pattern:
// oci_functions_function config map.
func TestPickSamplingRateTerraform_OCI_Config(t *testing.T) {
	tf := PickSamplingRateTerraform("oci", "ocifunc", "hello_oci")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "oci_functions_function" "hello_oci"`) {
		t.Errorf("snippet missing oci_functions_function block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "config = {") {
		t.Errorf("snippet missing config map; got:\n%s", tf)
	}
	if !strings.Contains(tf, `OTEL_TRACES_SAMPLER_ARG = "0.5"`) {
		t.Errorf("snippet missing OTEL_TRACES_SAMPLER_ARG=\"0.5\"; got:\n%s", tf)
	}
}

// TestPickSamplingRateTerraform_AllPatternsContainOtelSamplerArg pins
// the invariant: every per-cloud emitter MUST surface
// OTEL_TRACES_SAMPLER_ARG with the 0.5 default. This is the §8
// uniform-across-clouds invariant.
func TestPickSamplingRateTerraform_AllPatternsContainOtelSamplerArg(t *testing.T) {
	cases := []struct {
		provider string
		surface  string
	}{
		{"aws", "lambda"},
		{"gcp", "cloudrun"},
		{"gcp", "cloudfunc"},
		{"azure", "azfunc"},
		{"oci", "ocifunc"},
	}
	for _, c := range cases {
		t.Run(c.provider+"/"+c.surface, func(t *testing.T) {
			tf := PickSamplingRateTerraform(c.provider, c.surface, "my_resource")
			if tf == "" {
				t.Fatalf("empty snippet for %s/%s", c.provider, c.surface)
			}
			if !strings.Contains(tf, "OTEL_TRACES_SAMPLER_ARG") {
				t.Errorf("%s/%s snippet missing OTEL_TRACES_SAMPLER_ARG", c.provider, c.surface)
			}
			if !strings.Contains(tf, "0.5") {
				t.Errorf("%s/%s snippet missing 0.5 default; got:\n%s", c.provider, c.surface, tf)
			}
			if !strings.Contains(tf, "my_resource") {
				t.Errorf("%s/%s snippet missing resource name; got:\n%s", c.provider, c.surface, tf)
			}
		})
	}
}

// TestPickSamplingRateTerraform_EmptyResourceName_FallsBack pins the
// resourceTFName="" fallback to "<name>" — mirrors the cold-start
// picker's posture.
func TestPickSamplingRateTerraform_EmptyResourceName_FallsBack(t *testing.T) {
	tf := PickSamplingRateTerraform("aws", "lambda", "")
	if !strings.Contains(tf, `"<name>"`) {
		t.Errorf("expected <name> fallback when resourceTFName empty; got:\n%s", tf)
	}
}

// TestPickSamplingRateTerraform_UnknownProvider_ReturnsEmpty pins the
// defensive fall-through for unsupported provider tokens.
func TestPickSamplingRateTerraform_UnknownProvider_ReturnsEmpty(t *testing.T) {
	if got := PickSamplingRateTerraform("unknown", "lambda", "foo"); got != "" {
		t.Errorf("expected empty snippet for unknown provider, got: %q", got)
	}
}
