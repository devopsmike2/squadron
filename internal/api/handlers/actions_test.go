// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/actions"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// recordingAudit is a test-only AuditService that captures every
// Record call. List is a stub — these handler tests never read back
// audit history.
type recordingAudit struct {
	entries []services.AuditEntry
}

func (r *recordingAudit) Record(_ context.Context, e services.AuditEntry) error {
	r.entries = append(r.entries, e)
	return nil
}
func (r *recordingAudit) List(_ context.Context, _ services.AuditEventFilter) ([]*services.AuditEvent, error) {
	return nil, nil
}
func (r *recordingAudit) Get(_ context.Context, _ string) (*services.AuditEvent, error) {
	return nil, nil
}
func (r *recordingAudit) SetExplanation(_ context.Context, _, _, _ string, _ time.Time) error {
	return nil
}

func init() {
	gin.SetMode(gin.TestMode)
}

func newActionsTestServer(t *testing.T) (*gin.Engine, *ActionsHandlers, *memory.Store, *actions.Signer, *recordingAudit) {
	t.Helper()
	store := memory.NewStore()
	signer, err := actions.GenerateSigner()
	require.NoError(t, err)
	audit := &recordingAudit{}
	h := NewActionsHandlers(store, signer, actions.Default, audit, zap.NewNop())
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
	return r, h, store, signer, audit
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
	r, _, store, _, _ := newActionsTestServer(t)
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
	r, _, _, _, _ := newActionsTestServer(t)
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
	r, _, _, _, _ := newActionsTestServer(t)
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
	r, _, store, signer, _ := newActionsTestServer(t)
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
	r, _, _, _, _ := newActionsTestServer(t)
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
	r, _, _, _, _ := newActionsTestServer(t)
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
	r, _, _, _, _ := newActionsTestServer(t)
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
	r, _, store, _, _ := newActionsTestServer(t)
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
	r, _, _, _, _ := newActionsTestServer(t)
	w := postJSON(t, r, "/actions/anything/result", PostResultRequest{Status: "wonky"})
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestPostActionResult_UnknownAction returns 404.
func TestPostActionResult_UnknownAction(t *testing.T) {
	r, _, _, _, _ := newActionsTestServer(t)
	w := postJSON(t, r, "/actions/missing/result", PostResultRequest{Status: "success"})
	require.Equal(t, http.StatusNotFound, w.Code)
}

// ---- SQ-2.7 audit emission tests --------------------------------------------
//
// The action runner timeline is the evidence trail Squadron offers
// to anyone reviewing a "what did this fleet do" question after the
// fact. These tests pin the three event types we emit so a refactor
// can't quietly drop one and leave a gap in the timeline (or worse,
// in the SIEM fan-out the Enterprise build wires on top of audit).

// dispatchOne is a small helper: register a runner that allows
// restart-systemd-service for nginx*, then dispatch one action so
// we have a stored ActionRequest with a known ID. Returns the ID.
func dispatchOne(t *testing.T, r *gin.Engine) string {
	t.Helper()
	postJSON(t, r, "/runners/register", RegisterRunnerRequest{
		RunnerID: "rid-audit", Hostname: "h", PublicKeyPEM: "p",
		Capabilities: []actions.Capability{{
			Type: actions.RestartSystemdServiceType,
			Constraints: map[string]any{
				"unit_name_glob": []any{"nginx*"},
			},
		}},
	})
	params, _ := json.Marshal(actions.RestartSystemdServiceParameters{UnitName: "nginx.service"})
	w := postJSON(t, r, "/actions/dispatch", DispatchActionRequest{
		ProposalID: "prop-audit",
		RunnerID:   "rid-audit",
		ActionType: actions.RestartSystemdServiceType,
		Parameters: params,
		Phase:      "execute",
	})
	require.Equal(t, http.StatusOK, w.Code)
	var dispatchBody struct {
		Request *types.ActionRequest `json:"request"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &dispatchBody))
	require.NotNil(t, dispatchBody.Request)
	return dispatchBody.Request.ID
}

// lastEntry returns the most recently recorded audit entry of the
// given event type, or fails the test if none match.
func lastEntry(t *testing.T, audit *recordingAudit, eventType string) services.AuditEntry {
	t.Helper()
	for i := len(audit.entries) - 1; i >= 0; i-- {
		if audit.entries[i].EventType == eventType {
			return audit.entries[i]
		}
	}
	t.Fatalf("no audit entry of type %q (got: %+v)", eventType, audit.entries)
	return services.AuditEntry{}
}

// TestAudit_DispatchEmitsAction verifies a successful dispatch
// records action.dispatched carrying the request ID, runner, action
// type, phase, and a parameters fingerprint.
func TestAudit_DispatchEmitsAction(t *testing.T) {
	r, _, _, _, audit := newActionsTestServer(t)
	reqID := dispatchOne(t, r)

	entry := lastEntry(t, audit, services.AuditEventActionDispatched)
	assert.Equal(t, services.AuditTargetActionRequest, entry.TargetType)
	assert.Equal(t, reqID, entry.TargetID)
	assert.Equal(t, "dispatched", entry.Action)
	assert.Equal(t, "rid-audit", entry.Payload["runner_id"])
	assert.Equal(t, "prop-audit", entry.Payload["proposal_id"])
	assert.Equal(t, actions.RestartSystemdServiceType, entry.Payload["action_type"])
	assert.Equal(t, "execute", entry.Payload["phase"])
	// fingerprint is sha256 hex of the parameters JSON, so it should
	// be a 64-char string regardless of contents.
	fp, ok := entry.Payload["parameters_sha256"].(string)
	require.True(t, ok)
	assert.Len(t, fp, 64)
}

// TestAudit_ResultSuccessEmitsActionExecuted verifies the
// terminator event on the success branch.
func TestAudit_ResultSuccessEmitsActionExecuted(t *testing.T) {
	r, _, _, _, audit := newActionsTestServer(t)
	reqID := dispatchOne(t, r)
	w := postJSON(t, r, "/actions/"+reqID+"/result", PostResultRequest{
		Status: "success",
	})
	require.Equal(t, http.StatusOK, w.Code)

	entry := lastEntry(t, audit, services.AuditEventActionExecuted)
	assert.Equal(t, reqID, entry.TargetID)
	assert.Equal(t, "executed", entry.Action)
	assert.Equal(t, "rid-audit", entry.Payload["runner_id"])
}

// TestAudit_ResultFailureEmitsActionFailed verifies the terminator
// event on the failure branch.
func TestAudit_ResultFailureEmitsActionFailed(t *testing.T) {
	r, _, _, _, audit := newActionsTestServer(t)
	reqID := dispatchOne(t, r)
	w := postJSON(t, r, "/actions/"+reqID+"/result", PostResultRequest{
		Status: "failure",
	})
	require.Equal(t, http.StatusOK, w.Code)

	entry := lastEntry(t, audit, services.AuditEventActionFailed)
	assert.Equal(t, "failed", entry.Action)
}

// TestAudit_ResultDeniedEmitsActionDenied verifies the terminator
// event on the denial branch and that denied_for is carried through
// to the audit payload.
func TestAudit_ResultDeniedEmitsActionDenied(t *testing.T) {
	r, _, _, _, audit := newActionsTestServer(t)
	reqID := dispatchOne(t, r)
	w := postJSON(t, r, "/actions/"+reqID+"/result", PostResultRequest{
		Status:    "denied",
		DeniedFor: "signature: expired",
	})
	require.Equal(t, http.StatusOK, w.Code)

	entry := lastEntry(t, audit, services.AuditEventActionDenied)
	assert.Equal(t, "denied", entry.Action)
	assert.Equal(t, "signature: expired", entry.Payload["denied_for"])
}

// TestActionsHandlers_NilStore_503NotPanic pins the v0.89.212 defense: a
// handler built with a nil store (the eager-wiring bug captured a nil
// s.appStore before SetActionStoreAndSigner ran) must return a clean 503,
// never a nil-pointer panic -> 500. Covers the read + list routes.
func TestActionsHandlers_NilStore_503NotPanic(t *testing.T) {
	h := NewActionsHandlers(nil, nil, nil, nil, zap.NewNop())
	cases := []struct {
		name, method, path, route string
		fn                        gin.HandlerFunc
	}{
		{"list-actions", http.MethodGet, "/actions", "/actions", h.HandleListActions},
		{"get-action", http.MethodGet, "/actions/x", "/actions/:id", h.HandleGetAction},
		{"list-runners", http.MethodGet, "/runners", "/runners", h.HandleListRunners},
		{"get-runner", http.MethodGet, "/runners/x", "/runners/:id", h.HandleGetRunner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Handle(tc.method, tc.route, tc.fn)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			r.ServeHTTP(w, req) // must not panic
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s: status = %d, want 503; body=%s", tc.name, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "actions store not configured") {
				t.Errorf("%s: 503 should name the cause; got %s", tc.name, w.Body.String())
			}
		})
	}
}
