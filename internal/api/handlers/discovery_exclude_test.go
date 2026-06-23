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
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeExclusionStore is the test-side DiscoveryExclusionStore. Records
// the last upsert call and returns the prevExcluded value supplied at
// construction so the handler tests can exercise both the
// transition and no-op paths.
//
// v0.89.40 (#660 Stream 58): adds an in-memory list backing the new
// ListExcludedRecommendations surface. Tests seed rows via the
// `seeded` slice (matched per call by the scope tuple), so the same
// fake covers both the POST handler tests (chunk 4 + 5) and the new
// GET handler tests below.
type fakeExclusionStore struct {
	mu           sync.Mutex
	called       bool
	gotRec       types.ExcludedRecommendation
	gotExcluded  bool
	prevExcluded bool
	returnErr    error
	// seeded is the in-memory backing store for the list endpoint.
	// Per-row matching against (connection_id, account_id, region) is
	// done in ListExcludedRecommendations below.
	seeded []types.ExcludedRecommendation
	// listErr, when non-nil, makes ListExcludedRecommendations return
	// the canned error so the 500 path can be exercised.
	listErr error
	// gotListLimit captures the last limit value the handler passed
	// through. Asserted by the limit-default tests.
	gotListLimit int
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

func (f *fakeExclusionStore) ListExcludedRecommendations(
	_ context.Context,
	connectionID, accountID, region string,
	limit int,
) ([]types.ExcludedRecommendation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotListLimit = limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []types.ExcludedRecommendation
	for _, r := range f.seeded {
		if r.ConnectionID != connectionID || r.AccountID != accountID || r.Region != region {
			continue
		}
		out = append(out, r)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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

// --- v0.89.40 #660 Stream 58 (slice 2 chunk 5 follow-on) -----------
//
// Tests for HandleAWSRecommendationListExcluded. The GET endpoint
// surfaces the persisted iac_recommendation_verdicts rows for a
// given scope tuple so the UI can hydrate its excludedSet on tab
// mount. Three tests:
//   - empty scope → empty list returned (no 404, no special-case).
//   - mixed scopes → only the queried (C,A,R) tuple's rows surface.
//   - missing required query params → 400 with no store call.

// doListExcludedRequest issues a GET against the list endpoint. The
// scope params are required; the helper also accepts an optional
// limit so the default-limit assertion can be exercised.
func doListExcludedRequest(h *DiscoveryHandlers, qs string) *httptest.ResponseRecorder {
	r := gin.New()
	r.GET("/api/v1/discovery/aws/recommendations/excluded", h.HandleAWSRecommendationListExcluded)
	url := "/api/v1/discovery/aws/recommendations/excluded"
	if qs != "" {
		url += "?" + qs
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestListExcludedRecommendations_ReturnsEmptyOnEmptyScope: 0
// exclusions seeded → empty list returned. The wire shape MUST
// always emit an array (never null) so the UI's empty-state branch
// is a single .length check.
func TestListExcludedRecommendations_ReturnsEmptyOnEmptyScope(t *testing.T) {
	store := &fakeExclusionStore{}
	h := newExcludeHandlers(t, store, nil)
	w := doListExcludedRequest(h, "connection_id=c&account_id=a&region=r")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp awsRecommendationListExcludedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}
	if resp.Excluded == nil {
		t.Fatalf("excluded array should be non-nil even when empty; body=%s", w.Body.String())
	}
	if len(resp.Excluded) != 0 {
		t.Errorf("excluded length = %d, want 0", len(resp.Excluded))
	}
	// Default limit (100) must have flowed through to the store.
	if store.gotListLimit != 100 {
		t.Errorf("store call limit = %d, want default 100", store.gotListLimit)
	}
}

// TestListExcludedRecommendations_ReturnsScopedExclusions: seed 3
// exclusions across (C,A,R), (C,A,R-different), (C,A-different,R).
// Query (C,A,R). Assert: only 1 returned and it's the right one.
func TestListExcludedRecommendations_ReturnsScopedExclusions(t *testing.T) {
	now := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	store := &fakeExclusionStore{seeded: []types.ExcludedRecommendation{
		{
			RecommendationID:   "rec-in-scope",
			ConnectionID:       "C",
			AccountID:          "A",
			Region:             "R",
			RecommendationKind: "rds-pi-em",
			ResourceID:         "db-1",
			ExcludedAt:         now,
			ExcludedBy:         "alice",
		},
		{
			RecommendationID:   "rec-wrong-region",
			ConnectionID:       "C",
			AccountID:          "A",
			Region:             "R-different",
			RecommendationKind: "eks-observability-addon",
			ExcludedAt:         now,
			ExcludedBy:         "alice",
		},
		{
			RecommendationID:   "rec-wrong-account",
			ConnectionID:       "C",
			AccountID:          "A-different",
			Region:             "R",
			RecommendationKind: "lambda-otel-layer",
			ExcludedAt:         now,
			ExcludedBy:         "alice",
		},
	}}
	h := newExcludeHandlers(t, store, nil)
	w := doListExcludedRequest(h, "connection_id=C&account_id=A&region=R")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp awsRecommendationListExcludedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Excluded) != 1 {
		t.Fatalf("excluded length = %d, want 1; got=%+v", len(resp.Excluded), resp.Excluded)
	}
	got := resp.Excluded[0]
	if got.RecommendationID != "rec-in-scope" {
		t.Errorf("recommendation_id = %q, want rec-in-scope", got.RecommendationID)
	}
	if got.RecommendationKind != "rds-pi-em" {
		t.Errorf("recommendation_kind = %q, want rds-pi-em", got.RecommendationKind)
	}
	if got.ResourceID != "db-1" {
		t.Errorf("resource_id = %q, want db-1", got.ResourceID)
	}
	if got.ExcludedBy != "alice" {
		t.Errorf("excluded_by = %q, want alice", got.ExcludedBy)
	}
	if got.ExcludedAt.IsZero() {
		t.Errorf("excluded_at should be set; got zero")
	}
}

// TestListExcludedRecommendations_MissingFields_Returns400 covers
// the per-field validation rejections. The store must NOT be called
// on any of these — the handler short-circuits at the 400.
func TestListExcludedRecommendations_MissingFields_Returns400(t *testing.T) {
	cases := []struct {
		name string
		qs   string
		want string
	}{
		{
			name: "missing connection_id",
			qs:   "account_id=a&region=r",
			want: "connection_id",
		},
		{
			name: "missing account_id",
			qs:   "connection_id=c&region=r",
			want: "account_id",
		},
		{
			name: "missing region",
			qs:   "connection_id=c&account_id=a",
			want: "region",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeExclusionStore{seeded: []types.ExcludedRecommendation{
				{RecommendationID: "should-not-surface", ConnectionID: "c", AccountID: "a", Region: "r"},
			}}
			h := newExcludeHandlers(t, store, nil)
			w := doListExcludedRequest(h, tc.qs)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body should mention the missing field (%q): %s", tc.want, w.Body.String())
			}
			// gotListLimit stays at its zero value when the store was
			// never called.
			if store.gotListLimit != 0 {
				t.Errorf("store should NOT have been called on validation failure; gotListLimit=%d", store.gotListLimit)
			}
		})
	}
}

// TestListExcludedRecommendations_StoreNotWired_Returns503 covers the
// graceful-503 path when the trampoline didn't wire the store. Same
// posture as the POST sibling so AI-disabled deployments degrade
// consistently.
func TestListExcludedRecommendations_StoreNotWired_Returns503(t *testing.T) {
	h := newExcludeHandlers(t, nil, nil)
	w := doListExcludedRequest(h, "connection_id=c&account_id=a&region=r")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// --- v0.89.44 #665 Stream 63 (slice 1 chunk 4) ---------------------
//
// Tests for HandleAWSRecommendationExclude's PATCH-to-neutral chunk-4
// follow-up. The handler emits the discovery_recommendation.excluded
// audit event AND, when wired, looks up the in-flight check run and
// PATCHes it to conclusion=neutral via the Checks API. Five tests:
//   - false → true with in-flight check run → UpdateCheckRun called
//     with conclusion=neutral, iac.check_run.updated emitted with the
//     in_progress → neutral transition, storage row reconciled.
//   - false → true with NO check run row → no UpdateCheckRun call,
//     no iac.check_run.* audit event (only the discovery exclude
//     audit fires).
//   - false → true with completed check run → no UpdateCheckRun
//     call (the PR has already merged or closed; conclusion is
//     final per design doc §7).
//   - false → true with chunk-4 wiring unwired (no checksClient /
//     no PAT) → no UpdateCheckRun call. Fail-open posture: the
//     discovery exclude audit still fires; chunk-4 stays dormant.
//   - false → true with UpdateCheckRun returning a CheckRunError →
//     iac.check_run.failed emitted with the structured error_kind.

// newCheckRunExcludeHandlers builds a DiscoveryHandlers with all four
// chunk-4 follow-up surfaces wired. The chunk-2 fakes
// (fakeChecksClient, fakeCheckRunStore) defined in
// iac_github_checkrun_test.go are reused — both now satisfy the
// chunk-4-extended ChecksAPI + CheckRunStore interfaces.
func newCheckRunExcludeHandlers(
	t *testing.T,
	store *fakeExclusionStore,
	audit services.AuditService,
	checks ChecksAPI,
	crStore CheckRunStore,
	pat string,
) *DiscoveryHandlers {
	t.Helper()
	h := newExcludeHandlers(t, store, audit)
	if checks != nil {
		h.WithChecksClient(checks)
	}
	if crStore != nil {
		h.WithCheckRunStore(crStore)
	}
	if pat != "" {
		h.WithChecksPAT(pat)
	}
	h.WithSquadronHost("https://squadron.acme.example")
	return h
}

// TestExcludeRecommendation_WithInflightCheckRun_PATCHesNeutral pins
// the chunk-4 happy path. Seed a fakeCheckRunStore with an
// in_progress check run for rec_abc123; click exclude (false → true).
// Assert: UpdateCheckRun fired with conclusion=neutral, the
// iac.check_run.updated audit row landed with the in_progress →
// neutral transition, the discovery exclude audit also fired.
func TestExcludeRecommendation_WithInflightCheckRun_PATCHesNeutral(t *testing.T) {
	store := &fakeExclusionStore{prevExcluded: false}
	audit := &discoveryRecordingAudit{}
	checks := &fakeChecksClient{}
	crStore := &fakeCheckRunStore{
		seeded: map[string]fakeCheckRunStoreCall{
			"rec_abc123": {
				Ref: types.CheckRunRef{
					Owner: "octo", Repo: "widgets", CheckID: 9001, HeadSHA: "abc123",
				},
				Status:     iacgithub.CheckRunStatusInProgress,
				Conclusion: "",
			},
		},
	}
	h := newCheckRunExcludeHandlers(t, store, audit, checks, crStore, "pat-chk-write")

	w := doExcludeRequest(h, sampleExcludeRequest(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// UpdateCheckRun fired once.
	if len(checks.updateCalls) != 1 {
		t.Fatalf("UpdateCheckRun calls = %d, want 1", len(checks.updateCalls))
	}
	up := checks.updateCalls[0]
	if up.Ref.Owner != "octo" || up.Ref.Repo != "widgets" || up.Ref.CheckID != 9001 {
		t.Errorf("update ref = %+v, want octo/widgets#9001", up.Ref)
	}
	if up.Status != iacgithub.CheckRunStatusCompleted {
		t.Errorf("update status = %q, want completed", up.Status)
	}
	if up.Conclusion != iacgithub.CheckRunConclusionNeutral {
		t.Errorf("update conclusion = %q, want neutral", up.Conclusion)
	}
	if checks.updatePATs[0] != "pat-chk-write" {
		t.Errorf("update PAT = %q", checks.updatePATs[0])
	}
	if !strings.Contains(up.Output.Title, "NEUTRAL") {
		t.Errorf("update title = %q (expected NEUTRAL)", up.Output.Title)
	}

	// Audit log: discovery_recommendation.excluded + iac.check_run.updated.
	var sawExcluded, sawCRUpdated bool
	for _, e := range audit.entries {
		switch e.EventType {
		case services.AuditEventDiscoveryRecommendationExcluded:
			sawExcluded = true
		case services.AuditEventIaCCheckRunUpdated:
			sawCRUpdated = true
			if got, _ := e.Payload["new_conclusion"].(string); got != "neutral" {
				t.Errorf("new_conclusion = %q, want neutral", got)
			}
			if got, _ := e.Payload["new_status"].(string); got != "completed" {
				t.Errorf("new_status = %q, want completed", got)
			}
			if got, _ := e.Payload["previous_status"].(string); got != iacgithub.CheckRunStatusInProgress {
				t.Errorf("previous_status = %q, want in_progress", got)
			}
		}
	}
	if !sawExcluded {
		t.Errorf("expected discovery_recommendation.excluded audit; entries=%+v", audit.entries)
	}
	if !sawCRUpdated {
		t.Errorf("expected iac.check_run.updated audit; entries=%+v", audit.entries)
	}

	// Storage reconciliation: SetCheckRunForRecommendation fired with
	// completed + neutral so the next read sees the new state.
	if len(crStore.calls) != 1 {
		t.Fatalf("SetCheckRunForRecommendation calls = %d, want 1", len(crStore.calls))
	}
	if crStore.calls[0].Status != iacgithub.CheckRunStatusCompleted ||
		crStore.calls[0].Conclusion != iacgithub.CheckRunConclusionNeutral {
		t.Errorf("storage reconcile = (status=%q, conclusion=%q), want (completed, neutral)",
			crStore.calls[0].Status, crStore.calls[0].Conclusion)
	}
}

// TestExcludeRecommendation_NoCheckRunRow_SkipsPATCH pins the
// chunk-2-not-wired or row-pruned path. exists=false from the store
// → no UpdateCheckRun call, no iac.check_run.* audit.
func TestExcludeRecommendation_NoCheckRunRow_SkipsPATCH(t *testing.T) {
	store := &fakeExclusionStore{prevExcluded: false}
	audit := &discoveryRecordingAudit{}
	checks := &fakeChecksClient{}
	crStore := &fakeCheckRunStore{seeded: map[string]fakeCheckRunStoreCall{}}
	h := newCheckRunExcludeHandlers(t, store, audit, checks, crStore, "pat-chk-write")

	w := doExcludeRequest(h, sampleExcludeRequest(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(checks.updateCalls) != 0 {
		t.Errorf("UpdateCheckRun should not have fired; got %d calls", len(checks.updateCalls))
	}
	for _, e := range audit.entries {
		if e.EventType == services.AuditEventIaCCheckRunUpdated || e.EventType == services.AuditEventIaCCheckRunFailed {
			t.Errorf("no iac.check_run.* audit expected; got %s", e.EventType)
		}
	}
}

// TestExcludeRecommendation_CompletedCheckRun_SkipsPATCH pins the
// design doc §7 invariant: when the check run has already been
// PATCHed to completed (by the merge / close webhook), the exclusion
// handler does NOT overwrite the success / failure conclusion.
func TestExcludeRecommendation_CompletedCheckRun_SkipsPATCH(t *testing.T) {
	store := &fakeExclusionStore{prevExcluded: false}
	audit := &discoveryRecordingAudit{}
	checks := &fakeChecksClient{}
	crStore := &fakeCheckRunStore{
		seeded: map[string]fakeCheckRunStoreCall{
			"rec_abc123": {
				Ref: types.CheckRunRef{
					Owner: "octo", Repo: "widgets", CheckID: 9001, HeadSHA: "abc123",
				},
				Status:     iacgithub.CheckRunStatusCompleted,
				Conclusion: iacgithub.CheckRunConclusionSuccess,
			},
		},
	}
	h := newCheckRunExcludeHandlers(t, store, audit, checks, crStore, "pat-chk-write")

	w := doExcludeRequest(h, sampleExcludeRequest(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(checks.updateCalls) != 0 {
		t.Errorf("UpdateCheckRun should not have fired on completed check run; got %d calls", len(checks.updateCalls))
	}
	for _, e := range audit.entries {
		if e.EventType == services.AuditEventIaCCheckRunUpdated {
			t.Errorf("no iac.check_run.updated audit expected on completed check run; got %s", e.EventType)
		}
	}
}

// TestExcludeRecommendation_ChecksAPIUnwired_SkipsPATCH pins the
// fail-open posture for deployments that haven't enabled the chunk-4
// follow-up (nil checksClient or empty PAT). The discovery exclude
// audit still fires; no UpdateCheckRun call, no iac.check_run.*
// audit.
func TestExcludeRecommendation_ChecksAPIUnwired_SkipsPATCH(t *testing.T) {
	store := &fakeExclusionStore{prevExcluded: false}
	audit := &discoveryRecordingAudit{}
	crStore := &fakeCheckRunStore{
		seeded: map[string]fakeCheckRunStoreCall{
			"rec_abc123": {
				Ref:        types.CheckRunRef{Owner: "octo", Repo: "widgets", CheckID: 9001, HeadSHA: "abc"},
				Status:     iacgithub.CheckRunStatusInProgress,
				Conclusion: "",
			},
		},
	}
	// Pass a non-nil crStore + nil checks client AND empty PAT — both
	// short-circuit the follow-up.
	h := newCheckRunExcludeHandlers(t, store, audit, nil, crStore, "")

	w := doExcludeRequest(h, sampleExcludeRequest(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// The crStore.getCalls slice MUST stay empty — the handler
	// short-circuits BEFORE looking up the row.
	if len(crStore.getCalls) != 0 {
		t.Errorf("expected no Get calls when chunk-4 is unwired; got %d", len(crStore.getCalls))
	}
	for _, e := range audit.entries {
		if e.EventType == services.AuditEventIaCCheckRunUpdated || e.EventType == services.AuditEventIaCCheckRunFailed {
			t.Errorf("no iac.check_run.* audit expected on unwired chunk-4; got %s", e.EventType)
		}
	}
}

// TestExcludeRecommendation_UpdateCheckRunFails_EmitsFailedAudit pins
// the failure path. UpdateCheckRun returns a *CheckRunError; the
// handler emits iac.check_run.failed with the structured error_kind
// and the discovery exclude audit still fires. The PR open's
// existing check run stays where GitHub left it.
func TestExcludeRecommendation_UpdateCheckRunFails_EmitsFailedAudit(t *testing.T) {
	store := &fakeExclusionStore{prevExcluded: false}
	audit := &discoveryRecordingAudit{}
	checks := &fakeChecksClient{
		updateRespErr: &iacgithub.CheckRunError{
			Kind:    iacgithub.CheckRunErrorKindScopeMissing,
			Status:  403,
			Message: "PAT lacks checks:write scope",
		},
	}
	crStore := &fakeCheckRunStore{
		seeded: map[string]fakeCheckRunStoreCall{
			"rec_abc123": {
				Ref: types.CheckRunRef{
					Owner: "octo", Repo: "widgets", CheckID: 9001, HeadSHA: "abc123",
				},
				Status:     iacgithub.CheckRunStatusInProgress,
				Conclusion: "",
			},
		},
	}
	h := newCheckRunExcludeHandlers(t, store, audit, checks, crStore, "pat-chk-write")

	w := doExcludeRequest(h, sampleExcludeRequest(t, true))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(checks.updateCalls) != 1 {
		t.Fatalf("UpdateCheckRun calls = %d, want 1", len(checks.updateCalls))
	}

	var sawFailed bool
	for _, e := range audit.entries {
		if e.EventType == services.AuditEventIaCCheckRunFailed {
			sawFailed = true
			if got, _ := e.Payload["error_kind"].(string); got != iacgithub.CheckRunErrorKindScopeMissing {
				t.Errorf("error_kind = %q, want scope_missing", got)
			}
		}
		if e.EventType == services.AuditEventIaCCheckRunUpdated {
			t.Errorf("no iac.check_run.updated audit expected on UpdateCheckRun failure; got %s", e.EventType)
		}
	}
	if !sawFailed {
		t.Errorf("expected iac.check_run.failed audit; entries=%+v", audit.entries)
	}

	// Verify the unused import doesn't break compile when the
	// time.Time fields land on the audit entries.
	_ = time.Now()
}
