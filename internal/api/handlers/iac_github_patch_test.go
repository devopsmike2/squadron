// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/services"
)

// These tests cover the slice-2 (#628 Stream 29, v0.89.12) HCL-aware
// merge wiring in HandleIaCGitHubOpenPR. They exercise the three
// observable behaviors per design doc §4 + §9:
//
//   1. Merged path  — proposer emits hcl_patch, file parses, merge
//      succeeds. Assert: clean PR (no manual-merge title prefix, no
//      manual-merge label), audit disposition_actual =
//      "patch_existing_hcl_merged", PR body has the green "Clean
//      HCL merge" note.
//
//   2. Fallback path — proposer emits hcl_patch BUT the merge fails
//      (parse error on the placement file). Assert: slice-1.5
//      append-only behavior fully preserved (manual-merge title +
//      label), audit disposition_actual =
//      "patch_existing_fell_back_to_append" + hcl_patch_failure_reason
//      = "parse_error".
//
//   3. Lifecycle warning — successful merge but the target resource
//      carries lifecycle.ignore_changes referencing a patched
//      attribute. Assert: lifecycle_ignored=true in audit, PR body
//      carries the warning section.

// fixtureLambdaTFParseable is a valid Terraform fixture the slice-2
// merge engine accepts and modifies cleanly. The OpenPR happy-path
// test below feeds it as the placement file content; the handler
// reads it via the mock client, the merger drops in the OTel layer
// and env var.
const fixtureLambdaTFParseable = `# Module: app-lambdas
resource "aws_lambda_function" "squadron_test_function_node_2" {
  function_name = "squadron-test-function-node-2"
  role          = aws_iam_role.lambda.arn
  handler       = "index.handler"
  runtime       = "nodejs20.x"

  layers = ["arn:aws:lambda:us-east-1:111:layer:base:1"]

  environment {
    variables = {
      FOO = "bar"
    }
  }
}
`

// fixtureLambdaTFMalformed is the same shape with a missing closing
// brace on the prior block. The slice-2 parse fails; the handler
// must fall through to slice-1.5 append-only.
const fixtureLambdaTFMalformed = `resource "aws_lambda_function" "a" {
  function_name = "a"
  # forgot the closing brace

resource "aws_lambda_function" "squadron_test_function_node_2" {
  function_name = "squadron-test-function-node-2"
}
`

// fixtureLambdaTFLifecycle is the same parseable shape but with a
// lifecycle.ignore_changes entry naming `layers`. The HCL merger
// applies the patch successfully (the file IS modified) but flags
// LifecycleIgnoresPatchedAttr — the handler surfaces this as the
// `lifecycle_ignored` audit field and a PR body warning.
const fixtureLambdaTFLifecycle = `resource "aws_lambda_function" "squadron_test_function_node_2" {
  function_name = "squadron-test-function-node-2"
  layers        = ["arn:aws:lambda:us-east-1:111:layer:base:1"]

  lifecycle {
    ignore_changes = [layers]
  }
}
`

// hclPatchRequestBody returns a JSON request body string carrying a
// well-formed hcl_patch for the lambda-otel-layer kind. Helper so
// the three tests below don't duplicate the patch JSON literal.
func hclPatchRequestBody() string {
	return `{
		"scan_id": "abc1234567890",
		"step_idx": 0,
		"resource_kind": "lambda-otel-layer",
		"snippet": "resource \"aws_lambda_function\" \"squadron_test_function_node_2\" {\n  layers = [\"otel\"]\n}",
		"proposer_reasoning": "Lambda function emits no telemetry.",
		"affected_resources": ["arn:aws:lambda:us-east-1:111:function:test"],
		"account_id": "111111111111",
		"hcl_patch": {
			"kind": "lambda-otel-layer",
			"disposition": "patch_existing",
			"target_resource_address": "aws_lambda_function.squadron_test_function_node_2",
			"patches": [
				{"attribute_path": ["layers"], "op": "list_append_dedupe",
				 "value": ["arn:aws:lambda:us-east-1:901920570463:layer:aws-otel-nodejs-amd64-ver-1-18-1:4"]},
				{"attribute_path": ["environment", "variables"], "op": "map_merge",
				 "value": {"AWS_LAMBDA_EXEC_WRAPPER": "/opt/otel-handler"}}
			]
		}
	}`
}

