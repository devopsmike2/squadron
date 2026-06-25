// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withServer points the package-level flags at a test server for the
// duration of the test. Resets flags on cleanup.
func withServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prevServer := flags.Server
	prevToken := flags.Token
	prevOutput := flags.Output
	flags.Server = srv.URL
	flags.Token = ""
	flags.Output = "human"
	t.Cleanup(func() {
		flags.Server = prevServer
		flags.Token = prevToken
		flags.Output = prevOutput
	})
}

// --- iac list -----------------------------------------------------

func TestIacList_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/iac/github/connections", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connections":[
			{"connection_id":"conn-abc","provider":"github","auth_kind":"pat","repo_full_name":"acme/infra","default_branch":"main","repo_layout":"multi","placement_map":[{"provider":"aws","resource_kind":"ec2-otel-layer","file_path":"modules/ec2/main.tf"}],"created_at":"2026-06-20T14:00:00Z"}
		]}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCListCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "conn-abc")
	assert.Contains(t, out, "acme/infra")
	assert.Contains(t, out, "main")
	assert.Contains(t, out, "multi")
}

func TestIacList_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connections":[]}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCListCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	assert.Contains(t, buf.String(), "no connections")
}

func TestIacList_OutputFormatJSON(t *testing.T) {
	body := `{"connections":[{"connection_id":"conn-abc","provider":"github","auth_kind":"pat","repo_full_name":"acme/infra","default_branch":"main","repo_layout":"multi","placement_map":[],"created_at":"2026-06-20T14:00:00Z"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withServer(t, srv)
	prevOutput := flags.Output
	flags.Output = "json"
	t.Cleanup(func() { flags.Output = prevOutput })

	cmd := newIaCListCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	// Assert the output parses back into the same shape.
	var got struct {
		Connections []map[string]any `json:"connections"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Len(t, got.Connections, 1)
	assert.Equal(t, "conn-abc", got.Connections[0]["connection_id"])
	assert.Equal(t, "acme/infra", got.Connections[0]["repo_full_name"])
}

func TestIacList_OutputFormatHuman(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connections":[{"connection_id":"conn-abc","provider":"github","auth_kind":"pat","repo_full_name":"acme/infra","default_branch":"main","repo_layout":"multi","placement_map":[],"created_at":"2026-06-20T14:00:00Z"}]}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCListCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	out := buf.String()
	for _, col := range []string{"CONNECTION_ID", "REPO", "BRANCH", "LAYOUT", "PLACEMENTS", "CREATED"} {
		assert.Contains(t, out, col, "expected column %s in human output", col)
	}
}

func TestIacList_4xxRendersHumanizedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"IaCStoreUnavailable","message":"IaC store not configured","suggested_step":"set IAC_STORE_URL","doc_link":"https://docs/iac"}}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCListCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IaC store not configured")
	assert.Contains(t, err.Error(), "set IAC_STORE_URL")
	assert.Contains(t, err.Error(), "https://docs/iac")
}

