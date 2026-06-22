// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// v0.89.26 (#642 Stream 43) acceptance tests for the per-rollout
// exclude-from-learning toggle (#531 slice 2 §10 Q3). Two tests live
// here because they exercise the HTTP handler / audit emission paths:
//
//   - TestExcludeFromLearning_Toggle_Persists: POST -> Get round trip.
//   - TestExcludeFromLearning_AuditEventEmittedOnToggle: audit payload
//     contract.
//
// The three bridge-level acceptance tests
// (TestExcludeFromLearning_ExcludedRolloutNotInVerdictExamples,
// TestExcludeFromLearning_ChangedAfterCreation_AffectsNextProposal,
// TestExcludeFromLearning_GroupOptOutStillRespected) live in
// internal/proposer/exclude_from_learning_test.go because they need to
// drive bridge.assembleVerdicts directly. The two file layout keeps each
// test in the package that owns the function it asserts on.

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

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// excludeTestFixture builds a memory store + a wired rollout handler
// with an audit service connected so the handler's emit path is
// exercised end-to-end. Returns the handler, the in-memory rollout id
// the test will toggle, and the audit service so tests can assert on
// the recorded payload directly without round-tripping through HTTP.
func excludeTestFixture(t *testing.T) (h *RolloutHandlers, rolloutID string, audit services.AuditService, store *memory.Store) {
	t.Helper()
	store = memory.NewStore()
	const gid = "web-prod"
	require.NoError(t, store.CreateGroup(t.Context(), &types.Group{
		ID: gid, Name: "Web Prod",
		LearnFromVerdicts: true,
	}))
	require.NoError(t, store.CreateConfig(t.Context(), &types.Config{
		ID: "cfg-1", Name: "current", Content: "x",
		GroupID: ptrString(gid), CreatedAt: time.Now(),
	}))

	// Seed an AI-originated approved rollout. This is the row the
	// tests will toggle. Default ExcludeFromLearning=false matches
	// the storage column default.
	now := time.Now().UTC()
	rolloutID = "rlt_acceptance"
	require.NoError(t, store.CreateRollout(t.Context(), &types.Rollout{
		ID:                 rolloutID,
		Name:               "AI: drop container.id",
		GroupID:            gid,
		TargetConfigID:     "cfg-1",
		Stages:             []types.RolloutStage{{Mode: "percent", Percentage: 100, DwellSeconds: 60}},
		State:              types.RolloutStateSucceeded,
		ProposedBy:         types.RolloutProposedByAI,
		ProposalReasoning:  "container.id was driving 60% of the spike",
		ApprovedBy:         "operator@example.com",
		ApprovedAt:         &now,
		ApprovalNotes:      "good plan, ship it",
		ExcludeFromLearning: false,
		CreatedAt:          now,
		UpdatedAt:          now,
	}))

	audit = services.NewAuditService(store, nil, zap.NewNop())
	svc := services.NewRolloutService(store, nil, audit, zap.NewNop())
	h = NewRolloutHandlers(svc, zap.NewNop())
	return h, rolloutID, audit, store
}

// ptrString returns a *string for a string literal so the seed config
// can wire the optional GroupID field cleanly.
func ptrString(s string) *string { return &s }

