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
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
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
	return doWebhookRequestWithDelivery(t, h, body, sig, eventType, "")
}

// doWebhookRequestWithDelivery — v0.89.30 (#649) — extension of
// doWebhookRequest that also sets the X-GitHub-Delivery header used
// by the replay-protection path. Empty deliveryID omits the header
// (same shape as the legacy doWebhookRequest); a non-empty value
// flows into the dedupe insert path.
func doWebhookRequestWithDelivery(t *testing.T, h *IaCGitHubWebhookHandler, body []byte, sig string, eventType string, deliveryID string) *httptest.ResponseRecorder {
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
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// newTestWebhookHandlerWithDedupe — v0.89.30 (#649) — builds a
// webhook handler whose dedupe store is the in-memory
// applicationstore.Store. Returned alongside the handler + the iac
// connection store so a test can seed connections (for the audit
// row's connection_id) and inspect the dedupe store directly (for
// the GC test).
func newTestWebhookHandlerWithDedupe(t *testing.T, audit services.AuditService, secret []byte) (*IaCGitHubWebhookHandler, iacconnstore.Store, *memory.Store) {
	t.Helper()
	connStore := iacconnstore.NewMemoryStore()
	dedupeStore := memory.NewStore()
	h := NewIaCGitHubWebhookHandler(audit, connStore, secret, zap.NewNop()).WithDedupeStore(dedupeStore)
	return h, connStore, dedupeStore
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

// --- v0.89.30 (#649) webhook replay protection -----------------------
//
// The five tests below pin the slice-2 replay-protection contract:
// fresh deliveries pass through, repeated delivery_id values return
// 200 + ignored without emitting pr_merged audits, the replayed
// audit row carries the documented payload shape, and the GC sweep
// honors the supplied cutoff. Together they close the
// compromised-TLS-terminator and intermediary-proxy replay threat
// the slice-1 receiver explicitly left on the table.

// TestWebhookReplay_FreshDelivery_FirstTimeAccepted — a signed
// pull_request webhook with a fresh X-GitHub-Delivery UUID lands
// the audit and returns 200 with the normal "audit_event_emitted"
// shape (NOT the replayed-shape body).
func TestWebhookReplay_FreshDelivery_FirstTimeAccepted(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, connStore, _ := newTestWebhookHandlerWithDedupe(t, audit, webhookTestSecret)
	seedConnection(t, connStore, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 101,
		"squadron/rec/eks-observability-addon/abc123",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	deliveryID := "fresh-delivery-uuid-101"

	w := doWebhookRequestWithDelivery(t, h, body, sig, "pull_request", deliveryID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	if audit.entries[0].EventType != services.AuditEventRecommendationPRMerged {
		t.Errorf("event_type = %q, want %q", audit.entries[0].EventType, services.AuditEventRecommendationPRMerged)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// Fresh delivery must NOT be reported as replayed.
	if resp["reason"] == "replayed" {
		t.Errorf("response.reason = replayed on first delivery; should be the audit_event_emitted shape")
	}
	if resp["audit_event_emitted"] != true {
		t.Errorf("response.audit_event_emitted = %v, want true", resp["audit_event_emitted"])
	}
}

// TestWebhookReplay_DuplicateDelivery_Returns200Ignored — POST the
// same signed payload + same X-GitHub-Delivery UUID twice. The
// second POST returns 200 (NOT 4xx/5xx — GitHub redelivery contract)
// with body {ok, ignored, reason: "replayed", delivery_id}.
func TestWebhookReplay_DuplicateDelivery_Returns200Ignored(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, connStore, _ := newTestWebhookHandlerWithDedupe(t, audit, webhookTestSecret)
	seedConnection(t, connStore, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 202,
		"squadron/rec/rds-pi-em/dup-uuid-202",
		"2026-06-22T12:34:56Z", "bob")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	deliveryID := "replayed-delivery-uuid-202"

	// First POST — the legitimate delivery. Lands the audit.
	w1 := doWebhookRequestWithDelivery(t, h, body, sig, "pull_request", deliveryID)
	if w1.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200; body=%s", w1.Code, w1.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("after first POST: audit entries = %d, want 1", len(audit.entries))
	}

	// Second POST — same signed body, same delivery_id. Replay.
	w2 := doWebhookRequestWithDelivery(t, h, body, sig, "pull_request", deliveryID)
	if w2.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200 (GitHub redelivery contract); body=%s",
			w2.Code, w2.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("response.ok = %v, want true", resp["ok"])
	}
	if resp["ignored"] != true {
		t.Errorf("response.ignored = %v, want true", resp["ignored"])
	}
	if resp["reason"] != "replayed" {
		t.Errorf("response.reason = %v, want \"replayed\"", resp["reason"])
	}
	if resp["delivery_id"] != deliveryID {
		t.Errorf("response.delivery_id = %v, want %q", resp["delivery_id"], deliveryID)
	}
}

// TestWebhookReplay_DuplicateDelivery_DoesNotEmitPRMergedAudit —
// the audit row count for the recommendation.pr_merged event MUST
// stay at exactly 1 across the two POSTs. The slice-1 design's
// contract is that an attacker replaying a captured signed delivery
// can't double-emit the pr_merged event.
func TestWebhookReplay_DuplicateDelivery_DoesNotEmitPRMergedAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, connStore, _ := newTestWebhookHandlerWithDedupe(t, audit, webhookTestSecret)
	seedConnection(t, connStore, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 303,
		"squadron/rec/s3-access-logging/xyz303",
		"2026-06-22T12:34:56Z", "carol")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	deliveryID := "no-double-pr-merged-303"

	// First POST — legitimate merge. One pr_merged audit row.
	if w := doWebhookRequestWithDelivery(t, h, body, sig, "pull_request", deliveryID); w.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Second POST — replay. MUST NOT emit a second pr_merged audit row.
	if w := doWebhookRequestWithDelivery(t, h, body, sig, "pull_request", deliveryID); w.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	prMergedCount := 0
	for _, e := range audit.entries {
		if e.EventType == services.AuditEventRecommendationPRMerged {
			prMergedCount++
		}
	}
	if prMergedCount != 1 {
		t.Errorf("recommendation.pr_merged count = %d, want exactly 1 (replay must not double-emit)", prMergedCount)
	}
}

// TestWebhookReplay_DuplicateDelivery_EmitsReplayedAudit — the
// replay path emits exactly ONE webhook.delivery_replayed audit
// event with the documented payload shape (delivery_id, event_type,
// original_received_at). Actor "github_webhook"; TargetType
// AuditTargetIaCRecommendation so the timeline humanizer groups it
// with the pr_opened / pr_merged arc.
func TestWebhookReplay_DuplicateDelivery_EmitsReplayedAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, connStore, _ := newTestWebhookHandlerWithDedupe(t, audit, webhookTestSecret)
	seedConnection(t, connStore, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 404,
		"squadron/rec/lambda-otel-layer/abc404",
		"2026-06-22T12:34:56Z", "dana")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	deliveryID := "replayed-audit-uuid-404"

	// First POST — legitimate delivery; lands pr_merged.
	if w := doWebhookRequestWithDelivery(t, h, body, sig, "pull_request", deliveryID); w.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	// Second POST — replay; lands webhook.delivery_replayed.
	if w := doWebhookRequestWithDelivery(t, h, body, sig, "pull_request", deliveryID); w.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Walk the audit log and collect every replayed row.
	var replayed []services.AuditEntry
	for _, e := range audit.entries {
		if e.EventType == services.AuditEventWebhookDeliveryReplayed {
			replayed = append(replayed, e)
		}
	}
	if len(replayed) != 1 {
		t.Fatalf("webhook.delivery_replayed count = %d, want exactly 1", len(replayed))
	}
	e := replayed[0]
	if e.Actor != "github_webhook" {
		t.Errorf("actor = %q, want %q", e.Actor, "github_webhook")
	}
	if e.TargetType != services.AuditTargetIaCRecommendation {
		t.Errorf("target_type = %q, want %q", e.TargetType, services.AuditTargetIaCRecommendation)
	}
	if e.Action != "delivery_replayed" {
		t.Errorf("action = %q, want %q", e.Action, "delivery_replayed")
	}
	// Documented payload shape: delivery_id, event_type, original_received_at.
	if e.Payload["delivery_id"] != deliveryID {
		t.Errorf("payload.delivery_id = %v, want %q", e.Payload["delivery_id"], deliveryID)
	}
	if e.Payload["event_type"] != "pull_request" {
		t.Errorf("payload.event_type = %v, want %q", e.Payload["event_type"], "pull_request")
	}
	orig, ok := e.Payload["original_received_at"].(string)
	if !ok || orig == "" {
		t.Errorf("payload.original_received_at = %v, want non-empty RFC3339 string", e.Payload["original_received_at"])
	}
	// Round-trip parse so a future change to the format string is
	// caught here rather than silently passing through SIEM consumers.
	if _, err := time.Parse(time.RFC3339, orig); err != nil {
		t.Errorf("payload.original_received_at = %q is not RFC3339: %v", orig, err)
	}
}

// TestWebhookReplay_GCRemovesOldEntries — seeds three dedupe rows
// (10 days, 5 days, now) and verifies that
// GCWebhookDeliveries(now - 7 days) deletes exactly one row (the
// 10-day-old one) and leaves the 5-day and now rows in place. A
// subsequent record call against a brand-new delivery_id should
// return firstTime=true, confirming the table remains healthy.
func TestWebhookReplay_GCRemovesOldEntries(t *testing.T) {
	store := memory.NewStore()
	ctx := context.Background()
	now := time.Now().UTC()

	// Seed three rows by recording fresh, then back-dating the
	// receivedAt on two of them. The memory store doesn't expose
	// back-dating directly, so we reach in via a second record
	// call on the same id — that's a replay that returns the prior
	// receivedAt; instead we run three separate ids, then GC.
	if first, _, err := store.RecordWebhookDelivery(ctx, "old-uuid", "pull_request"); err != nil || !first {
		t.Fatalf("seed old: first=%v err=%v", first, err)
	}
	if first, _, err := store.RecordWebhookDelivery(ctx, "mid-uuid", "pull_request"); err != nil || !first {
		t.Fatalf("seed mid: first=%v err=%v", first, err)
	}
	if first, _, err := store.RecordWebhookDelivery(ctx, "fresh-uuid", "pull_request"); err != nil || !first {
		t.Fatalf("seed fresh: first=%v err=%v", first, err)
	}
	// Back-date the first two rows so the GC cutoff has something
	// to delete. Access the unexported map directly so the test
	// can simulate the time gap without sleeping. v0.89.30 (#649).
	store.SetWebhookDeliveryReceivedAtForTest("old-uuid", now.Add(-10*24*time.Hour))
	store.SetWebhookDeliveryReceivedAtForTest("mid-uuid", now.Add(-5*24*time.Hour))

	cutoff := now.Add(-7 * 24 * time.Hour)
	deleted, err := store.GCWebhookDeliveries(ctx, cutoff)
	if err != nil {
		t.Fatalf("GCWebhookDeliveries: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the 10-day-old row should fall outside the 7-day window)", deleted)
	}

	// The 5-day row must still be present — calling Record with its
	// id returns firstTime=false (replay path).
	if first, _, err := store.RecordWebhookDelivery(ctx, "mid-uuid", "pull_request"); err != nil || first {
		t.Errorf("after GC, mid-uuid: first=%v err=%v; want first=false (row should still exist)", first, err)
	}
	// The now row must still be present.
	if first, _, err := store.RecordWebhookDelivery(ctx, "fresh-uuid", "pull_request"); err != nil || first {
		t.Errorf("after GC, fresh-uuid: first=%v err=%v; want first=false (row should still exist)", first, err)
	}
	// A brand-new delivery_id must come back firstTime=true — the
	// table is healthy and accepting writes after the sweep.
	if first, _, err := store.RecordWebhookDelivery(ctx, "post-gc-uuid", "pull_request"); err != nil || !first {
		t.Errorf("post-GC fresh record: first=%v err=%v; want first=true", first, err)
	}
	// The old row must NOT be present — calling Record with its id
	// should return firstTime=true (clean re-insert).
	if first, _, err := store.RecordWebhookDelivery(ctx, "old-uuid", "pull_request"); err != nil || !first {
		t.Errorf("after GC, old-uuid: first=%v err=%v; want first=true (row should have been deleted)", first, err)
	}
}
