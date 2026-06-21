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
