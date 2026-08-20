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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runCmd executes a freshly-built command against the test server and
// returns its combined stdout/stderr buffer. Keeps each test terse.
func runCmd(t *testing.T, cmd interface {
	SetOut(io.Writer)
	SetErr(io.Writer)
	SetArgs([]string)
	SetContext(context.Context)
	Execute() error
}, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	return buf.String(), err
}

// --- agents restart -----------------------------------------------

func TestAgentsRestart_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"Restart command dispatched to agent over OpAMP."}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsRestartCommand(), "11111111-1111-1111-1111-111111111111")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/agents/11111111-1111-1111-1111-111111111111/restart", gotPath)
	assert.Contains(t, out, "Restart command dispatched to agent over OpAMP.")
}

func TestAgentsRestart_OutputJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"ok"}`))
	}))
	defer srv.Close()
	withServer(t, srv)
	prev := flags.Output
	flags.Output = "json"
	t.Cleanup(func() { flags.Output = prev })

	out, err := runCmd(t, newAgentsRestartCommand(), "agent-1")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, true, got["success"])
	assert.Equal(t, "ok", got["message"])
}

func TestAgentsRestart_ForbiddenNamesScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden","detail":"token does not have the required scope","required_scope":"agents:write"}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	_, err := runCmd(t, newAgentsRestartCommand(), "agent-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agents:write")
}

func TestAgentsRestart_UnauthorizedHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","detail":"invalid or revoked token"}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	_, err := runCmd(t, newAgentsRestartCommand(), "agent-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SQUADRON_TOKEN")
}

// --- agents set-group ---------------------------------------------

func TestAgentsSetGroup_ByID(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"agent-1","name":"web-01","status":"online","drift_status":"synced","group_id":"22222222-2222-2222-2222-222222222222","group_name":"prod"}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsSetGroupCommand(), "agent-1", "--group", "22222222-2222-2222-2222-222222222222")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPatch, gotMethod)
	assert.Equal(t, "/api/v1/agents/agent-1/group", gotPath)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", sent["group_id"])
	assert.Contains(t, out, "prod")
}

func TestAgentsSetGroup_ByName_ResolvesViaGroupsList(t *testing.T) {
	var patchBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/groups":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"groups":[
				{"id":"22222222-2222-2222-2222-222222222222","name":"prod","created_at":"2026-01-01T00:00:00Z"},
				{"id":"33333333-3333-3333-3333-333333333333","name":"staging","created_at":"2026-01-01T00:00:00Z"}
			]}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/agents/agent-1/group":
			patchBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"agent-1","name":"web-01","status":"online","drift_status":"synced","group_id":"22222222-2222-2222-2222-222222222222","group_name":"prod"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsSetGroupCommand(), "agent-1", "--group", "prod")
	require.NoError(t, err)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(patchBody, &sent))
	// The name must have resolved to the group's UUID before the PATCH.
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", sent["group_id"])
	assert.Contains(t, out, "prod")
}

func TestAgentsSetGroup_UnknownName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"groups":[{"id":"22222222-2222-2222-2222-222222222222","name":"prod","created_at":"2026-01-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	_, err := runCmd(t, newAgentsSetGroupCommand(), "agent-1", "--group", "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no group named")
}

func TestAgentsSetGroup_NoneClears(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"agent-1","name":"web-01","status":"online","drift_status":"no_intent"}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsSetGroupCommand(), "agent-1", "--none")
	require.NoError(t, err)
	// Clearing sends {"group_id":null}.
	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	v, ok := sent["group_id"]
	assert.True(t, ok, "group_id key must be present")
	assert.Nil(t, v, "group_id must be null to clear")
	assert.Contains(t, out, "cleared")
}

func TestAgentsSetGroup_RejectsGroupAndNoneTogether(t *testing.T) {
	// No server call should happen — validation fails first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server must not be called")
	}))
	defer srv.Close()
	withServer(t, srv)

	_, err := runCmd(t, newAgentsSetGroupCommand(), "agent-1", "--group", "prod", "--none")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not both")
}

// --- agents clear-group -------------------------------------------

func TestAgentsClearGroup_SendsNullGroupID(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"agent-1","name":"web-01","status":"online","drift_status":"no_intent"}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsClearGroupCommand(), "agent-1")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPatch, gotMethod)
	assert.Equal(t, "/api/v1/agents/agent-1/group", gotPath)
	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	assert.Nil(t, sent["group_id"])
	assert.Contains(t, out, "cleared")
}

// --- agents clear-config ------------------------------------------

func TestAgentsClearConfig_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cleared":true,"fell_back_to":"group","group_id":"g-1","pushed":true,"message":"Cleared the agent config. The group config is now in effect and was delivered to the agent."}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsClearConfigCommand(), "agent-1")
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/api/v1/agents/agent-1/config", gotPath)
	assert.Contains(t, out, "fell_back_to: group")
	assert.Contains(t, out, "group_id:     g-1")
	assert.Contains(t, out, "pushed:       true")
}

func TestAgentsClearConfig_OutputJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cleared":false,"fell_back_to":"none","pushed":false,"message":"nothing to clear"}`))
	}))
	defer srv.Close()
	withServer(t, srv)
	prev := flags.Output
	flags.Output = "json"
	t.Cleanup(func() { flags.Output = prev })

	out, err := runCmd(t, newAgentsClearConfigCommand(), "agent-1")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "none", got["fell_back_to"])
	assert.Equal(t, false, got["pushed"])
}

// --- agents get --effective / --drift -----------------------------

func TestAgentsGet_Effective_PrintsRawConfig(t *testing.T) {
	const effCfg = "receivers:\n  otlp:\n    protocols:\n      grpc:\n"
	body, _ := json.Marshal(map[string]any{
		"id":               "agent-1",
		"name":             "web-01",
		"status":           "online",
		"drift_status":     "synced",
		"effective_config": effCfg,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/agents/agent-1", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsGetCommand(), "agent-1", "--effective")
	require.NoError(t, err)
	// The raw effective config is printed verbatim; no "ID:" header row.
	assert.Contains(t, out, "receivers:")
	assert.Contains(t, out, "protocols:")
	assert.NotContains(t, out, "Status:")
}

func TestAgentsGet_DefaultShowsIntentAndDrift(t *testing.T) {
	body := `{
		"id":"agent-1","name":"web-01","status":"online","drift_status":"drifted",
		"group_name":"prod",
		"config_intent":{"source":"group","source_name":"prod","config_id":"cfg-abcdef123","version":3,"hash":"h1"}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsGetCommand(), "agent-1")
	require.NoError(t, err)
	assert.Contains(t, out, "Drift:   drifted")
	assert.Contains(t, out, "Group:   prod")
	assert.Contains(t, out, "Intent:  group (prod)")
	assert.Contains(t, out, "v3")
}

func TestAgentsGet_DriftFlagShowsDetail(t *testing.T) {
	body := `{
		"id":"agent-1","name":"web-01","status":"online","drift_status":"drifted",
		"drift_details":{"intent_hash":"aaa","effective_hash":"bbb","diff":"- old\n+ new"}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newAgentsGetCommand(), "agent-1", "--drift")
	require.NoError(t, err)
	assert.Contains(t, out, "Drift detail:")
	assert.Contains(t, out, "intent_hash:    aaa")
	assert.Contains(t, out, "effective_hash: bbb")
	assert.Contains(t, out, "+ new")
}
