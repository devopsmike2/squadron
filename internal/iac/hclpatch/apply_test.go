// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package hclpatch

import (
	"errors"
	"strings"
	"testing"
)

// The seven acceptance tests in this file mirror design doc
// 603-slice-2-hcl-aware-merging.md §12, one test per
// patch_existing kind plus the lifecycle.ignore_changes cross-
// cutting case and a parse-failure case. The seventh test (parse
// failure → fall through to slice-1.5 append) lives at the
// handler level in internal/api/handlers/iac_github_patch_test.go;
// this file asserts the package-level ErrParseFailed surface.

// TestPatch_LambdaOtelLayer_AppendsLayerAndMergesEnv covers the
// lambda-otel-layer kind: list_append_dedupe on layers plus
// scalar_set on a nested environment.variables.AWS_LAMBDA_EXEC_WRAPPER
// path. Asserts the existing layer ARN is preserved, the new one
// appended; the existing FOO=bar env var is preserved; the new
// wrapper key is added.
func TestPatch_LambdaOtelLayer_AppendsLayerAndMergesEnv(t *testing.T) {
	existing := []byte(`# Module: app-lambdas
resource "aws_lambda_function" "squadron_test_function_node_2" {
  function_name = "squadron-test-function-node-2"
  role          = aws_iam_role.lambda.arn
  handler       = "index.handler"
  runtime       = "nodejs20.x"

  # existing layer for shared utilities
  layers = ["arn:aws:lambda:us-east-1:123456789012:layer:existing-layer:1"]

  environment {
    variables = {
      FOO = "bar"
    }
  }
}
`)

	patch := &Patch{
		Kind:                  "lambda-otel-layer",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_lambda_function.squadron_test_function_node_2",
		Patches: []PatchOp{
			{
				AttributePath: []string{"layers"},
				Op:            OpListAppendDedupe,
				Value:         []any{"arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-nodejs-amd64-ver-1-18-1:4"},
			},
			{
				AttributePath: []string{"environment", "variables"},
				Op:            OpMapMerge,
				Value: map[string]any{
					"AWS_LAMBDA_EXEC_WRAPPER": "/opt/otel-handler",
				},
			},
		},
	}

	out, result, err := ApplyPatch(existing, patch)
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if result.LifecycleIgnoresPatchedAttr {
		t.Errorf("did not expect lifecycle.ignore_changes detection here")
	}
	got := string(out)

	// New layer appended, original preserved.
	if !strings.Contains(got, "existing-layer:1") {
		t.Errorf("output dropped the existing layer:\n%s", got)
	}
	if !strings.Contains(got, "aws-otel-nodejs-amd64-ver-1-18-1:4") {
		t.Errorf("output missing the new OTel layer:\n%s", got)
	}
	if !strings.Contains(got, "FOO") || !strings.Contains(got, "bar") {
		t.Errorf("FOO env var dropped:\n%s", got)
	}
	if !strings.Contains(got, "AWS_LAMBDA_EXEC_WRAPPER") || !strings.Contains(got, "/opt/otel-handler") {
		t.Errorf("AWS_LAMBDA_EXEC_WRAPPER not set:\n%s", got)
	}
	// Outside-attribute comment preserved.
	if !strings.Contains(got, "# Module: app-lambdas") {
		t.Errorf("top-level comment dropped:\n%s", got)
	}
	if !strings.Contains(got, "# existing layer for shared utilities") {
		t.Errorf("adjacent comment dropped:\n%s", got)
	}
}

// TestPatch_RdsPiEm_SetsScalarBundle covers the rds-pi-em kind:
// four scalar_set ops on aws_db_instance.main with no prior PI/EM
// attributes. Asserts all four appear with the proposer values.
func TestPatch_RdsPiEm_SetsScalarBundle(t *testing.T) {
	existing := []byte(`resource "aws_db_instance" "main" {
  identifier        = "prod-db"
  engine            = "postgres"
  instance_class    = "db.t3.medium"
  allocated_storage = 100
  username          = "postgres"
}
`)

	patch := &Patch{
		Kind:                  "rds-pi-em",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_db_instance.main",
		Patches: []PatchOp{
			{AttributePath: []string{"performance_insights_enabled"}, Op: OpScalarSet, Value: true},
			{AttributePath: []string{"performance_insights_retention_period"}, Op: OpScalarSet, Value: 7},
			{AttributePath: []string{"monitoring_interval"}, Op: OpScalarSet, Value: 30},
			{AttributePath: []string{"monitoring_role_arn"}, Op: OpScalarSet, Value: "arn:aws:iam::123456789012:role/rds-monitoring"},
		},
	}
	out, _, err := ApplyPatch(existing, patch)
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	got := string(out)
	for _, needle := range []string{
		"performance_insights_enabled  = true",
		"performance_insights_retention_period = 7",
		"monitoring_interval                   = 30",
		`monitoring_role_arn                   = "arn:aws:iam::123456789012:role/rds-monitoring"`,
		// Original attrs preserved.
		`identifier        = "prod-db"`,
		`engine            = "postgres"`,
	} {
		if !strings.Contains(got, needle) {
			// hclwrite may pick slightly different spacing on the
			// `=` alignment. Fall back to a looser substring check
			// that doesn't depend on exact column alignment.
			looser := strings.ReplaceAll(needle, "  ", " ")
			compact := normalizeSpaces(got)
			if !strings.Contains(compact, normalizeSpaces(looser)) {
				t.Errorf("missing %q in output:\n%s", needle, got)
			}
		}
	}
}

