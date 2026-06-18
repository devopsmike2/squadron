// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// fakeAnthropicForExplain spins up a one-shot HTTP server that
// returns a canned Anthropic-shaped reply. counter counts inbound
// requests so tests can assert that cache hits skip the LLM call.
type fakeAnthropicForExplain struct {
	respText string
	status   int
	counter  int
}

func newFakeAnthropicForExplain(respText string) *fakeAnthropicForExplain {
	return &fakeAnthropicForExplain{respText: respText, status: 200}
}

func (f *fakeAnthropicForExplain) start() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.counter++
		if f.status >= 300 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"forced failure"}}`))
			return
		}
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

// setupExplainHandlers wires a memory store, an audit service, the
// AI service pointed at the fake Anthropic server, and an AuditHandlers
// with all three. The returned audit service is the one used to record
// the seed audit row.
func setupExplainHandlers(t *testing.T, fake *fakeAnthropicForExplain) (
	*AuditHandlers, services.AuditService, *memory.Store, *httptest.Server,
) {
	t.Helper()
	srv := fake.start()
	t.Cleanup(srv.Close)

	store := memory.NewStore()
	svc := services.NewAuditService(store, nil, zap.NewNop())
	aiSvc := ai.NewService(ai.Config{
		Enabled:      true,
		APIKey:       "test-key",
		BaseURL:      srv.URL,
		ExplainModel: "claude-haiku-4-5-20251001",
	}, zap.NewNop())
	h := NewAuditHandlers(svc, aiSvc, store, zap.NewNop())
	return h, svc, store, srv
}

func seedAuditRow(t *testing.T, svc services.AuditService, _ *memory.Store) string {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, svc.Record(ctx, services.AuditEntry{
		Actor:      "operator:alice@example.com",
		EventType:  "rollout.approved",
		TargetType: "rollout",
		TargetID:   "rollout-xyz",
		Action:     "approved",
		Payload: map[string]any{
			"approval_notes": "looks good",
		},
	}))
	rows, err := svc.List(ctx, services.AuditEventFilter{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), 1)
	return rows[0].ID
}

func TestExplainAuditEvent_HappyPath(t *testing.T) {
	fake := newFakeAnthropicForExplain(
		"Squadron approved a rollout to the web-prod group at 14:23 UTC.")
	h, svc, store, _ := setupExplainHandlers(t, fake)

	id := seedAuditRow(t, svc, store)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost,
		"/api/v1/audit/"+id+"/explain", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.HandleExplainAuditEvent(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Contains(t, resp["explanation"], "web-prod")
	assert.Equal(t, false, resp["cached"])
	assert.Equal(t, 1, fake.counter, "first call hits the LLM")
}

func TestExplainAuditEvent_CacheHitSkipsLLM(t *testing.T) {
	fake := newFakeAnthropicForExplain("first explanation.")
	h, svc, store, _ := setupExplainHandlers(t, fake)
	id := seedAuditRow(t, svc, store)

	// First call populates the cache.
	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	c1.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c1.Params = gin.Params{{Key: "id", Value: id}}
	h.HandleExplainAuditEvent(c1)
	require.Equal(t, http.StatusOK, w1.Code)
	require.Equal(t, 1, fake.counter)

	// Second call should serve from cache.
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c2.Params = gin.Params{{Key: "id", Value: id}}
	h.HandleExplainAuditEvent(c2)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, 1, fake.counter, "cache hit must not call the LLM again")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["cached"])
	assert.Equal(t, "first explanation.", resp["explanation"])
}

func TestExplainAuditEvent_RegenerateBypassesCache(t *testing.T) {
	fake := newFakeAnthropicForExplain("first explanation.")
	h, svc, store, _ := setupExplainHandlers(t, fake)
	id := seedAuditRow(t, svc, store)

	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	c1.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c1.Params = gin.Params{{Key: "id", Value: id}}
	h.HandleExplainAuditEvent(c1)
	require.Equal(t, http.StatusOK, w1.Code)

	// Change the fake response, force a regenerate.
	fake.respText = "fresh explanation after regenerate."

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest(http.MethodPost,
		"/?regenerate=1", nil)
	c2.Params = gin.Params{{Key: "id", Value: id}}
	h.HandleExplainAuditEvent(c2)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, 2, fake.counter, "regenerate must call the LLM again")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["cached"])
	assert.Equal(t, "fresh explanation after regenerate.", resp["explanation"])
}

func TestExplainAuditEvent_LLMErrorReturns502(t *testing.T) {
	fake := newFakeAnthropicForExplain("")
	fake.status = http.StatusInternalServerError
	h, svc, store, _ := setupExplainHandlers(t, fake)
	id := seedAuditRow(t, svc, store)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.HandleExplainAuditEvent(c)

	assert.Equal(t, http.StatusBadGateway, w.Code)
	body, _ := io.ReadAll(w.Body)
	assert.Contains(t, string(body), "failed to generate explanation")
}

func TestExplainAuditEvent_NotFoundReturns404(t *testing.T) {
	fake := newFakeAnthropicForExplain("x")
	h, _, _, _ := setupExplainHandlers(t, fake)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Params = gin.Params{{Key: "id", Value: "no-such-id"}}
	h.HandleExplainAuditEvent(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, 0, fake.counter, "missing row must not call the LLM")
}

func TestExplainAuditEvent_DisabledReturns503(t *testing.T) {
	store := memory.NewStore()
	svc := services.NewAuditService(store, nil, zap.NewNop())
	// Construct an AI service with no API key — Enabled() reports false.
	disabledAI := ai.NewService(ai.Config{Enabled: false}, zap.NewNop())
	h := NewAuditHandlers(svc, disabledAI, store, zap.NewNop())

	id := seedAuditRow(t, svc, store)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.HandleExplainAuditEvent(c)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "AI assist is not configured")
}

func TestExplainAuditEvent_PersistsCacheOnSuccess(t *testing.T) {
	fake := newFakeAnthropicForExplain("cached explanation body.")
	h, svc, store, _ := setupExplainHandlers(t, fake)
	id := seedAuditRow(t, svc, store)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.HandleExplainAuditEvent(c)
	require.Equal(t, http.StatusOK, w.Code)

	event, err := svc.Get(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, event)
	assert.Equal(t, "cached explanation body.", event.AIExplanation)
	assert.Equal(t, "claude-haiku-4-5-20251001", event.AIExplanationModel)
	require.NotNil(t, event.AIExplanationGeneratedAt)
}