// doExcludePost wires a gin request body with the supplied excluded
// flag and optional reason, runs the handler, and returns the
// response recorder for the caller to assert against.
func doExcludePost(t *testing.T, h *RolloutHandlers, rolloutID string, excluded bool, reason string, actor string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"excluded": excluded}
	if reason != "" {
		body["reason"] = reason
	}
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{gin.Param{Key: "id", Value: rolloutID}}
	c.Request, _ = http.NewRequest(http.MethodPost, "/", bytes.NewBuffer(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	if actor != "" {
		c.Set("auth_actor", actor)
	}
	h.HandleExcludeFromLearning(c)
	return w
}

// TestExcludeFromLearning_Toggle_Persists is acceptance test #1.
// POST excluded=true → 200 → GetRollout reflects true.
// POST excluded=false → 200 → reflects false. The column round-trips
// through the memory store; the SQLite store's identical column wire
// is covered by the storage package's existing round-trip tests.
func TestExcludeFromLearning_Toggle_Persists(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, rolloutID, _, store := excludeTestFixture(t)

	// Start state: false (storage default).
	stored, err := store.GetRollout(context.Background(), rolloutID)
	require.NoError(t, err)
	require.False(t, stored.ExcludeFromLearning, "seed row should default to ExcludeFromLearning=false")

	// First toggle: false -> true.
	w := doExcludePost(t, h, rolloutID, true, "", "operator:alice@example.com")
	require.Equal(t, http.StatusOK, w.Code, "POST excluded=true must return 200; body=%s", w.Body.String())
	var resp services.Rollout
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.ExcludeFromLearning, "response should reflect new state")
	// Storage reads back the same value.
	stored, err = store.GetRollout(context.Background(), rolloutID)
	require.NoError(t, err)
	assert.True(t, stored.ExcludeFromLearning, "storage round-trip should reflect excluded=true")

	// Second toggle: true -> false. Use a fresh decode target — the
	// service.Rollout JSON tag is `exclude_from_learning,omitempty`
	// so a `false` value is omitted from the response body. If we
	// reused `resp` from the first POST the previous `true` would
	// silently survive the unmarshal and the assertion would pass
	// against stale state. Storage is the load-bearing assertion;
	// the response decode is a sanity check that the handler echoed
	// the post-toggle value back.
	w = doExcludePost(t, h, rolloutID, false, "", "operator:alice@example.com")
	require.Equal(t, http.StatusOK, w.Code, "POST excluded=false must return 200")
	var resp2 services.Rollout
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp2))
	assert.False(t, resp2.ExcludeFromLearning, "response should reflect new state")
	stored, err = store.GetRollout(context.Background(), rolloutID)
	require.NoError(t, err)
	assert.False(t, stored.ExcludeFromLearning, "storage round-trip should reflect excluded=false")
}

// TestExcludeFromLearning_AuditEventEmittedOnToggle is acceptance
// test #4. POST → exactly one rollout.excluded_from_learning row is
// recorded with the documented payload (rollout_id, previous_state,
// new_state, reason). SIEM rules parse this payload — the shape is
// a public contract; this test pins it.
func TestExcludeFromLearning_AuditEventEmittedOnToggle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, rolloutID, audit, _ := excludeTestFixture(t)

	// Seed start state false → flip to true with a reason note.
	const reason = "test note"
	const actor = "operator:alice@example.com"
	w := doExcludePost(t, h, rolloutID, true, reason, actor)
	require.Equal(t, http.StatusOK, w.Code)

	// Pull the audit list, narrowed to the new event type.
	events, err := audit.List(context.Background(), services.AuditEventFilter{
		EventType: services.AuditEventRolloutExcludedFromLearning,
		Limit:     10,
	})
	require.NoError(t, err)
	require.Len(t, events, 1, "exactly one rollout.excluded_from_learning row")
	e := events[0]
	assert.Equal(t, services.AuditEventRolloutExcludedFromLearning, e.EventType)
	assert.Equal(t, "rollout", e.TargetType)
	assert.Equal(t, rolloutID, e.TargetID)
	assert.Equal(t, actor, e.Actor)
	// Action verb encodes direction so SIEM rules can fan out without
	// cracking the payload. exclude_from_learning here because
	// new_state=true.
	assert.Equal(t, "exclude_from_learning", e.Action)
	// Payload contract: rollout_id, previous_state, new_state, reason.
	require.NotNil(t, e.Payload)
	assert.Equal(t, rolloutID, e.Payload["rollout_id"])
	assert.Equal(t, false, e.Payload["previous_state"], "previous_state should be the seed value (false)")
	assert.Equal(t, true, e.Payload["new_state"], "new_state should be the post-toggle value (true)")
	assert.Equal(t, reason, e.Payload["reason"], "reason flows verbatim onto the payload")
}