func TestIacList_5xxRendersGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<html>panic</html>`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCListCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	// On a non-JSON 5xx, decodeErrorBody falls through to the
	// http.StatusText fallback path; we just want some recognisable
	// "Internal Server Error" hint in the message.
	assert.Contains(t, strings.ToLower(err.Error()), "internal server error")
}

// --- iac get ------------------------------------------------------

func TestIacGet_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connections":[
			{"connection_id":"conn-abc","provider":"github","auth_kind":"pat","repo_full_name":"acme/infra","default_branch":"main","repo_layout":"multi","placement_map":[{"provider":"aws","resource_kind":"ec2-otel-layer","file_path":"modules/ec2/main.tf"}],"created_at":"2026-06-20T14:00:00Z"}
		]}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCGetCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"conn-abc"})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "conn-abc")
	assert.Contains(t, out, "acme/infra")
	assert.Contains(t, out, "ec2-otel-layer")
	assert.Contains(t, out, "modules/ec2/main.tf")
}

func TestIacGet_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connections":[]}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCGetCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"nope"})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// --- iac delete ---------------------------------------------------

func TestIacDelete_HappyPath_WithYes(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCDeleteCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"conn-abc", "--yes"})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/api/v1/iac/github/connections/conn-abc", gotPath)
	assert.Contains(t, buf.String(), "deleted conn-abc")
}

func TestIacDelete_ConfirmationDeclined(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCDeleteCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{"conn-abc"})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	assert.False(t, called, "server must not be called when confirmation is declined")
	assert.Contains(t, buf.String(), "aborted")
}

// --- iac connect (--file path) ------------------------------------

func TestIacConnect_FromFile(t *testing.T) {
	var (
		validateBody []byte
		saveBody     []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/v1/iac/github/validate":
			validateBody = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repo_full_name":"acme/infra","default_branch":"main","preflight_results":[
				{"provider":"aws","resource_kind":"ec2-otel-layer","file_path":"modules/ec2/main.tf","exists":true,"sha_short":"abc1234"}
			]}`))
		case "/api/v1/iac/github/connections":
			saveBody = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"connection_id":"conn-new","repo_full_name":"acme/infra","status":"connected"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withServer(t, srv)

	// Write a YAML config file. The token in this file is a fake
	// placeholder string we'll also assert never appears in the
	// rendered output.
	const fakeToken = "ghp_FAKE_TEST_TOKEN_BYTES_NEVER_SHOWN"
	const cfgYAML = `
token: ` + fakeToken + `
repo_full_name: acme/infra
default_branch: main
repo_layout: multi
placement_map:
  - { provider: aws, resource_kind: ec2-otel-layer, file_path: modules/ec2/main.tf }
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "connect.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgYAML), 0600))

	cmd := newIaCConnectCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--file", cfgPath})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	// Both bodies must carry the token (the server needs it) — but
	// neither the rendered human output nor any other channel may.
	assert.Contains(t, string(validateBody), fakeToken,
		"validate body must carry the PAT")
	assert.Contains(t, string(saveBody), fakeToken,
		"save body must carry the PAT")
	assert.NotContains(t, buf.String(), fakeToken,
		"PAT must never leak into rendered output")
	assert.Contains(t, buf.String(), "conn-new")
	assert.Contains(t, buf.String(), "connection saved")
}

func TestIacConnect_FileInvalidRepoFails(t *testing.T) {
	const cfgYAML = `
token: ghp_x
repo_full_name: this-is-not-owner-slash-repo
repo_layout: multi
placement_map: []
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgYAML), 0600))

	cmd := newIaCConnectCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--file", cfgPath})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner/repo")
}

// --- iac connect (PAT-never-echoed defense-in-depth) --------------

func TestIacConnect_PATNeverEchoedToStdout(t *testing.T) {
	// Drive the wizard via piped stdin (so the no-TTY readSecret
	// path runs). Capture every byte of stdout, then assert the
	// PAT bytes never appear in the rendered output.
	const fakeToken = "ghp_SECRET_DO_NOT_LEAK_1234567890"
	var validateBody, saveBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/v1/iac/github/validate":
			validateBody = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repo_full_name":"acme/infra","default_branch":"main","preflight_results":[]}`))
		case "/api/v1/iac/github/connections":
			saveBody = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"connection_id":"conn-x","repo_full_name":"acme/infra","status":"connected"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	withServer(t, srv)

	// Wizard input: PAT, repo, layout default, branch default,
	// prefix default, reviewer default, 8 placement skips, save? y.
	stdin := strings.Join([]string{
		fakeToken,                          // PAT
		"acme/infra",                       // repo
		"",                                 // layout (default multi)
		"",                                 // default branch
		"",                                 // branch prefix
		"",                                 // reviewer team
		"", "", "", "", "", "", "", "", "", // 9 placement rows (all skipped)
		"y", // save?
		"",
	}, "\n")

	cmd := newIaCConnectCommand()
	var buf bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	assert.Contains(t, string(validateBody), fakeToken,
		"the PAT must reach the server in the validate body")
	assert.Contains(t, string(saveBody), fakeToken,
		"the PAT must reach the server in the save body")
	assert.NotContains(t, buf.String(), fakeToken,
		"defense in depth: the PAT must never appear in any rendered byte")
}