// TestHandleIaCGitHubOpenPR_PatchExisting_HCLMerged_HasNoManualMergeLabel
// covers the slice-2 happy path: the placement file parses, the
// target resource exists, the patch applies, the PR ships without
// any manual-merge marker.
func TestHandleIaCGitHubOpenPR_PatchExisting_HCLMerged_HasNoManualMergeLabel(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		fileResp: map[string]*iacgithub.FileContent{
			"modules/lambda/main.tf": {
				Path: "modules/lambda/main.tf", SHA: "existingblobsha",
				DecodedContent: []byte(fixtureLambdaTFParseable),
			},
		},
		branchSHAResp: "tipofmaindefaultsha",
		openPRResp: &iacgithub.PullRequest{
			Number: 42, HTMLURL: "https://github.com/octo/widgets/pull/42", HeadSHA: "newcommit",
		},
	}
	audit := &discoveryRecordingAudit{}
	h, _ := newTestIaCHandlers(t, mc, audit)
	connID := saveConnectionForOpenPR(t, h,
		`{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}`)

	w := doIaCRequest(t, http.MethodPost,
		"/api/v1/iac/github/connections/"+connID+"/open-pr",
		openPRRegisterFor(h), hclPatchRequestBody())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp iacGitHubOpenPRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.DispositionActual != "patch_existing_hcl_merged" {
		t.Errorf("disposition_actual = %q, want patch_existing_hcl_merged", resp.DispositionActual)
	}
	if resp.ManualMergeRequired {
		t.Errorf("manual_merge_required = true on the merged path (should be false)")
	}
	if resp.HCLPatchFailureReason != "" {
		t.Errorf("hcl_patch_failure_reason = %q, want empty on the merged path", resp.HCLPatchFailureReason)
	}

	// PR title must NOT carry the manual-merge prefix.
	if len(mc.openPRCalls) != 1 {
		t.Fatalf("openPR calls = %d, want 1", len(mc.openPRCalls))
	}
	if strings.HasPrefix(mc.openPRCalls[0].Title, "[needs manual merge]") {
		t.Errorf("PR title still carries [needs manual merge] prefix on the merged path: %q",
			mc.openPRCalls[0].Title)
	}

	// PR body must carry the slice-2 "Clean HCL merge" note and
	// must NOT carry the slice-1.5 manual-merge warning text.
	prBody := mc.openPRCalls[0].Body
	if !strings.Contains(prBody, "Clean HCL merge") {
		t.Errorf("PR body missing slice-2 'Clean HCL merge' note:\n%s", prBody)
	}
	if strings.Contains(prBody, "Manual merge required") {
		t.Errorf("PR body still carries slice-1.5 manual-merge warning on the merged path:\n%s", prBody)
	}

	// Labels: no squadron/needs-manual-merge on the merged path.
	if len(mc.addLabelsCalls) != 1 {
		t.Fatalf("addLabels calls = %d, want 1", len(mc.addLabelsCalls))
	}
	for _, l := range mc.addLabelsCalls[0].Labels {
		if l == "squadron/needs-manual-merge" {
			t.Errorf("squadron/needs-manual-merge label applied on the merged path; labels=%+v",
				mc.addLabelsCalls[0].Labels)
		}
	}

	// PUT content: the new layer ARN must appear; the existing
	// layer ARN must be preserved; the AWS_LAMBDA_EXEC_WRAPPER env
	// var must appear; FOO=bar must be preserved (proves the merge
	// actually ran, not just the slice-1.5 append).
	if len(mc.putFileCalls) != 1 {
		t.Fatalf("putFile calls = %d, want 1", len(mc.putFileCalls))
	}
	put := string(mc.putFileCalls[0].Content)
	if !strings.Contains(put, "aws-otel-nodejs-amd64-ver-1-18-1:4") {
		t.Errorf("merged file missing new OTel layer ARN:\n%s", put)
	}
	if !strings.Contains(put, "base:1") {
		t.Errorf("merged file dropped existing layer ARN:\n%s", put)
	}
	if !strings.Contains(put, "AWS_LAMBDA_EXEC_WRAPPER") {
		t.Errorf("merged file missing wrapper env var:\n%s", put)
	}
	if !strings.Contains(put, "FOO") {
		t.Errorf("merged file dropped existing FOO env var:\n%s", put)
	}

	// Audit: disposition_actual = patch_existing_hcl_merged,
	// manual_merge_required = false, no hcl_patch_failure_reason
	// stamped on the success path.
	var prOpenedEntry *services.AuditEntry
	for i := range audit.entries {
		if audit.entries[i].EventType == services.AuditEventRecommendationPROpened {
			prOpenedEntry = &audit.entries[i]
		}
	}
	if prOpenedEntry == nil {
		t.Fatalf("no recommendation.pr_opened audit event")
	}
	if got := prOpenedEntry.Payload["disposition_actual"]; got != "patch_existing_hcl_merged" {
		t.Errorf("audit disposition_actual = %v, want patch_existing_hcl_merged", got)
	}
	if got := prOpenedEntry.Payload["manual_merge_required"]; got != false {
		t.Errorf("audit manual_merge_required = %v, want false", got)
	}
	if _, present := prOpenedEntry.Payload["hcl_patch_failure_reason"]; present {
		t.Errorf("audit hcl_patch_failure_reason should be absent on success; got %v",
			prOpenedEntry.Payload["hcl_patch_failure_reason"])
	}
}

