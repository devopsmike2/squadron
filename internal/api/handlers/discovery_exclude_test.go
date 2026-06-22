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
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeExclusionStore is the test-side DiscoveryExclusionStore. Records
// the last upsert call and returns the prevExcluded value supplied at
// construction so the handler tests can exercise both the
// transition and no-op paths.
type fakeExclusionStore struct {
	mu           sync.Mutex
	called       bool
	gotRec       types.ExcludedRecommendation
	gotExcluded  bool
	prevExcluded bool
	returnErr    error
}

func (f *fakeExclusionStore) SetRecommendationExclusion(
	_ context.Context,
	rec types.ExcludedRecommendation,
	excluded bool,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.gotRec = rec
	f.gotExcluded = excluded
	if f.returnErr != nil {
		return false, f.returnErr
	}
	return f.prevExcluded, nil
}

// newExcludeHandlers wires DiscoveryHandlers with an exclusion store
// and an audit recorder. The credstore is nil because the exclude
// route does not consult it; logger is a no-op.
func newExcludeHandlers(t *testing.T, store *fakeExclusionStore, audit services.AuditService) *DiscoveryHandlers {
	t.Helper()
	h := NewDiscoveryHandlers(nil, zap.NewNop())
	if audit != nil {
		h.WithAuditService(audit)
	}
	if store != nil {
		h.WithExclusionStore(store)
	}
	return h
}

func doExcludeRequest(h *DiscoveryHandlers, body string) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/api/v1/discovery/aws/recommendations/exclude", h.HandleAWSRecommendationExclude)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/discovery/aws/recommendations/exclude", nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/discovery/aws/recommendations/exclude", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func sampleExcludeRequest(t *testing.T, excluded bool) string {
	t.Helper()
	payload := map[string]any{
		"recommendation_id":   "rec_abc123",
		"connection_id":       "conn-1",
		"account_id":          "123456789012",
		"region":              "us-east-1",
		"recommendation_kind": "eks-observability-addon",
		"excluded":            excluded,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal exclude body: %v", err)
	}
	return string(buf)
}

