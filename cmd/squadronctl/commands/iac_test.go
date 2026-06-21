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
		fakeToken,    // PAT
		"acme/infra", // repo
		"",           // layout (default multi)
		"",           // default branch
		"",           // branch prefix
		"",           // reviewer team
		"", "", "", "", "", "", "", "", // 8 placement rows (all skipped)
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

// --- helper sanity checks -----------------------------------------

func TestPlacementHintFor(t *testing.T) {
	assert.Equal(t, "environments/prod/{kind}/main.tf", placementHintFor("mono"))
	assert.Equal(t, "modules/{kind}/main.tf", placementHintFor("multi"))
}