// --- iac validate -------------------------------------------------

func TestIacValidate_HappyPathFromFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/iac/github/validate", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repo_full_name":"acme/infra","default_branch":"main","preflight_results":[
			{"provider":"aws","resource_kind":"ec2-otel-layer","file_path":"modules/ec2/main.tf","exists":true,"sha_short":"abc1234"},
			{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf","exists":false,"err":{"code":"FileNotFound","message":"modules/lambda/main.tf is missing on branch main","suggested_step":"create the file or change the placement","doc_link":""}}
		]}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	const cfgYAML = `
token: ghp_x
repo_full_name: acme/infra
repo_layout: multi
placement_map:
  - { provider: aws, resource_kind: ec2-otel-layer, file_path: modules/ec2/main.tf }
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "validate.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgYAML), 0600))

	cmd := newIaCValidateCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--file", cfgPath})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	out := buf.String()
	assert.Contains(t, out, "acme/infra")
	assert.Contains(t, out, "ec2-otel-layer")
	assert.Contains(t, out, "ok")
	assert.Contains(t, out, "lambda-otel-layer")
	assert.Contains(t, out, "error")
}

func TestIacValidate_RepoErrPropagatedToExitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repo_full_name":"acme/infra","default_branch":"","preflight_results":[],"repo_err":{"code":"AuthFailed","message":"PAT rejected by GitHub","suggested_step":"check the token scope","doc_link":""}}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	const cfgYAML = `
token: ghp_x
repo_full_name: acme/infra
repo_layout: multi
placement_map: []
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "validate.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgYAML), 0600))

	cmd := newIaCValidateCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--file", cfgPath})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PAT rejected by GitHub")
}

// --- iac open-pr --------------------------------------------------
//
// v0.89.15 (#631, Stream 32). Mirrors v0.89.8's iac_test.go depth:
// happy path + each of the four disposition branches + the dry-run
// path + envelope-source coverage (--from-file vs stdin) + the
// NoPlacementMapping error humaniser.

// openPRHappyResponse is the canonical success body the open-pr
// happy-path tests assert against. Centralised so each test reads
// the same shape and accidental drift surfaces as a compile/test
// error rather than a silent mismatch.
const openPRHappyResponse = `{
	"pr_number": 42,
	"pr_url": "https://github.com/acme/infra/pull/42",
	"branch": "squadron/rec-abc1234-0",
	"commit_sha": "deadbeef1234",
	"file_path": "modules/lambda/main.tf",
	"repo_full_name": "acme/infra",
	"disposition": "patch_existing",
	"disposition_actual": "patch_existing_hcl_merged",
	"manual_merge_required": false
}`

// openPRSampleEnvelope is the canonical recommendation envelope the
// tests pipe into --from-file or stdin. Mirrors the shape of the
// Phase 4 Stream 19 design doc §5 — every field the server reads.
const openPRSampleEnvelope = `{
	"scan_id": "scan-abc1234",
	"step_idx": 0,
	"resource_kind": "lambda-otel-layer",
	"snippet": "resource \"aws_lambda_layer_version\" \"otel\" { ... }",
	"proposer_reasoning": "Attaches the AWS-managed OTel Lambda layer.",
	"affected_resources": ["arn:aws:lambda:us-east-1:111111111111:function:foo"],
	"account_id": "111111111111"
}`

func writeOpenPREnvelope(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "envelope.json")
	require.NoError(t, os.WriteFile(p, []byte(openPRSampleEnvelope), 0600))
	return p
}

func TestIacOpenPR_HappyPath_PrintsPRURLAndDisposition(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openPRHappyResponse))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCOpenPRCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--connection-id", "conn-abc", "--from-file", writeOpenPREnvelope(t)})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/iac/github/connections/conn-abc/open-pr", gotPath)

	// Body must carry the envelope verbatim (scan_id, snippet,
	// account_id etc) — the server keys placement off this.
	var sentBody map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sentBody))
	assert.Equal(t, "scan-abc1234", sentBody["scan_id"])
	assert.Equal(t, "lambda-otel-layer", sentBody["resource_kind"])
	assert.Equal(t, "111111111111", sentBody["account_id"])

	out := buf.String()
	assert.Contains(t, out, "https://github.com/acme/infra/pull/42")
	assert.Contains(t, out, "patch_existing_hcl_merged")
	assert.Contains(t, out, "squadron/rec-abc1234-0")
	assert.Contains(t, out, "modules/lambda/main.tf")
}

