// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
	"testing"
)

// error_rate_test.go — Error rate correlation slice 1 chunk 2
// (v0.89.128, #768 Stream 166). Pins the per-cloud
// PickErrorRateTerraform emitter for AWS Lambda / GCP Cloud Run /
// GCP Cloud Functions / Azure Functions / OCI Functions per §8 of
// the design doc.

// TestPickErrorRateTerraform_AWS_LambdaMemoryAndConcurrency pins
// the AWS Lambda pattern: memory_size = 1024 +
// reserved_concurrent_executions = 100 on aws_lambda_function.
func TestPickErrorRateTerraform_AWS_LambdaMemoryAndConcurrency(t *testing.T) {
	tf := PickErrorRateTerraform("aws", "lambda", "order_processor")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "aws_lambda_function" "order_processor"`) {
		t.Errorf("snippet missing aws_lambda_function block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "memory_size") || !strings.Contains(tf, "1024") {
		t.Errorf("snippet missing memory_size = 1024; got:\n%s", tf)
	}
	if !strings.Contains(tf, "reserved_concurrent_executions") || !strings.Contains(tf, "100") {
		t.Errorf("snippet missing reserved_concurrent_executions = 100; got:\n%s", tf)
	}
	if !strings.Contains(tf, "operator tunes") {
		t.Errorf("snippet missing operator-tunable comment; got:\n%s", tf)
	}
}

// TestPickErrorRateTerraform_GCPCloudRun_ContainerConcurrencyAndMemory
// pins the Cloud Run pattern: container_concurrency = 80 +
// resources.limits.memory = "1Gi" on the template/spec.
func TestPickErrorRateTerraform_GCPCloudRun_ContainerConcurrencyAndMemory(t *testing.T) {
	tf := PickErrorRateTerraform("gcp", "cloudrun", "hello_run")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "google_cloud_run_service" "hello_run"`) {
		t.Errorf("snippet missing google_cloud_run_service block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "container_concurrency") || !strings.Contains(tf, "80") {
		t.Errorf("snippet missing container_concurrency = 80; got:\n%s", tf)
	}
	if !strings.Contains(tf, "resources") || !strings.Contains(tf, "limits") || !strings.Contains(tf, `"1Gi"`) {
		t.Errorf("snippet missing resources.limits.memory = 1Gi; got:\n%s", tf)
	}
}

// TestPickErrorRateTerraform_GCPCloudFunctions_AvailableMemory pins
// the Cloud Functions Gen 2 pattern: service_config.available_memory
// = "1Gi" on google_cloudfunctions2_function.
func TestPickErrorRateTerraform_GCPCloudFunctions_AvailableMemory(t *testing.T) {
	tf := PickErrorRateTerraform("gcp", "cloudfunc", "hello_func")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "google_cloudfunctions2_function" "hello_func"`) {
		t.Errorf("snippet missing google_cloudfunctions2_function block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "service_config") {
		t.Errorf("snippet missing service_config block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "available_memory") || !strings.Contains(tf, `"1Gi"`) {
		t.Errorf("snippet missing available_memory = 1Gi; got:\n%s", tf)
	}
}

// TestPickErrorRateTerraform_Azure_PremiumPlanBump pins the Azure
// pattern: azurerm_service_plan sku_name = "EP2" with the EP1
// reference in the snippet's comment to signal the bump direction.
func TestPickErrorRateTerraform_Azure_PremiumPlanBump(t *testing.T) {
	tf := PickErrorRateTerraform("azure", "azfunc", "hello_az")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "azurerm_service_plan" "hello_az"`) {
		t.Errorf("snippet missing azurerm_service_plan block; got:\n%s", tf)
	}
	if !strings.Contains(tf, `sku_name`) || !strings.Contains(tf, `"EP2"`) {
		t.Errorf("snippet missing sku_name = EP2; got:\n%s", tf)
	}
	if !strings.Contains(tf, "EP1") {
		t.Errorf("snippet missing EP1 reference (bump direction); got:\n%s", tf)
	}
}

// TestPickErrorRateTerraform_OCI_MemoryInMbs pins the OCI Functions
// pattern: oci_functions_function memory_in_mbs = 1024.
func TestPickErrorRateTerraform_OCI_MemoryInMbs(t *testing.T) {
	tf := PickErrorRateTerraform("oci", "ocifunc", "hello_oci")
	if tf == "" {
		t.Fatalf("returned empty snippet")
	}
	if !strings.Contains(tf, `resource "oci_functions_function" "hello_oci"`) {
		t.Errorf("snippet missing oci_functions_function block; got:\n%s", tf)
	}
	if !strings.Contains(tf, "memory_in_mbs") || !strings.Contains(tf, "1024") {
		t.Errorf("snippet missing memory_in_mbs = 1024; got:\n%s", tf)
	}
}

// TestPickErrorRateTerraform_EmptyName_FallsBack pins the empty-
// resourceTFName fallback to "<name>".
func TestPickErrorRateTerraform_EmptyName_FallsBack(t *testing.T) {
	tf := PickErrorRateTerraform("aws", "lambda", "")
	if !strings.Contains(tf, `"<name>"`) {
		t.Errorf("empty resourceTFName should fall back to <name>; got:\n%s", tf)
	}
}

// TestPickErrorRateTerraform_UnknownProvider_ReturnsEmpty pins the
// total-function default — unsupported provider returns "" rather
// than panicking.
func TestPickErrorRateTerraform_UnknownProvider_ReturnsEmpty(t *testing.T) {
	if tf := PickErrorRateTerraform("alibaba", "lambda", "x"); tf != "" {
		t.Errorf("unsupported provider should return empty; got %q", tf)
	}
}
