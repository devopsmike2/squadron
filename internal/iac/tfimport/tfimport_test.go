// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package tfimport

import (
	"strings"
	"testing"
)

func blockByType(blocks []ImportBlock, tfType string) (ImportBlock, bool) {
	for _, b := range blocks {
		if b.TFType == tfType {
			return b, true
		}
	}
	return ImportBlock{}, false
}

func TestGenerate_AWSTypes_ImportIDs(t *testing.T) {
	in := []Resource{
		{Provider: "aws", Category: "compute", ResourceID: "i-0abc", Region: "us-east-1"},
		{Provider: "aws", Category: "object_store", ResourceID: "my-logs-bucket", Region: "us-east-1"},
		{Provider: "aws", Category: "function", ResourceID: "arn:aws:lambda:us-east-1:123:function:hello", Name: "hello", Region: "us-east-1"},
		{Provider: "aws", Category: "database", ResourceID: "arn:aws:rds:us-east-1:123:db:orders", Region: "us-east-1"},
		{Provider: "aws", Category: "load_balancer", ResourceID: "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/web/abc", Region: "us-east-1"},
	}
	blocks, skipped := Generate(in)
	if len(skipped) != 0 {
		t.Fatalf("expected no skips, got %v", skipped)
	}
	cases := map[string]string{
		"aws_instance":        "i-0abc",
		"aws_s3_bucket":       "my-logs-bucket",
		"aws_lambda_function": "hello",
		"aws_db_instance":     "orders",
		"aws_lb":              "arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer/app/web/abc",
	}
	for tfType, wantID := range cases {
		b, ok := blockByType(blocks, tfType)
		if !ok {
			t.Errorf("missing block for %s", tfType)
			continue
		}
		if b.ImportID != wantID {
			t.Errorf("%s import id = %q, want %q", tfType, b.ImportID, wantID)
		}
		if !strings.HasPrefix(b.TFAddress, tfType+".imported_") {
			t.Errorf("%s address = %q, want imported_ prefix", tfType, b.TFAddress)
		}
	}
}

func TestGenerate_FunctionArnFallback(t *testing.T) {
	// No Name → fall back to last ARN segment.
	blocks, _ := Generate([]Resource{
		{Provider: "aws", Category: "function", ResourceID: "arn:aws:lambda:us-east-1:123:function:worker"},
	})
	b, ok := blockByType(blocks, "aws_lambda_function")
	if !ok || b.ImportID != "worker" {
		t.Errorf("function ARN fallback failed: %+v", blocks)
	}
}

func TestGenerate_LoadBalancerNonARNSkipped(t *testing.T) {
	// aws_lb import requires an ARN; a non-ARN id must be skipped, not guessed.
	blocks, skipped := Generate([]Resource{
		{Provider: "aws", Category: "load_balancer", ResourceID: "web-lb"},
	})
	if len(blocks) != 0 {
		t.Errorf("expected no blocks for non-ARN LB, got %v", blocks)
	}
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skip, got %v", skipped)
	}
}

func TestGenerate_UnsupportedProviderAndCategorySkipped(t *testing.T) {
	_, skipped := Generate([]Resource{
		{Provider: "gcp", Category: "compute", ResourceID: "x"},
		{Provider: "aws", Category: "queue", ResourceID: "q"},
	})
	if len(skipped) != 2 {
		t.Fatalf("expected 2 skips, got %v", skipped)
	}
}

func TestGenerate_AddressDedup(t *testing.T) {
	// Two buckets with the same name fragment → unique addresses.
	blocks, _ := Generate([]Resource{
		{Provider: "aws", Category: "object_store", ResourceID: "dup"},
		{Provider: "aws", Category: "object_store", ResourceID: "dup"},
	})
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	if blocks[0].TFAddress == blocks[1].TFAddress {
		t.Errorf("addresses collided: %s", blocks[0].TFAddress)
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"i-0abc":         "i_0abc",
		"my.bucket/name": "my_bucket_name",
		"123start":       "r_123start",
		"":               "",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRender_EmitsValidImportBlocks(t *testing.T) {
	blocks, _ := Generate([]Resource{
		{Provider: "aws", Category: "compute", ResourceID: "i-0abc", Region: "us-east-1"},
	})
	out := Render(blocks, nil)
	for _, want := range []string{
		"terraform plan -generate-config-out=generated.tf",
		"import {",
		"to = aws_instance.imported_i_0abc",
		`id = "i-0abc"`,
		"# region: us-east-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q, got:\n%s", want, out)
		}
	}
}

