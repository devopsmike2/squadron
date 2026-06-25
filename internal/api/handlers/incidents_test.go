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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/incidents"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

func newIncidentsTestServer(t *testing.T) (*gin.Engine, *memory.Store, *recordingAudit) {
	t.Helper()
	store := memory.NewStore()
	audit := &recordingAudit{}
	h := NewIncidentsHandlers(store, audit, nil, zap.NewNop())
	r := gin.New()
	r.GET("/incidents/drafts", h.HandleListDrafts)
	r.GET("/incidents/drafts/:id", h.HandleGetDraft)
	r.PATCH("/incidents/drafts/:id", h.HandlePatchDraft)
	r.POST("/incidents/drafts/:id/dismiss", h.HandleDismissDraft)
	r.POST("/incidents/drafts/:id/publish", h.HandlePublishDraft)
	return r, store, audit
}

// seedDraft inserts a draft for the given action request ID and
// returns the persisted record.
func seedDraft(t *testing.T, store *memory.Store, actionRequestID, status string) *types.IncidentDraft {
	t.Helper()
	d := &types.IncidentDraft{
		ID:              uuid.NewString(),
		ActionRequestID: actionRequestID,
		RolloutID:       "rollout-" + actionRequestID,
		Status:          status,
		Title:           "Test draft for " + actionRequestID,
		BodyMarkdown:    "# Test\n\nbody",
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	require.NoError(t, store.CreateIncidentDraft(context.Background(), d))
	return d
}

func patchJSON(t *testing.T, r http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPatch, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestListDrafts_DefaultsToDraftStatus pins the inbox-view default:
// without ?status= the handler returns only drafts in status=draft
// so the operator's UI inbox does not silently hide dismissed and
// published items.
func TestListDrafts_DefaultsToDraftStatus(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	seedDraft(t, store, "a1", "draft")
	seedDraft(t, store, "a2", "dismissed")
	seedDraft(t, store, "a3", "published")

	w := getJSON(t, r, "/incidents/drafts")
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Drafts []*types.IncidentDraft `json:"drafts"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Drafts, 1)
	assert.Equal(t, "a1", body.Drafts[0].ActionRequestID)
}

func TestListDrafts_StatusFilter(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	seedDraft(t, store, "a1", "draft")
	seedDraft(t, store, "a2", "dismissed")

	w := getJSON(t, r, "/incidents/drafts?status=dismissed")
	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Drafts []*types.IncidentDraft `json:"drafts"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Drafts, 1)
	assert.Equal(t, "a2", body.Drafts[0].ActionRequestID)
}

func TestGetDraft_FoundAndNotFound(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "draft")

	w := getJSON(t, r, "/incidents/drafts/"+d.ID)
	require.Equal(t, http.StatusOK, w.Code)

	w = getJSON(t, r, "/incidents/drafts/missing")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestPatchDraft_UpdatesTitleAndBody(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "draft")

	newTitle := "Operator edited title"
	newBody := "# Edited\n\nupdated by hand"
	w := patchJSON(t, r, "/incidents/drafts/"+d.ID, PatchDraftRequest{
		Title: &newTitle, BodyMarkdown: &newBody,
	})
	require.Equal(t, http.StatusOK, w.Code)

	stored, _ := store.GetIncidentDraft(context.Background(), d.ID)
	assert.Equal(t, newTitle, stored.Title)
	assert.Equal(t, newBody, stored.BodyMarkdown)
}

func TestPatchDraft_RejectsNonDraftStatus(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "published")

	newTitle := "Cannot edit"
	w := patchJSON(t, r, "/incidents/drafts/"+d.ID, PatchDraftRequest{Title: &newTitle})
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestDismissDraft_FlipsStatusAndAudits(t *testing.T) {
	r, store, audit := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "draft")

	w := postJSON(t, r, "/incidents/drafts/"+d.ID+"/dismiss", struct{}{})
	require.Equal(t, http.StatusOK, w.Code)

	stored, _ := store.GetIncidentDraft(context.Background(), d.ID)
	assert.Equal(t, "dismissed", stored.Status)

	// Audit event emitted.
	require.NotEmpty(t, audit.entries)
	assert.Equal(t, "incident.dismissed", audit.entries[len(audit.entries)-1].EventType)
}

func TestDismissDraft_Idempotent(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "dismissed")

	w := postJSON(t, r, "/incidents/drafts/"+d.ID+"/dismiss", struct{}{})
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPublishDraft_Clipboard_StampsAndAudits(t *testing.T) {
	r, store, audit := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "draft")

	w := postJSON(t, r, "/incidents/drafts/"+d.ID+"/publish", PublishDraftRequest{
		Provider:    "clipboard",
		ExternalID:  "",
		ExternalURL: "",
	})
	require.Equal(t, http.StatusOK, w.Code)

	stored, _ := store.GetIncidentDraft(context.Background(), d.ID)
	assert.Equal(t, "published", stored.Status)
	assert.Equal(t, "clipboard", stored.Provider)

	require.NotEmpty(t, audit.entries)
	assert.Equal(t, "incident.published", audit.entries[len(audit.entries)-1].EventType)
}