// TestHandleIaCGitHubOpenPR_PatchExisting_ParseFails_FallsBackToAppend
// covers the slice-2 fallback. Placement file is malformed HCL;
// the merger refuses; the handler MUST land the slice-1.5 behavior
// untouched so the operator's recommendation isn't dropped.
func TestHandleIaCGitHubOpenPR_PatchExisting_ParseFails_FallsBackToAppend(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		fileResp: map[string]*iacgithub.FileContent{
			"modules/lambda/main.tf": {
				Path: "modules/lambda/main.tf", SHA: "existingblobsha",
				DecodedContent: []byte(fixtureLambdaTFMalformed),
			},
		},
		branchSHAResp: "tipofmaindefaultsha",
		openPRResp: &iacgithub.PullRequest{
			Number: 43, HTMLURL: "https://github.com/octo/widgets/pull/43", HeadSHA: "newcommit",
		},
	}
	audit := &discoveryRecordingAudit{}
	h, _ := newTestIaCHandlers(t, mc, audit)
	connID := saveConnectionForOpenPR(t, h,
		`{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}`)

	w := doIaCRequest(t, http.MethodPost,
		"/api/v1/iac/github/connections/"+connID+"/open-pr",
		openPRRegisterFor(h), hclPatchRequestBody())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp iacGitHubOpenPRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.DispositionActual != "patch_existing_fell_back_to_append" {
		t.Errorf("disposition_actual = %q, want patch_existing_fell_back_to_append",
			resp.DispositionActual)
	}
	if !resp.ManualMergeRequired {
		t.Errorf("manual_merge_required = false on the fallback path (should be true)")
	}
	if resp.HCLPatchFailureReason != "parse_error" {
		t.Errorf("hcl_patch_failure_reason = %q, want parse_error", resp.HCLPatchFailureReason)
	}

	// PR title MUST carry the slice-1.5 manual-merge prefix.
	if len(mc.openPRCalls) != 1 {
		t.Fatalf("openPR calls = %d, want 1", len(mc.openPRCalls))
	}
	if !strings.HasPrefix(mc.openPRCalls[0].Title, "[needs manual merge] ") {
		t.Errorf("PR title missing [needs manual merge] prefix on fallback: %q",
			mc.openPRCalls[0].Title)
	}

	// PR body carries the slice-1.5 manual-merge banner PLUS the
	// slice-2 failure-reason callout.
	prBody := mc.openPRCalls[0].Body
	if !strings.Contains(prBody, "Manual merge required") {
		t.Errorf("PR body missing slice-1.5 'Manual merge required' callout:\n%s", prBody)
	}
	if !strings.Contains(prBody, "parse_error") {
		t.Errorf("PR body missing slice-2 fallback reason 'parse_error':\n%s", prBody)
	}

	// Labels: squadron/needs-manual-merge MUST be present.
	if len(mc.addLabelsCalls) != 1 {
		t.Fatalf("addLabels calls = %d, want 1", len(mc.addLabelsCalls))
	}
	foundMM := false
	for _, l := range mc.addLabelsCalls[0].Labels {
		if l == "squadron/needs-manual-merge" {
			foundMM = true
		}
	}
	if !foundMM {
		t.Errorf("squadron/needs-manual-merge label missing on fallback path; labels=%+v",
			mc.addLabelsCalls[0].Labels)
	}

	// PUT content must be the slice-1.5 append result: existing
	// (malformed) bytes followed by the snippet. The new OTel layer
	// from the patch should NOT appear (the merger refused; the
	// snippet's verbatim "layers = [\"otel\"]" is what the
	// slice-1.5 append carries).
	if len(mc.putFileCalls) != 1 {
		t.Fatalf("putFile calls = %d, want 1", len(mc.putFileCalls))
	}
	put := string(mc.putFileCalls[0].Content)
	if !strings.Contains(put, "# forgot the closing brace") {
		t.Errorf("fallback PUT content dropped the original malformed file:\n%s", put)
	}
	if !strings.Contains(put, `layers = ["otel"]`) {
		t.Errorf("fallback PUT content missing the slice-1.5 appended snippet:\n%s", put)
	}
	if strings.Contains(put, "aws-otel-nodejs-amd64-ver-1-18-1:4") {
		t.Errorf("fallback PUT content includes the patch ARN — the merger should not have applied:\n%s", put)
	}

	// Audit: disposition_actual = patch_existing_fell_back_to_append,
	// hcl_patch_failure_reason = parse_error, manual_merge_required = true.
	var prOpenedEntry *services.AuditEntry
	for i := range audit.entries {
		if audit.entries[i].EventType == services.AuditEventRecommendationPROpened {
			prOpenedEntry = &audit.entries[i]
		}
	}
	if prOpenedEntry == nil {
		t.Fatalf("no recommendation.pr_opened audit event")
	}
	if got := prOpenedEntry.Payload["disposition_actual"]; got != "patch_existing_fell_back_to_append" {
		t.Errorf("audit disposition_actual = %v, want patch_existing_fell_back_to_append", got)
	}
	if got := prOpenedEntry.Payload["hcl_patch_failure_reason"]; got != "parse_error" {
		t.Errorf("audit hcl_patch_failure_reason = %v, want parse_error", got)
	}
	if got := prOpenedEntry.Payload["manual_merge_required"]; got != true {
		t.Errorf("audit manual_merge_required = %v, want true", got)
	}
}

