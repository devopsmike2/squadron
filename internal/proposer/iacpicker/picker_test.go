// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
	"testing"
)

// TestPick_AKS_WithOmsAgent_ExtendsExisting — §5 disjunction rule 1:
// iacContent has oms_agent block; picker extends that.
func TestPick_AKS_WithOmsAgent_ExtendsExisting(t *testing.T) {
	iac := `resource "azurerm_kubernetes_cluster" "prod" {
  name = "prod"
  oms_agent {
    log_analytics_workspace_id = azurerm_log_analytics_workspace.aks.id
  }
}
`
	p := Pick(RecommendationContext{Provider: "azure", Tier: "k8s", ResourceTFName: "prod"}, iac)
	if !strings.Contains(p.PrimaryTerraform, "oms_agent") {
		t.Errorf("expected oms_agent in PrimaryTerraform; got:\n%s", p.PrimaryTerraform)
	}
	if !strings.Contains(p.Reasoning, "extending existing oms_agent") {
		t.Errorf("expected reasoning to mention extending existing oms_agent; got %q", p.Reasoning)
	}
	if p.FallbackUsed {
		t.Errorf("FallbackUsed should be false when iacContent parses")
	}
}

// TestPick_AKS_WithMonitorProfile_ExtendsMonitorProfile — §5 rule 2.
func TestPick_AKS_WithMonitorProfile_ExtendsMonitorProfile(t *testing.T) {
	iac := `resource "azurerm_kubernetes_cluster" "prod" {
  azure_monitor_profile {
    metrics {
      labels_allowed = ""
    }
  }
}
`
	p := Pick(RecommendationContext{Provider: "azure", Tier: "k8s", ResourceTFName: "prod"}, iac)
	if !strings.Contains(p.PrimaryTerraform, "azure_monitor_profile") {
		t.Errorf("expected azure_monitor_profile in PrimaryTerraform; got:\n%s", p.PrimaryTerraform)
	}
	if !strings.Contains(p.Reasoning, "azure_monitor_profile") {
		t.Errorf("expected reasoning to cite azure_monitor_profile; got %q", p.Reasoning)
	}
	if p.FallbackUsed {
		t.Errorf("FallbackUsed should be false when iacContent parses")
	}
}

// TestPick_AKS_Empty_DefaultsToMonitorMetrics — §5 rule 3.
func TestPick_AKS_Empty_DefaultsToMonitorMetrics(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "azure", Tier: "k8s"}, "")
	if !strings.Contains(p.PrimaryTerraform, "monitor_metrics") {
		t.Errorf("expected monitor_metrics in PrimaryTerraform; got:\n%s", p.PrimaryTerraform)
	}
	if !strings.Contains(p.Reasoning, "monitor_metrics") {
		t.Errorf("expected reasoning to cite monitor_metrics; got %q", p.Reasoning)
	}
	if p.FallbackUsed {
		t.Errorf("FallbackUsed should be false on the documented-default path")
	}
}

// TestPick_AKS_Unparseable_FallsBackToDefault — §11 threat model:
// mismatched braces means the picker can't trust the iacContent, so it
// falls back to the documented default and surfaces FallbackUsed=true.
func TestPick_AKS_Unparseable_FallsBackToDefault(t *testing.T) {
	// Mismatched braces — three opens, one close.
	iac := `resource "azurerm_kubernetes_cluster" "prod" {
  oms_agent {
    settings {
}
`
	p := Pick(RecommendationContext{Provider: "azure", Tier: "k8s"}, iac)
	if !p.FallbackUsed {
		t.Errorf("expected FallbackUsed=true on unparseable HCL")
	}
	if !strings.Contains(p.PrimaryTerraform, "monitor_metrics") {
		t.Errorf("fallback path should produce monitor_metrics; got:\n%s", p.PrimaryTerraform)
	}
}

// TestPick_AKS_OmsAgentInsideComment_DoesNotMatch — guard the
// containsBlock heuristic against comment-line false-positives.
func TestPick_AKS_OmsAgentInsideComment_DoesNotMatch(t *testing.T) {
	iac := `resource "azurerm_kubernetes_cluster" "prod" {
  # we used to have oms_agent { here
  name = "prod"
}
`
	p := Pick(RecommendationContext{Provider: "azure", Tier: "k8s", ResourceTFName: "prod"}, iac)
	if strings.Contains(p.Reasoning, "extending existing oms_agent") {
		t.Errorf("commented-out oms_agent should NOT trigger extend; reasoning=%q", p.Reasoning)
	}
	if !strings.Contains(p.PrimaryTerraform, "monitor_metrics") {
		t.Errorf("expected default monitor_metrics when oms_agent only appears in comments; got:\n%s", p.PrimaryTerraform)
	}
}

