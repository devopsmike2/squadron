// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/services"
)

// webhookTestSecret is the slice-1 deployment-wide HMAC secret used
// by every webhook test. The same bytes seed both the handler and
// the per-test signature-computation helper so the round-trip
// matches GitHub's view bit-for-bit.
var webhookTestSecret = []byte("test-webhook-secret-do-not-use-in-production")

// signGitHubWebhook computes the X-Hub-Signature-256 header value for
// a request body using the test secret. Returns the full header
// value including the "sha256=" prefix GitHub prepends.
func signGitHubWebhook(t *testing.T, body []byte, secret []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// newTestWebhookHandler builds an IaCGitHubWebhookHandler wired with
// an in-memory store, the discoveryRecordingAudit, the test secret,
// and a no-op logger. The store is returned so individual tests can
// pre-populate it with a connection before the request goes through.
func newTestWebhookHandler(t *testing.T, audit services.AuditService, secret []byte) (*IaCGitHubWebhookHandler, iacconnstore.Store) {
	t.Helper()
	store := iacconnstore.NewMemoryStore()
	h := NewIaCGitHubWebhookHandler(audit, store, secret, zap.NewNop())
	return h, store
}

// seedConnection inserts a connection for repoFullName into store
// and returns the stamped ConnectionID. Used by happy-path tests
// that need to assert the audit row's TargetID == the connection's
// ID.
func seedConnection(t *testing.T, store iacconnstore.Store, repoFullName string) string {
	t.Helper()
	conn := &iacconnstore.IaCConnection{
		Provider:       iacconnstore.ProviderGitHub,
		AuthKind:       iacconnstore.AuthKindPAT,
		RepoFullName:   repoFullName,
		DefaultBranch:  "main",
		RepoLayout:     iacconnstore.RepoLayoutMono,
		CredCiphertext: []byte("opaque-test-blob"),
	}
	if err := store.Create(context.Background(), conn); err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	return conn.ConnectionID
}

// doWebhookRequest fires a POST against /api/v1/webhooks/github with
// the supplied body, signature header, and event type. Returns the
// recorder so tests can assert status + body shape.
func doWebhookRequest(t *testing.T, h *IaCGitHubWebhookHandler, body []byte, sig string, eventType string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/webhooks/github", h.HandleWebhook)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	if eventType != "" {
		req.Header.Set("X-GitHub-Event", eventType)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// makePREventBody builds a minimal pull_request webhook JSON payload
// for the supplied fields. Tests vary the inputs to exercise each
// branch of the merge-detection logic; the helper keeps the per-test
// JSON noise out of the test bodies.
func makePREventBody(t *testing.T, action string, merged bool, repo string, prNumber int, branch, mergedAt, mergedByLogin string) []byte {
	t.Helper()
	type mergedByT struct {
		Login string `json:"login"`
	}
	type prT struct {
		Number   int        `json:"number"`
		Merged   bool       `json:"merged"`
		MergedAt string     `json:"merged_at"`
		HTMLURL  string     `json:"html_url"`
		Head     struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		MergedBy *mergedByT `json:"merged_by"`
	}
	type ev struct {
		Action      string `json:"action"`
		PullRequest prT    `json:"pull_request"`
	}
	pr := prT{Number: prNumber, Merged: merged, MergedAt: mergedAt, HTMLURL: "https://github.com/" + repo + "/pull/0"}
	pr.Head.Ref = branch
	pr.Base.Repo.FullName = repo
	if mergedByLogin != "" {
		pr.MergedBy = &mergedByT{Login: mergedByLogin}
	}
	body, err := json.Marshal(ev{Action: action, PullRequest: pr})
	if err != nil {
		t.Fatalf("marshal pr event: %v", err)
	}
	return body
}

// --- tests ---------------------------------------------------------

func TestGitHubWebhook_SignatureValid_PRMerged_EmitsAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/eks-observability-addon/abc123",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventRecommendationPRMerged {
		t.Errorf("event_type = %q, want %q", e.EventType, services.AuditEventRecommendationPRMerged)
	}
	if e.TargetType != services.AuditTargetIaCRecommendation {
		t.Errorf("target_type = %q, want %q", e.TargetType, services.AuditTargetIaCRecommendation)
	}
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	if e.Actor != "github_webhook" {
		t.Errorf("actor = %q, want %q", e.Actor, "github_webhook")
	}
	if e.Action != "pr_merged" {
		t.Errorf("action = %q, want %q", e.Action, "pr_merged")
	}
	// Payload assertions: each documented field must be present
	// and carry the value the GitHub event named.
	pay := e.Payload
	if pay["repo_full_name"] != "octo/widgets" {
		t.Errorf("payload.repo_full_name = %v, want %q", pay["repo_full_name"], "octo/widgets")
	}
	if pay["pr_number"] != 42 {
		t.Errorf("payload.pr_number = %v, want 42", pay["pr_number"])
	}
	if pay["branch"] != "squadron/rec/eks-observability-addon/abc123" {
		t.Errorf("payload.branch = %v, unexpected", pay["branch"])
	}
	if pay["merged_at"] != "2026-06-22T12:34:56Z" {
		t.Errorf("payload.merged_at = %v, unexpected", pay["merged_at"])
	}
	if pay["merged_by"] != "alice" {
		t.Errorf("payload.merged_by = %v, want %q", pay["merged_by"], "alice")
	}
	if pay["recommendation_kind"] != "eks-observability-addon" {
		t.Errorf("payload.recommendation_kind = %v, want eks-observability-addon", pay["recommendation_kind"])
	}
	if pay["connection_id"] != connectionID {
		t.Errorf("payload.connection_id = %v, want %q", pay["connection_id"], connectionID)
	}
}

