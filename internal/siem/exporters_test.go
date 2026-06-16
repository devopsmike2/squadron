// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package siem

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sampleEvent() Event {
	return Event{
		ID:         "evt-1",
		Timestamp:  time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC),
		Actor:      "alice@example.com",
		EventType:  "rollout.approved",
		TargetType: "rollout",
		TargetID:   "r-1",
		Action:     "approved",
		Payload:    map[string]any{"notes": "looks good"},
		Source:     "squadron",
	}
}

// TestSplunkHEC_AuthHeader verifies the auth scheme is "Splunk
// <token>", not "Bearer". Splunk HEC explicitly requires this and
// rejects Bearer; getting it wrong is a common foot-gun.
func TestSplunkHEC_AuthHeader(t *testing.T) {
	var gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	e := &SplunkHECExporter{URL: srv.URL, Token: "abc-token"}
	if err := e.Send(context.Background(), sampleEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotAuth != "Splunk abc-token" {
		t.Errorf("auth = %q, want %q", gotAuth, "Splunk abc-token")
	}
	// Splunk wraps the event in { time, source, sourcetype, event }.
	var got map[string]any
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["sourcetype"] != "squadron:audit" {
		t.Errorf("sourcetype = %v, want squadron:audit", got["sourcetype"])
	}
	if got["event"] == nil {
		t.Errorf("event key missing from HEC envelope")
	}
}

func TestSplunkHEC_SurfaceErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"text":"invalid token","code":4}`))
	}))
	defer srv.Close()
	e := &SplunkHECExporter{URL: srv.URL, Token: "bad"}
	err := e.Send(context.Background(), sampleEvent())
	if err == nil {
		t.Fatal("expected error from 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("expected error to surface status + body, got %v", err)
	}
}

// TestWebhook_Signature verifies the HMAC-SHA256 signature matches
// the body. This is the proof of provenance receivers rely on; if
// the algorithm or header format drifts, every webhook integration
// breaks.
func TestWebhook_Signature(t *testing.T) {
	secret := []byte("very-secret-key")
	var gotSig string
	var gotBody []byte
	var gotEventType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Squadron-Signature")
		gotEventType = r.Header.Get("X-Squadron-Event-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	e := &WebhookExporter{URL: srv.URL, Secret: secret}
	ev := sampleEvent()
	if err := e.Send(context.Background(), ev); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Recompute the expected signature exactly the way a receiver
	// would, and compare.
	mac := hmac.New(sha256.New, secret)
	mac.Write(gotBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("signature mismatch:\n got: %s\nwant: %s", gotSig, want)
	}
	if gotEventType != ev.EventType {
		t.Errorf("X-Squadron-Event-Type = %q, want %q", gotEventType, ev.EventType)
	}
}

// TestBuildExporter_RejectsUnknownType ensures a typo in the
// destination type doesn't silently fall through to a no-op exporter.
func TestBuildExporter_RejectsUnknownType(t *testing.T) {
	d := &Destination{Type: "splunky"}
	if _, err := BuildExporter(d, []byte("x")); err == nil {
		t.Errorf("expected error for unknown type, got nil")
	}
}

// TestDestination_Filter covers the EventTypePrefix allowlist
// behavior — empty = forward everything; non-empty = only matching
// prefixes get through.
func TestDestination_Filter(t *testing.T) {
	d := &Destination{}
	if !d.MatchesFilter("anything.goes") {
		t.Errorf("empty filter should match everything")
	}
	d.EventTypePrefix = []string{"rollout."}
	if !d.MatchesFilter("rollout.approved") {
		t.Errorf("prefix should match")
	}
	if d.MatchesFilter("config.created") {
		t.Errorf("non-matching prefix should be filtered out")
	}
}