// TestExcludeRecommendation_NewExclusion_EmitsExcludedAudit covers the
// false → true transition. The store reports prevExcluded=false; the
// handler emits services.AuditEventDiscoveryRecommendationExcluded
// with the recommendation_id and connection scope in the payload.
func TestExcludeRecommendation_NewExclusion_EmitsExcludedAudit(t *testing.T) {
	store := &fakeExclusionStore{prevExcluded: false}
	audit := &discoveryRecordingAudit{}
	h := newExcludeHandlers(t, store, audit)
	w := doExcludeRequest(h, sampleExcludeRequest(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !store.called {
		t.Fatalf("store should have been called")
	}
	if !store.gotExcluded {
		t.Errorf("store should have been called with excluded=true")
	}
	if store.gotRec.RecommendationID != "rec_abc123" {
		t.Errorf("store call recommendation_id = %q, want rec_abc123", store.gotRec.RecommendationID)
	}
	if store.gotRec.ExcludedBy == "" {
		t.Errorf("ExcludedBy should be populated from the actor")
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1; entries=%+v", len(audit.entries), audit.entries)
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventDiscoveryRecommendationExcluded {
		t.Errorf("event_type = %q, want %q", e.EventType, services.AuditEventDiscoveryRecommendationExcluded)
	}
	if e.Action != "excluded" {
		t.Errorf("action = %q, want excluded", e.Action)
	}
	if got, _ := e.Payload["recommendation_id"].(string); got != "rec_abc123" {
		t.Errorf("payload.recommendation_id = %q, want rec_abc123", got)
	}
	if got, _ := e.Payload["connection_id"].(string); got != "conn-1" {
		t.Errorf("payload.connection_id = %q", got)
	}
	if got, _ := e.Payload["recommendation_kind"].(string); got != "eks-observability-addon" {
		t.Errorf("payload.recommendation_kind = %q", got)
	}
	if _, ok := e.Payload["excluded_by"]; !ok {
		t.Errorf("payload missing excluded_by")
	}
	if _, ok := e.Payload["cleared_by"]; ok {
		t.Errorf("payload should not carry cleared_by on the excluded event")
	}
}

// TestExcludeRecommendation_ClearExclusion_EmitsClearedAudit covers
// the true → false transition. The store reports prevExcluded=true;
// the handler emits services.AuditEventDiscoveryRecommendationExclude-
// Cleared with the actor flowing into payload.cleared_by.
func TestExcludeRecommendation_ClearExclusion_EmitsClearedAudit(t *testing.T) {
	store := &fakeExclusionStore{prevExcluded: true}
	audit := &discoveryRecordingAudit{}
	h := newExcludeHandlers(t, store, audit)
	w := doExcludeRequest(h, sampleExcludeRequest(t, false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !store.called {
		t.Fatalf("store should have been called")
	}
	if store.gotExcluded {
		t.Errorf("store should have been called with excluded=false")
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventDiscoveryRecommendationExcludeCleared {
		t.Errorf("event_type = %q, want %q", e.EventType, services.AuditEventDiscoveryRecommendationExcludeCleared)
	}
	if e.Action != "exclude_cleared" {
		t.Errorf("action = %q, want exclude_cleared", e.Action)
	}
	if _, ok := e.Payload["cleared_by"]; !ok {
		t.Errorf("payload missing cleared_by")
	}
	if _, ok := e.Payload["excluded_by"]; ok {
		t.Errorf("payload should not carry excluded_by on the cleared event")
	}
}

// TestExcludeRecommendation_NoOp_NoAuditEvent covers the case where
// the store reports prevExcluded already matches the requested state
// (true → true or false → false). The handler returns 200 with the
// canonical state but emits no audit row.
func TestExcludeRecommendation_NoOp_NoAuditEvent(t *testing.T) {
	// Already excluded; operator clicks exclude again — no-op.
	store := &fakeExclusionStore{prevExcluded: true}
	audit := &discoveryRecordingAudit{}
	h := newExcludeHandlers(t, store, audit)
	w := doExcludeRequest(h, sampleExcludeRequest(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 0 {
		t.Fatalf("no-op should emit zero audit rows; got %d", len(audit.entries))
	}

	// Already cleared; operator clicks un-exclude — also no-op.
	store2 := &fakeExclusionStore{prevExcluded: false}
	audit2 := &discoveryRecordingAudit{}
	h2 := newExcludeHandlers(t, store2, audit2)
	w2 := doExcludeRequest(h2, sampleExcludeRequest(t, false))
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	if len(audit2.entries) != 0 {
		t.Fatalf("no-op should emit zero audit rows; got %d", len(audit2.entries))
	}
}

// TestExcludeRecommendation_MissingFields_Returns400 covers the per-
// field validation rejections. The store must NOT be called on any of
// these — the handler short-circuits at the 400.
func TestExcludeRecommendation_MissingFields_Returns400(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing recommendation_id",
			body: `{"connection_id":"c","account_id":"a","region":"r","recommendation_kind":"k","excluded":true}`,
			want: "recommendation_id is required",
		},
		{
			name: "missing connection_id",
			body: `{"recommendation_id":"r","account_id":"a","region":"r","recommendation_kind":"k","excluded":true}`,
			want: "connection_id is required",
		},
		{
			name: "missing account_id",
			body: `{"recommendation_id":"r","connection_id":"c","region":"r","recommendation_kind":"k","excluded":true}`,
			want: "account_id is required",
		},
		{
			name: "missing region",
			body: `{"recommendation_id":"r","connection_id":"c","account_id":"a","recommendation_kind":"k","excluded":true}`,
			want: "region is required",
		},
		{
			name: "missing recommendation_kind",
			body: `{"recommendation_id":"r","connection_id":"c","account_id":"a","region":"r","excluded":true}`,
			want: "recommendation_kind is required",
		},
		{
			name: "malformed body",
			body: `{`,
			want: "could not be parsed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeExclusionStore{}
			audit := &discoveryRecordingAudit{}
			h := newExcludeHandlers(t, store, audit)
			w := doExcludeRequest(h, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body should explain the validation failure (%q): %s", tc.want, w.Body.String())
			}
			if store.called {
				t.Errorf("store should NOT have been called on validation failure")
			}
			if len(audit.entries) != 0 {
				t.Errorf("audit should NOT have been emitted on validation failure")
			}
		})
	}
}

// TestExcludeRecommendation_StoreNotWired_Returns503 covers the
// graceful-503 path when the trampoline didn't wire the store (the
// production wiring on test_server.go's no-substrate path).
func TestExcludeRecommendation_StoreNotWired_Returns503(t *testing.T) {
	h := newExcludeHandlers(t, nil, &discoveryRecordingAudit{})
	w := doExcludeRequest(h, sampleExcludeRequest(t, true))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ExclusionStoreNotWired") && !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("body should explain the wiring miss: %s", w.Body.String())
	}
}