// TestPick_AWS_EC2_DefaultPattern — verify §4.1 snippet.
func TestPick_AWS_EC2_DefaultPattern(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "aws", Tier: "compute"}, "")
	expected := []string{
		"aws_ssm_association",
		"AWS-ConfigureAWSPackage",
		`key    = "tag:otel-collector"`,
		`name   = "AWSDistroOTel-Collector"`,
	}
	for _, want := range expected {
		if !strings.Contains(p.PrimaryTerraform, want) {
			t.Errorf("expected %q in PrimaryTerraform; got:\n%s", want, p.PrimaryTerraform)
		}
	}
}

// TestPick_GKE_DefaultPattern — verify §4.6 snippet.
func TestPick_GKE_DefaultPattern(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "gcp", Tier: "k8s"}, "")
	expected := []string{
		"google_gke_hub_feature",
		`name     = "servicemesh"`,
		`location = "global"`,
	}
	for _, want := range expected {
		if !strings.Contains(p.PrimaryTerraform, want) {
			t.Errorf("expected %q in PrimaryTerraform; got:\n%s", want, p.PrimaryTerraform)
		}
	}
}

// TestPick_AllTwelveTiers_ReturnNonEmptyTerraform — every (provider,
// tier) pair in §4 produces a non-empty Terraform snippet.
func TestPick_AllTwelveTiers_ReturnNonEmptyTerraform(t *testing.T) {
	cases := []struct {
		provider, tier string
		mustContain    string
	}{
		{"aws", "compute", "aws_ssm_association"},
		{"aws", "db", "performance_insights_retention_period"},
		{"aws", "k8s", "aws_eks_addon"},
		{"gcp", "compute", "enable-osconfig"},
		{"gcp", "db", "record_application_tags"},
		{"gcp", "k8s", "google_gke_hub_feature"},
		{"azure", "compute", "AzureMonitorLinuxAgent"},
		{"azure", "db", "azurerm_mssql_database_extended_auditing_policy"},
		{"azure", "k8s", "monitor_metrics"},
		{"oci", "compute", "oci_core_instance"},
		{"oci", "db", "oci_database_management_managed_database_group"},
		{"oci", "k8s", "kubernetes_manifest"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"-"+tc.tier, func(t *testing.T) {
			p := Pick(RecommendationContext{Provider: tc.provider, Tier: tc.tier}, "")
			if p.PrimaryTerraform == "" {
				t.Fatalf("PrimaryTerraform empty for (%s, %s)", tc.provider, tc.tier)
			}
			if !strings.Contains(p.PrimaryTerraform, tc.mustContain) {
				t.Errorf("expected %q in PrimaryTerraform for (%s, %s); got:\n%s",
					tc.mustContain, tc.provider, tc.tier, p.PrimaryTerraform)
			}
			if p.Reasoning == "" {
				t.Errorf("Reasoning empty for (%s, %s)", tc.provider, tc.tier)
			}
		})
	}
}

// TestPick_UnknownProviderTier_ReturnsFallback — sanity-check the
// dispatcher's default arm.
func TestPick_UnknownProviderTier_ReturnsFallback(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "ibm", Tier: "compute"}, "")
	if !p.FallbackUsed {
		t.Errorf("expected FallbackUsed=true for unrecognized provider")
	}
	if p.PrimaryTerraform != "" {
		t.Errorf("PrimaryTerraform should be empty on unrecognized (provider, tier) pair")
	}
}

// TestPick_OCICompute_FlagsCloudInitMaintenanceCaveat — §4.10 says the
// recommendation reasoning must call out that cloud-init runs only on
// first boot. The picker's Reasoning carries that caveat.
func TestPick_OCICompute_FlagsCloudInitMaintenanceCaveat(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "oci", Tier: "compute"}, "")
	if !strings.Contains(strings.ToLower(p.Reasoning), "cloud-init") {
		t.Errorf("expected OCI compute reasoning to mention cloud-init; got %q", p.Reasoning)
	}
	if !strings.Contains(strings.ToLower(p.Reasoning), "first boot") {
		t.Errorf("expected OCI compute reasoning to mention first boot; got %q", p.Reasoning)
	}
}

// TestPick_AWSRDS_ResourceTFNameThreadedIntoSnippet — the picker
// inlines a best-effort Terraform resource name when supplied.
func TestPick_AWSRDS_ResourceTFNameThreadedIntoSnippet(t *testing.T) {
	p := Pick(RecommendationContext{Provider: "aws", Tier: "db", ResourceTFName: "prod_orders"}, "")
	if !strings.Contains(p.PrimaryTerraform, `"prod_orders"`) {
		t.Errorf("expected ResourceTFName threaded into Terraform; got:\n%s", p.PrimaryTerraform)
	}
}