func TestIacOpenPR_PatchExistingHclMerged_NoWarningRendered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openPRHappyResponse))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCOpenPRCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--connection-id", "conn-abc", "--from-file", writeOpenPREnvelope(t)})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	out := buf.String()
	// Clean HCL merge: no manual-merge warning, no lifecycle warning.
	assert.NotContains(t, out, "WARNING")
	assert.NotContains(t, out, "fell back to append")
	assert.NotContains(t, out, "lifecycle.ignore_changes")
}

func TestIacOpenPR_FellBackToAppend_RendersWarningWithReason(t *testing.T) {
	body := `{
		"pr_number": 43,
		"pr_url": "https://github.com/acme/infra/pull/43",
		"branch": "squadron/rec-def5678-1",
		"commit_sha": "cafef00d",
		"file_path": "modules/rds/main.tf",
		"repo_full_name": "acme/infra",
		"disposition": "patch_existing",
		"disposition_actual": "patch_existing_fell_back_to_append",
		"manual_merge_required": true,
		"hcl_patch_failure_reason": "resource_not_found"
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCOpenPRCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--connection-id", "conn-abc", "--from-file", writeOpenPREnvelope(t)})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.Contains(t, out, "WARNING")
	assert.Contains(t, out, "fell back to append-only")
	assert.Contains(t, out, "resource_not_found")
	assert.Contains(t, out, "manual-merge")
	assert.Contains(t, out, "patch_existing_fell_back_to_append")
}

func TestIacOpenPR_LifecycleIgnored_RendersWarning(t *testing.T) {
	body := `{
		"pr_number": 44,
		"pr_url": "https://github.com/acme/infra/pull/44",
		"branch": "squadron/rec-ghi9012-0",
		"commit_sha": "1234abcd",
		"file_path": "modules/eks/main.tf",
		"repo_full_name": "acme/infra",
		"disposition": "patch_existing",
		"disposition_actual": "patch_existing_hcl_merged",
		"lifecycle_ignored": true
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCOpenPRCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--connection-id", "conn-abc", "--from-file", writeOpenPREnvelope(t)})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.Contains(t, out, "WARNING")
	assert.Contains(t, out, "lifecycle.ignore_changes")
	// Clean HCL merge — the fallback warning must NOT fire.
	assert.NotContains(t, out, "fell back to append")
}

func TestIacOpenPR_DryRun_HitsValidateNotOpenPR(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// Dry-run loads the connection list to find the
		// placement row; no /open-pr call should be issued.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"connections":[
			{"connection_id":"conn-abc","provider":"github","auth_kind":"pat","repo_full_name":"acme/infra","default_branch":"main","repo_layout":"multi","placement_map":[{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}],"created_at":"2026-06-20T14:00:00Z"}
		]}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCOpenPRCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--connection-id", "conn-abc", "--from-file", writeOpenPREnvelope(t), "--dry-run"})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	// Dry run must NOT hit the open-pr endpoint.
	assert.NotContains(t, gotPath, "open-pr")
	assert.Equal(t, "/api/v1/iac/github/connections", gotPath)

	out := buf.String()
	assert.Contains(t, out, "Dry run")
	assert.Contains(t, out, "modules/lambda/main.tf")
	assert.Contains(t, out, "lambda-otel-layer")
}

func TestIacOpenPR_NoPlacementMapping_HumanizedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"NoPlacementMapping","message":"No placement-map row exists for resource_kind \"lambda-otel-layer\".","suggested_step":"placement-map"}}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCOpenPRCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--connection-id", "conn-abc", "--from-file", writeOpenPREnvelope(t)})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "placement")
	assert.Contains(t, msg, "squadronctl iac update-placement")
}

