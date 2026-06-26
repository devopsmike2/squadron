// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacpicker

import (
	"strings"
)

// RecommendationContext is the input to Pick. The caller threads the
// (provider, tier) pair the inventory row belongs to plus a best-effort
// Terraform resource name (when one was discoverable from the repo
// content) and the names of any related blocks already present. Pick
// dispatches to the per-cloud handler keyed by (Provider, Tier).
type RecommendationContext struct {
	Provider       string // "aws" / "gcp" / "azure" / "oci"
	Tier           string // "compute" / "db" / "k8s"
	ResourceTFName string // the Terraform resource name found in the repo (best-effort)
	ExistingBlocks []string
}

// PickedPattern is Pick's output. The caller threads PrimaryTerraform
// into the recommendation's snippet field and Reasoning into the
// proposer prompt context so the model can cite the picker's choice in
// the recommendation reasoning.
type PickedPattern struct {
	PrimaryTerraform string
	Reasoning        string
	FallbackUsed     bool
}

// Pick selects the Terraform pattern for the given context. The
// iacContent string is the raw HCL of the file the recommendation
// targets (empty means we'll use the default pattern).
//
// Per docs/proposals/trace-integration-slice2.md §5, when multiple
// valid Terraform paths exist for a (provider, tier) pair (today: Azure
// AKS with three possible observability blocks), Pick reads iacContent
// to decide whether to extend an existing block or introduce the
// documented default. Unparseable iacContent falls back to the default
// and surfaces FallbackUsed=true so the audit trail records the
// classification miss.
func Pick(ctx RecommendationContext, iacContent string) PickedPattern {
	switch ctx.Provider {
	case "aws":
		switch ctx.Tier {
		case "compute":
			return pickAWSCompute(ctx, iacContent)
		case "db":
			return pickAWSDB(ctx, iacContent)
		case "k8s":
			return pickAWSK8s(ctx, iacContent)
		}
	case "gcp":
		switch ctx.Tier {
		case "compute":
			return pickGCPCompute(ctx, iacContent)
		case "db":
			return pickGCPDB(ctx, iacContent)
		case "k8s":
			return pickGCPK8s(ctx, iacContent)
		}
	case "azure":
		switch ctx.Tier {
		case "compute":
			return pickAzureCompute(ctx, iacContent)
		case "db":
			return pickAzureDB(ctx, iacContent)
		case "k8s":
			return pickAzureK8s(ctx, iacContent)
		}
	case "oci":
		switch ctx.Tier {
		case "compute":
			return pickOCICompute(ctx, iacContent)
		case "db":
			return pickOCIDB(ctx, iacContent)
		case "k8s":
			return pickOCIK8s(ctx, iacContent)
		}
	}
	return PickedPattern{
		PrimaryTerraform: "",
		Reasoning:        "iacpicker: unrecognized (provider, tier) pair; no Terraform pattern available",
		FallbackUsed:     true,
	}
}

// --- AWS ---

// AWS EC2 (§4.1) — install the ADOT (AWS Distro for OpenTelemetry)
// Collector via the AWS-managed SSM Distributor package
// AWSDistroOTel-Collector. Tag-based association so the operator scopes
// coverage by tag rather than by individual instance.
//
// This installs the ADOT Collector — NOT AmazonCloudWatchAgent, which is a
// different agent that does not emit OpenTelemetry traces. The AWS-managed
// package auto-selects the correct arm64/amd64 build for the target's
// architecture, so this path is correct on Graviton/arm64 and x86_64 alike
// with no hand-rolled arch-match (the reason §4.1 prefers it). Targets need
// the SSM agent and AmazonSSMManagedInstanceCore on their instance role.
func pickAWSCompute(_ RecommendationContext, _ string) PickedPattern {
	return PickedPattern{
		PrimaryTerraform: `resource "aws_ssm_association" "otel_collector_install" {
  name = "AWS-ConfigureAWSPackage"

  targets {
    key    = "tag:otel-collector"
    values = ["v1"]
  }

  parameters = {
    action = "Install"
    # AWS-managed ADOT Collector package; auto-selects arm64/amd64.
    name   = "AWSDistroOTel-Collector"
  }
}
`,
		Reasoning: "AWS EC2 trace-emission: introducing aws_ssm_association with the AWS-managed SSM Distributor package AWSDistroOTel-Collector to install the ADOT Collector (auto arch-selected, so it is correct on Graviton/arm64 and x86_64 alike) per §4.1. This is the ADOT Collector, not the CloudWatch Agent — the latter does not emit OpenTelemetry traces.",
	}
}