func TestGitHubWebhook_SignatureValid_PRClosedWithoutMerge_NoAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, _ := newTestWebhookHandler(t, audit, webhookTestSecret)
	// action=closed, merged=false → operator closed without merging.
	body := makePREventBody(t, "closed", false, "octo/widgets", 7,
		"squadron/rec/lambda-otel-layer/xyz", "", "")
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 0 {
		t.Fatalf("audit entries = %d, want 0", len(audit.entries))
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["ignored"] != true {
		t.Errorf("response.ignored = %v, want true", resp["ignored"])
	}
	if resp["reason"] != "pr_closed_not_merged" {
		t.Errorf("response.reason = %v, want pr_closed_not_merged", resp["reason"])
	}
}

func TestGitHubWebhook_SignatureValid_PROpened_NoAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, _ := newTestWebhookHandler(t, audit, webhookTestSecret)
	body := makePREventBody(t, "opened", false, "octo/widgets", 99,
		"squadron/rec/rds-pi-em/abc", "", "")
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 0 {
		t.Fatalf("audit entries = %d, want 0", len(audit.entries))
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["reason"] != "pr_action_not_closed" {
		t.Errorf("response.reason = %v, want pr_action_not_closed", resp["reason"])
	}
}

func TestGitHubWebhook_SignatureInvalid_Returns401(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, _ := newTestWebhookHandler(t, audit, webhookTestSecret)
	body := makePREventBody(t, "closed", true, "octo/widgets", 1,
		"squadron/rec/eks-cluster-logging/aaa", "2026-06-22T12:34:56Z", "bob")
	// Sign with a different secret so the HMAC compare fails.
	wrongSig := signGitHubWebhook(t, body, []byte("a-different-secret"))

	w := doWebhookRequest(t, h, body, wrongSig, "pull_request")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 0 {
		t.Errorf("audit entries = %d, want 0 (signature should reject before audit)", len(audit.entries))
	}
}

func TestGitHubWebhook_SignatureValid_WrongEventType_NoAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, _ := newTestWebhookHandler(t, audit, webhookTestSecret)
	// Any well-formed JSON body works — the wrong event_type short-
	// circuits BEFORE the body is unmarshaled.
	body := []byte(`{"zen":"Anything added dilutes everything else."}`)
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "ping")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 0 {
		t.Fatalf("audit entries = %d, want 0", len(audit.entries))
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["ignored"] != true {
		t.Errorf("response.ignored = %v, want true", resp["ignored"])
	}
	if resp["event"] != "ping" {
		t.Errorf("response.event = %v, want ping", resp["event"])
	}
}

func TestGitHubWebhook_SignatureValid_NoMatchingConnection_StillEmitsAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	// Note: store is empty — no connection seeded.
	h, _ := newTestWebhookHandler(t, audit, webhookTestSecret)
	body := makePREventBody(t, "closed", true, "stranger/repo", 5,
		"squadron/rec/s3-access-logging/zzz", "2026-06-22T00:00:00Z", "charlie")
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 (audit must fire even without a matching connection)", len(audit.entries))
	}
	e := audit.entries[0]
	if e.TargetID != "" {
		t.Errorf("target_id = %q, want empty (no connection matched)", e.TargetID)
	}
	if e.Payload["connection_id"] != "" {
		t.Errorf("payload.connection_id = %v, want empty", e.Payload["connection_id"])
	}
	if e.Payload["repo_full_name"] != "stranger/repo" {
		t.Errorf("payload.repo_full_name = %v, want stranger/repo", e.Payload["repo_full_name"])
	}
}

