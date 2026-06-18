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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

func newIncidentsTestServer(t *testing.T) (*gin.Engine, *memory.Store, *recordingAudit) {
	t.Helper()
	store := memory.NewStore()
	audit := &recordingAudit{}
	h := NewIncidentsHandlers(store, audit, zap.NewNop())
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
