// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import "testing"

// TestClassifyResourceKind pins the snippet-and-name -> placement-
// map-row-key classifier to the seven slice-1 resource_kind values.
// The Recommendations tab's Open-PR button reads this field to
// decide whether the per-card action is available.
//
// Why two layers? The classifier prefers the Terraform resource type
// the proposer emits (stable across prompt revisions) and falls back
// to the step name (drifts with prompt copy edits). Both rules are
// exercised here so a prompt copy edit on the proposer side that
// stops naming the category in the step title still classifies via
// the Terraform body.
//
// EKS classification is the case that does NOT have a clean
// answer — the proposer emits a single step covering both the
// control-plane-logging axis and the addon axis. The classifier
// resolves to eks-observability-addon when the snippet creates an
// aws_eks_addon resource (the more specific lever) and to
// eks-cluster-logging when only aws_eks_cluster is touched. Both
// kinds exist as separate placement-map rows; the operator's
// placement map decides which file the PR lands in.
func TestClassifyResourceKind(t *testing.T) {
	cases := []struct {
		name    string
		stepNam string
		snippet string
		want    string
	}{
		{
			name:    "lambda by snippet",
			stepNam: "AI plan step 0: instrument 2 Lambda functions with OpenTelemetry layer",
			snippet: `resource "aws_lambda_function" "hello" { layers = [...] }`,
			want:    "lambda-otel-layer",
		},
		{
			name:    "ec2 by ssm snippet",
			stepNam: "AI plan step 1: instrument 1 EC2 instance with ADOT collector",
			snippet: `resource "aws_ssm_association" "adot" { name = "..." }`,
			want:    "ec2-otel-layer",
		},
		{
			name:    "rds pi em by snippet",
			stepNam: "Enable PI on RDS",
			snippet: `resource "aws_db_instance" "x" { performance_insights_enabled = true }`,
			want:    "rds-pi-em",
		},
		{
			name:    "s3 access logging by snippet",
			stepNam: "Enable S3 access logging",
			snippet: `resource "aws_s3_bucket_logging" "x" { target_bucket = "logs" }`,
			want:    "s3-access-logging",
		},
		{
			name:    "alb access logs by snippet",
			stepNam: "Enable ALB access logs",
			snippet: `resource "aws_lb" "x" { access_logs { bucket = "logs" enabled = true } }`,
			want:    "alb-access-logs",
		},
		{
			name:    "eks addon wins over cluster axis",
			stepNam: "Enable control plane logging AND install adot addon",
			snippet: `resource "aws_eks_cluster" "x" {} resource "aws_eks_addon" "adot" { addon_name = "adot" }`,
			want:    "eks-observability-addon",
		},
		{
			name:    "eks cluster logging only",
			stepNam: "Enable control plane logging",
			snippet: `resource "aws_eks_cluster" "x" { enabled_cluster_log_types = ["api", "audit"] }`,
			want:    "eks-cluster-logging",
		},
		{
			// Slice 4 (v0.89.6): the snippet-first classifier matches
			// aws_dynamodb_contributor_insights and returns the new
			// canonical kind. The Open-PR button on the
			// Recommendations tab reads this field to look up the
			// 8th placement-map row.
			name:    "dynamodb contributor insights by snippet",
			stepNam: "Enable Contributor Insights on orders + events",
			snippet: `resource "aws_dynamodb_contributor_insights" "orders" { table_name = "orders" }`,
			want:    "dynamodb-contributor-insights",
		},
		{
			// Step-name fallback: snippet doesn't name the canonical
			// resource type but the step title is unambiguous.
			name:    "dynamodb by step name fallback",
			stepNam: "Enable Contributor Insights on DynamoDB tables",
			snippet: `# Terraform body redacted`,
			want:    "dynamodb-contributor-insights",
		},
		{
			// Slice 5 (v0.89.10): the snippet-first classifier
			// matches aws_ecs_cluster and returns the new canonical
			// kind. The Open-PR button on the Recommendations tab
			// reads this field to look up the 9th placement-map row.
			name:    "ecs container insights by snippet",
			stepNam: "Enable Container Insights on prod + staging",
			snippet: `resource "aws_ecs_cluster" "prod" { name = "prod" setting { name = "containerInsights" value = "enabled" } }`,
			want:    "ecs-container-insights",
		},
		{
			// Step-name fallback: snippet doesn't name the canonical
			// resource type but the step title is unambiguous —
			// "ECS" / "container insights" route to the slice 5 kind.
			name:    "ecs by step name fallback",
			stepNam: "Enable Container Insights on ECS clusters",
			snippet: `# Terraform body redacted`,
			want:    "ecs-container-insights",
		},
		// Name-fallback cases: snippet doesn't contain a known
		// resource type, classification falls back to the step name.
		{
			name:    "lambda by step name fallback",
			stepNam: "instrument Lambda functions",
			snippet: `# Terraform snippet placeholder — actual body redacted.`,
			want:    "lambda-otel-layer",
		},
		{
			name:    "ec2 by step name fallback",
			stepNam: "instrument N EC2 instances with ADOT",
			snippet: `# placeholder`,
			want:    "ec2-otel-layer",
		},
		{
			name:    "no classification",
			stepNam: "unknown shape",
			snippet: "",
			want:    "",
		},

		// --- #182: GCP / Azure / OCI ---
		{
			name:    "gcp gcs bucket logging",
			stepNam: "enable storage logging",
			snippet: `resource "google_storage_bucket" "b" { logging { log_bucket = "x" } }`,
			want:    "gcs-logging-enable",
		},
		{
			name:    "gcp backend-service logging",
			stepNam: "enable LB logging",
			snippet: `resource "google_compute_backend_service" "b" { log_config { enable = true } }`,
			want:    "gclb-logging-enable",
		},
		{
			name:    "gcp cloud sql",
			stepNam: "enable query insights",
			snippet: `resource "google_sql_database_instance" "db" {}`,
			want:    "cloudsql-pi-enable",
		},
		{
			name:    "gcp gke",
			stepNam: "enable managed prometheus",
			snippet: `resource "google_container_cluster" "c" {}`,
			want:    "gke-mp-enable",
		},
		{
			name:    "gcp compute instance label",
			stepNam: "tag GCE",
			snippet: `resource "google_compute_instance" "vm" { labels = {} }`,
			want:    "gce-otel-label",
		},
		{
			name:    "azure blob diag (disambiguated by storage account)",
			stepNam: "enable blob logging",
			snippet: `resource "azurerm_storage_account" "s" {} resource "azurerm_monitor_diagnostic_setting" "d" {}`,
			want:    "azblob-diag-enable",
		},
		{
			name:    "azure lb diag (disambiguated by azurerm_lb)",
			stepNam: "enable lb logging",
			snippet: `resource "azurerm_lb" "lb" {} resource "azurerm_monitor_diagnostic_setting" "d" {}`,
			want:    "azlb-diag-enable",
		},
		{
			name:    "azure sql diag",
			stepNam: "enable sqlinsights",
			snippet: `resource "azurerm_mssql_database" "db" {} resource "azurerm_monitor_diagnostic_setting" "d" {}`,
			want:    "azsql-diag-enable",
		},
		{
			name:    "azure aks",
			stepNam: "enable aks monitoring",
			snippet: `resource "azurerm_kubernetes_cluster" "k" {}`,
			want:    "aks-monitor-enable",
		},
		{
			name:    "azure vm",
			stepNam: "tag azure vm",
			snippet: `resource "azurerm_linux_virtual_machine" "vm" { tags = {} }`,
			want:    "vm-otel-tag",
		},
		{
			name:    "oci bucket logging (oci_logging_log + objectstorage source)",
			stepNam: "enable bucket logging",
			snippet: `resource "oci_logging_log" "l" { configuration { source { service = "objectstorage" } } }`,
			want:    "ocibucket-logging-enable",
		},
		{
			name:    "oci lb logging (oci_logging_log + loadbalancer source)",
			stepNam: "enable lb logging",
			snippet: `resource "oci_logging_log" "l" { configuration { source { service = "loadbalancer" } } }`,
			want:    "ocilb-logging-enable",
		},
		{
			name:    "oci database",
			stepNam: "enable operations insights",
			snippet: `resource "oci_database_db_system" "db" {}`,
			want:    "ocidb-perfhub-enable",
		},
		{
			name:    "oci oke",
			stepNam: "enroll oke",
			snippet: `resource "oci_containerengine_cluster" "c" {}`,
			want:    "oke-ops-insights-enable",
		},
		{
			name:    "oci compute tag",
			stepNam: "tag oci instance",
			snippet: `resource "oci_core_instance" "vm" { freeform_tags = {} }`,
			want:    "compute-otel-tag",
		},
		{
			name:    "non-aws provider marker blocks aws name fallback",
			stepNam: "enable load balancer logging",             // would hit alb-access-logs by name
			snippet: `resource "google_compute_network" "n" {}`, // google_ marker, no matched rule
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyResourceKind(tc.stepNam, tc.snippet)
			if got != tc.want {
				t.Errorf("classifyResourceKind(%q, %q) = %q, want %q",
					tc.stepNam, tc.snippet, got, tc.want)
			}
		})
	}
}