func TestGitHubWebhook_SecretNotConfigured_Returns503(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	// Empty secret → handler should 503 every request.
	h, _ := newTestWebhookHandler(t, audit, nil)
	body := makePREventBody(t, "closed", true, "octo/widgets", 1,
		"squadron/rec/alb-access-logs/aaa", "2026-06-22T00:00:00Z", "dave")
	// Even a "valid" signature should NOT recover the response when
	// the deployment didn't configure a secret — the handler short-
	// circuits BEFORE HMAC check so it can name the missing env var.
	sig := signGitHubWebhook(t, body, []byte("whatever"))

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 0 {
		t.Errorf("audit entries = %d, want 0", len(audit.entries))
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// The detail must name the env var explicitly so the operator
	// reading the GitHub webhook delivery log knows which knob to
	// turn.
	detail, _ := resp["detail"].(string)
	if detail == "" || !bytes.Contains([]byte(detail), []byte("SQUADRON_GITHUB_WEBHOOK_SECRET")) {
		t.Errorf("response.detail = %q, want it to name SQUADRON_GITHUB_WEBHOOK_SECRET", detail)
	}
}

func TestGitHubWebhook_BranchPrefixParse(t *testing.T) {
	cases := []struct {
		name    string
		branch  string
		prefix  string
		wantK   string
		wantOK  bool
	}{
		{
			name:   "squadron-shaped branch with kind segment",
			branch: "squadron/rec/eks-observability-addon/abc123",
			prefix: "squadron/rec/",
			wantK:  "eks-observability-addon",
			wantOK: true,
		},
		{
			name:   "non-squadron branch",
			branch: "feature/something",
			prefix: "squadron/rec/",
			wantK:  "",
			wantOK: false,
		},
		{
			name:   "empty post-prefix segment is not a valid kind",
			branch: "squadron/rec/",
			prefix: "squadron/rec/",
			wantK:  "",
			wantOK: false,
		},
		{
			name:   "empty branch",
			branch: "",
			prefix: "squadron/rec/",
			wantK:  "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotK, gotOK := parseRecommendationKindFromBranch(tc.branch, tc.prefix)
			if gotK != tc.wantK || gotOK != tc.wantOK {
				t.Errorf("parseRecommendationKindFromBranch(%q, %q) = (%q, %v), want (%q, %v)",
					tc.branch, tc.prefix, gotK, gotOK, tc.wantK, tc.wantOK)
			}
		})
	}
}

// TestGitHubWebhook_BranchScopeParse — v0.89.28 (#643 slice 1).
// Pins both backward-compat shapes (pre-extension 4-segment branches
// that carry kind only) and the new 6-segment encoding that also
// carries account_id + region. The discovery proposer's accepted-
// examples lookup is scoped by (connection_id, account_id, region);
// the only way that scope round-trips from PR open through PR merge
// is via this parse so the audit row carries the right fields.
func TestGitHubWebhook_BranchScopeParse(t *testing.T) {
	cases := []struct {
		name       string
		branch     string
		prefix     string
		wantKind   string
		wantAcct   string
		wantRegion string
		wantOK     bool
	}{
		{
			name:       "new 6-segment shape with account and region",
			branch:     "squadron/rec/rds-pi-em/123456789012/us-east-1/abc1234-0",
			prefix:     "squadron/rec/",
			wantKind:   "rds-pi-em",
			wantAcct:   "123456789012",
			wantRegion: "us-east-1",
			wantOK:     true,
		},
		{
			name:       "old 4-segment shape preserves kind, empty scope",
			branch:     "squadron/rec/eks-observability-addon/abc123",
			prefix:     "squadron/rec/",
			wantKind:   "eks-observability-addon",
			wantAcct:   "",
			wantRegion: "",
			wantOK:     true,
		},
		{
			name:       "non-squadron branch",
			branch:     "feature/something",
			prefix:     "squadron/rec/",
			wantKind:   "",
			wantAcct:   "",
			wantRegion: "",
			wantOK:     false,
		},
		{
			name:       "empty post-prefix returns ok=false",
			branch:     "squadron/rec/",
			prefix:     "squadron/rec/",
			wantKind:   "",
			wantAcct:   "",
			wantRegion: "",
			wantOK:     false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotK, gotA, gotR, gotOK := parseRecommendationScopeFromBranch(tc.branch, tc.prefix)
			if gotK != tc.wantKind || gotA != tc.wantAcct || gotR != tc.wantRegion || gotOK != tc.wantOK {
				t.Errorf("parseRecommendationScopeFromBranch(%q, %q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					tc.branch, tc.prefix, gotK, gotA, gotR, gotOK,
					tc.wantKind, tc.wantAcct, tc.wantRegion, tc.wantOK)
			}
		})
	}
}