// AWS RDS (§4.2) — enable Performance Insights long-term retention. The
// resource block extends an existing aws_db_instance; we patch the two
// attributes.
func pickAWSDB(ctx RecommendationContext, _ string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	return PickedPattern{
		PrimaryTerraform: `resource "aws_db_instance" "` + name + `" {
  # ... existing fields ...
  performance_insights_enabled          = true
  performance_insights_retention_period = 731  # 2 years (LTR tier)
}
`,
		Reasoning: "AWS RDS trace-emission: extending the existing aws_db_instance to enable Performance Insights LTR (retention=731) per §4.2.",
	}
}

// AWS EKS (§4.3) — install the ADOT operator via the EKS addon mechanism.
func pickAWSK8s(ctx RecommendationContext, _ string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	return PickedPattern{
		PrimaryTerraform: `resource "aws_eks_addon" "adot" {
  cluster_name             = aws_eks_cluster.` + name + `.name
  addon_name               = "adot"
  service_account_role_arn = aws_iam_role.adot.arn
}
`,
		Reasoning: "AWS EKS trace-emission: introducing aws_eks_addon \"adot\" to install the ADOT operator on the cluster per §4.3.",
	}
}

// --- GCP ---

// GCP GCE (§4.4) — Ops Agent metadata block.
func pickGCPCompute(ctx RecommendationContext, _ string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	return PickedPattern{
		PrimaryTerraform: `resource "google_compute_instance" "` + name + `" {
  # ... existing fields ...
  metadata = {
    enable-osconfig = "TRUE"
    google-logging-enabled = "true"
    google-monitoring-enabled = "true"
  }
}
`,
		Reasoning: "GCP GCE trace-emission: extending google_compute_instance.metadata with enable-osconfig + google-logging-enabled + google-monitoring-enabled per §4.4.",
	}
}

// GCP Cloud SQL (§4.5) — Query Insights enhanced fields.
func pickGCPDB(ctx RecommendationContext, _ string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	return PickedPattern{
		PrimaryTerraform: `resource "google_sql_database_instance" "` + name + `" {
  settings {
    insights_config {
      query_insights_enabled  = true
      record_application_tags = true
      record_client_address   = true
    }
  }
}
`,
		Reasoning: "GCP Cloud SQL trace-emission: extending insights_config with record_application_tags + record_client_address so application correlation lands per §4.5.",
	}
}

// GCP GKE (§4.6) — Cloud Service Mesh hub feature.
func pickGCPK8s(_ RecommendationContext, _ string) PickedPattern {
	return PickedPattern{
		PrimaryTerraform: `resource "google_gke_hub_feature" "service_mesh" {
  name     = "servicemesh"
  location = "global"
}
`,
		Reasoning: "GCP GKE trace-emission: introducing google_gke_hub_feature \"servicemesh\" to deploy the OpenTelemetry Operator via Cloud Service Mesh per §4.6.",
	}
}

// --- Azure ---

// Azure VM (§4.7) — Azure Monitor Agent extension.
//
// The Azure Monitor Agent extension is OS-SPECIFIC: Linux VMs take
// AzureMonitorLinuxAgent (on azurerm_linux_virtual_machine), Windows VMs
// take AzureMonitorWindowsAgent (on azurerm_windows_virtual_machine).
// Installing the Linux agent extension on a Windows VM fails to provision.
// The deterministic picker can't see the candidate's OSFamily, so the
// snippet defaults to the Linux agent and the comment + reasoning flag the
// Windows swap; the LLM discovery path derives the variant from os_family.
func pickAzureCompute(ctx RecommendationContext, _ string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	return PickedPattern{
		PrimaryTerraform: `resource "azurerm_virtual_machine_extension" "azure_monitor_agent" {
  # OS-SPECIFIC — this is the LINUX agent. For a WINDOWS VM set both name and
  # type to "AzureMonitorWindowsAgent" and point virtual_machine_id at
  # azurerm_windows_virtual_machine.` + name + `.id — the Linux agent
  # extension fails to provision on a Windows VM.
  name                 = "AzureMonitorLinuxAgent"
  virtual_machine_id   = azurerm_linux_virtual_machine.` + name + `.id
  publisher            = "Microsoft.Azure.Monitor"
  type                 = "AzureMonitorLinuxAgent"
  type_handler_version = "1.0"
}
`,
		Reasoning: "Azure VM trace-emission: introducing azurerm_virtual_machine_extension with the Azure Monitor Agent per §4.7. OS-specific: the snippet defaults to the LINUX agent (AzureMonitorLinuxAgent); for a Windows VM swap both name and type to AzureMonitorWindowsAgent on azurerm_windows_virtual_machine — the wrong-OS agent extension fails to provision.",
	}
}