// TestPatch_AlbAccessLogs_CreatesSingletonNestedBlock covers the
// alb-access-logs kind: nested_block_set on aws_lb.edge's
// access_logs nested block when no block exists. Then re-run
// against a placement that ALREADY has access_logs and assert
// in-place update.
func TestPatch_AlbAccessLogs_CreatesSingletonNestedBlock(t *testing.T) {
	existing := []byte(`resource "aws_lb" "edge" {
  name               = "edge-alb"
  load_balancer_type = "application"
  internal           = false
  security_groups    = ["sg-1234"]
  subnets            = ["subnet-1", "subnet-2"]
}
`)

	patch := &Patch{
		Kind:                  "alb-access-logs",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_lb.edge",
		Patches: []PatchOp{
			{
				AttributePath: []string{"access_logs"},
				Op:            OpNestedBlockSet,
				Value: map[string]any{
					"bucket":  "my-bucket",
					"enabled": true,
					"prefix":  "alb",
				},
			},
		},
	}
	out, _, err := ApplyPatch(existing, patch)
	if err != nil {
		t.Fatalf("ApplyPatch (creates): %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "access_logs {") {
		t.Errorf("access_logs block missing:\n%s", got)
	}
	if !strings.Contains(got, `bucket  = "my-bucket"`) && !strings.Contains(got, `bucket = "my-bucket"`) {
		t.Errorf("bucket attr missing:\n%s", got)
	}
	if !strings.Contains(got, "enabled = true") {
		t.Errorf("enabled attr missing:\n%s", got)
	}
	if !strings.Contains(got, `prefix  = "alb"`) && !strings.Contains(got, `prefix = "alb"`) {
		t.Errorf("prefix attr missing:\n%s", got)
	}

	// Second run: existing access_logs with DIFFERENT values.
	// Updated in place, NOT duplicated.
	existing2 := []byte(`resource "aws_lb" "edge" {
  name               = "edge-alb"
  load_balancer_type = "application"

  access_logs {
    bucket  = "old-bucket"
    enabled = false
    prefix  = "old-prefix"
  }
}
`)
	out2, _, err := ApplyPatch(existing2, patch)
	if err != nil {
		t.Fatalf("ApplyPatch (updates): %v", err)
	}
	got2 := string(out2)
	if !strings.Contains(got2, `"my-bucket"`) {
		t.Errorf("new bucket value not written:\n%s", got2)
	}
	if strings.Contains(got2, `"old-bucket"`) {
		t.Errorf("old bucket value not overwritten:\n%s", got2)
	}
	// Single occurrence — no duplicate block.
	if strings.Count(got2, "access_logs {") != 1 {
		t.Errorf("access_logs block was duplicated:\n%s", got2)
	}
}

// TestPatch_EksClusterLogging_AppendsAndDedupes covers
// eks-cluster-logging: list_append_dedupe on
// enabled_cluster_log_types. Existing ["api", "audit"]; patch adds
// ["audit", "authenticator", "controllerManager"]. Result must be
// ["api", "audit", "authenticator", "controllerManager"] — dupe
// folded, original order first.
func TestPatch_EksClusterLogging_AppendsAndDedupes(t *testing.T) {
	existing := []byte(`resource "aws_eks_cluster" "prod" {
  name     = "prod"
  role_arn = aws_iam_role.eks.arn

  enabled_cluster_log_types = ["api", "audit"]

  vpc_config {
    subnet_ids = ["subnet-1"]
  }
}
`)
	patch := &Patch{
		Kind:                  "eks-cluster-logging",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_eks_cluster.prod",
		Patches: []PatchOp{
			{
				AttributePath: []string{"enabled_cluster_log_types"},
				Op:            OpListAppendDedupe,
				Value:         []any{"audit", "authenticator", "controllerManager"},
			},
		},
	}
	out, _, err := ApplyPatch(existing, patch)
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	got := string(out)
	// All four should appear in order; "audit" should only appear
	// once in the enabled_cluster_log_types list (the original).
	if !strings.Contains(got, `"api"`) ||
		!strings.Contains(got, `"audit"`) ||
		!strings.Contains(got, `"authenticator"`) ||
		!strings.Contains(got, `"controllerManager"`) {
		t.Errorf("expected all four log types:\n%s", got)
	}
	// Dedupe: "audit" appears exactly once in the rendered list
	// (within the enabled_cluster_log_types attribute). Count
	// quoted occurrences in the rendered file body — there are no
	// other "audit"s in the fixture.
	if c := strings.Count(got, `"audit"`); c != 1 {
		t.Errorf(`expected "audit" exactly once, got %d:\n%s`, c, got)
	}
	// Original order: api before audit before authenticator before controllerManager.
	apiIdx := strings.Index(got, `"api"`)
	auditIdx := strings.Index(got, `"audit"`)
	authIdx := strings.Index(got, `"authenticator"`)
	cmIdx := strings.Index(got, `"controllerManager"`)
	if !(apiIdx < auditIdx && auditIdx < authIdx && authIdx < cmIdx) {
		t.Errorf("order wrong: api=%d audit=%d auth=%d cm=%d\n%s", apiIdx, auditIdx, authIdx, cmIdx, got)
	}
	// Sibling vpc_config block preserved.
	if !strings.Contains(got, "vpc_config") {
		t.Errorf("vpc_config nested block dropped:\n%s", got)
	}
}

// TestPatch_EcsContainerInsights_FindsOrCreatesSettingBlock covers
// ecs-container-insights. Two sub-cases plus the sibling-block
// invariant. (a) Existing setting{name=containerInsights value=disabled}
// → updated to "enabled". (b) No setting → new block appended.
func TestPatch_EcsContainerInsights_FindsOrCreatesSettingBlock(t *testing.T) {
	patch := &Patch{
		Kind:                  "ecs-container-insights",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_ecs_cluster.main",
		Patches: []PatchOp{
			{
				AttributePath: []string{"setting"},
				Op:            OpNestedBlockFindOrCreate,
				BlockKey:      "name",
				BlockKeyValue: "containerInsights",
				Value: map[string]any{
					"set": map[string]any{
						"value": "enabled",
					},
				},
			},
		},
	}

	// Sub-case (a): existing disabled → updated to enabled.
	existingA := []byte(`resource "aws_ecs_cluster" "main" {
  name = "prod"

  setting {
    name  = "containerInsights"
    value = "disabled"
  }

  setting {
    name  = "executeCommandConfiguration"
    value = "DEFAULT"
  }
}
`)
	outA, _, err := ApplyPatch(existingA, patch)
	if err != nil {
		t.Fatalf("ApplyPatch (a): %v", err)
	}
	gotA := string(outA)
	// containerInsights value flipped to "enabled".
	if !strings.Contains(gotA, `value = "enabled"`) {
		t.Errorf("containerInsights value not flipped:\n%s", gotA)
	}
	if strings.Contains(gotA, `value = "disabled"`) {
		t.Errorf("old disabled value not overwritten:\n%s", gotA)
	}
	// Sibling setting block untouched.
	if !strings.Contains(gotA, "executeCommandConfiguration") {
		t.Errorf("sibling setting block dropped:\n%s", gotA)
	}
	if !strings.Contains(gotA, `value = "DEFAULT"`) {
		t.Errorf("sibling DEFAULT value changed:\n%s", gotA)
	}

	// Sub-case (b): no setting at all → new block appended.
	existingB := []byte(`resource "aws_ecs_cluster" "main" {
  name = "prod"

  setting {
    name  = "executeCommandConfiguration"
    value = "DEFAULT"
  }
}
`)
	outB, _, err := ApplyPatch(existingB, patch)
	if err != nil {
		t.Fatalf("ApplyPatch (b): %v", err)
	}
	gotB := string(outB)
	if !strings.Contains(gotB, "containerInsights") {
		t.Errorf("new setting block missing name=containerInsights:\n%s", gotB)
	}
	if !strings.Contains(gotB, `value = "enabled"`) {
		t.Errorf("new setting block missing value=enabled:\n%s", gotB)
	}
	if !strings.Contains(gotB, "executeCommandConfiguration") {
		t.Errorf("sibling setting block dropped:\n%s", gotB)
	}
}

// TestPatch_LifecycleIgnoreChanges_WarnsInPRBody asserts the
// ApplyResult.LifecycleIgnoresPatchedAttr signal fires when the
// target resource carries lifecycle { ignore_changes = [layers] }
// AND the patch touches ["layers"]. Patch IS still applied.
func TestPatch_LifecycleIgnoreChanges_WarnsInPRBody(t *testing.T) {
	existing := []byte(`resource "aws_lambda_function" "test1" {
  function_name = "test1"
  layers        = ["arn:aws:lambda:us-east-1:123:layer:base:1"]

  lifecycle {
    ignore_changes = [layers]
  }
}
`)
	patch := &Patch{
		Kind:                  "lambda-otel-layer",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_lambda_function.test1",
		Patches: []PatchOp{
			{
				AttributePath: []string{"layers"},
				Op:            OpListAppendDedupe,
				Value:         []any{"arn:aws:lambda:us-east-1:999:layer:otel:1"},
			},
		},
	}
	out, result, err := ApplyPatch(existing, patch)
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if !result.LifecycleIgnoresPatchedAttr {
		t.Errorf("expected LifecycleIgnoresPatchedAttr=true")
	}
	if result.IgnoredAttrPath != "layers" {
		t.Errorf("expected IgnoredAttrPath=layers, got %q", result.IgnoredAttrPath)
	}
	// The patch IS still applied (the file change is real even if
	// terraform apply would no-op the change).
	if !strings.Contains(string(out), "otel:1") {
		t.Errorf("patch not applied even though warning expected:\n%s", out)
	}
}

// TestPatch_FailedParse_ReturnsErrParseFailed covers the package-
// level fallback signal. The handler-level "falls through to slice
// 1.5 append" assertion lives in
// internal/api/handlers/iac_github_patch_test.go.
func TestPatch_FailedParse_ReturnsErrParseFailed(t *testing.T) {
	// Missing closing brace on a prior block — invalid HCL.
	existing := []byte(`resource "aws_lambda_function" "a" {
  function_name = "a"
  # missing closing brace
resource "aws_lambda_function" "b" {
  function_name = "b"
}
`)
	patch := &Patch{
		Kind:                  "lambda-otel-layer",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_lambda_function.b",
		Patches: []PatchOp{
			{AttributePath: []string{"layers"}, Op: OpListAppendDedupe, Value: []any{"arn:..."}},
		},
	}
	_, _, err := ApplyPatch(existing, patch)
	if err == nil {
		t.Fatalf("ApplyPatch: expected ErrParseFailed, got nil")
	}
	if !errors.Is(err, ErrParseFailed) {
		t.Errorf("expected ErrParseFailed, got %T: %v", err, err)
	}
}

// --- Additional unit tests for the sentinel errors --------------------

func TestApplyPatch_UnknownOp_ReturnsErrUnknownOp(t *testing.T) {
	existing := []byte(`resource "aws_db_instance" "main" { identifier = "x" }` + "\n")
	patch := &Patch{
		Kind:                  "rds-pi-em",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_db_instance.main",
		Patches: []PatchOp{
			{AttributePath: []string{"x"}, Op: "imaginary_op", Value: "y"},
		},
	}
	_, _, err := ApplyPatch(existing, patch)
	if !errors.Is(err, ErrUnknownOp) {
		t.Errorf("expected ErrUnknownOp, got %v", err)
	}
}

func TestApplyPatch_ResourceNotFound_ReturnsErrResourceNotFound(t *testing.T) {
	existing := []byte(`resource "aws_db_instance" "main" { identifier = "x" }` + "\n")
	patch := &Patch{
		Kind:                  "rds-pi-em",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_db_instance.does_not_exist",
		Patches: []PatchOp{
			{AttributePath: []string{"performance_insights_enabled"}, Op: OpScalarSet, Value: true},
		},
	}
	_, _, err := ApplyPatch(existing, patch)
	if !errors.Is(err, ErrResourceNotFound) {
		t.Errorf("expected ErrResourceNotFound, got %v", err)
	}
	// Hint includes the existing addresses.
	if !strings.Contains(err.Error(), "aws_db_instance.main") {
		t.Errorf("expected existing-addresses hint in error: %v", err)
	}
}

func TestApplyPatch_InvalidValueType_ReturnsErrInvalidValueType(t *testing.T) {
	existing := []byte(`resource "aws_db_instance" "main" { identifier = "x" }` + "\n")
	patch := &Patch{
		Kind:                  "rds-pi-em",
		Disposition:           "patch_existing",
		TargetResourceAddress: "aws_db_instance.main",
		Patches: []PatchOp{
			// scalar_set fed a slice — wrong type for op.
			{AttributePath: []string{"performance_insights_enabled"}, Op: OpScalarSet, Value: []any{"a"}},
		},
	}
	_, _, err := ApplyPatch(existing, patch)
	if !errors.Is(err, ErrInvalidValueType) {
		t.Errorf("expected ErrInvalidValueType, got %v", err)
	}
}

// --- Helpers ---

// normalizeSpaces collapses runs of whitespace into a single space
// so the RDS test can do a column-alignment-tolerant substring
// check. Internal use only.
func normalizeSpaces(s string) string {
	out := make([]byte, 0, len(s))
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			if !prevSpace {
				out = append(out, ' ')
			}
			prevSpace = true
			continue
		}
		out = append(out, c)
		prevSpace = false
	}
	return string(out)
}