func TestRender_EmptyWhenNoBlocks(t *testing.T) {
	if out := Render(nil, nil); out != "" {
		t.Errorf("expected empty render for no blocks, got:\n%s", out)
	}
}

func TestGenerate_MultiCloud_Compute(t *testing.T) {
	in := []Resource{
		{Provider: "azure", Category: "compute", ResourceID: "vm-linux", OSFamily: "linux",
			ImportID: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-linux", Region: "eastus"},
		{Provider: "azure", Category: "compute", ResourceID: "vm-win", OSFamily: "windows",
			ImportID: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-win", Region: "eastus"},
		{Provider: "gcp", Category: "compute", ResourceID: "gce-1",
			ImportID: "proj/us-central1-a/gce-1", Region: "us-central1"},
		{Provider: "oci", Category: "compute", ResourceID: "oci-1",
			ImportID: "ocid1.instance.oc1..abc", Region: "us-ashburn-1"},
	}
	blocks, skipped := Generate(in)
	if len(skipped) != 0 {
		t.Fatalf("expected no skips, got %v", skipped)
	}
	want := map[string]string{
		"azurerm_linux_virtual_machine":   "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-linux",
		"azurerm_windows_virtual_machine": "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-win",
		"google_compute_instance":         "proj/us-central1-a/gce-1",
		"oci_core_instance":               "ocid1.instance.oc1..abc",
	}
	for tfType, wantID := range want {
		b, ok := blockByType(blocks, tfType)
		if !ok {
			t.Errorf("missing block for %s", tfType)
			continue
		}
		if b.ImportID != wantID {
			t.Errorf("%s import id = %q, want %q", tfType, b.ImportID, wantID)
		}
	}
}

func TestGenerate_AzureLoadBalancer_AndNonComputeVisibleSkips(t *testing.T) {
	lbID := "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/web-lb"
	blocks, skipped := Generate([]Resource{
		{Provider: "azure", Category: "load_balancer", ResourceID: lbID, ImportID: lbID, Name: "web-lb", Region: "eastus"},
		// No mapper yet — must surface as a *visible* Skipped, not vanish.
		{Provider: "azure", Category: "database", ResourceID: "/subscriptions/s/db", ImportID: "/subscriptions/s/db"},
		{Provider: "azure", Category: "object_store", ResourceID: "https://acct.blob.core.windows.net/data", ImportID: "https://acct.blob.core.windows.net/data"},
		{Provider: "gcp", Category: "load_balancer", ResourceID: "fr-url", ImportID: "fr-url"},
	})

	b, ok := blockByType(blocks, "azurerm_lb")
	if !ok {
		t.Fatalf("expected an azurerm_lb block, got %+v", blocks)
	}
	if b.ImportID != lbID {
		t.Fatalf("azurerm_lb import id = %q, want the ARM resource id %q", b.ImportID, lbID)
	}
	if len(blocks) != 1 {
		t.Fatalf("only the Azure LB should map; got %d blocks", len(blocks))
	}
	// database + object_store (azure) + load_balancer (gcp) → 3 visible skips.
	if len(skipped) != 3 {
		t.Fatalf("expected 3 visible skips, got %d: %+v", len(skipped), skipped)
	}
}

func TestGenerate_MultiCloud_SkipsWhenNoImportID(t *testing.T) {
	// Without a captured canonical ImportID, non-AWS compute is skipped
	// (never guessed from the friendly ResourceID).
	_, skipped := Generate([]Resource{
		{Provider: "azure", Category: "compute", ResourceID: "vm-x", OSFamily: "linux"},
		{Provider: "gcp", Category: "compute", ResourceID: "gce-x"},
		{Provider: "oci", Category: "compute", ResourceID: "oci-x"},
	})
	if len(skipped) != 3 {
		t.Fatalf("expected 3 skips for missing ImportID, got %v", skipped)
	}
}