// Azure SQL (§4.8) — extended auditing policy with log_monitoring_enabled.
func pickAzureDB(ctx RecommendationContext, _ string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	return PickedPattern{
		PrimaryTerraform: `resource "azurerm_mssql_database_extended_auditing_policy" "` + name + `" {
  database_id            = azurerm_mssql_database.` + name + `.id
  log_monitoring_enabled = true
}
`,
		Reasoning: "Azure SQL trace-emission: extending azurerm_mssql_database_extended_auditing_policy with log_monitoring_enabled=true per §4.8.",
	}
}

// Azure AKS (§4.9) — three-way disjunction per the design doc:
//
//  1. If iacContent has oms_agent, extend that block.
//  2. Else if iacContent has azure_monitor_profile, extend that.
//  3. Else default to monitor_metrics (the newer pattern).
//
// Unparseable iacContent (mismatched braces) falls back to monitor_metrics
// and surfaces FallbackUsed=true so the audit trail records the miss.
func pickAzureK8s(ctx RecommendationContext, iacContent string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	monitorMetrics := `resource "azurerm_kubernetes_cluster" "` + name + `" {
  # ... existing fields ...
  monitor_metrics {
    # azurerm monitor_metrics.annotations_allowed is a COMMA-SEPARATED STRING,
    # not a list — a list value fails terraform plan with a type error.
    annotations_allowed = "service.name,service.instance.id"
  }
}
`
	omsAgent := `resource "azurerm_kubernetes_cluster" "` + name + `" {
  # ... existing fields ...
  oms_agent {
    log_analytics_workspace_id      = azurerm_log_analytics_workspace.aks.id
    msi_auth_for_monitoring_enabled = true
  }
}
`
	monitorProfile := `resource "azurerm_kubernetes_cluster" "` + name + `" {
  # ... existing fields ...
  azure_monitor_profile {
    metrics {
      annotations_allowed = "service.name,service.instance.id"
      labels_allowed      = ""
    }
  }
}
`
	// Heuristic parse check — count braces. Mismatched braces means the
	// repo content is not HCL we can trust; fall back to the default.
	if iacContent != "" && !hclLooksParseable(iacContent) {
		return PickedPattern{
			PrimaryTerraform: monitorMetrics,
			Reasoning:        "Azure AKS trace-emission: iacContent failed brace-balance check; falling back to the documented default monitor_metrics pattern per §4.9 / §5.",
			FallbackUsed:     true,
		}
	}
	switch {
	case iacContent == "":
		return PickedPattern{
			PrimaryTerraform: monitorMetrics,
			Reasoning:        "Azure AKS trace-emission: no existing IaC content; defaulting to the newer monitor_metrics block per §5.",
		}
	case containsBlock(iacContent, "oms_agent"):
		return PickedPattern{
			PrimaryTerraform: omsAgent,
			Reasoning:        "Azure AKS trace-emission: extending existing oms_agent block (legacy Container Insights addon) per §5.",
		}
	case containsBlock(iacContent, "azure_monitor_profile"):
		return PickedPattern{
			PrimaryTerraform: monitorProfile,
			Reasoning:        "Azure AKS trace-emission: extending existing azure_monitor_profile.metrics block per §5.",
		}
	default:
		return PickedPattern{
			PrimaryTerraform: monitorMetrics,
			Reasoning:        "Azure AKS trace-emission: no oms_agent or azure_monitor_profile block in IaC; defaulting to the newer monitor_metrics block per §5.",
		}
	}
}

// --- OCI ---

