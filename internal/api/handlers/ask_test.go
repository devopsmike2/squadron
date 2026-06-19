// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/services"
)

// v0.63 — the handler's job is to walk the read services, build a
// small context bag, and pass to ai.Service.Ask. The test verifies
// the bag contents land in the outbound Anthropic call so a
// regression on the context assembly is loud.

// fakeAskAI captures the inbound Anthropic body so the test can
// inspect the system + user message. Returns a canned response so
// the parsing path still exercises the citation strip.
type fakeAskAI struct {
	respText string
	lastBody []byte
}

func (f *fakeAskAI) start(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		out := map[string]any{
			"id":    "msg_test",
			"model": "claude-haiku-4-5-20251001",
			"role":  "assistant",
			"content": []map[string]string{
				{"type": "text", "text": f.respText},
			},
			"usage": map[string]int{
				"input_tokens":  10,
				"output_tokens": 20,
			},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// stubRollouts implements services.RolloutService minimally for the
// handler test. Only List is exercised.
type stubRollouts struct {
	rollouts []*services.Rollout
}

func (s *stubRollouts) List(_ context.Context, _ services.RolloutFilter) ([]*services.Rollout, error) {
	return s.rollouts, nil
}

// stubAudit returns canned audit events. Only List is exercised.
type stubAudit struct {
	events []*services.AuditEvent
}

func (s *stubAudit) List(_ context.Context, _ services.AuditEventFilter) ([]*services.AuditEvent, error) {
	return s.events, nil
}

func TestAskHandler_BuildsContextBagAndForwardsToAI(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fake := &fakeAskAI{
		respText: "The **web-prod-canary** rollout is paused [cite:rollout:r-1]. " +
			"That was triggered by an approval rejection [cite:audit:e-1].",
	}
	srv := fake.start(t)
	defer srv.Close()

	aiSvc := ai.NewService(ai.Config{
		Enabled:      true,
		APIKey:       "test-key",
		BaseURL:      srv.URL,
		ExplainModel: "claude-haiku-4-5-20251001",
	}, zap.NewNop())

	rollouts := &askRolloutListOnly{stub: &stubRollouts{
		rollouts: []*services.Rollout{
			{
				ID:        "r-1",
				Name:      "web-prod-canary",
				GroupID:   "web-prod",
				State:     services.RolloutStatePaused,
				CreatedAt: time.Now().Add(-2 * time.Hour),
			},
		},
	}}
	audit := &askAuditListOnly{stub: &stubAudit{
		events: []*services.AuditEvent{
			{
				ID:         "e-1",
				EventType:  "rollout.rejected",
				Actor:      "operator:bob@example.com",
				TargetType: "rollout",
				TargetID:   "r-1",
				Action:     "rejected",
				Timestamp:  time.Now().Add(-1 * time.Hour),
			},
		},
	}}

	h := NewAskHandler(aiSvc, rollouts, audit, zap.NewNop())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := bytes.NewBufferString(`{"question":"Why is the canary paused?"}`)
	c.Request, _ = http.NewRequest(http.MethodPost, "/", body)
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleAsk(c)

	require.Equal(t, http.StatusOK, w.Code)

	var resp ai.AskResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotContains(t, resp.Answer, "[cite:", "citation tags should be stripped")
	assert.Contains(t, resp.Answer, "web-prod-canary")
	require.Len(t, resp.Citations, 2)
	assert.Equal(t, "rollout", resp.Citations[0].Kind)
	assert.Equal(t, "r-1", resp.Citations[0].ID)
	assert.Equal(t, "audit", resp.Citations[1].Kind)
	assert.Equal(t, "e-1", resp.Citations[1].ID)

	// The outbound Anthropic body must include the rollout + audit
	// bag entries so a regression on bag assembly is loud.
	outbound := string(fake.lastBody)
	assert.Contains(t, outbound, "rollout:r-1")
	assert.Contains(t, outbound, "web-prod-canary")
	assert.Contains(t, outbound, "audit:e-1")
	assert.Contains(t, outbound, "rollout.rejected")
	// The system prompt must have landed.
	assert.Contains(t, outbound, "operator deputy")
}

func TestAskHandler_RejectsEmptyQuestion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAskHandler(nil, nil, nil, zap.NewNop())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := bytes.NewBufferString(`{"question":"   "}`)
	c.Request, _ = http.NewRequest(http.MethodPost, "/", body)
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleAsk(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "question is required")
}

func TestAskHandler_RejectsLongQuestion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAskHandler(nil, nil, nil, zap.NewNop())

	long := strings.Repeat("a", 600)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := bytes.NewBufferString(`{"question":"` + long + `"}`)
	c.Request, _ = http.NewRequest(http.MethodPost, "/", body)
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleAsk(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "too long")
}

// askRolloutListOnly adapts a small stub to the full
// services.RolloutService interface. Only List is exercised by the
// handler; the rest panics intentionally so a future change that
// reaches for another method gets caught in tests rather than
// production.
type askRolloutListOnly struct {
	stub *stubRollouts
}

func (a *askRolloutListOnly) List(ctx context.Context, f services.RolloutFilter) ([]*services.Rollout, error) {
	return a.stub.List(ctx, f)
}
func (askRolloutListOnly) Create(context.Context, services.RolloutInput) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) Get(context.Context, string) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) Abort(context.Context, string, string) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) Pause(context.Context, string) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) Resume(context.Context, string) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) Approve(context.Context, string, string, string) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) Reject(context.Context, string, string, string) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) Persist(context.Context, *services.Rollout) error { panic("not used") }
func (askRolloutListOnly) RollBack(context.Context, string, string) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) Preview(context.Context, string, string) (*services.RolloutPreview, error) {
	panic("not used")
}

// askAuditListOnly is the audit counterpart.
type askAuditListOnly struct {
	stub *stubAudit
}

func (a *askAuditListOnly) List(ctx context.Context, f services.AuditEventFilter) ([]*services.AuditEvent, error) {
	return a.stub.List(ctx, f)
}
func (askAuditListOnly) Record(context.Context, services.AuditEntry) error { panic("not used") }
func (askAuditListOnly) Get(context.Context, string) (*services.AuditEvent, error) {
	panic("not used")
}
func (askAuditListOnly) SetExplanation(context.Context, string, string, string, time.Time) error {
	panic("not used")
}
