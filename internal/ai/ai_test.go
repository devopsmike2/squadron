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

// fakeAnthropic spins up an httptest.Server that records the
// inbound request and replies with a canned Anthropic-shaped JSON
// body. Tests use it to verify request shape (model, headers,
// system prompt fragments) without hitting the real API.
type fakeAnthropic struct {
	t            *testing.T
	respText     string  // text returned in the first content block
	respModel    string  // model echoed back
	respStatus   int     // default 200
	tokensIn     int
	tokensOut    int
	lastRequest  *http.Request
	lastBodyJSON map[string]any
}

func newFake(t *testing.T, respText string) *fakeAnthropic {
	t.Helper()
	return &fakeAnthropic{
		t:          t,
		respText:   respText,
		respModel:  "claude-haiku-4-5-20251001",
		respStatus: 200,
		tokensIn:   42,
		tokensOut:  17,
	}
}

func (f *fakeAnthropic) start() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastRequest = r
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &f.lastBodyJSON)

		w.Header().Set("Content-Type", "application/json")
		if f.respStatus != 0 && f.respStatus != 200 {
			w.WriteHeader(f.respStatus)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"forced failure"}}`))
			return
		}
		out := map[string]any{
			"id":    "msg_test",
			"model": f.respModel,
			"role":  "assistant",
			"content": []map[string]string{
				{"type": "text", "text": f.respText},
			},
			"usage": map[string]int{
				"input_tokens":  f.tokensIn,
				"output_tokens": f.tokensOut,
			},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

// mkService builds a Service pointed at the fake server.
func mkService(t *testing.T, srvURL string) *Service {
	t.Helper()
	return NewService(Config{
		Enabled:      true,
		APIKey:       "test-key-not-real",
		BaseURL:      srvURL,
		ExplainModel: "claude-haiku-4-5-20251001",
		MergeModel:   "claude-sonnet-4-6",
		MaxTokens:    256,
	}, zap.NewNop())
}

// TestEnabled covers the two cases the trampoline cares about:
// missing key disables; explicit Enabled+key enables.
func TestEnabled(t *testing.T) {
	disabled := NewService(Config{Enabled: false}, zap.NewNop())
	if disabled.Enabled() {
		t.Errorf("Enabled=false service should report Enabled()=false")
	}
	noKey := NewService(Config{Enabled: true, APIKey: ""}, zap.NewNop())
	if noKey.Enabled() {
		t.Errorf("Enabled=true but APIKey empty should report Enabled()=false")
	}
	yes := NewService(Config{Enabled: true, APIKey: "k"}, zap.NewNop())
	if !yes.Enabled() {
		t.Errorf("Enabled=true + APIKey set should report Enabled()=true")
	}
}

// TestCapabilities never leaks the API key.
func TestCapabilities_NeverLeaksAPIKey(t *testing.T) {
	s := NewService(Config{Enabled: true, APIKey: "super-secret"}, zap.NewNop())
	caps := s.Capabilities()
	enc, _ := json.Marshal(caps)
	if strings.Contains(string(enc), "super-secret") {
		t.Fatalf("API key leaked in Capabilities JSON: %s", enc)
	}
	if !caps.Enabled {
		t.Errorf("Enabled should be true when service is enabled")
	}
}

// TestExplainSnippet_HappyPath verifies request shape + response decode.
func TestExplainSnippet_HappyPath(t *testing.T) {
	fake := newFake(t, "This processor drops the http.url attribute from the metrics pipeline.")
	srv := fake.start()
	defer srv.Close()
	svc := mkService(t, srv.URL)

	resp, err := svc.ExplainSnippet(context.Background(), ExplainSnippetRequest{
		Snippet: "processors:\n  attributes/drop_http_url:\n    actions:\n      - key: \"http.url\"\n        action: delete",
		Signal:  "metrics",
		Goal:    "Drop attribute \"http.url\" from metrics",
	})
	if err != nil {
		t.Fatalf("ExplainSnippet: %v", err)
	}
	if !strings.Contains(resp.Explanation, "http.url") {
		t.Errorf("explanation didn't mention http.url: %q", resp.Explanation)
	}
	if resp.TokensIn != 42 || resp.TokensOut != 17 {
		t.Errorf("token counts: got (%d, %d), want (42, 17)", resp.TokensIn, resp.TokensOut)
	}

	// Request shape checks.
	if got := fake.lastRequest.Header.Get("x-api-key"); got != "test-key-not-real" {
		t.Errorf("x-api-key header: got %q", got)
	}
	if got := fake.lastRequest.Header.Get("anthropic-version"); got != apiVersion {
		t.Errorf("anthropic-version header: got %q", got)
	}
	if got, _ := fake.lastBodyJSON["model"].(string); got != "claude-haiku-4-5-20251001" {
		t.Errorf("model in body: got %q", got)
	}
	if mt, _ := fake.lastBodyJSON["max_tokens"].(float64); int(mt) != 256 {
		t.Errorf("max_tokens: got %v", mt)
	}
	sys, _ := fake.lastBodyJSON["system"].(string)
	if !strings.Contains(sys, "OpenTelemetry Collector") {
		t.Errorf("system prompt missing OTel mention: %q", sys)
	}
}

func TestExplainSnippet_DisabledShortCircuits(t *testing.T) {
	svc := NewService(Config{Enabled: false}, zap.NewNop())
	_, err := svc.ExplainSnippet(context.Background(), ExplainSnippetRequest{Snippet: "x"})
	if err != ErrDisabled {
		t.Errorf("expected ErrDisabled, got %v", err)
	}
}

func TestExplainSnippet_EmptySnippetRejected(t *testing.T) {
	svc := NewService(Config{Enabled: true, APIKey: "k", BaseURL: "http://unreachable"}, zap.NewNop())
	_, err := svc.ExplainSnippet(context.Background(), ExplainSnippetRequest{Snippet: "   "})
	if err == nil || !strings.Contains(err.Error(), "snippet is required") {
		t.Errorf("expected snippet-required error, got %v", err)
	}
}

// TestMergeIntoConfig parses the JSON envelope and uses the Merge model.
func TestMergeIntoConfig_HappyPath(t *testing.T) {
	merged := `receivers:
  otlp: {}