// TestHandleIaCGitHubOpenPR_PatchExisting_LifecycleIgnoreChanges_WarnsInPRBody
// covers the cross-cutting design-doc §6 case: the merge succeeds
// (clean drop-in) AND the target resource carries
// lifecycle.ignore_changes referencing one of the patched
// attribute paths. PR body must carry the warning section so the
// operator sees that terraform apply will no-op the patched attr.
func TestHandleIaCGitHubOpenPR_PatchExisting_LifecycleIgnoreChanges_WarnsInPRBody(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		fileResp: map[string]*iacgithub.FileContent{
			"modules/lambda/main.tf": {
				Path: "modules/lambda/main.tf", SHA: "existingblobsha",
				DecodedContent: []byte(fixtureLambdaTFLifecycle),
			},
		},
		branchSHAResp: "tipofmaindefaultsha",
		openPRResp: &iacgithub.PullRequest{
			Number: 44, HTMLURL: "https://github.com/octo/widgets/pull/44", HeadSHA: "newcommit",
		},
	}
	audit := &discoveryRecordingAudit{}
	h, _ := newTestIaCHandlers(t, mc, audit)
	connID := saveConnectionForOpenPR(t, h,
		`{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}`)

	// Use a layers-only patch so the lifecycle warning fires on
	// "layers" specifically (the fixture's ignore_changes names it).
	body := `{
		"scan_id": "abc1234567890",
		"step_idx": 0,
		"resource_kind": "lambda-otel-layer",
		"snippet": "resource \"aws_lambda_function\" \"squadron_test_function_node_2\" { layers = [\"otel\"] }",
		"proposer_reasoning": "Add OTel layer.",
		"affected_resources": ["arn:aws:lambda:us-east-1:111:function:test"],
		"hcl_patch": {
			"kind": "lambda-otel-layer",
			"disposition": "patch_existing",
			"target_resource_address": "aws_lambda_function.squadron_test_function_node_2",
			"patches": [
				{"attribute_path": ["layers"], "op": "list_append_dedupe",
				 "value": ["arn:aws:lambda:us-east-1:999:layer:otel:4"]}
			]
		}
	}`
	w := doIaCRequest(t, http.MethodPost,
		"/api/v1/iac/github/connections/"+connID+"/open-pr",
		openPRRegisterFor(h), body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp iacGitHubOpenPRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// Merge succeeded — clean path, lifecycle warning is a soft
	// signal, not a failure.
	if resp.DispositionActual != "patch_existing_hcl_merged" {
		t.Errorf("disposition_actual = %q, want patch_existing_hcl_merged", resp.DispositionActual)
	}
	if !resp.LifecycleIgnored {
		t.Errorf("lifecycle_ignored = false; expected true since ignore_changes covers layers")
	}

	// PR body carries the lifecycle warning section.
	if len(mc.openPRCalls) != 1 {
		t.Fatalf("openPR calls = %d, want 1", len(mc.openPRCalls))
	}
	prBody := mc.openPRCalls[0].Body
	if !strings.Contains(prBody, "lifecycle.ignore_changes") {
		t.Errorf("PR body missing lifecycle.ignore_changes warning section:\n%s", prBody)
	}
	if !strings.Contains(prBody, "layers") {
		t.Errorf("PR body lifecycle warning should name the ignored attribute 'layers':\n%s", prBody)
	}

	// Audit: lifecycle_ignored = true, lifecycle_ignored_attr = layers.
	var prOpenedEntry *services.AuditEntry
	for i := range audit.entries {
		if audit.entries[i].EventType == services.AuditEventRecommendationPROpened {
			prOpenedEntry = &audit.entries[i]
		}
	}
	if prOpenedEntry == nil {
		t.Fatalf("no recommendation.pr_opened audit event")
	}
	if got := prOpenedEntry.Payload["lifecycle_ignored"]; got != true {
		t.Errorf("audit lifecycle_ignored = %v, want true", got)
	}
	if got := prOpenedEntry.Payload["lifecycle_ignored_attr"]; got != "layers" {
		t.Errorf("audit lifecycle_ignored_attr = %v, want layers", got)
	}
}

