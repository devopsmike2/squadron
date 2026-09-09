// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// TestSetAPIKey_FlipsEnabled verifies the runtime key rotation turns the
// service on when a key arrives and off when it's cleared.
func TestSetAPIKey_FlipsEnabled(t *testing.T) {
	s := NewService(Config{Enabled: true, APIKey: ""}, zap.NewNop())
	if s.Enabled() {
		t.Fatal("Enabled() true with no key")
	}
	s.SetAPIKey("tk-live-key")
	if !s.Enabled() {
		t.Fatal("Enabled() false after SetAPIKey with a key")
	}
	s.SetAPIKey("")
	if s.Enabled() {
		t.Fatal("Enabled() true after SetAPIKey(\"\")")
	}
}

// TestSetAPIKey_ProviderUsesNewKey proves the rebuilt provider actually
// sends the NEW key on the wire — not the key captured at NewService.
func TestSetAPIKey_ProviderUsesNewKey(t *testing.T) {
	var mu sync.Mutex
	var lastKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastKey = r.Header.Get("x-api-key")
		mu.Unlock()
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"model":"m","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	s := NewService(Config{Enabled: true, APIKey: "old-key", BaseURL: srv.URL}, zap.NewNop())

	if _, err := s.ExplainSnippet(context.Background(), ExplainSnippetRequest{Snippet: "processors: {}"}); err != nil {
		t.Fatalf("ExplainSnippet #1: %v", err)
	}
	mu.Lock()
	got := lastKey
	mu.Unlock()
	if got != "old-key" {
		t.Fatalf("first call used key %q, want old-key", got)
	}

	s.SetAPIKey("new-key")
	if _, err := s.ExplainSnippet(context.Background(), ExplainSnippetRequest{Snippet: "processors: {}"}); err != nil {
		t.Fatalf("ExplainSnippet #2: %v", err)
	}
	mu.Lock()
	got = lastKey
	mu.Unlock()
	if got != "new-key" {
		t.Fatalf("second call used key %q, want new-key (provider did not pick up the rotated key)", got)
	}
}

// TestCapabilities_KeySourceAndLast4 checks the non-secret status hints
// track the key lifecycle and never expose the full key.
func TestCapabilities_KeySourceAndLast4(t *testing.T) {
	const envKey = "tk-env-abcd1234"
	s := NewService(Config{Enabled: true, APIKey: envKey}, zap.NewNop())

	caps := s.Capabilities()
	if caps.KeySource != KeySourceEnv {
		t.Errorf("KeySource = %q, want %q", caps.KeySource, KeySourceEnv)
	}
	if caps.KeyLast4 != "1234" {
		t.Errorf("KeyLast4 = %q, want 1234", caps.KeyLast4)
	}
	if caps.KeyLast4 == envKey {
		t.Error("KeyLast4 equals the full key")
	}
	enc, _ := json.Marshal(caps)
	if strings.Contains(string(enc), envKey) {
		t.Fatalf("full key leaked in Capabilities JSON: %s", enc)
	}

	s.SetAPIKey("fixture-stored-wxyz9876")
	caps = s.Capabilities()
	if caps.KeySource != KeySourceStored {
		t.Errorf("KeySource after set = %q, want %q", caps.KeySource, KeySourceStored)
	}
	if caps.KeyLast4 != "9876" {
		t.Errorf("KeyLast4 after set = %q, want 9876", caps.KeyLast4)
	}

	s.SetAPIKey("")
	caps = s.Capabilities()
	if caps.KeySource != KeySourceNone {
		t.Errorf("KeySource after clear = %q, want %q", caps.KeySource, KeySourceNone)
	}
	if caps.KeyLast4 != "" {
		t.Errorf("KeyLast4 after clear = %q, want empty", caps.KeyLast4)
	}
}

// TestSetAPIKey_ConcurrentNoRace hammers SetAPIKey against the read path
// so `go test -race` flags any unguarded access to cfg / provider.
func TestSetAPIKey_ConcurrentNoRace(t *testing.T) {
	s := NewService(Config{Enabled: true, APIKey: "seed"}, zap.NewNop())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if j%2 == 0 {
					s.SetAPIKey("k")
				} else {
					s.SetAPIKey("")
				}
				_ = s.Enabled()
				_ = s.Capabilities()
			}
		}(i)
	}
	wg.Wait()
}
