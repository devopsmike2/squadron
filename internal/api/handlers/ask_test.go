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
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// v0.66 stubs for the two new bag sources.
type stubCostSpikes struct{ events []*storetypes.CostSpikeEvent }

func (s *stubCostSpikes) ListCostSpikeEvents(_ context.Context, _ storetypes.CostSpikeFilter) ([]*storetypes.CostSpikeEvent, error) {
	return s.events, nil
}

type stubRecs struct{ recs []AskRec }

func (s *stubRecs) ListForAsk(_ context.Context, _ int) ([]AskRec, error) {
	return s.recs, nil
}

// v0.68 stub for the agents lister.
type stubAgents struct{ agents []AskAgent }

func (s *stubAgents) ListForAsk(_ context.Context, _ int) ([]AskAgent, error) {
	return s.agents, nil
}

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

	h := NewAskHandler(aiSvc, rollouts, audit, nil, nil, nil, zap.NewNop())

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

// v0.66 — extending the bag to cost spikes + recommendations. The
// handler must walk both sources when wired, summarize each row in
// the bag, and the outbound prompt must contain enough of each
// summary that a regression on the walks is loud.
func TestAskHandler_IncludesSpikesAndRecsInBag(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fake := &fakeAskAI{
		respText: "Costs spiked on the otlp_logs signal [cite:spike:sp-1]. " +
			"Consider dropping the http.url attribute [cite:rec:rec-1].",
	}
	srv := fake.start(t)
	defer srv.Close()

	aiSvc := ai.NewService(ai.Config{
		Enabled:      true,
		APIKey:       "test-key",
		BaseURL:      srv.URL,
		ExplainModel: "claude-haiku-4-5-20251001",
	}, zap.NewNop())

	spikes := &stubCostSpikes{events: []*storetypes.CostSpikeEvent{
		{
			ID:                   "sp-1",
			Severity:             "critical",
			Signal:               "otlp_logs",
			BaselineMonthlyUSD:   400,
			PeakMonthlyUSD:       1600,
			PeakPctAboveBaseline: 300,
			StartedAt:            time.Now().Add(-30 * time.Minute),
		},
	}}
	recs := &stubRecs{recs: []AskRec{
		{
			ID:     "rec-1",
			Title:  "Drop attribute http.url from metrics",
			Detail: "Cardinality from this attribute is 3x the median; dropping cuts ~$400/mo.",
		},
	}}

	h := NewAskHandler(aiSvc, nil, nil, spikes, recs, nil, zap.NewNop())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/",
		bytes.NewBufferString(`{"question":"Why are costs up?"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleAsk(c)
	require.Equal(t, http.StatusOK, w.Code)

	var resp ai.AskResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Citations, 2)
	assert.Equal(t, "spike", resp.Citations[0].Kind)
	assert.Equal(t, "sp-1", resp.Citations[0].ID)
	assert.Equal(t, "rec", resp.Citations[1].Kind)
	assert.Equal(t, "rec-1", resp.Citations[1].ID)

	// Outbound Anthropic body must include the spike + rec summaries.
	outbound := string(fake.lastBody)
	assert.Contains(t, outbound, "spike:sp-1")
	assert.Contains(t, outbound, "severity=critical")
	assert.Contains(t, outbound, "peak=$1600/mo")
	assert.Contains(t, outbound, "rec:rec-1")
	assert.Contains(t, outbound, "Drop attribute http.url")
}

// v0.68 — agents enter the bag as the fourth source. The wiring
// adapter only forwards offline + drifted agents (healthy ones do
// not belong in a JARVIS bag), and the handler quotes the slim
// summary verbatim. Verifies the summarize path emits the four
// fields the prompt prioritizes: name, status, drift, group.
func TestAskHandler_IncludesAgentsInBag(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fake := &fakeAskAI{
		respText: "Two agents need attention. **host-09** is offline [cite:agent:a-9] and **host-12** is drifted [cite:agent:a-12].",
	}
	srv := fake.start(t)
	defer srv.Close()

	aiSvc := ai.NewService(ai.Config{
		Enabled: true, APIKey: "k", BaseURL: srv.URL,
		ExplainModel: "claude-haiku-4-5-20251001",
	}, zap.NewNop())

	now := time.Now()
	agents := &stubAgents{agents: []AskAgent{
		{
			ID: "a-9", Name: "host-09", Status: "offline",
			DriftStatus: "synced", GroupName: "web-prod",
			LastSeen: now.Add(-15 * time.Minute),
		},
		{
			ID: "a-12", Name: "host-12", Status: "online",
			DriftStatus: "drifted", GroupName: "web-prod",
			LastSeen: now,
		},
	}}

	h := NewAskHandler(aiSvc, nil, nil, nil, nil, agents, zap.NewNop())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/",
		bytes.NewBufferString(`{"question":"anything wrong in the fleet?"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleAsk(c)
	require.Equal(t, http.StatusOK, w.Code)

	var resp ai.AskResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Citations, 2)
	assert.Equal(t, "agent", resp.Citations[0].Kind)
	assert.Equal(t, "a-9", resp.Citations[0].ID)
	assert.Equal(t, "agent", resp.Citations[1].Kind)
	assert.Equal(t, "a-12", resp.Citations[1].ID)

	// Outbound bag must carry the prioritized fields per agent.
	outbound := string(fake.lastBody)
	assert.Contains(t, outbound, "agent:a-9")
	assert.Contains(t, outbound, "name=host-09")
	assert.Contains(t, outbound, "status=offline")
	assert.Contains(t, outbound, "agent:a-12")
	assert.Contains(t, outbound, "drift=drifted")
	assert.Contains(t, outbound, "group=web-prod")
}

// And when neither lister is wired, the handler still answers
// against the rollout + audit sources. Verifies the nil guards in
// buildBag.
func TestAskHandler_HandlesMissingSpikesAndRecs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fake := &fakeAskAI{respText: "I don't have cost data loaded."}
	srv := fake.start(t)
	defer srv.Close()

	aiSvc := ai.NewService(ai.Config{
		Enabled: true, APIKey: "k", BaseURL: srv.URL,
		ExplainModel: "claude-haiku-4-5-20251001",
	}, zap.NewNop())

	h := NewAskHandler(aiSvc, nil, nil, nil, nil, nil, zap.NewNop())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/",
		bytes.NewBufferString(`{"question":"anything?"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.HandleAsk(c)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAskHandler_RejectsEmptyQuestion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAskHandler(nil, nil, nil, nil, nil, nil, zap.NewNop())

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
	h := NewAskHandler(nil, nil, nil, nil, nil, nil, zap.NewNop())

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
func (askRolloutListOnly) NextPlanStep(context.Context, string, int) (*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) CancelPlanFollowers(context.Context, string, int) ([]*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) RollBackPlanPredecessors(context.Context, string, int, string) ([]*services.Rollout, error) {
	panic("not used")
}
func (askRolloutListOnly) CreatePlan(context.Context, []services.RolloutInput) ([]*services.Rollout, string, error) {
	panic("not used")
}
func (askRolloutListOnly) GetPlan(context.Context, string) (*services.Plan, error) {
	panic("not used")
}
func (askRolloutListOnly) ListPlans(context.Context, services.PlanFilter) ([]*services.Plan, error) {
	panic("not used")
}
func (askRolloutListOnly) SetExcludeFromLearning(context.Context, string, string, string, bool) (*services.Rollout, error) {
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

// VerifyChain — ADR 0027 slice 1. Test stub: self-tenant audit chain
// verify. Not exercised by these tests; returns a trivially OK result.
func (askAuditListOnly) VerifyChain(context.Context) (*applicationstore.AuditChainVerification, error) {
	return &applicationstore.AuditChainVerification{OK: true}, nil
}