// TestHandleIaCGitHubOpenPR_PatchExisting_NoHCLPatchEmitted_FallsBackCleanly
// covers the pre-v0.89.12 backward-compatibility path: the
// proposer's prompt didn't yet emit hcl_patch, the request body
// has no patch field. The handler must fall back to slice-1.5
// behavior with `no_patch_emitted` as the fallback reason so an
// auditor can correlate.
func TestHandleIaCGitHubOpenPR_PatchExisting_NoHCLPatchEmitted_FallsBackCleanly(t *testing.T) {
	mc := &mockGitHubClient{
		repoResp: &iacgithub.Repo{FullName: "octo/widgets", DefaultBranch: "main"},
		fileResp: map[string]*iacgithub.FileContent{
			"modules/lambda/main.tf": {
				Path: "modules/lambda/main.tf", SHA: "existingblobsha",
				DecodedContent: []byte(fixtureLambdaTFParseable),
			},
		},
		branchSHAResp: "tipofmaindefaultsha",
		openPRResp: &iacgithub.PullRequest{
			Number: 45, HTMLURL: "https://github.com/octo/widgets/pull/45", HeadSHA: "newcommit",
		},
	}
	audit := &discoveryRecordingAudit{}
	h, _ := newTestIaCHandlers(t, mc, audit)
	connID := saveConnectionForOpenPR(t, h,
		`{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}`)
	body := `{
		"scan_id": "abc1234567890",
		"step_idx": 0,
		"resource_kind": "lambda-otel-layer",
		"snippet": "resource \"x\" \"y\" {}",
		"proposer_reasoning": "Pre-slice-2 recommendation.",
		"affected_resources": []
	}`
	w := doIaCRequest(t, http.MethodPost,
		"/api/v1/iac/github/connections/"+connID+"/open-pr",
		openPRRegisterFor(h), body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp iacGitHubOpenPRResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.DispositionActual != "patch_existing_fell_back_to_append" {
		t.Errorf("disposition_actual = %q, want patch_existing_fell_back_to_append",
			resp.DispositionActual)
	}
	if resp.HCLPatchFailureReason != "no_patch_emitted" {
		t.Errorf("hcl_patch_failure_reason = %q, want no_patch_emitted",
			resp.HCLPatchFailureReason)
	}
}