func TestIacOpenPR_StdinPipe_ReadsEnvelope(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openPRHappyResponse))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCOpenPRCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader(openPRSampleEnvelope))
	cmd.SetArgs([]string{"--connection-id", "conn-abc"})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	var sentBody map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sentBody))
	assert.Equal(t, "scan-abc1234", sentBody["scan_id"])
	assert.Equal(t, "lambda-otel-layer", sentBody["resource_kind"])
}

func TestIacOpenPR_FromFile_ReadsEnvelope(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(openPRHappyResponse))
	}))
	defer srv.Close()
	withServer(t, srv)

	cmd := newIaCOpenPRCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--connection-id", "conn-abc", "--from-file", writeOpenPREnvelope(t)})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	var sentBody map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sentBody))
	assert.Equal(t, "scan-abc1234", sentBody["scan_id"])
	assert.Equal(t, "lambda-otel-layer", sentBody["resource_kind"])
}

// --- iac update-placement -----------------------------------------
//
// v0.89.15 (#631, Stream 32). Same depth posture as v0.89.8's tests:
// the non-interactive --file path + the diff preview + validation
// rejection + the confirm-prompt flows.

// updatePlacementConnList is the canonical list response the
// update-placement tests load to discover the existing placement
// rows before computing the diff.
const updatePlacementConnList = `{"connections":[
	{"connection_id":"conn-abc","provider":"github","auth_kind":"pat","repo_full_name":"acme/infra","default_branch":"main","repo_layout":"multi","placement_map":[
		{"provider":"aws","resource_kind":"ec2-otel-layer","file_path":"modules/ec2/main.tf"},
		{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}
	],"created_at":"2026-06-20T14:00:00Z"}
]}`

// updatePlacementHappyResponse is the canonical PATCH body. Echoes
// the server-side wire shape: connection_id + repo_full_name +
// placement_map.
const updatePlacementHappyResponse = `{
	"connection_id": "conn-abc",
	"repo_full_name": "acme/infra",
	"placement_map": [
		{"provider":"aws","resource_kind":"ec2-otel-layer","file_path":"modules/compute-mixed/main.tf"},
		{"provider":"aws","resource_kind":"lambda-otel-layer","file_path":"modules/lambda/main.tf"}
	]
}`

// updatePlacementServer is a small router that serves both the
// /connections (GET) load + the /:id/placement-map (PATCH) write.
// Each test composes its own variant when it needs to fail one
// side; this helper covers the common happy path.
func updatePlacementServer(t *testing.T, patchBody *[]byte, patchPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/iac/github/connections":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(updatePlacementConnList))
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/placement-map"):
			if patchBody != nil {
				*patchBody, _ = io.ReadAll(r.Body)
			}
			if patchPath != nil {
				*patchPath = r.URL.Path
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(updatePlacementHappyResponse))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
}

func writeUpdatePlacementFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "placement.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0600))
	return p
}

func TestIacUpdatePlacement_NonInteractive_FromFile(t *testing.T) {
	var patchBody []byte
	var patchPath string
	srv := updatePlacementServer(t, &patchBody, &patchPath)
	defer srv.Close()
	withServer(t, srv)

	const yaml = `
token: ignored-but-needed-by-readConnectFile
repo_full_name: acme/infra
repo_layout: multi
placement_map:
  - { provider: aws, resource_kind: ec2-otel-layer, file_path: modules/compute-mixed/main.tf }
  - { provider: aws, resource_kind: lambda-otel-layer, file_path: modules/lambda/main.tf }
`
	cmd := newIaCUpdatePlacementCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--connection-id", "conn-abc",
		"--file", writeUpdatePlacementFile(t, yaml),
		"--yes",
	})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	assert.Equal(t, "/api/v1/iac/github/connections/conn-abc/placement-map", patchPath)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(patchBody, &sent))
	rows, ok := sent["placement_map"].([]any)
	require.True(t, ok, "PATCH body must contain placement_map array")
	require.GreaterOrEqual(t, len(rows), 2)

	out := buf.String()
	assert.Contains(t, out, "placement map updated")
	assert.Contains(t, out, "acme/infra")
}