// OCI Compute (§4.10) — cloud-init script in user_data. Note: only
// runs on first boot, so the recommendation reasoning flags this as an
// upgrade-during-maintenance change.
func pickOCICompute(ctx RecommendationContext, _ string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	return PickedPattern{
		PrimaryTerraform: `resource "oci_core_instance" "` + name + `" {
  # ... existing fields ...
  metadata = {
    # OCI APM uses the Java agent (Linux + JVM workloads only). There is NO
    # public curl|bash installer. The real flow: download
    # apm-java-agent-installer-<ver>.jar from your APM domain (Console:
    # Observability & Management -> Application Performance Monitoring ->
    # Administration -> Download APM Agents -> Java), stage it on the host,
    # provision it against the domain, then add -javaagent to the app's JVM.
    user_data = base64encode(<<-EOT
      #!/bin/bash
      # Stage apm-java-agent-installer-<ver>.jar on the host first (e.g. from
      # Object Storage), then provision against your APM domain. Fill in the
      # private-data-key and data-upload-endpoint from the APM domain.
      java -jar /opt/apm/apm-java-agent-installer-<ver>.jar provision-agent -service-name "<service-name>" -destination /opt/apm -private-data-key "<APM_PRIVATE_DATA_KEY>" -data-upload-endpoint "<APM_DATA_UPLOAD_ENDPOINT>"
      # Then attach to the app's JVM startup:
      #   -javaagent:/opt/apm/oracle-apm-agent/bootstrap/ApmAgent.jar
    EOT
    )
  }
}
`,
		Reasoning: "OCI Compute trace-emission: extending oci_core_instance.metadata.user_data to provision the OCI APM Java agent per §4.10. The installer JAR is downloaded from your APM domain (no public curl|bash URL exists) and provisioned with the APM private-data-key + data-upload-endpoint, then attached via -javaagent. Linux + JVM workloads only; cloud-init runs on first boot, so flag this as upgrade-during-maintenance.",
	}
}

// OCI Autonomous DB (§4.11) — Database Management managed group.
func pickOCIDB(ctx RecommendationContext, _ string) PickedPattern {
	name := ctx.ResourceTFName
	if name == "" {
		name = "<name>"
	}
	return PickedPattern{
		PrimaryTerraform: `resource "oci_database_management_managed_database_group" "` + name + `" {
  compartment_id = var.compartment_ocid
  name           = "squadron-managed"
}
`,
		Reasoning: "OCI Autonomous DB trace-emission: introducing oci_database_management_managed_database_group to enable Operations Insights / Database Management per §4.11.",
	}
}

// OCI OKE (§4.12) — OCI Service Operator via kubernetes_manifest. The
// pattern's a soft IaC-pure path; the kubernetes_manifest resource
// targets the upstream Service Operator install.
func pickOCIK8s(_ RecommendationContext, _ string) PickedPattern {
	return PickedPattern{
		PrimaryTerraform: `resource "kubernetes_manifest" "oci_service_operator" {
  manifest = yamldecode(file("${path.module}/manifests/oci-service-operator.yaml"))
}

resource "oci_containerengine_cluster" "this" {
  # ... existing fields ...
  options {
    # The kubernetes_manifest above installs the OCI Service Operator
    # into the cluster, which provides Operations Insights integration
    # for workloads. See §4.12.
  }
}
`,
		Reasoning: "OCI OKE trace-emission: installing the OCI Service Operator via kubernetes_manifest for Operations Insights integration per §4.12.",
	}
}

// --- HCL parse helpers ---

// hclLooksParseable is a permissive brace-balance heuristic. Returns
// true when iacContent has equal numbers of '{' and '}'. False signals
// likely-malformed HCL, which sends Pick to the documented fallback.
// We do NOT pull in hashicorp/hcl/v2 here — the picker only needs to
// distinguish "obviously broken" from "good enough" for the §5 split.
func hclLooksParseable(iacContent string) bool {
	open := strings.Count(iacContent, "{")
	closeC := strings.Count(iacContent, "}")
	return open == closeC
}

// containsBlock returns true when iacContent has a Terraform block
// declaration (or attribute reference) matching the given name. We
// match on "<name> {" with optional whitespace because both block
// openings ("oms_agent {") and named block references ("azure_monitor_profile {")
// follow that shape. A substring search alone would false-positive on
// comments and string literals.
func containsBlock(iacContent, name string) bool {
	if !strings.Contains(iacContent, name) {
		return false
	}
	// Scan for "<name>" followed by whitespace and "{" — block opening.
	idx := 0
	for idx < len(iacContent) {
		found := strings.Index(iacContent[idx:], name)
		if found < 0 {
			return false
		}
		end := idx + found + len(name)
		if end >= len(iacContent) {
			return false
		}
		// Check what follows. Skip whitespace, then look for "{".
		j := end
		for j < len(iacContent) && (iacContent[j] == ' ' || iacContent[j] == '\t') {
			j++
		}
		if j < len(iacContent) && iacContent[j] == '{' {
			// Confirm this isn't inside a comment line by walking back to
			// the previous newline and checking for #.
			lineStart := strings.LastIndex(iacContent[:idx+found], "\n") + 1
			line := iacContent[lineStart : idx+found]
			if !strings.Contains(line, "#") && !strings.Contains(line, "//") {
				return true
			}
		}
		idx = end
	}
	return false
}
