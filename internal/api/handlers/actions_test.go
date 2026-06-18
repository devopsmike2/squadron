// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newActionsTestServer(t *testing.T) (*gin.Engine, *ActionsHandlers, *memory.Store, *actions.Signer) {
	t.Helper()
	store := memory.NewStore()
	signer, err := actions.GenerateSigner()
	require.NoError(t, err)
	h := NewActionsHandlers(store, signer, actions.Default, zap.NewNop())
	r := gin.New()
	r.POST("/runners/register", h.HandleRegisterRunner)
	r.GET("/runners", h.HandleListRunners)
	r.GET("/runners/:id", h.HandleGetRunner)
	r.POST("/runners/:id/revoke", h.HandleRevokeRunner)
	r.GET("/runners/:id/pending", h.HandleRunnerPending)
	r.POST("/actions/dispatch", h.HandleDispatchAction)
	r.GET("/actions", h.HandleListActions)
	r.GET("/actions/:id", h.HandleGetAction)
	r.POST("/actions/:id/result", h.HandlePostActionResult)
	return r, h, store, signer
}

func postJSON(t *testing.T, r http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func getJSON(t *testing.T, r http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestRegisterRunner_Idempotent verifies re-registration updates
// the existing record rather than erroring.
func TestRegisterRunner_Idempotent(t *testing.T) {
	r, _, store, _ := newActionsTestServer(t)
	body := RegisterRunnerRequest{
		RunnerID:     "ed25519:abc",
		Hostname:     "web-prod-1",
		PublicKeyPEM: "PEM-1",
		Capabilities: []actions.Capability{{Type: actions.RestartSystemdServiceType}},
	}
	w := postJSON(t, r, "/runners/register", body)
	require.Equal(t, http.StatusOK, w.Code)

	body.Hostname = "web-prod-2"
	body.PublicKeyPEM = "PEM-2"
	w = postJSON(t, r, "/runners/register", body)
	require.Equal(t, http.StatusOK, w.Code)

	reg, _ := store.GetActionRunnerRegistration(context.Background(), "ed25519:abc")
	require.NotNil(t, reg)
	assert.Equal(t, "web-prod-2", reg.Hostname)
	assert.Equal(t, "PEM-2", reg.PublicKeyPEM)
}

// TestListAndGetRunners covers the basic browse paths.
func TestListAndGetRunners(t *testing.T) {
	r, _, _, _ := newActionsTestServer(t)
	postJSON(t, r, "/runners/register", RegisterRunnerRequest{
		RunnerID: "rid-1", Hostname: "h1", PublicKeyPEM: "p1",
	})
	postJSON(t, r, "/runners/register", RegisterRunnerRequest{
		RunnerID: "rid-2", Hostname: "h2", PublicKeyPEM: "p2",
	})
	w := getJSON(t, r, "/runners")
	require.Equal(t, http.StatusOK, w.Code)
	var listBody struct {
		Runners []*types.ActionRunnerRegistration `json:"runners"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listBody))
	assert.Len(t, listBody.Runners, 2)

	w = getJSON(t, r, "/runners/rid-1")
	require.Equal(t, http.StatusOK, w.Code)

	w = getJSON(t, r, "/runners/missing")
	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestRevokeRunner sets revoked_at and refuses subsequent dispatch.
func TestRevokeRunner(t *testing.T) {
	r, _, _, _ := newActionsTestServer(t)
	postJSON(t, r, "/runners/register", RegisterRunnerRequest{
		RunnerID: "rid-rev", Hostname: "h", PublicKeyPEM: "p",
		Capabilities: []actions.Capability{{Type: actions.RestartSystemdServiceType}},
	})
	w := postJSON(t, r, "/runners/rid-rev/revoke", struct{}{})
	require.Equal(t, http.StatusOK, w.Code)

	// dispatch must now refuse with 403
	params, _ := json.Marshal(actions.RestartSystemdServiceParameters{UnitName: "nginx"})
	w = postJSON(t, r, "/actions/dispatch", DispatchActionRequest{
		RunnerID:   "rid-rev",
		ActionType: actions.RestartSystemdServiceType,
		Parameters: params,
		Phase:      "dry_run",
	})
	require.Equal(t, http.StatusForbidden, w.Code)
}

// TestDispatchAction_HappyPath signs + persists + returns the
// signed request for runner consumption.
func TestDispatchAction_HappyPath(t *testing.T) {
	r, _, store, signer := newActionsTestServer(t)
	postJSON(t, r, "/runners/register", RegisterRunnerRequest{
		RunnerID: "rid-d", Hostname: "h", PublicKeyPEM: "p",
		Capabilities: []actions.Capability{{
			Type: actions.RestartSystemdServiceType,
			Constraints: map[string]any{
				"unit_name_glob": []any{"nginx*"},
			},
		}},
	})
	params, _ := json.Marshal(actions.RestartSystemdServiceParameters{UnitName: "nginx"})
	w := postJSON(t, r, "/actions/dispatch", DispatchActionRequest{
		ProposalID: "prop-1",
		RunnerID:   "rid-d",
		ActionType: actions.RestartSystemdServiceType,
		Parameters: params,
		Phase:      "dry_run",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Pending poll returns it.
	w = getJSON(t, r, "/runners/rid-d/pending")
	require.Equal(t, http.StatusOK, w.Code)
	var pendingBody struct {
		Requests []*types.ActionRequest `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &pendingBody))
	require.Len(t, pendingBody.Requests, 1)
	assert.Equal(t, "pending", pendingBody.Requests[0].Status)
	assert.Equal(t, "rid-d", pendingBody.Requests[0].RunnerID)

	// Verify the signature on the stored request actually validates
	// against the signer's public key. This is the integration check
	// between the handler and the crypto layer.
	stored, _ := store.GetActionRequest(context.Background(), pendingBody.Requests[0].ID)
	require.NotNil(t, stored)
	v, err := actions.NewVerifier(signer.PublicKey())
	require.NoError(t, err)
	require.NoError(t, v.Verify(&actions.Request{
		RequestID:  stored.ID,
		ProposalID: stored.ProposalID,
		RunnerID:   stored.RunnerID,
		Action: actions.ActionPayload{
			Type:       stored.ActionType,
			Parameters: json.RawMessage(stored.ParametersJSON),
		},
		IssuedAt:  stored.IssuedAt,
		ExpiresAt: stored.ExpiresAt,
		Phase:     actions.Phase(stored.Phase),
		Signature: stored.Signature,
	}, stored.IssuedAt.Add(time.Second*30)))
}

// TestDispatchAction_OutOfPolicy refuses when the parameters don't
// satisfy the runner's capability constraints.
func TestDispatchAction_OutOfPolicy(t *testing.T) {
	r, _, _, _ := newActionsTestServer(t)
	postJSON(t, r, "/runners/register", RegisterRunnerRequest{
		RunnerID: "rid-strict", Hostname: "h", PublicKeyPEM: "p",
		Capabilities: []actions.Capability{{
			Type:        actions.RestartSystemdServiceType,
			Constraints: map[string]any{"unit_name_glob": []any{"squadron-*"}},
		}},
	})
	params, _ := json.Marshal(actions.RestartSystemdServiceParameters{UnitName: "sshd"})
	w := postJSON(t, r, "/actions/dispatch", DispatchActionRequest{
		RunnerID:   "rid-strict",
		ActionType: actions.RestartSystemdServiceType,
		Parameters: params,
		Phase:      "dry_run",
	})
	require.Equal(t, http.StatusForbidden, w.Code)
}

// TestDispatchAction_UnknownRunner returns 404.
func TestDispatchAction_UnknownRunner(t *testing.T) {
	r, _, _, _ := newActionsTestServer(t)
	params, _ := json.Marshal(actions.RestartSystemdServiceParameters{UnitName: "nginx"})
	w := postJSON(t, r, "/actions/dispatch", DispatchActionRequest{
		RunnerID:   "missing",
		ActionType: actions.RestartSystemdServiceType,
		Parameters: params,
		Phase:      "dry_run",
	})
	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestDispatchAction_BadParameters returns 400.
func TestDispatchAction_BadParameters(t *testing.T) {
	r, _, _, _ := newActionsTestServer(t)
	postJSON(t, r, "/runners/register", RegisterRunnerRequest{
		RunnerID: "rid-x", Hostname: "h", PublicKeyPEM: "p",
		Capabilities: []actions.Capability{{Type: actions.RestartSystemdServiceType}},
	})
	bad, _ := json.Marshal(actions.RestartSystemdServiceParameters{UnitName: ""})
	w := postJSON(t, r, "/actions/dispatch", DispatchActionRequest{
		RunnerID:   "rid-x",
		ActionType: actions.RestartSystemdServiceType,
		Parameters: bad,
		Phase:      "dry_run",
	})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestPostActionResult_HappyPath records a success result.
func TestPostActionResult_HappyPath(t *testing.T) {
	r, _, store, _ := newActionsTestServer(t)
	postJSON(t, r, "/runners/register", RegisterRunnerRequest{
		RunnerID: "rid-r", Hostname: "h", PublicKeyPEM: "p",
		Capabilities: []actions.Capability{{Type: actions.RestartSystemdServiceType}},
	})
	params, _ := json.Marshal(actions.RestartSystemdServiceParameters{UnitName: "nginx"})
	postJSON(t, r, "/actions/dispatch", DispatchActionRequest{
		RunnerID:   "rid-r",
		ActionType: actions.RestartSystemdServiceType,
		Parameters: params,
		Phase:      "dry_run",
	})
	// pick up the request ID
	list, _ := store.ListActionRequests(context.Background(), types.ActionRequestFilter{RunnerID: "rid-r"})
	require.Len(t, list, 1)
	reqID := list[0].ID

	w := postJSON(t, r, "/actions/"+reqID+"/result", PostResultRequest{
		Status:           "success",
		DryRunOutputJSON: `{"would_restart":"nginx"}`,
	})
	require.Equal(t, http.StatusOK, w.Code)

	stored, _ := store.GetActionRequest(context.Background(), reqID)
	assert.Equal(t, "success", stored.Status)
	assert.Equal(t, `{"would_restart":"nginx"}`, stored.DryRunOutputJSON)
	require.NotNil(t, stored.CompletedAt)
}

// TestPostActionResult_BadStatus rejects invalid status values.
func TestPostActionResult_BadStatus(t *testing.T) {
	r, _, _, _ := newActionsTestServer(t)
	w := postJSON(t, r, "/actions/anything/result", PostResultRequest{Status: "wonky"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestPostActionResult_UnknownAction returns 404.
func TestPostActionResult_UnknownAction(t *testing.T) {
	r, _, _, _ := newActionsTestServer(t)
	w := postJSON(t, r, "/actions/missing/result", PostResultRequest{Status: "success"})
	require.Equal(t, http.StatusNotFound, w.Code)
}
