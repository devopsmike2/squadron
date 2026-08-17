// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- groups assign-config -----------------------------------------

func TestGroupsAssignConfig_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"Config assigned to group successfully","config":{"id":"newcfg-1","group_id":"group-1","config_hash":"h","content":"receivers: {}","version":4,"created_at":"2026-01-01T00:00:00Z"}}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	out, err := runCmd(t, newGroupsAssignConfigCommand(), "group-1", "--config", "cfg-src-1")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/groups/group-1/config", gotPath)

	// The handler takes a config_id reference, not inline content.
	var sent map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &sent))
	assert.Equal(t, "cfg-src-1", sent["config_id"])
	_, hasContent := sent["content"]
	assert.False(t, hasContent, "assign-config must send a config_id reference, not inline content")

	assert.Contains(t, out, "Config assigned to group successfully")
	assert.Contains(t, out, "config_id: newcfg-1")
	assert.Contains(t, out, "group_id:  group-1")
	assert.Contains(t, out, "version:   4")
}

func TestGroupsAssignConfig_OutputJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"ok","config":{"id":"newcfg-1","group_id":"group-1","version":4}}`))
	}))
	defer srv.Close()
	withServer(t, srv)
	prev := flags.Output
	flags.Output = "json"
	t.Cleanup(func() { flags.Output = prev })

	out, err := runCmd(t, newGroupsAssignConfigCommand(), "group-1", "--config", "cfg-src-1")
	require.NoError(t, err)
	var got struct {
		Message string         `json:"message"`
		Config  map[string]any `json:"config"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "ok", got.Message)
	assert.Equal(t, "newcfg-1", got.Config["id"])
}

func TestGroupsAssignConfig_ConfigNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Config not found"}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	_, err := runCmd(t, newGroupsAssignConfigCommand(), "group-1", "--config", "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config not found")
}

func TestGroupsAssignConfig_ForbiddenNamesScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden","detail":"token does not have the required scope","required_scope":"groups:write"}`))
	}))
	defer srv.Close()
	withServer(t, srv)

	_, err := runCmd(t, newGroupsAssignConfigCommand(), "group-1", "--config", "cfg-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "groups:write")
}
