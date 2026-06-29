// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package iac owns the cross-cutting structural facts about how
// Squadron lands Terraform on the operator's IaC repo. Slice 1
// (v0.89.3 #603) shipped an append-only file edit — for the 5-of-9
// recommendation kinds whose Terraform shape is a NEW top-level
// resource, that produces a clean drop-in; for the 4-of-9 kinds
// whose Terraform shape MODIFIES an EXISTING resource block, the
// appended snippet produces a duplicate-resource error at terraform
// plan time.
//
// Slice 1.5 (v0.89.11 #626 Stream 27) introduces per-kind
// DISPOSITION as the structural fact the proposer outputs and the
// Open-PR handler routes on. Two values:
//
//   - "new_file" — the snippet defines a NET-NEW top-level resource
//     that does not conflict with any existing block. Squadron writes
//     a SIBLING FILE squadron_<resource_kind>.tf in the placement
//     file's directory. Clean drop-in; merge-clean by construction.
//
//   - "patch_existing" — the snippet MODIFIES an attribute or nested
//     block on an EXISTING top-level resource. The slice-1
//     append-only behavior is preserved (no HCL parsing), but the PR
//     is clearly labeled "[needs manual merge]" so the operator
//     knows before reviewing that they must hand-integrate the
//     change. Slice 2 (Stream 28 design landing in parallel as
//     docs/proposals/603-slice-2-hcl-aware-merging.md) will replace
//     this with proper HCL-aware merging.
//
// The classification is STRUCTURAL — it follows from the Terraform
// resource shape each recommendation kind emits, not from a model
// judgment. The proposer prompt teaches the model to emit the
// disposition; the Open-PR handler OVERRIDES the model's choice
// with the lookup here on every request. Trust-but-verify: the model
// can't improvise a structural fact.
package iac

// Disposition constants. Stable string values on the wire — the
// audit payload, the proposer prompt, and the UI all reference them
// by these exact strings.
const (
	// DispositionNewFile names the disposition for recommendation
	// kinds whose Terraform shape is a NET-NEW top-level resource.
	// The Open-PR handler creates a sibling file
	// squadron_<resource_kind>.tf in the placement file's directory.
	DispositionNewFile = "new_file"

	// DispositionPatchExisting names the disposition for kinds whose
	// Terraform shape MODIFIES an EXISTING top-level resource block.
	// The Open-PR handler appends to the placement file (slice-1
	// behavior) and labels the PR "[needs manual merge]".
	DispositionPatchExisting = "patch_existing"
)

// KindDispositions is the locked structural classification for the
// 9 slice-1 recommendation kinds. Each value follows from the
// Terraform shape the proposer emits for that kind:
//
//   - ec2-otel-layer: aws_ssm_association is a NEW top-level
//     resource that references existing EC2 instances by tag/id.
//     The association block does not modify the aws_instance block.
//   - lambda-otel-layer: modifies aws_lambda_function.layers and
//     environment.variables. The proposer's snippet redeclares the
//     aws_lambda_function block, which conflicts with the existing
//     module's declaration of the same resource.
//   - rds-pi-em: modifies aws_db_instance.performance_insights_enabled
//     and aws_db_instance.monitoring_interval on an existing block.
//   - s3-access-logging: aws_s3_bucket_logging is its own top-level
//     resource that references an existing bucket by id.
//   - alb-access-logs: modifies the access_logs nested block on an
//     existing aws_lb resource.
//   - eks-cluster-logging: modifies aws_eks_cluster.enabled_cluster_log_types
//     on an existing cluster block.
//   - eks-observability-addon: aws_eks_addon is its own top-level
//     resource that references an existing cluster by name.
//   - dynamodb-contributor-insights: aws_dynamodb_contributor_insights
//     is its own top-level resource per table.
//   - ecs-container-insights: modifies the setting nested block on
//     an existing aws_ecs_cluster resource.
//
// Four kinds map to new_file; five kinds map to patch_existing.
// (Per the structural Terraform-shape lock in #626 Stream 27.)
var KindDispositions = map[string]string{
	"ec2-otel-layer":                DispositionNewFile,
	"lambda-otel-layer":             DispositionPatchExisting,
	"rds-pi-em":                     DispositionPatchExisting,
	"s3-access-logging":             DispositionNewFile,
	"alb-access-logs":               DispositionPatchExisting,
	"eks-cluster-logging":           DispositionPatchExisting,
	"eks-observability-addon":       DispositionNewFile,
	"dynamodb-contributor-insights": DispositionNewFile,
	"ecs-container-insights":        DispositionPatchExisting,

	// GCP (#182). Label/instance/block edits patch the existing
	// resource; none create a standalone new resource.
	"gce-otel-label":      DispositionPatchExisting,
	"cloudsql-pi-enable":  DispositionPatchExisting,
	"gke-mp-enable":       DispositionPatchExisting,
	"gcs-logging-enable":  DispositionPatchExisting,
	"gclb-logging-enable": DispositionPatchExisting,

	// Azure (#182). Diagnostic-setting kinds create a NEW
	// azurerm_monitor_diagnostic_setting resource; the VM/AKS edits
	// patch the existing resource.
	"vm-otel-tag":        DispositionPatchExisting,
	"aks-monitor-enable": DispositionPatchExisting,
	"azsql-diag-enable":  DispositionNewFile,
	"azblob-diag-enable": DispositionNewFile,
	"azlb-diag-enable":   DispositionNewFile,

	// OCI (#182). Logging-log kinds create a NEW oci_logging_log
	// (+ log group) resource; tag/instance edits patch existing.
	"compute-otel-tag":         DispositionPatchExisting,
	"ocidb-perfhub-enable":     DispositionPatchExisting,
	"oke-ops-insights-enable":  DispositionPatchExisting,
	"ocibucket-logging-enable": DispositionNewFile,
	"ocilb-logging-enable":     DispositionNewFile,
}

// DispositionFor returns the disposition for a given resource_kind.
// Unknown kinds default to DispositionPatchExisting — the SAFE
// default: when Squadron can't be sure whether the snippet's
// resource block already exists in the placement file, the
// append-with-manual-merge-warning posture is strictly safer than
// silently writing a new file that might shadow an existing one.
//
// Empty input returns DispositionPatchExisting for the same reason.
func DispositionFor(resourceKind string) string {
	if resourceKind == "" {
		return DispositionPatchExisting
	}
	d, ok := KindDispositions[resourceKind]
	if !ok {
		return DispositionPatchExisting
	}
	return d
}