func TestPublishDraft_ExternalLinkRecorded(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "draft")

	w := postJSON(t, r, "/incidents/drafts/"+d.ID+"/publish", PublishDraftRequest{
		Provider:    "linear",
		ExternalID:  "LIN-123",
		ExternalURL: "https://linear.app/issue/LIN-123",
	})
	require.Equal(t, http.StatusOK, w.Code)

	stored, _ := store.GetIncidentDraft(context.Background(), d.ID)
	assert.Equal(t, "linear", stored.Provider)
	assert.Equal(t, "LIN-123", stored.ExternalID)
	assert.Equal(t, "https://linear.app/issue/LIN-123", stored.ExternalURL)
}

func TestPublishDraft_RejectsUnknownProvider(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "draft")

	w := postJSON(t, r, "/incidents/drafts/"+d.ID+"/publish", PublishDraftRequest{
		Provider: "myspace",
	})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPublishDraft_RejectsDismissedDraft(t *testing.T) {
	r, store, _ := newIncidentsTestServer(t)
	d := seedDraft(t, store, "a1", "dismissed")

	w := postJSON(t, r, "/incidents/drafts/"+d.ID+"/publish", PublishDraftRequest{
		Provider: "clipboard",
	})
	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestPublishDraft_GitHubProviderStampsResponse verifies the
// publisher integration path: the handler invokes the registered
// GitHub Issues publisher, takes the returned external_id and url
// from the publisher's response (overriding what the operator
// supplied), and persists the published row.
func TestPublishDraft_GitHubProviderStampsResponse(t *testing.T) {
	store := memory.NewStore()
	audit := &recordingAudit{}

	// Fake GitHub API that returns issue 99 with a canned URL.
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":99,"html_url":"https://github.com/devopsmike2/squadron/issues/99"}`))
	}))
	defer ghSrv.Close()

	publishers := incidents.NewPublisherRegistry()
	gh, err := incidents.NewGitHubIssuesPublisher(incidents.GitHubIssuesConfig{
		Owner:      "devopsmike2",
		Repo:       "squadron",
		Token:      "test-token",
		APIBaseURL: ghSrv.URL,
		HTTPClient: ghSrv.Client(),
	})
	require.NoError(t, err)
	publishers.Register(gh)

	h := NewIncidentsHandlers(store, audit, publishers, zap.NewNop())
	r := gin.New()
	r.POST("/incidents/drafts/:id/publish", h.HandlePublishDraft)

	d := seedDraft(t, store, "a1", "draft")

	// Operator supplies a placeholder external_id/url; the
	// publisher response should override.
	w := postJSON(t, r, "/incidents/drafts/"+d.ID+"/publish", PublishDraftRequest{
		Provider:    "github",
		ExternalID:  "placeholder",
		ExternalURL: "https://example.com/placeholder",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	stored, _ := store.GetIncidentDraft(context.Background(), d.ID)
	assert.Equal(t, "published", stored.Status)
	assert.Equal(t, "github", stored.Provider)
	assert.Equal(t, "99", stored.ExternalID)
	assert.Equal(t, "https://github.com/devopsmike2/squadron/issues/99", stored.ExternalURL)
}

// TestPublishDraft_GitHubFailureReturns502 verifies the handler
// surfaces publisher errors cleanly without persisting a half
// published draft.
func TestPublishDraft_GitHubFailureReturns502(t *testing.T) {
	store := memory.NewStore()
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer ghSrv.Close()

	publishers := incidents.NewPublisherRegistry()
	gh, err := incidents.NewGitHubIssuesPublisher(incidents.GitHubIssuesConfig{
		Owner: "o", Repo: "r", Token: "bad",
		APIBaseURL: ghSrv.URL,
		HTTPClient: ghSrv.Client(),
	})
	require.NoError(t, err)
	publishers.Register(gh)

	h := NewIncidentsHandlers(store, nil, publishers, zap.NewNop())
	r := gin.New()
	r.POST("/incidents/drafts/:id/publish", h.HandlePublishDraft)

	d := seedDraft(t, store, "a1", "draft")
	w := postJSON(t, r, "/incidents/drafts/"+d.ID+"/publish", PublishDraftRequest{
		Provider: "github",
	})
	assert.Equal(t, http.StatusBadGateway, w.Code)

	stored, _ := store.GetIncidentDraft(context.Background(), d.ID)
	assert.Equal(t, "draft", stored.Status, "draft must stay in status=draft when publisher fails")
}

// TestIncidentsHandlers_NilStore_503NotPanic pins the v0.89.211 defense:
// a handler whose store is nil (the eager-wiring bug captured a nil
// s.appStore before SetActionStoreAndSigner ran) must return a clean 503,
// never a nil-pointer panic -> 500. All five routes share the guard.
func TestIncidentsHandlers_NilStore_503NotPanic(t *testing.T) {
	h := NewIncidentsHandlers(nil, nil, nil, zap.NewNop())

	cases := []struct {
		name   string
		method string
		path   string
		route  string
		fn     gin.HandlerFunc
	}{
		{"list", http.MethodGet, "/incidents/drafts", "/incidents/drafts", h.HandleListDrafts},
		{"get", http.MethodGet, "/incidents/drafts/x", "/incidents/drafts/:id", h.HandleGetDraft},
		{"dismiss", http.MethodPost, "/incidents/drafts/x/dismiss", "/incidents/drafts/:id/dismiss", h.HandleDismissDraft},
		{"publish", http.MethodPost, "/incidents/drafts/x/publish", "/incidents/drafts/:id/publish", h.HandlePublishDraft},
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
			if !strings.Contains(w.Body.String(), "incidents store not configured") {
				t.Errorf("%s: 503 should name the cause; got %s", tc.name, w.Body.String())
			}
		})
	}
}