processors:
  batch:
    timeout: 10s
  attributes/drop_http_url:
    actions:
      - key: http.url
        action: delete
exporters:
  otlp: {}
service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [attributes/drop_http_url, batch]
      exporters: [otlp]`
	body := map[string]string{
		"merged_yaml": merged,
		"summary":     "Added attributes/drop_http_url to metrics pipeline.",
	}
	enc, _ := json.Marshal(body)
	fake := newFake(t, string(enc))
	fake.respModel = "claude-sonnet-4-6"
	srv := fake.start()
	defer srv.Close()
	svc := mkService(t, srv.URL)

	resp, err := svc.MergeIntoConfig(context.Background(), MergeIntoConfigRequest{
		BaseYAML: "receivers:\n  otlp: {}\nprocessors:\n  batch:\n    timeout: 10s",
		SnippetYAML: "processors:\n  attributes/drop_http_url:\n    actions:\n      - key: http.url\n        action: delete",
		Goal: "Drop http.url from metrics",
	})
	if err != nil {
		t.Fatalf("MergeIntoConfig: %v", err)
	}
	if !strings.Contains(resp.MergedYAML, "attributes/drop_http_url") {
		t.Errorf("merged yaml missing new processor: %s", resp.MergedYAML)
	}
	if resp.Summary == "" {
		t.Errorf("summary should be set")
	}
	// Merge must hit the Merge model, not the Explain model.
	if model, _ := fake.lastBodyJSON["model"].(string); model != "claude-sonnet-4-6" {
		t.Errorf("merge should use Sonnet; got %q", model)
	}
}

// TestMergeIntoConfig_NonJSONFallback exercises the defensive path
// when the model returns prose instead of the expected JSON envelope.
// We surface the prose as merged_yaml + a defensive summary rather
// than failing the request — operators can review the output.
func TestMergeIntoConfig_NonJSONFallback(t *testing.T) {
	fake := newFake(t, "Here's the merged YAML:\nreceivers:\n  otlp: {}\n")
	srv := fake.start()
	defer srv.Close()
	svc := mkService(t, srv.URL)

	resp, err := svc.MergeIntoConfig(context.Background(), MergeIntoConfigRequest{
		BaseYAML:    "receivers:\n  otlp: {}",
		SnippetYAML: "processors:\n  batch:\n    timeout: 10s",
	})
	if err != nil {
		t.Fatalf("MergeIntoConfig: %v", err)
	}
	if resp.MergedYAML == "" {
		t.Errorf("MergedYAML should not be empty in fallback")
	}
	if !strings.Contains(resp.Summary, "non-JSON") {
		t.Errorf("fallback summary should flag the issue: %q", resp.Summary)
	}
}

// TestMergeIntoConfig_StripsMarkdownFences covers the case the
// extractJSONBlock helper exists for.
func TestMergeIntoConfig_StripsMarkdownFences(t *testing.T) {
	body := `{"merged_yaml":"receivers:\n  otlp: {}","summary":"ok"}`
	wrapped := "```json\n" + body + "\n```"
	fake := newFake(t, wrapped)
	srv := fake.start()
	defer srv.Close()
	svc := mkService(t, srv.URL)

	resp, err := svc.MergeIntoConfig(context.Background(), MergeIntoConfigRequest{
		BaseYAML:    "receivers:\n  otlp: {}",
		SnippetYAML: "processors:\n  batch:\n    timeout: 10s",
	})
	if err != nil {
		t.Fatalf("MergeIntoConfig: %v", err)
	}
	if !strings.Contains(resp.MergedYAML, "receivers") {
		t.Errorf("expected merged YAML with receivers; got %q", resp.MergedYAML)
	}
	if resp.Summary != "ok" {
		t.Errorf("summary: got %q, want \"ok\"", resp.Summary)
	}
}

// TestExplainConfig parses the pipeline map.
func TestExplainConfig_PipelineMap(t *testing.T) {
	body := `{
		"summary": "Ingests OTLP traces + metrics, samples traces at 10%, exports to Honeycomb.",
		"pipelines": {
			"traces": "OTLP → probabilistic sampler (10%) → batch → otlp",
			"metrics": "OTLP → batch → otlp"
		}
	}`
	fake := newFake(t, body)
	srv := fake.start()
	defer srv.Close()
	svc := mkService(t, srv.URL)

	resp, err := svc.ExplainConfig(context.Background(), ExplainConfigRequest{YAML: "receivers: {}"})
	if err != nil {
		t.Fatalf("ExplainConfig: %v", err)
	}
	if !strings.Contains(resp.Summary, "Ingests OTLP") {
		t.Errorf("summary wrong: %q", resp.Summary)
	}
	if got := resp.Pipelines["traces"]; !strings.Contains(got, "probabilistic sampler") {
		t.Errorf("pipelines[traces]: %q", got)
	}
}

// TestAnthropicError surfaces 4xx responses verbatim. Operators
// need the original error (invalid api key, rate limit, etc.) to
// fix the problem.
func TestAnthropicError(t *testing.T) {
	fake := newFake(t, "")
	fake.respStatus = 401
	srv := fake.start()
	defer srv.Close()
	svc := mkService(t, srv.URL)

	_, err := svc.ExplainSnippet(context.Background(), ExplainSnippetRequest{Snippet: "x"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 surfaced in error; got %v", err)
	}
	if !strings.Contains(err.Error(), "forced failure") {
		t.Errorf("expected Anthropic error body in error; got %v", err)
	}
}

func TestExtractJSONBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain json", `{"a":1}`, `{"a":1}`},
		{"trimmed whitespace", `   {"a":1}   `, `{"a":1}`},
		{"with fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"bare fence", "```\n{\"a\":1}\n```", `{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.TrimSpace(extractJSONBlock(tc.in))
			if got != tc.want {
				t.Errorf("extractJSONBlock(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