func TestIacUpdatePlacement_NonInteractive_DiffPreviewRendered(t *testing.T) {
	srv := updatePlacementServer(t, nil, nil)
	defer srv.Close()
	withServer(t, srv)

	const yaml = `
token: ignored
repo_full_name: acme/infra
repo_layout: multi
placement_map:
  - { provider: aws, resource_kind: ec2-otel-layer, file_path: modules/compute-mixed/main.tf }
  - { provider: aws, resource_kind: lambda-otel-layer, file_path: modules/lambda/main.tf }
`
	cmd := newIaCUpdatePlacementCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--connection-id", "conn-abc",
		"--file", writeUpdatePlacementFile(t, yaml),
		"--yes",
	})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())

	out := buf.String()
	// Diff preview must show the EC2 row changing and quote both
	// before and after file paths.
	assert.Contains(t, out, "Placement map diff:")
	assert.Contains(t, out, "EC2 OTel layer")
	assert.Contains(t, out, "modules/ec2/main.tf")
	assert.Contains(t, out, "modules/compute-mixed/main.tf")
	// The Lambda row didn't change — its diff line must NOT appear.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Lambda") && strings.Contains(line, "->") {
			t.Fatalf("unchanged Lambda row should not appear in diff: %q", line)
		}
	}
}

func TestIacUpdatePlacement_ValidationError_RejectsUnknownResourceKind(t *testing.T) {
	srv := updatePlacementServer(t, nil, nil)
	defer srv.Close()
	withServer(t, srv)

	const yaml = `
token: ignored
repo_full_name: acme/infra
repo_layout: multi
placement_map:
  - { provider: aws, resource_kind: not-a-real-kind, file_path: modules/foo/main.tf }
`
	cmd := newIaCUpdatePlacementCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"--connection-id", "conn-abc",
		"--file", writeUpdatePlacementFile(t, yaml),
		"--yes",
	})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-real-kind")
	assert.Contains(t, err.Error(), "unknown resource_kind")
}

func TestIacUpdatePlacement_ConfirmPromptDeclined_NoApiCall(t *testing.T) {
	patchCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCalled = true
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/iac/github/connections":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(updatePlacementConnList))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(updatePlacementHappyResponse))
		}
	}))
	defer srv.Close()
	withServer(t, srv)

	const yaml = `
token: ignored
repo_full_name: acme/infra
repo_layout: multi
placement_map:
  - { provider: aws, resource_kind: ec2-otel-layer, file_path: modules/compute-mixed/main.tf }
`
	cmd := newIaCUpdatePlacementCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{
		"--connection-id", "conn-abc",
		"--file", writeUpdatePlacementFile(t, yaml),
		// no --yes
	})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	assert.False(t, patchCalled, "PATCH must not be called when confirm is declined")
	assert.Contains(t, buf.String(), "aborted")
}

func TestIacUpdatePlacement_YesFlag_SkipsConfirm(t *testing.T) {
	var patchPath string
	srv := updatePlacementServer(t, nil, &patchPath)
	defer srv.Close()
	withServer(t, srv)

	const yaml = `
token: ignored
repo_full_name: acme/infra
repo_layout: multi
placement_map:
  - { provider: aws, resource_kind: ec2-otel-layer, file_path: modules/compute-mixed/main.tf }
`
	cmd := newIaCUpdatePlacementCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	// No stdin attached — if --yes didn't bypass the prompt the
	// command would block waiting on input and the test would hang.
	cmd.SetArgs([]string{
		"--connection-id", "conn-abc",
		"--file", writeUpdatePlacementFile(t, yaml),
		"--yes",
	})
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	assert.Equal(t, "/api/v1/iac/github/connections/conn-abc/placement-map", patchPath)
}

// --- helper sanity checks -----------------------------------------

func TestPlacementHintFor(t *testing.T) {
	assert.Equal(t, "environments/prod/{kind}/main.tf", placementHintFor("mono"))
	assert.Equal(t, "modules/{kind}/main.tf", placementHintFor("multi"))
}
