// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// fakeOpenAI spins up an httptest.Server that records the inbound
// request and replies with a canned Chat Completions-shaped JSON
// body. Mirrors fakeAnthropic in ai_test.go so the provider seam is
// tested in the same style.
type fakeOpenAI struct {
	t            *testing.T
	respText     string
	respModel    string
	respStatus   int
	promptTokens int
	compTokens   int
	lastRequest  *http.Request
	lastBodyJSON map[string]any
}

func newFakeOpenAI(t *testing.T, respText string) *fakeOpenAI {
	t.Helper()
	return &fakeOpenAI{
		t:            t,
		respText:     respText,
		respModel:    "gpt-4o-mini",
		respStatus:   200,
		promptTokens: 55,
		compTokens:   12,
	}
}

func (f *fakeOpenAI) start() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastRequest = r
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &f.lastBodyJSON)

		// The OpenAI-compatible endpoint is /chat/completions.
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"type":"not_found","message":"wrong path"}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if f.respStatus != 0 && f.respStatus != 200 {
			w.WriteHeader(f.respStatus)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"forced failure"}}`))
			return
		}
		out := map[string]any{
			"id":     "chatcmpl_test",
			"object": "chat.completion",
			"model":  f.respModel,
			"choices": []map[string]any{
				{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]string{
						"role":    "assistant",
						"content": f.respText,
					},
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     f.promptTokens,
				"completion_tokens": f.compTokens,
				"total_tokens":      f.promptTokens + f.compTokens,
			},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// mkOpenAIService builds a Service pointed at the fake OpenAI server.
func mkOpenAIService(t *testing.T, srvURL string) *Service {
	t.Helper()
	return NewService(Config{
		Enabled:      true,
		Provider:     "openai",
		APIKey:       "sk-test-not-real",
		BaseURL:      srvURL,
		ExplainModel: "gpt-4o-mini",
		MergeModel:   "gpt-4o",
		MaxTokens:    256,
	}, zap.NewNop())
}

// TestOpenAIProvider_HappyPath verifies Bearer auth, the Anthropic
// system field -> OpenAI system-message translation, and the
// choices[0].message.content + usage decode.
func TestOpenAIProvider_HappyPath(t *testing.T) {
	fake := newFakeOpenAI(t, "This processor drops the http.url attribute from the metrics pipeline.")
	srv := fake.start()
	defer srv.Close()
	svc := mkOpenAIService(t, srv.URL)

	resp, err := svc.ExplainSnippet(context.Background(), ExplainSnippetRequest{
		Snippet: "processors:\n  attributes/drop_http_url:\n    actions:\n      - key: \"http.url\"\n        action: delete",
		Signal:  "metrics",
		Goal:    "Drop attribute \"http.url\" from metrics",
	})
	if err != nil {
		t.Fatalf("ExplainSnippet (openai): %v", err)
	}
	if !strings.Contains(resp.Explanation, "http.url") {
		t.Errorf("explanation didn't mention http.url: %q", resp.Explanation)
	}
	if resp.TokensIn != 55 || resp.TokensOut != 12 {
		t.Errorf("token counts: got (%d, %d), want (55, 12)", resp.TokensIn, resp.TokensOut)
	}

	// Bearer auth (not x-api-key / not Azure api-key).
	if got := fake.lastRequest.Header.Get("Authorization"); got != "Bearer sk-test-not-real" {
		t.Errorf("Authorization header: got %q, want Bearer sk-test-not-real", got)
	}
	if got := fake.lastRequest.Header.Get("api-key"); got != "" {
		t.Errorf("non-Azure request should not set api-key header; got %q", got)
	}
	if got := fake.lastRequest.Header.Get("x-api-key"); got != "" {
		t.Errorf("openai path must not send x-api-key; got %q", got)
	}

	// Model routed through and endpoint path correct.
	if got, _ := fake.lastBodyJSON["model"].(string); got != "gpt-4o-mini" {
		t.Errorf("model in body: got %q", got)
	}
	if !strings.HasSuffix(fake.lastRequest.URL.Path, "/chat/completions") {
		t.Errorf("endpoint path: got %q", fake.lastRequest.URL.Path)
	}
	if mt, _ := fake.lastBodyJSON["max_tokens"].(float64); int(mt) != 256 {
		t.Errorf("max_tokens: got %v", mt)
	}

	// messages: [system(prompt), user(snippet)].
	msgs, ok := fake.lastBodyJSON["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system+user); got %v", fake.lastBodyJSON["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Errorf("first message role: got %q, want system", role)
	}
	if sys, _ := first["content"].(string); !strings.Contains(sys, "OpenTelemetry Collector") {
		t.Errorf("system message missing OTel mention: %q", sys)
	}
	second, _ := msgs[1].(map[string]any)
	if role, _ := second["role"].(string); role != "user" {
		t.Errorf("second message role: got %q, want user", role)
	}
	if usr, _ := second["content"].(string); !strings.Contains(usr, "http.url") {
		t.Errorf("user message missing snippet: %q", usr)
	}
}

// TestOpenAIProvider_OmitsEmptySystem verifies an empty System yields
// only a single user message (no leading empty system message).
func TestOpenAIProvider_OmitsEmptySystem(t *testing.T) {
	fake := newFakeOpenAI(t, "ok")
	srv := fake.start()
	defer srv.Close()
	svc := mkOpenAIService(t, srv.URL)

	// callMessages is unexported; drive it through the provider directly
	// with an empty System to assert the omit-system branch.
	p := &openaiProvider{cfg: Config{Provider: "openai", APIKey: "sk-x", BaseURL: srv.URL, MaxTokens: 64}, client: svc.client, logger: zap.NewNop()}
	_, err := p.Complete(context.Background(), callOpts{Model: "gpt-4o-mini", System: "", UserText: "hello"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	msgs, ok := fake.lastBodyJSON["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("empty System should yield 1 message; got %v", fake.lastBodyJSON["messages"])
	}
	only, _ := msgs[0].(map[string]any)
	if role, _ := only["role"].(string); role != "user" {
		t.Errorf("sole message role: got %q, want user", role)
	}
}

// TestOpenAIProvider_ErrorSurfaced surfaces a >=400 body verbatim,
// mirroring the Anthropic error path.
func TestOpenAIProvider_ErrorSurfaced(t *testing.T) {
	fake := newFakeOpenAI(t, "")
	fake.respStatus = 401
	srv := fake.start()
	defer srv.Close()
	svc := mkOpenAIService(t, srv.URL)

	_, err := svc.ExplainSnippet(context.Background(), ExplainSnippetRequest{Snippet: "x"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 surfaced; got %v", err)
	}
	if !strings.Contains(err.Error(), "forced failure") {
		t.Errorf("expected upstream error body surfaced; got %v", err)
	}
}

// TestOpenAIProvider_AzureAuthHeader verifies that an Azure-looking
// base URL switches auth to the api-key header instead of Bearer.
func TestOpenAIProvider_AzureAuthHeader(t *testing.T) {
	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	// Base URL host contains "azure" -> api-key header branch.
	base := srv.URL + "/azure-openai/deployments/gpt-4o"
	p := &openaiProvider{cfg: Config{Provider: "openai", APIKey: "azkey", BaseURL: base, MaxTokens: 64}, client: srv.Client(), logger: zap.NewNop()}
	resp, err := p.Complete(context.Background(), callOpts{Model: "gpt-4o", System: "sys", UserText: "u"})
	if err != nil {
		t.Fatalf("Complete (azure): %v", err)
	}
	if resp.Text != "hi" {
		t.Errorf("content parse: got %q", resp.Text)
	}
	if gotAPIKey != "azkey" {
		t.Errorf("azure branch should set api-key header; got %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Errorf("azure branch must not set Authorization; got %q", gotAuth)
	}
}
