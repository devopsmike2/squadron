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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
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
//
// When merged=true the supplied timestamp is set as `merged_at` and
// the login flows into `merged_by`. When merged=false (close-without-
// merge, v0.89.36 audit-emit path) the timestamp is set as
// `closed_at` and the login flows into the top-level `sender.login`
// + `pull_request.user.login` so the handler's closed_by fallback
// chain has data to resolve.
func makePREventBody(t *testing.T, action string, merged bool, repo string, prNumber int, branch, ts, login string) []byte {
	t.Helper()
	type loginT struct {
		Login string `json:"login"`
	}
	type prT struct {
		Number   int     `json:"number"`
		Merged   bool    `json:"merged"`
		MergedAt string  `json:"merged_at,omitempty"`
		ClosedAt string  `json:"closed_at,omitempty"`
		HTMLURL  string  `json:"html_url"`
		Head     struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		MergedBy *loginT `json:"merged_by,omitempty"`
		User     *loginT `json:"user,omitempty"`
	}
	type ev struct {
		Action      string  `json:"action"`
		PullRequest prT     `json:"pull_request"`
		Sender      *loginT `json:"sender,omitempty"`
	}
	pr := prT{Number: prNumber, Merged: merged, HTMLURL: "https://github.com/" + repo + "/pull/0"}
	pr.Head.Ref = branch
	pr.Base.Repo.FullName = repo
	body := ev{Action: action, PullRequest: pr}
	if merged {
		body.PullRequest.MergedAt = ts
		if login != "" {
			body.PullRequest.MergedBy = &loginT{Login: login}
		}
	} else if action == "closed" {
		body.PullRequest.ClosedAt = ts
		if login != "" {
			body.PullRequest.User = &loginT{Login: login}
			body.Sender = &loginT{Login: login}
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal pr event: %v", err)
	}
	return raw
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

// TestWebhook_PRClosedNotMerged_EmitsAudit — v0.89.36 (#655 Stream 53,
// #531 slice 2 chunk 3). Replaces the v0.89.28
// TestGitHubWebhook_SignatureValid_PRClosedWithoutMerge_NoAudit which
// pinned the prior "no audit on close-without-merge" contract. Slice
// 2 chunk 3 promotes this case to a proper audit emit so the
// discovery proposer's verdict pool gets the negative signal. The
// response body still carries ignored=true + reason=pr_closed_not_merged
// for the GitHub redelivery contract.
func TestWebhook_PRClosedNotMerged_EmitsAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")
	// action=closed, merged=false → operator closed without merging.
	body := makePREventBody(t, "closed", false, "octo/widgets", 7,
		"squadron/rec/lambda-otel-layer/123456789012/us-east-1/xyz",
		"2026-06-22T01:02:03Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)

	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.EventType != services.AuditEventRecommendationPRClosedNotMerged {
		t.Errorf("event_type = %q, want %q", e.EventType, services.AuditEventRecommendationPRClosedNotMerged)
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
	if e.Action != "pr_closed_not_merged" {
		t.Errorf("action = %q, want %q", e.Action, "pr_closed_not_merged")
	}
	pay := e.Payload
	if pay["repo_full_name"] != "octo/widgets" {
		t.Errorf("payload.repo_full_name = %v, want octo/widgets", pay["repo_full_name"])
	}
	if pay["pr_number"] != 7 {
		t.Errorf("payload.pr_number = %v, want 7", pay["pr_number"])
	}
	if pay["closed_at"] != "2026-06-22T01:02:03Z" {
		t.Errorf("payload.closed_at = %v, unexpected", pay["closed_at"])
	}
	if pay["closed_by"] != "alice" {
		t.Errorf("payload.closed_by = %v, want alice", pay["closed_by"])
	}
	if pay["recommendation_kind"] != "lambda-otel-layer" {
		t.Errorf("payload.recommendation_kind = %v, want lambda-otel-layer", pay["recommendation_kind"])
	}
	if pay["account_id"] != "123456789012" {
		t.Errorf("payload.account_id = %v, want 123456789012", pay["account_id"])
	}
	if pay["region"] != "us-east-1" {
		t.Errorf("payload.region = %v, want us-east-1", pay["region"])
	}
	if pay["connection_id"] != connectionID {
		t.Errorf("payload.connection_id = %v, want %q", pay["connection_id"], connectionID)
	}
	// The merged-side keys MUST NOT appear on the closed_not_merged
	// payload (and vice versa) — keeps SIEM consumers from
	// conflating the two states.
	if _, ok := pay["merged_at"]; ok {
		t.Errorf("payload.merged_at should be absent on pr_closed_not_merged event; got %v", pay["merged_at"])
	}
	if _, ok := pay["merged_by"]; ok {
		t.Errorf("payload.merged_by should be absent on pr_closed_not_merged event; got %v", pay["merged_by"])
	}
	// Response body still uses the ignored/reason shape for GitHub.
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

// --- v0.89.31 (#650) per-connection webhook secrets ------------------
//
// The five tests below pin the slice-2 follow-on to v0.89.23 slice 2:
// per-connection HMAC secrets stored sealed alongside the existing
// PAT in iac_connections, looked up by repo_full_name on the inbound
// delivery, and used to verify X-Hub-Signature-256 in preference to
// the env-var global. Backward compatibility for connections without
// a per-connection secret is exercised explicitly.
//
// All five tests use the credstore.Key wired into the webhook handler
// to unseal stored secrets at HMAC-verify time. The PATCH handler is
// exercised in test #1 + #4 to prove the end-to-end seal-on-write
// path; tests #2 / #3 / #5 seal directly to keep the assertion focus
// on the verification side.

// newTestWebhookHandlerWithCredKey — v0.89.31 (#650) — builds a
// webhook handler wired with a credstore Key (so per-connection
// secrets can be unsealed at verify-time). The Key is the same
// fixed test key the iac_github_test.go helpers use, so a secret
// PATCH'd through the API handler can be unsealed by the webhook
// handler in the same test process.
func newTestWebhookHandlerWithCredKey(t *testing.T, audit services.AuditService, secret []byte) (*IaCGitHubWebhookHandler, iacconnstore.Store, *credstore.Key) {
	t.Helper()
	store := iacconnstore.NewMemoryStore()
	key := newTestCredKey(t)
	h := NewIaCGitHubWebhookHandler(audit, store, secret, zap.NewNop()).WithCredstoreKey(key)
	return h, store, key
}

// sealAndStorePerConnSecret seals a plaintext webhook secret with
// the supplied credstore Key and writes it into the store under
// connectionID. Used by the verification-focused tests below so the
// assertion stays on "the right secret was chosen" rather than the
// PATCH-handler round-trip.
func sealAndStorePerConnSecret(t *testing.T, store iacconnstore.Store, key *credstore.Key, connectionID, plaintext string) {
	t.Helper()
	sealed, err := credstore.SealWebhookSecret(key, []byte(plaintext))
	if err != nil {
		t.Fatalf("SealWebhookSecret: %v", err)
	}
	if err := store.SetWebhookSecret(context.Background(), connectionID, sealed); err != nil {
		t.Fatalf("SetWebhookSecret: %v", err)
	}
}

// TestPerConnectionWebhookSecret_StoredAndUsedForVerification —
// v0.89.31 (#650) — the per-connection secret takes priority over
// the env-var global. PATCH the secret onto the connection through
// the production HandleIaCGitHubUpdateConnection path (proving the
// seal-on-write round-trip), then POST a pull_request signed with
// the per-connection key — assert 200 + recommendation.pr_merged
// audit. POST the same payload signed with the env-var global —
// assert 401 (the per-connection secret took priority).
func TestPerConnectionWebhookSecret_StoredAndUsedForVerification(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, key := newTestWebhookHandlerWithCredKey(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "acme/infra")

	// Drive the PATCH through the production handler so the
	// seal-on-write path is on the test trail. The PATCH handler
	// does not consult the GitHub client, so the factory is
	// intentionally unwired.
	iacH := NewIaCGitHubHandlers(store, zap.NewNop()).
		WithCredstoreKey(key)
	register := func(r *gin.Engine) {
		r.PATCH("/api/v1/iac/github/connections/:id", iacH.HandleIaCGitHubUpdateConnection)
	}
	patchBody := `{"webhook_secret":"per-connection-key-123"}`
	patchW := doIaCRequest(t, http.MethodPatch,
		"/api/v1/iac/github/connections/"+connectionID, register, patchBody)
	if patchW.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body=%s", patchW.Code, patchW.Body.String())
	}
	// Sanity: the sealed blob actually landed on the row.
	sealed, err := store.GetWebhookSecret(context.Background(), connectionID)
	if err != nil {
		t.Fatalf("GetWebhookSecret after PATCH: %v", err)
	}
	if len(sealed) == 0 {
		t.Fatalf("GetWebhookSecret returned empty after PATCH; the seal-on-write path did not persist")
	}

	body := makePREventBody(t, "closed", true, "acme/infra", 7,
		"squadron/rec/eks-observability-addon/abc123",
		"2026-06-22T12:34:56Z", "alice")

	// Signed with the per-connection key: must succeed.
	perConnSig := signGitHubWebhook(t, body, []byte("per-connection-key-123"))
	w := doWebhookRequest(t, h, body, perConnSig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("per-connection-signed status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries after per-conn POST = %d, want 1", len(audit.entries))
	}
	if audit.entries[0].EventType != services.AuditEventRecommendationPRMerged {
		t.Errorf("audit event_type = %q, want %q", audit.entries[0].EventType, services.AuditEventRecommendationPRMerged)
	}
	if audit.entries[0].TargetID != connectionID {
		t.Errorf("audit target_id = %q, want %q", audit.entries[0].TargetID, connectionID)
	}

	// Signed with the env-var global: must FAIL with 401, proving
	// the per-connection secret took priority (otherwise the global
	// would still verify and we'd see another 200).
	globalSig := signGitHubWebhook(t, body, webhookTestSecret)
	w2 := doWebhookRequest(t, h, body, globalSig, "pull_request")
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("global-signed status = %d, want 401 (per-connection secret should reject global sig); body=%s",
			w2.Code, w2.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Errorf("audit entries after global POST = %d, want still 1 (global sig must not pass)", len(audit.entries))
	}
}

// TestPerConnectionWebhookSecret_DifferentSecretForDifferentConnection —
// v0.89.31 (#650) — two connections with different per-connection
// secrets must each verify against their own secret and reject the
// other. Proves the lookup-by-repo path picks the right row.
func TestPerConnectionWebhookSecret_DifferentSecretForDifferentConnection(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, key := newTestWebhookHandlerWithCredKey(t, audit, webhookTestSecret)
	connA := seedConnection(t, store, "acme/infra")
	connB := seedConnection(t, store, "acme/platform")
	sealAndStorePerConnSecret(t, store, key, connA, "key-A")
	sealAndStorePerConnSecret(t, store, key, connB, "key-B")

	// acme/infra payload, signed with key-A → 200.
	bodyA := makePREventBody(t, "closed", true, "acme/infra", 1,
		"squadron/rec/eks-cluster-logging/aaaA", "2026-06-22T00:00:00Z", "alice")
	sigA := signGitHubWebhook(t, bodyA, []byte("key-A"))
	wA := doWebhookRequest(t, h, bodyA, sigA, "pull_request")
	if wA.Code != http.StatusOK {
		t.Fatalf("acme/infra signed with key-A: status = %d, want 200; body=%s", wA.Code, wA.Body.String())
	}
	preCount := len(audit.entries)
	if preCount != 1 {
		t.Fatalf("audit entries after key-A POST = %d, want 1", preCount)
	}
	if audit.entries[0].TargetID != connA {
		t.Errorf("audit target_id = %q, want %q (connA)", audit.entries[0].TargetID, connA)
	}

	// acme/infra payload, signed with key-B → 401 (wrong secret).
	sigBOnA := signGitHubWebhook(t, bodyA, []byte("key-B"))
	wBOnA := doWebhookRequest(t, h, bodyA, sigBOnA, "pull_request")
	if wBOnA.Code != http.StatusUnauthorized {
		t.Fatalf("acme/infra signed with key-B: status = %d, want 401; body=%s",
			wBOnA.Code, wBOnA.Body.String())
	}
	if len(audit.entries) != preCount {
		t.Errorf("audit entries grew on mismatched sig: was %d, now %d", preCount, len(audit.entries))
	}

	// acme/platform payload, signed with key-B → 200.
	bodyB := makePREventBody(t, "closed", true, "acme/platform", 2,
		"squadron/rec/s3-access-logging/bbbB", "2026-06-22T00:00:00Z", "bob")
	sigB := signGitHubWebhook(t, bodyB, []byte("key-B"))
	wB := doWebhookRequest(t, h, bodyB, sigB, "pull_request")
	if wB.Code != http.StatusOK {
		t.Fatalf("acme/platform signed with key-B: status = %d, want 200; body=%s",
			wB.Code, wB.Body.String())
	}
	if len(audit.entries) != preCount+1 {
		t.Fatalf("audit entries after key-B POST = %d, want %d", len(audit.entries), preCount+1)
	}
	if audit.entries[preCount].TargetID != connB {
		t.Errorf("audit target_id = %q, want %q (connB)", audit.entries[preCount].TargetID, connB)
	}
}

// TestPerConnectionWebhookSecret_FallsBackToGlobalWhenUnset —
// v0.89.31 (#650) — a connection that does not have a per-connection
// secret continues to verify against the env-var global. Backward
// compatibility for pre-v0.89.31 deployments. The seed connection
// has NO secret stored; the POST is signed with the env-var global.
func TestPerConnectionWebhookSecret_FallsBackToGlobalWhenUnset(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, _ := newTestWebhookHandlerWithCredKey(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "acme/infra")
	// Sanity: no per-connection secret on this connection.
	sealed, err := store.GetWebhookSecret(context.Background(), connectionID)
	if err != nil {
		t.Fatalf("GetWebhookSecret: %v", err)
	}
	if len(sealed) != 0 {
		t.Fatalf("connection seeded with no secret, but GetWebhookSecret returned %d bytes", len(sealed))
	}

	body := makePREventBody(t, "closed", true, "acme/infra", 9,
		"squadron/rec/rds-pi-em/zzz", "2026-06-22T00:00:00Z", "carol")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (env-var global must verify); body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	if audit.entries[0].TargetID != connectionID {
		t.Errorf("audit target_id = %q, want %q", audit.entries[0].TargetID, connectionID)
	}
}

// TestPerConnectionWebhookSecret_NeverRevealedInResponse —
// v0.89.31 (#650) — after PATCHing a per-connection secret, the
// list connections endpoint MUST NOT echo the plaintext or the
// sealed bytes in its response. The `json:"-"` tag on
// IaCConnection.WebhookSecret combined with the redacted
// iacGitHubConnectionRow projection is what enforces this; the test
// is the wire-level guard against a future refactor leaking it.
func TestPerConnectionWebhookSecret_NeverRevealedInResponse(t *testing.T) {
	store := iacconnstore.NewMemoryStore()
	key := newTestCredKey(t)
	iacH := NewIaCGitHubHandlers(store, zap.NewNop()).
		WithCredstoreKey(key)
	connectionID := seedConnection(t, store, "acme/infra")

	const plaintext = "supersecret"
	patchBody := `{"webhook_secret":"` + plaintext + `"}`
	patchRegister := func(r *gin.Engine) {
		r.PATCH("/api/v1/iac/github/connections/:id", iacH.HandleIaCGitHubUpdateConnection)
	}
	patchW := doIaCRequest(t, http.MethodPatch,
		"/api/v1/iac/github/connections/"+connectionID, patchRegister, patchBody)
	if patchW.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body=%s", patchW.Code, patchW.Body.String())
	}
	// PATCH response must not echo the plaintext either.
	if strings.Contains(patchW.Body.String(), plaintext) {
		t.Errorf("PATCH response echoed plaintext: %s", patchW.Body.String())
	}

	// Now GET the list — confirm neither the plaintext nor the
	// sealed bytes appear in the wire shape.
	listRegister := func(r *gin.Engine) {
		r.GET("/api/v1/iac/github/connections", iacH.HandleListIaCGitHubConnections)
	}
	listW := doIaCRequest(t, http.MethodGet, "/api/v1/iac/github/connections", listRegister, "")
	if listW.Code != http.StatusOK {
		t.Fatalf("LIST status = %d, want 200; body=%s", listW.Code, listW.Body.String())
	}
	listBody := listW.Body.String()
	if strings.Contains(listBody, plaintext) {
		t.Errorf("LIST response leaked plaintext %q in body: %s", plaintext, listBody)
	}
	// The sealed bytes are AES-GCM ciphertext + nonce — they
	// shouldn't be JSON-encodable in a way that surfaces them
	// cleanly, but if a future refactor JSON-encoded the byte
	// slice it would land as a base64 string. The wire surface
	// must not include the field name either.
	if strings.Contains(listBody, "webhook_secret") {
		t.Errorf("LIST response contains the field name `webhook_secret`: %s", listBody)
	}
	if strings.Contains(listBody, "webhook_secret_sealed") {
		t.Errorf("LIST response contains the field name `webhook_secret_sealed`: %s", listBody)
	}
	// Sanity: the other connection fields ARE present — proves the
	// row is being rendered, just without the secret.
	if !strings.Contains(listBody, connectionID) {
		t.Errorf("LIST response missing connection_id %q: %s", connectionID, listBody)
	}
	if !strings.Contains(listBody, "acme/infra") {
		t.Errorf("LIST response missing repo_full_name acme/infra: %s", listBody)
	}
}

// --- v0.89.44 (#664 Stream 62, slice 1 chunk 3) Checks API webhook follow-up ---
//
// The five tests below pin the chunk-3 contract: on inbound
// recommendation.pr_merged / recommendation.pr_closed_not_merged
// audit emits, the receiver looks up the chunk-2
// iac.check_run.created audit pivot, PATCHes the live check run on
// GitHub via UpdateCheckRun, persists the new state via
// SetCheckRunForRecommendation, and emits iac.check_run.updated.
// Fail-open is exercised: missing wire (nil client / nil store /
// empty PAT) silently no-ops; absent pivot row silently no-ops; a
// CheckRunError surfaces as iac.check_run.failed without breaking
// the original pr_merged emit.

// fakeWebhookChecksClient is the test-side WebhookChecksAPI
// implementation. Per-test canned response (err) + a recorded
// request slice so tests can assert the wire shape that reached the
// wrapper.
type fakeWebhookChecksClient struct {
	mu       sync.Mutex
	updates  []iacgithub.CheckRunUpdate
	updPATs  []string
	respErr  error
}

func (f *fakeWebhookChecksClient) UpdateCheckRun(_ context.Context, pat string, req iacgithub.CheckRunUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, req)
	f.updPATs = append(f.updPATs, pat)
	return f.respErr
}

// fakeWebhookCheckRunStore is the test-side WebhookCheckRunStore.
// Records every Get / Set call. Pre-seed via SeedGet to pin the
// canned response for the lookup half of the chunk-3 dance.
type fakeWebhookCheckRunStore struct {
	mu sync.Mutex

	// seededGet: by recommendation_id → (ref, status, conclusion,
	// exists, err). When unset, Get returns (zero, "", "", false, nil).
	seededGet map[string]fakeWebhookCheckRunGetResult

	getCalls []string
	setCalls []fakeWebhookCheckRunSet
	setErr   error
}

type fakeWebhookCheckRunGetResult struct {
	Ref        types.CheckRunRef
	Status     string
	Conclusion string
	Exists     bool
	Err        error
}

type fakeWebhookCheckRunSet struct {
	Rec        types.ExcludedRecommendation
	Ref        types.CheckRunRef
	Status     string
	Conclusion string
}

func (s *fakeWebhookCheckRunStore) GetCheckRunForRecommendation(_ context.Context, recommendationID string) (types.CheckRunRef, string, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls = append(s.getCalls, recommendationID)
	if r, ok := s.seededGet[recommendationID]; ok {
		return r.Ref, r.Status, r.Conclusion, r.Exists, r.Err
	}
	return types.CheckRunRef{}, "", "", false, nil
}

func (s *fakeWebhookCheckRunStore) SetCheckRunForRecommendation(
	_ context.Context,
	rec types.ExcludedRecommendation,
	ref types.CheckRunRef,
	status, conclusion string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCalls = append(s.setCalls, fakeWebhookCheckRunSet{
		Rec: rec, Ref: ref, Status: status, Conclusion: conclusion,
	})
	return s.setErr
}

// seedCheckRunCreatedAuditPivot pre-records the chunk-2
// iac.check_run.created audit row the chunk-3 helper pivots on.
// Calling this BEFORE doWebhookRequest makes the pivot resolve and
// the chunk-3 dance fires; omitting it makes the helper silently
// no-op (no pivot found path).
func seedCheckRunCreatedAuditPivot(
	t *testing.T,
	audit *discoveryRecordingAudit,
	connectionID, recommendationID, prURL string,
) {
	t.Helper()
	if err := audit.Record(context.Background(), services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventIaCCheckRunCreated,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   connectionID,
		Action:     "check_run_created",
		Payload: map[string]any{
			"connection_id":     connectionID,
			"recommendation_id": recommendationID,
			"pr_url":            prURL,
			"check_run_id":      int64(7777),
		},
	}); err != nil {
		t.Fatalf("seed chunk-2 audit pivot: %v", err)
	}
}

// newTestWebhookHandlerWithChecksAPI builds an IaCGitHubWebhookHandler
// with the chunk-3 surfaces wired: ChecksAPI + CheckRunStore + PAT +
// SquadronHost. The dedupe / cred-key surfaces stay unwired (the
// chunk-3 tests don't exercise them). Returns the handler, the iac
// connection store (so tests can seed connections), the fake checks
// client (so tests can poke respErr or read updates), and the fake
// store (so tests can seed Get results or assert Set calls).
func newTestWebhookHandlerWithChecksAPI(
	t *testing.T,
	audit services.AuditService,
	secret []byte,
) (*IaCGitHubWebhookHandler, iacconnstore.Store, *fakeWebhookChecksClient, *fakeWebhookCheckRunStore) {
	t.Helper()
	connStore := iacconnstore.NewMemoryStore()
	checks := &fakeWebhookChecksClient{}
	crStore := &fakeWebhookCheckRunStore{}
	h := NewIaCGitHubWebhookHandler(audit, connStore, secret, zap.NewNop()).
		WithChecksAPI(checks).
		WithCheckRunStore(crStore).
		WithPAT("pat-checks-write-webhook").
		WithSquadronHost("https://squadron.acme.example")
	return h, connStore, checks, crStore
}

// TestWebhook_PRMerged_UpdatesCheckRunToSuccess — happy path on the
// merge branch. Seed the chunk-2 audit pivot + the stored check run.
// Fire a pull_request webhook with merged=true. Assert: (a)
// recommendation.pr_merged audit fires (existing), (b)
// iac.check_run.updated audit fires with new_conclusion=success and
// previous_conclusion=in_progress (the seeded value), (c)
// UpdateCheckRun was called with the right wire shape, and (d)
// SetCheckRunForRecommendation persisted status=completed +
// conclusion=success.
func TestWebhook_PRMerged_UpdatesCheckRunToSuccess(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, checks, crStore := newTestWebhookHandlerWithChecksAPI(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	const recID = "rec-merged-1"
	const prURL = "https://github.com/octo/widgets/pull/0"
	seedCheckRunCreatedAuditPivot(t, audit, connectionID, recID, prURL)
	crStore.seededGet = map[string]fakeWebhookCheckRunGetResult{
		recID: {
			Ref:        types.CheckRunRef{Owner: "octo", Repo: "widgets", CheckID: 12345, HeadSHA: "abc123"},
			Status:     iacgithub.CheckRunStatusInProgress,
			Conclusion: "",
			Exists:     true,
		},
	}

	body := makePREventBody(t, "closed", true, "octo/widgets", 0,
		"squadron/rec/rds-pi-em/111111111111/us-east-1/abc1234-0",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// UpdateCheckRun: called once with the seeded ref + new
	// status=completed + conclusion=success.
	if len(checks.updates) != 1 {
		t.Fatalf("UpdateCheckRun calls = %d, want 1", len(checks.updates))
	}
	got := checks.updates[0]
	if got.Ref.CheckID != 12345 {
		t.Errorf("UpdateCheckRun ref.check_id = %d, want 12345", got.Ref.CheckID)
	}
	if got.Status != iacgithub.CheckRunStatusCompleted {
		t.Errorf("UpdateCheckRun status = %q, want %q", got.Status, iacgithub.CheckRunStatusCompleted)
	}
	if got.Conclusion != iacgithub.CheckRunConclusionSuccess {
		t.Errorf("UpdateCheckRun conclusion = %q, want %q", got.Conclusion, iacgithub.CheckRunConclusionSuccess)
	}
	if got.CompletedAt.IsZero() {
		t.Errorf("UpdateCheckRun completed_at is zero; want stamped")
	}
	if !strings.Contains(got.Output.Title, "SUCCESS") {
		t.Errorf("UpdateCheckRun title = %q, want SUCCESS in it", got.Output.Title)
	}
	if checks.updPATs[0] != "pat-checks-write-webhook" {
		t.Errorf("UpdateCheckRun pat = %q", checks.updPATs[0])
	}

	// SetCheckRunForRecommendation persisted the new state.
	if len(crStore.setCalls) != 1 {
		t.Fatalf("Set calls = %d, want 1", len(crStore.setCalls))
	}
	sc := crStore.setCalls[0]
	if sc.Rec.RecommendationID != recID {
		t.Errorf("Set rec.recommendation_id = %q", sc.Rec.RecommendationID)
	}
	if sc.Status != iacgithub.CheckRunStatusCompleted {
		t.Errorf("Set status = %q, want %q", sc.Status, iacgithub.CheckRunStatusCompleted)
	}
	if sc.Conclusion != iacgithub.CheckRunConclusionSuccess {
		t.Errorf("Set conclusion = %q, want %q", sc.Conclusion, iacgithub.CheckRunConclusionSuccess)
	}

	// Audit log: pr_merged + iac.check_run.updated (and the seeded
	// iac.check_run.created already lives in the log as the pivot).
	var prMergedCount, updatedCount int
	var updatedEntry services.AuditEntry
	for _, e := range audit.entries {
		switch e.EventType {
		case services.AuditEventRecommendationPRMerged:
			prMergedCount++
		case services.AuditEventIaCCheckRunUpdated:
			updatedCount++
			updatedEntry = e
		}
	}
	if prMergedCount != 1 {
		t.Errorf("recommendation.pr_merged count = %d, want 1", prMergedCount)
	}
	if updatedCount != 1 {
		t.Fatalf("iac.check_run.updated count = %d, want 1", updatedCount)
	}
	if updatedEntry.Payload["new_conclusion"] != iacgithub.CheckRunConclusionSuccess {
		t.Errorf("updated payload.new_conclusion = %v, want success", updatedEntry.Payload["new_conclusion"])
	}
	if updatedEntry.Payload["previous_status"] != iacgithub.CheckRunStatusInProgress {
		t.Errorf("updated payload.previous_status = %v, want in_progress", updatedEntry.Payload["previous_status"])
	}
	if updatedEntry.Payload["new_status"] != iacgithub.CheckRunStatusCompleted {
		t.Errorf("updated payload.new_status = %v, want completed", updatedEntry.Payload["new_status"])
	}
	if updatedEntry.Payload["recommendation_id"] != recID {
		t.Errorf("updated payload.recommendation_id = %v, want %q", updatedEntry.Payload["recommendation_id"], recID)
	}
	if updatedEntry.TargetID != connectionID {
		t.Errorf("updated target_id = %q, want %q", updatedEntry.TargetID, connectionID)
	}
}

// TestWebhook_PRClosed_UpdatesCheckRunToFailure — parallel happy
// path on the close-without-merge branch: new_conclusion=failure,
// the chunk-3 dance + audit emit fire correctly.
func TestWebhook_PRClosed_UpdatesCheckRunToFailure(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, checks, crStore := newTestWebhookHandlerWithChecksAPI(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	const recID = "rec-closed-1"
	const prURL = "https://github.com/octo/widgets/pull/0"
	seedCheckRunCreatedAuditPivot(t, audit, connectionID, recID, prURL)
	crStore.seededGet = map[string]fakeWebhookCheckRunGetResult{
		recID: {
			Ref:        types.CheckRunRef{Owner: "octo", Repo: "widgets", CheckID: 22222, HeadSHA: "def456"},
			Status:     iacgithub.CheckRunStatusInProgress,
			Conclusion: "",
			Exists:     true,
		},
	}

	body := makePREventBody(t, "closed", false, "octo/widgets", 0,
		"squadron/rec/lambda-otel-layer/222222222222/us-west-2/xyz9-1",
		"2026-06-22T01:02:03Z", "bob")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	if len(checks.updates) != 1 {
		t.Fatalf("UpdateCheckRun calls = %d, want 1", len(checks.updates))
	}
	if checks.updates[0].Conclusion != iacgithub.CheckRunConclusionFailure {
		t.Errorf("UpdateCheckRun conclusion = %q, want %q",
			checks.updates[0].Conclusion, iacgithub.CheckRunConclusionFailure)
	}

	var closedCount, updatedCount int
	var updatedEntry services.AuditEntry
	for _, e := range audit.entries {
		switch e.EventType {
		case services.AuditEventRecommendationPRClosedNotMerged:
			closedCount++
		case services.AuditEventIaCCheckRunUpdated:
			updatedCount++
			updatedEntry = e
		}
	}
	if closedCount != 1 {
		t.Errorf("recommendation.pr_closed_not_merged count = %d, want 1", closedCount)
	}
	if updatedCount != 1 {
		t.Fatalf("iac.check_run.updated count = %d, want 1", updatedCount)
	}
	if updatedEntry.Payload["new_conclusion"] != iacgithub.CheckRunConclusionFailure {
		t.Errorf("updated payload.new_conclusion = %v, want failure", updatedEntry.Payload["new_conclusion"])
	}
}

// TestWebhook_PRMerged_NoCheckRunStored_NoUpdateAttempted — fire a
// merged webhook for a PR that never had a chunk-2 audit pivot
// recorded. The chunk-3 helper silently no-ops: the original
// recommendation.pr_merged audit still fires, but no UpdateCheckRun
// call happens and no iac.check_run.updated / .failed audit emits.
func TestWebhook_PRMerged_NoCheckRunStored_NoUpdateAttempted(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, checks, crStore := newTestWebhookHandlerWithChecksAPI(t, audit, webhookTestSecret)
	seedConnection(t, store, "octo/widgets")

	// Deliberately NOT calling seedCheckRunCreatedAuditPivot — this
	// PR predates the Checks API enablement (or chunk-2 emit
	// failed). The chunk-3 helper must fail-open silent.

	body := makePREventBody(t, "closed", true, "octo/widgets", 0,
		"squadron/rec/rds-pi-em/111111111111/us-east-1/no-pivot-0",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// The chunk-3 helper short-circuits BEFORE GetCheckRunForRecommendation
	// because the pivot lookup returns empty — no Get call lands.
	if len(crStore.getCalls) != 0 {
		t.Errorf("Get calls = %d, want 0 (pivot absent should short-circuit before lookup)", len(crStore.getCalls))
	}
	if len(checks.updates) != 0 {
		t.Errorf("UpdateCheckRun calls = %d, want 0", len(checks.updates))
	}
	if len(crStore.setCalls) != 0 {
		t.Errorf("Set calls = %d, want 0", len(crStore.setCalls))
	}

	var prMergedCount, updatedCount, failedCount int
	for _, e := range audit.entries {
		switch e.EventType {
		case services.AuditEventRecommendationPRMerged:
			prMergedCount++
		case services.AuditEventIaCCheckRunUpdated:
			updatedCount++
		case services.AuditEventIaCCheckRunFailed:
			failedCount++
		}
	}
	if prMergedCount != 1 {
		t.Errorf("recommendation.pr_merged count = %d, want 1", prMergedCount)
	}
	if updatedCount != 0 {
		t.Errorf("iac.check_run.updated count = %d, want 0 (no check run was ever created)", updatedCount)
	}
	if failedCount != 0 {
		t.Errorf("iac.check_run.failed count = %d, want 0 (absent pivot is not a failure)", failedCount)
	}
}

// TestWebhook_PRMerged_UpdateCheckRunRateLimited_EmitsFailedAudit —
// pivot resolves and Get returns a stored check run, but the
// fakeWebhookChecksClient's UpdateCheckRun returns a
// CheckRunError{Kind: rate_limit}. Assert: iac.check_run.failed
// emits with error_kind=rate_limit AND the original
// recommendation.pr_merged event still fires correctly (the order
// guarantee from design doc §7.1).
func TestWebhook_PRMerged_UpdateCheckRunRateLimited_EmitsFailedAudit(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store, checks, crStore := newTestWebhookHandlerWithChecksAPI(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	const recID = "rec-rate-limited-1"
	const prURL = "https://github.com/octo/widgets/pull/0"
	seedCheckRunCreatedAuditPivot(t, audit, connectionID, recID, prURL)
	crStore.seededGet = map[string]fakeWebhookCheckRunGetResult{
		recID: {
			Ref:        types.CheckRunRef{Owner: "octo", Repo: "widgets", CheckID: 33333, HeadSHA: "rate1"},
			Status:     iacgithub.CheckRunStatusInProgress,
			Conclusion: "",
			Exists:     true,
		},
	}
	checks.respErr = &iacgithub.CheckRunError{
		Kind:    iacgithub.CheckRunErrorKindRateLimit,
		Status:  403,
		Message: "GitHub API rate limit exceeded",
	}

	body := makePREventBody(t, "closed", true, "octo/widgets", 0,
		"squadron/rec/rds-pi-em/111111111111/us-east-1/rate-0",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// UpdateCheckRun was attempted (the call landed on the fake) but
	// returned the rate_limit error.
	if len(checks.updates) != 1 {
		t.Fatalf("UpdateCheckRun calls = %d, want 1", len(checks.updates))
	}
	// On failure: NO Set persists — the GitHub side rejected the
	// PATCH so our durable state stays at in_progress, awaiting the
	// next attempt or slice-2's reconciliation pass.
	if len(crStore.setCalls) != 0 {
		t.Errorf("Set calls = %d, want 0 on failed PATCH", len(crStore.setCalls))
	}

	var prMergedCount, updatedCount, failedCount int
	var failedEntry services.AuditEntry
	for _, e := range audit.entries {
		switch e.EventType {
		case services.AuditEventRecommendationPRMerged:
			prMergedCount++
		case services.AuditEventIaCCheckRunUpdated:
			updatedCount++
		case services.AuditEventIaCCheckRunFailed:
			failedCount++
			failedEntry = e
		}
	}
	if prMergedCount != 1 {
		t.Errorf("recommendation.pr_merged count = %d, want 1 (original event must still fire)", prMergedCount)
	}
	if updatedCount != 0 {
		t.Errorf("iac.check_run.updated count = %d, want 0 (PATCH failed → no success audit)", updatedCount)
	}
	if failedCount != 1 {
		t.Fatalf("iac.check_run.failed count = %d, want 1", failedCount)
	}
	if failedEntry.Payload["error_kind"] != iacgithub.CheckRunErrorKindRateLimit {
		t.Errorf("failed payload.error_kind = %v, want %q",
			failedEntry.Payload["error_kind"], iacgithub.CheckRunErrorKindRateLimit)
	}
	if failedEntry.Payload["intended_conclusion"] != iacgithub.CheckRunConclusionSuccess {
		t.Errorf("failed payload.intended_conclusion = %v, want success",
			failedEntry.Payload["intended_conclusion"])
	}
}

// TestWebhook_PRMerged_NilChecksClient_NoOps — wire the webhook
// handler WITHOUT the chunk-3 ChecksAPI client. Fire a merged
// webhook for a PR that DOES have a chunk-2 pivot in the audit log.
// The chunk-3 helper must fail-open silent: no Get call, no
// UpdateCheckRun, no chunk-3 audits, but the original
// recommendation.pr_merged audit still fires.
func TestWebhook_PRMerged_NilChecksClient_NoOps(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	// NOT using newTestWebhookHandlerWithChecksAPI — we want a
	// handler with NO ChecksAPI wired.
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	const recID = "rec-nil-checks-1"
	const prURL = "https://github.com/octo/widgets/pull/0"
	// Pivot exists in the audit log — to prove the helper's
	// short-circuit lands on the nil-ChecksAPI gate, not the
	// missing-pivot gate.
	seedCheckRunCreatedAuditPivot(t, audit, connectionID, recID, prURL)

	body := makePREventBody(t, "closed", true, "octo/widgets", 0,
		"squadron/rec/rds-pi-em/111111111111/us-east-1/nil-0",
		"2026-06-22T12:34:56Z", "alice")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var prMergedCount, updatedCount, failedCount int
	for _, e := range audit.entries {
		switch e.EventType {
		case services.AuditEventRecommendationPRMerged:
			prMergedCount++
		case services.AuditEventIaCCheckRunUpdated:
			updatedCount++
		case services.AuditEventIaCCheckRunFailed:
			failedCount++
		}
	}
	if prMergedCount != 1 {
		t.Errorf("recommendation.pr_merged count = %d, want 1", prMergedCount)
	}
	if updatedCount != 0 {
		t.Errorf("iac.check_run.updated count = %d, want 0 (nil ChecksAPI must short-circuit)", updatedCount)
	}
	if failedCount != 0 {
		t.Errorf("iac.check_run.failed count = %d, want 0 (nil ChecksAPI is not a failure)", failedCount)
	}
}

// TestPerConnectionWebhookSecret_NoConnectionFound_UsesGlobal —
// v0.89.31 (#650) — when the inbound delivery names a repo that
// matches no connection, the receiver falls back to the env-var
// global (same as the unset-per-connection-secret path). The audit
// row goes out with an empty connection_id, mirroring v0.89.23's
// TestGitHubWebhook_SignatureValid_NoMatchingConnection_StillEmitsAudit
// contract. Proves the env-var fallback is reachable along both the
// "no connection matched" and "connection matched but no secret"
// paths.
func TestPerConnectionWebhookSecret_NoConnectionFound_UsesGlobal(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, _, _ := newTestWebhookHandlerWithCredKey(t, audit, webhookTestSecret)
	// Note: no connection seeded — the repo is a stranger.
	body := makePREventBody(t, "closed", true, "some-randomperson/repo", 11,
		"squadron/rec/alb-access-logs/qqq", "2026-06-22T00:00:00Z", "dana")
	sig := signGitHubWebhook(t, body, webhookTestSecret)
	w := doWebhookRequest(t, h, body, sig, "pull_request")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	if audit.entries[0].TargetID != "" {
		t.Errorf("audit target_id = %q, want empty (no connection matched)", audit.entries[0].TargetID)
	}
	if audit.entries[0].Payload["connection_id"] != "" {
		t.Errorf("audit payload.connection_id = %v, want empty", audit.entries[0].Payload["connection_id"])
	}
	if audit.entries[0].Payload["repo_full_name"] != "some-randomperson/repo" {
		t.Errorf("audit payload.repo_full_name = %v, want some-randomperson/repo", audit.entries[0].Payload["repo_full_name"])
	}
}

// --- v0.89.48 (#671 Stream 69) GCP discovery slice 1 chunk 5 tests ---

// TestWebhook_GCERecommendationKind_AuditPayloadCarriesProjectID —
// chunk 5 acceptance: when the merged PR's branch encodes a GCP
// recommendation kind (gce- prefix) and a 6-segment scope tuple, the
// emitted recommendation.pr_merged audit payload carries
// project_id=<scope_id>, account_id="", and provider="gcp".
//
// Branch shape per docs/proposals/gcp-discovery-slice1.md §9.1:
//   squadron/rec/gce-otel-label/<project_id>/<region>/<short_id>
func TestWebhook_GCERecommendationKind_AuditPayloadCarriesProjectID(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/gce-otel-label/my-project/us-central1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "gce-otel-label" {
		t.Errorf("payload.recommendation_kind = %v, want gce-otel-label", pay["recommendation_kind"])
	}
	if pay["provider"] != "gcp" {
		t.Errorf("payload.provider = %v, want gcp", pay["provider"])
	}
	if pay["project_id"] != "my-project" {
		t.Errorf("payload.project_id = %v, want my-project", pay["project_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["region"] != "us-central1" {
		t.Errorf("payload.region = %v, want us-central1", pay["region"])
	}
}

// TestWebhook_AWSRecommendationKind_AuditPayloadStillCarriesAccountID
// — chunk 5 acceptance: existing v0.89.36+ behavior on AWS kinds is
// preserved. The 6-segment AWS branch (squadron/rec/rds-pi-em/<account_id>/
// <region>/<id>) yields payload.account_id=<account_id>, payload.project_id="",
// payload.provider="aws".
func TestWebhook_AWSRecommendationKind_AuditPayloadStillCarriesAccountID(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/rds-pi-em/123456789012/us-east-1/def456",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "rds-pi-em" {
		t.Errorf("payload.recommendation_kind = %v, want rds-pi-em", pay["recommendation_kind"])
	}
	if pay["provider"] != "aws" {
		t.Errorf("payload.provider = %v, want aws", pay["provider"])
	}
	if pay["account_id"] != "123456789012" {
		t.Errorf("payload.account_id = %v, want 123456789012", pay["account_id"])
	}
	if pay["project_id"] != "" {
		t.Errorf("payload.project_id = %v, want empty string", pay["project_id"])
	}
	if pay["region"] != "us-east-1" {
		t.Errorf("payload.region = %v, want us-east-1", pay["region"])
	}
}

// TestProviderFromRecommendationKind — chunk 5 unit test on the
// kind-prefix dispatch. Pins the slice 1 contract that "gce-" prefix
// implies GCP and everything else (including the empty kind from the
// pre-extension 4-segment branch shape) implies AWS.
//
// v0.89.53 (#678 Stream 76, Azure discovery slice 1 chunk 5) extends
// the table with "vm-" → "azure" cases below.
func TestProviderFromRecommendationKind(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{kind: "gce-otel-label", want: "gcp"},
		{kind: "ec2-otel-layer", want: "aws"},
		{kind: "lambda-otel-layer", want: "aws"},
		{kind: "rds-pi-em", want: "aws"},
		{kind: "eks-cluster-logging", want: "aws"},
		{kind: "", want: "aws"},
		{kind: "gce", want: "aws"},      // "gce" alone (no hyphen) is not the GCP prefix
		{kind: "gcestuff", want: "aws"}, // boundary: requires the literal "gce-" prefix
		// v0.89.53 chunk 5 — Azure prefix dispatch.
		{kind: "vm-otel-tag", want: "azure"},
		{kind: "vm", want: "aws"},      // "vm" alone (no hyphen) is not the Azure prefix
		{kind: "vmstuff", want: "aws"}, // boundary: requires the literal "vm-" prefix
		// v0.89.58 chunk 5 — OCI prefix dispatch.
		{kind: "compute-otel-tag", want: "oci"},
		{kind: "compute", want: "aws"},      // "compute" alone (no hyphen) is not the OCI prefix
		{kind: "computestuff", want: "aws"}, // boundary: requires the literal "compute-" prefix
	}
	for _, tc := range cases {
		if got := providerFromRecommendationKind(tc.kind); got != tc.want {
			t.Errorf("providerFromRecommendationKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestWebhook_VMRecommendationKind_AuditPayloadCarriesSubscriptionID
// — v0.89.53 (#678 Stream 76, Azure discovery slice 1 chunk 5)
// acceptance. When the merged PR's branch encodes an Azure
// recommendation kind (vm- prefix) and a 6-segment scope tuple, the
// emitted recommendation.pr_merged audit payload carries
// subscription_id=<scope_id>, account_id="", project_id="", and
// provider="azure". Branch shape per
// docs/proposals/azure-discovery-slice1.md §10:
//
//	squadron/rec/vm-otel-tag/<subscription_id>/<region>/<short_id>
func TestWebhook_VMRecommendationKind_AuditPayloadCarriesSubscriptionID(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/vm-otel-tag/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/eastus/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "vm-otel-tag" {
		t.Errorf("payload.recommendation_kind = %v, want vm-otel-tag", pay["recommendation_kind"])
	}
	if pay["provider"] != "azure" {
		t.Errorf("payload.provider = %v, want azure", pay["provider"])
	}
	if pay["subscription_id"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("payload.subscription_id = %v, want aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", pay["subscription_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["project_id"] != "" {
		t.Errorf("payload.project_id = %v, want empty string", pay["project_id"])
	}
	if pay["region"] != "eastus" {
		t.Errorf("payload.region = %v, want eastus", pay["region"])
	}
}

// TestWebhook_ComputeRecommendationKind_AuditPayloadCarriesTenancyOCID
// — v0.89.58 (#685 Stream 83, OCI discovery slice 1 chunk 5)
// acceptance. When the merged PR's branch encodes an OCI
// recommendation kind (compute- prefix) and a 6-segment scope tuple,
// the emitted recommendation.pr_merged audit payload carries
// tenancy_ocid=<scope_id>, account_id="", project_id="",
// subscription_id="", and provider="oci". Branch shape per
// docs/proposals/oci-discovery-slice1.md §10:
//
//	squadron/rec/compute-otel-tag/<tenancy_ocid>/<region>/<short_id>
func TestWebhook_ComputeRecommendationKind_AuditPayloadCarriesTenancyOCID(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/compute-otel-tag/ocid1.tenancy.oc1..aaaaaaaa/us-phoenix-1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "compute-otel-tag" {
		t.Errorf("payload.recommendation_kind = %v, want compute-otel-tag", pay["recommendation_kind"])
	}
	if pay["provider"] != "oci" {
		t.Errorf("payload.provider = %v, want oci", pay["provider"])
	}
	if pay["tenancy_ocid"] != "ocid1.tenancy.oc1..aaaaaaaa" {
		t.Errorf("payload.tenancy_ocid = %v, want ocid1.tenancy.oc1..aaaaaaaa", pay["tenancy_ocid"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["project_id"] != "" {
		t.Errorf("payload.project_id = %v, want empty string", pay["project_id"])
	}
	if pay["subscription_id"] != "" {
		t.Errorf("payload.subscription_id = %v, want empty string", pay["subscription_id"])
	}
	if pay["region"] != "us-phoenix-1" {
		t.Errorf("payload.region = %v, want us-phoenix-1", pay["region"])
	}
}

// TestWebhook_CloudSQLKind_RoutesToGCP — database tier slice 2
// chunk 5 (v0.89.66, #695 Stream 93). Branch
// `squadron/rec/cloudsql-pi-enable/<project_id>/<region>/<short_id>`
// is a Cloud SQL recommendation kind that must route through the
// providerFromRecommendationKind helper to provider="gcp" and
// populate project_id (not account_id) in the audit payload.
// Branch shape per docs/proposals/database-tier-slice2.md §6.
func TestWebhook_CloudSQLKind_RoutesToGCP(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/cloudsql-pi-enable/my-prod-project/us-central1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "cloudsql-pi-enable" {
		t.Errorf("payload.recommendation_kind = %v, want cloudsql-pi-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "gcp" {
		t.Errorf("payload.provider = %v, want gcp", pay["provider"])
	}
	if pay["project_id"] != "my-prod-project" {
		t.Errorf("payload.project_id = %v, want my-prod-project", pay["project_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["region"] != "us-central1" {
		t.Errorf("payload.region = %v, want us-central1", pay["region"])
	}
}

// TestWebhook_AzSQLKind_RoutesToAzure — database tier slice 2 chunk
// 5 (v0.89.66, #695 Stream 93). Branch
// `squadron/rec/azsql-diag-enable/<subscription_id>/<region>/<short_id>`
// is an Azure SQL recommendation kind that must route through the
// providerFromRecommendationKind helper to provider="azure" and
// populate subscription_id (not account_id) in the audit payload.
func TestWebhook_AzSQLKind_RoutesToAzure(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/azsql-diag-enable/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/eastus/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "azsql-diag-enable" {
		t.Errorf("payload.recommendation_kind = %v, want azsql-diag-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "azure" {
		t.Errorf("payload.provider = %v, want azure", pay["provider"])
	}
	if pay["subscription_id"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("payload.subscription_id = %v, want aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", pay["subscription_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["project_id"] != "" {
		t.Errorf("payload.project_id = %v, want empty string", pay["project_id"])
	}
	if pay["region"] != "eastus" {
		t.Errorf("payload.region = %v, want eastus", pay["region"])
	}
}

// TestWebhook_OCIDBKind_RoutesToOCI — database tier slice 2 chunk 5
// (v0.89.66, #695 Stream 93). Branch
// `squadron/rec/ocidb-perfhub-enable/<tenancy_ocid>/<region>/<short_id>`
// is an OCI Database recommendation kind that must route through
// the providerFromRecommendationKind helper to provider="oci" and
// populate tenancy_ocid (not account_id) in the audit payload.
func TestWebhook_OCIDBKind_RoutesToOCI(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/ocidb-perfhub-enable/ocid1.tenancy.oc1..aaaaaaaa/us-phoenix-1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "ocidb-perfhub-enable" {
		t.Errorf("payload.recommendation_kind = %v, want ocidb-perfhub-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "oci" {
		t.Errorf("payload.provider = %v, want oci", pay["provider"])
	}
	if pay["tenancy_ocid"] != "ocid1.tenancy.oc1..aaaaaaaa" {
		t.Errorf("payload.tenancy_ocid = %v, want ocid1.tenancy.oc1..aaaaaaaa", pay["tenancy_ocid"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["project_id"] != "" {
		t.Errorf("payload.project_id = %v, want empty string", pay["project_id"])
	}
	if pay["subscription_id"] != "" {
		t.Errorf("payload.subscription_id = %v, want empty string", pay["subscription_id"])
	}
	if pay["region"] != "us-phoenix-1" {
		t.Errorf("payload.region = %v, want us-phoenix-1", pay["region"])
	}
}

// TestProviderFromRecommendationKind_DatabaseTierExtension — database
// tier slice 2 chunk 5 (v0.89.66, #695 Stream 93). The three new
// database recommendation kind prefixes must route to the matching
// provider, and the existing slice 1 routing must remain green.
// The boundary cases ("cloudsql" / "azsql" / "ocidb" alone with no
// trailing hyphen) must still return "aws" because the substrate
// requires the literal hyphenated prefix.
func TestProviderFromRecommendationKind_DatabaseTierExtension(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		// Slice 1 — must stay green.
		{kind: "gce-otel-label", want: "gcp"},
		{kind: "vm-otel-tag", want: "azure"},
		{kind: "compute-otel-tag", want: "oci"},
		{kind: "ec2-otel-layer", want: "aws"},
		{kind: "lambda-otel-layer", want: "aws"},
		{kind: "rds-pi-em", want: "aws"},
		// Database tier slice 2 — new routing.
		{kind: "cloudsql-pi-enable", want: "gcp"},
		{kind: "azsql-diag-enable", want: "azure"},
		{kind: "ocidb-perfhub-enable", want: "oci"},
		// Boundary cases — bare keyword without trailing hyphen
		// falls through to AWS.
		{kind: "cloudsql", want: "aws"},
		{kind: "azsql", want: "aws"},
		{kind: "ocidb", want: "aws"},
		{kind: "cloudsqlstuff", want: "aws"},
		{kind: "azsqlstuff", want: "aws"},
		{kind: "ocidbstuff", want: "aws"},
	}
	for _, tc := range cases {
		if got := providerFromRecommendationKind(tc.kind); got != tc.want {
			t.Errorf("providerFromRecommendationKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestWebhook_GKEKind_RoutesToGCP — Kubernetes tier slice 2 chunk 5
// (v0.89.71, #702 Stream 100). Branch
// `squadron/rec/gke-mp-enable/<project_id>/<region>/<short_id>` is
// a GKE recommendation kind that must route through the
// providerFromRecommendationKind helper to provider="gcp" and
// populate project_id (not account_id) in the audit payload.
// Branch shape per docs/proposals/kubernetes-tier-slice2.md §6.
func TestWebhook_GKEKind_RoutesToGCP(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/gke-mp-enable/my-prod-project/us-central1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "gke-mp-enable" {
		t.Errorf("payload.recommendation_kind = %v, want gke-mp-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "gcp" {
		t.Errorf("payload.provider = %v, want gcp", pay["provider"])
	}
	if pay["project_id"] != "my-prod-project" {
		t.Errorf("payload.project_id = %v, want my-prod-project", pay["project_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["region"] != "us-central1" {
		t.Errorf("payload.region = %v, want us-central1", pay["region"])
	}
}

// TestWebhook_AKSKind_RoutesToAzure — Kubernetes tier slice 2 chunk 5
// (v0.89.71, #702 Stream 100). Branch
// `squadron/rec/aks-monitor-enable/<subscription_id>/<region>/<short_id>`
// is an AKS recommendation kind that must route through the
// providerFromRecommendationKind helper to provider="azure" and
// populate subscription_id (not account_id) in the audit payload.
func TestWebhook_AKSKind_RoutesToAzure(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/aks-monitor-enable/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/eastus/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "aks-monitor-enable" {
		t.Errorf("payload.recommendation_kind = %v, want aks-monitor-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "azure" {
		t.Errorf("payload.provider = %v, want azure", pay["provider"])
	}
	if pay["subscription_id"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("payload.subscription_id = %v, want aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", pay["subscription_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["project_id"] != "" {
		t.Errorf("payload.project_id = %v, want empty string", pay["project_id"])
	}
	if pay["region"] != "eastus" {
		t.Errorf("payload.region = %v, want eastus", pay["region"])
	}
}

// TestWebhook_OKEKind_RoutesToOCI — Kubernetes tier slice 2 chunk 5
// (v0.89.71, #702 Stream 100). Branch
// `squadron/rec/oke-ops-insights-enable/<tenancy_ocid>/<region>/<short_id>`
// is an OKE recommendation kind that must route through the
// providerFromRecommendationKind helper to provider="oci" and
// populate tenancy_ocid (not account_id) in the audit payload.
func TestWebhook_OKEKind_RoutesToOCI(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/oke-ops-insights-enable/ocid1.tenancy.oc1..aaaaaaaa/us-phoenix-1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "oke-ops-insights-enable" {
		t.Errorf("payload.recommendation_kind = %v, want oke-ops-insights-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "oci" {
		t.Errorf("payload.provider = %v, want oci", pay["provider"])
	}
	if pay["tenancy_ocid"] != "ocid1.tenancy.oc1..aaaaaaaa" {
		t.Errorf("payload.tenancy_ocid = %v, want ocid1.tenancy.oc1..aaaaaaaa", pay["tenancy_ocid"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["project_id"] != "" {
		t.Errorf("payload.project_id = %v, want empty string", pay["project_id"])
	}
	if pay["subscription_id"] != "" {
		t.Errorf("payload.subscription_id = %v, want empty string", pay["subscription_id"])
	}
	if pay["region"] != "us-phoenix-1" {
		t.Errorf("payload.region = %v, want us-phoenix-1", pay["region"])
	}
}

// TestProviderFromRecommendationKind_K8sTierExtension — Kubernetes
// tier slice 2 chunk 5 (v0.89.71, #702 Stream 100). The three new
// K8s recommendation kind prefixes must route to the matching
// provider, and the existing slice 1 + database tier routing must
// remain green. Boundary cases ("gke" / "aks" / "oke" alone with
// no trailing hyphen) must still return "aws" because the substrate
// requires the literal hyphenated prefix.
func TestProviderFromRecommendationKind_K8sTierExtension(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		// Slice 1 + database tier — must stay green.
		{kind: "gce-otel-label", want: "gcp"},
		{kind: "vm-otel-tag", want: "azure"},
		{kind: "compute-otel-tag", want: "oci"},
		{kind: "cloudsql-pi-enable", want: "gcp"},
		{kind: "azsql-diag-enable", want: "azure"},
		{kind: "ocidb-perfhub-enable", want: "oci"},
		{kind: "ec2-otel-layer", want: "aws"},
		{kind: "lambda-otel-layer", want: "aws"},
		// K8s tier slice 2 — new routing.
		{kind: "gke-mp-enable", want: "gcp"},
		{kind: "aks-monitor-enable", want: "azure"},
		{kind: "oke-ops-insights-enable", want: "oci"},
		// Boundary cases — bare keyword without trailing hyphen
		// falls through to AWS.
		{kind: "gke", want: "aws"},
		{kind: "aks", want: "aws"},
		{kind: "oke", want: "aws"},
		{kind: "gkesomething", want: "aws"},
		{kind: "akssomething", want: "aws"},
		{kind: "okesomething", want: "aws"},
	}
	for _, tc := range cases {
		if got := providerFromRecommendationKind(tc.kind); got != tc.want {
			t.Errorf("providerFromRecommendationKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestProviderFromTraceEmissionKind_Cases — Trace integration slice 2
// chunk 2 (v0.89.81, #712 Stream 110). Unit test on the
// providerFromTraceEmissionKind helper that drives the trace-emission
// case in providerFromRecommendationKind's switch. The helper must
// return the cloud provider segment for every recognized
// `trace-emission-<provider>-<tier>` shape AND return "" for every
// off-shape input so the switch's fallback policy kicks in cleanly.
// See docs/proposals/trace-integration-slice2.md §6.
func TestProviderFromTraceEmissionKind_Cases(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		// Happy path: each of the 12 trace-emission kinds parses
		// cleanly to its provider segment.
		{kind: "trace-emission-aws-compute", want: "aws"},
		{kind: "trace-emission-aws-db", want: "aws"},
		{kind: "trace-emission-aws-k8s", want: "aws"},
		{kind: "trace-emission-gcp-compute", want: "gcp"},
		{kind: "trace-emission-gcp-db", want: "gcp"},
		{kind: "trace-emission-gcp-k8s", want: "gcp"},
		{kind: "trace-emission-azure-compute", want: "azure"},
		{kind: "trace-emission-azure-db", want: "azure"},
		{kind: "trace-emission-azure-k8s", want: "azure"},
		{kind: "trace-emission-oci-compute", want: "oci"},
		{kind: "trace-emission-oci-db", want: "oci"},
		{kind: "trace-emission-oci-k8s", want: "oci"},
		// Off-shape inputs all return "" so the switch falls back.
		{kind: "trace-emission-bogus-compute", want: ""}, // unrecognized provider segment
		{kind: "gce-otel-label", want: ""},               // not a trace-emission kind
		{kind: "", want: ""},                             // empty
		{kind: "trace-emission-", want: ""},              // prefix only, no provider segment
		{kind: "trace-emission-aws", want: ""},           // no tier segment (no internal hyphen)
		{kind: "trace-emission--compute", want: ""},      // empty provider segment
	}
	for _, tc := range cases {
		if got := providerFromTraceEmissionKind(tc.kind); got != tc.want {
			t.Errorf("providerFromTraceEmissionKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// TestWebhook_TraceEmissionKinds_RouteCorrectly — Trace integration
// slice 2 chunk 2 (v0.89.81, #712 Stream 110), acceptance tests 7
// and 8 from docs/proposals/trace-integration-slice2.md §10. Each of
// the 12 trace-emission recommendation kinds, when encoded into a
// merged PR's head branch, must route through
// providerFromRecommendationKind to the matching provider segment of
// the kind. The audit payload's `provider` field carries that
// provider; this pins acceptance tests 7 and 8 ("webhook routes
// trace-emission-gcp-k8s to provider=gcp" and "webhook routes
// trace-emission-azure-db to provider=azure") plus the 10 other
// trace-emission permutations for symmetry.
//
// Branch shape per docs/proposals/trace-integration-slice2.md §6
// reuses the v0.89.28 6-segment encoding:
//
//	squadron/rec/<kind>/<scope_id>/<region>/<short_id>
//
// The scope_id segment routes via writeScopePayloadFields to the
// provider-appropriate field (account_id / project_id /
// subscription_id / tenancy_ocid). We pick a representative scope_id
// + region per provider so the audit payload is the same shape SIEM
// consumers see in production.
func TestWebhook_TraceEmissionKinds_RouteCorrectly(t *testing.T) {
	cases := []struct {
		kind           string
		wantProvider   string
		scopeID        string
		region         string
		scopeFieldName string // payload key the scope_id routes to
	}{
		{
			kind:           "trace-emission-aws-compute",
			wantProvider:   "aws",
			scopeID:        "123456789012",
			region:         "us-east-1",
			scopeFieldName: "account_id",
		},
		{
			kind:           "trace-emission-aws-db",
			wantProvider:   "aws",
			scopeID:        "123456789012",
			region:         "us-east-1",
			scopeFieldName: "account_id",
		},
		{
			kind:           "trace-emission-aws-k8s",
			wantProvider:   "aws",
			scopeID:        "123456789012",
			region:         "us-east-1",
			scopeFieldName: "account_id",
		},
		{
			kind:           "trace-emission-gcp-compute",
			wantProvider:   "gcp",
			scopeID:        "my-prod-project",
			region:         "us-central1",
			scopeFieldName: "project_id",
		},
		{
			kind:           "trace-emission-gcp-db",
			wantProvider:   "gcp",
			scopeID:        "my-prod-project",
			region:         "us-central1",
			scopeFieldName: "project_id",
		},
		{
			kind:           "trace-emission-gcp-k8s",
			wantProvider:   "gcp",
			scopeID:        "my-prod-project",
			region:         "us-central1",
			scopeFieldName: "project_id",
		},
		{
			kind:           "trace-emission-azure-compute",
			wantProvider:   "azure",
			scopeID:        "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			region:         "eastus",
			scopeFieldName: "subscription_id",
		},
		{
			kind:           "trace-emission-azure-db",
			wantProvider:   "azure",
			scopeID:        "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			region:         "eastus",
			scopeFieldName: "subscription_id",
		},
		{
			kind:           "trace-emission-azure-k8s",
			wantProvider:   "azure",
			scopeID:        "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			region:         "eastus",
			scopeFieldName: "subscription_id",
		},
		{
			kind:           "trace-emission-oci-compute",
			wantProvider:   "oci",
			scopeID:        "ocid1.tenancy.oc1..aaaaaaaa",
			region:         "us-phoenix-1",
			scopeFieldName: "tenancy_ocid",
		},
		{
			kind:           "trace-emission-oci-db",
			wantProvider:   "oci",
			scopeID:        "ocid1.tenancy.oc1..aaaaaaaa",
			region:         "us-phoenix-1",
			scopeFieldName: "tenancy_ocid",
		},
		{
			kind:           "trace-emission-oci-k8s",
			wantProvider:   "oci",
			scopeID:        "ocid1.tenancy.oc1..aaaaaaaa",
			region:         "us-phoenix-1",
			scopeFieldName: "tenancy_ocid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			audit := &discoveryRecordingAudit{}
			h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
			connectionID := seedConnection(t, store, "octo/widgets")

			branch := "squadron/rec/" + tc.kind + "/" + tc.scopeID + "/" + tc.region + "/abc123"
			body := makePREventBody(t, "closed", true, "octo/widgets", 42,
				branch, "2026-06-22T12:34:56Z", "alice")
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
			if e.TargetID != connectionID {
				t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
			}
			pay := e.Payload
			if pay["recommendation_kind"] != tc.kind {
				t.Errorf("payload.recommendation_kind = %v, want %s", pay["recommendation_kind"], tc.kind)
			}
			if pay["provider"] != tc.wantProvider {
				t.Errorf("payload.provider = %v, want %s", pay["provider"], tc.wantProvider)
			}
			if pay[tc.scopeFieldName] != tc.scopeID {
				t.Errorf("payload.%s = %v, want %s", tc.scopeFieldName, pay[tc.scopeFieldName], tc.scopeID)
			}
			if pay["region"] != tc.region {
				t.Errorf("payload.region = %v, want %s", pay["region"], tc.region)
			}
		})
	}
}

// TestWebhook_PreExistingKinds_StillRouteCorrectly — Trace
// integration slice 2 chunk 2 (v0.89.81, #712 Stream 110) regression
// test. Confirms the new trace-emission case sitting at the top of
// providerFromRecommendationKind's switch does NOT accidentally
// swallow other prefixes. Every kind in the pre-existing per-cloud
// catalog (slice 1 + database tier slice 2 + Kubernetes tier slice 2)
// must continue to route to the same provider it did before chunk 2.
//
// The unit-level coverage already lives in
// TestProviderFromRecommendationKind /
// TestProviderFromRecommendationKind_DatabaseTierExtension /
// TestProviderFromRecommendationKind_K8sTierExtension; this test
// re-pins the routing through the helper directly as a single
// regression surface that's quick to scan when reviewing the chunk-2
// diff.
func TestWebhook_PreExistingKinds_StillRouteCorrectly(t *testing.T) {
	cases := []struct {
		kind         string
		wantProvider string
	}{
		// Slice 1 — compute kinds.
		{kind: "gce-otel-label", wantProvider: "gcp"},
		{kind: "vm-otel-tag", wantProvider: "azure"},
		{kind: "compute-otel-tag", wantProvider: "oci"},
		{kind: "ec2-otel-layer", wantProvider: "aws"},
		{kind: "lambda-otel-layer", wantProvider: "aws"},
		{kind: "rds-pi-em", wantProvider: "aws"},
		// Database tier slice 2.
		{kind: "cloudsql-pi-enable", wantProvider: "gcp"},
		{kind: "azsql-diag-enable", wantProvider: "azure"},
		{kind: "ocidb-perfhub-enable", wantProvider: "oci"},
		// Kubernetes tier slice 2.
		{kind: "gke-mp-enable", wantProvider: "gcp"},
		{kind: "aks-monitor-enable", wantProvider: "azure"},
		{kind: "oke-ops-insights-enable", wantProvider: "oci"},
		// Pre-extension empty kind from a legacy 4-segment branch
		// still routes to AWS.
		{kind: "", wantProvider: "aws"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			if got := providerFromRecommendationKind(tc.kind); got != tc.wantProvider {
				t.Errorf("providerFromRecommendationKind(%q) = %q, want %q", tc.kind, got, tc.wantProvider)
			}
		})
	}
}

// --- Serverless tier slice 1 chunk 5 (v0.89.92, #725 Stream 123) ----
//
// The four routing tests below pin the §11 acceptance tests 14-17:
//   14. webhook routes lambda-xray-active to provider=aws
//   15. webhook routes cloudrun-otel-sidecar to provider=gcp
//   16. webhook routes azfunc-appinsights-enable to provider=azure
//   17. webhook routes ocifunc-apm-enable to provider=oci
//
// Plus a kind-prefix dispatch table extension test asserting that the
// 11 new serverless kinds route to the expected provider AND the prior
// tiers (slice 1, database tier, k8s tier, trace-emission) remain
// green.

// TestWebhook_LambdaXrayActiveKind_RoutesToAWS — §11 acceptance test 14.
func TestWebhook_LambdaXrayActiveKind_RoutesToAWS(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/lambda-xray-active/123456789012/us-east-1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "lambda-xray-active" {
		t.Errorf("payload.recommendation_kind = %v, want lambda-xray-active", pay["recommendation_kind"])
	}
	if pay["provider"] != "aws" {
		t.Errorf("payload.provider = %v, want aws", pay["provider"])
	}
	if pay["account_id"] != "123456789012" {
		t.Errorf("payload.account_id = %v, want 123456789012", pay["account_id"])
	}
	if pay["region"] != "us-east-1" {
		t.Errorf("payload.region = %v, want us-east-1", pay["region"])
	}
}

// TestWebhook_CloudRunOTelSidecarKind_RoutesToGCP — §11 acceptance test 15.
func TestWebhook_CloudRunOTelSidecarKind_RoutesToGCP(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/cloudrun-otel-sidecar/my-prod-project/us-central1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "cloudrun-otel-sidecar" {
		t.Errorf("payload.recommendation_kind = %v, want cloudrun-otel-sidecar", pay["recommendation_kind"])
	}
	if pay["provider"] != "gcp" {
		t.Errorf("payload.provider = %v, want gcp", pay["provider"])
	}
	if pay["project_id"] != "my-prod-project" {
		t.Errorf("payload.project_id = %v, want my-prod-project", pay["project_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["region"] != "us-central1" {
		t.Errorf("payload.region = %v, want us-central1", pay["region"])
	}
}

// TestWebhook_AzFuncAppInsightsKind_RoutesToAzure — §11 acceptance test 16.
func TestWebhook_AzFuncAppInsightsKind_RoutesToAzure(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/azfunc-appinsights-enable/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/eastus/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "azfunc-appinsights-enable" {
		t.Errorf("payload.recommendation_kind = %v, want azfunc-appinsights-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "azure" {
		t.Errorf("payload.provider = %v, want azure", pay["provider"])
	}
	if pay["subscription_id"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("payload.subscription_id = %v, want aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", pay["subscription_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
	if pay["region"] != "eastus" {
		t.Errorf("payload.region = %v, want eastus", pay["region"])
	}
}

// TestWebhook_OCIFuncAPMKind_RoutesToOCI — §11 acceptance test 17.
func TestWebhook_OCIFuncAPMKind_RoutesToOCI(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/ocifunc-apm-enable/ocid1.tenancy.oc1..aaaaaaaa/us-phoenix-1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "ocifunc-apm-enable" {
		t.Errorf("payload.recommendation_kind = %v, want ocifunc-apm-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "oci" {
		t.Errorf("payload.provider = %v, want oci", pay["provider"])
	}
	if pay["tenancy_ocid"] != "ocid1.tenancy.oc1..aaaaaaaa" {
		t.Errorf("payload.tenancy_ocid = %v, want ocid1.tenancy.oc1..aaaaaaaa", pay["tenancy_ocid"])
	}
}

// TestProviderFromRecommendationKind_ServerlessTierExtension — pins
// the dispatch table for the 11 new serverless kinds and reasserts the
// prior-tier routing remains green. Boundary cases ("lambda" / "azfunc"
// / "ocifunc" / "cloudrun" / "cloudfunc" alone, no trailing hyphen)
// fall through to the default (aws) because the substrate requires the
// literal hyphenated prefix.
func TestProviderFromRecommendationKind_ServerlessTierExtension(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		// AWS Lambda — three new kinds.
		{kind: "lambda-xray-active", want: "aws"},
		{kind: "lambda-otel-layer", want: "aws"},
		{kind: "lambda-otel-wrapper", want: "aws"},
		// GCP Cloud Run — three new kinds.
		{kind: "cloudrun-trace-enable", want: "gcp"},
		{kind: "cloudrun-otel-sidecar", want: "gcp"},
		{kind: "cloudrun-otel-export-endpoint", want: "gcp"},
		// GCP Cloud Functions — two new kinds.
		{kind: "cloudfunc-trace-enable", want: "gcp"},
		{kind: "cloudfunc-otel-layer", want: "gcp"},
		// Azure Functions — two new kinds.
		{kind: "azfunc-appinsights-enable", want: "azure"},
		{kind: "azfunc-otel-distro", want: "azure"},
		// OCI Functions — two new kinds.
		{kind: "ocifunc-apm-enable", want: "oci"},
		{kind: "ocifunc-otel-distro", want: "oci"},
		// Prior tiers — must stay green.
		{kind: "gce-otel-label", want: "gcp"},
		{kind: "vm-otel-tag", want: "azure"},
		{kind: "compute-otel-tag", want: "oci"},
		{kind: "cloudsql-pi-enable", want: "gcp"},
		{kind: "azsql-diag-enable", want: "azure"},
		{kind: "ocidb-perfhub-enable", want: "oci"},
		{kind: "gke-mp-enable", want: "gcp"},
		{kind: "aks-monitor-enable", want: "azure"},
		{kind: "oke-ops-insights-enable", want: "oci"},
		// Boundary cases — bare keywords without the trailing hyphen
		// fall through to AWS.
		{kind: "lambda", want: "aws"},
		{kind: "cloudrun", want: "aws"},
		{kind: "cloudfunc", want: "aws"},
		{kind: "azfunc", want: "aws"},
		{kind: "ocifunc", want: "aws"},
	}
	for _, tc := range cases {
		if got := providerFromRecommendationKind(tc.kind); got != tc.want {
			t.Errorf("providerFromRecommendationKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// --- Orchestration tier slice 1 chunk 4 (v0.89.97, #731 Stream 129) -

// TestWebhook_StepfuncKinds_RouteToAWS — §11 acceptance test 13.
// The two AWS Step Functions kinds (stepfunc-xray-active,
// stepfunc-logging-enable) must route through the webhook receiver
// as provider=aws with the parsed account_id surfaced on the audit
// payload.
func TestWebhook_StepfuncKinds_RouteToAWS(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/stepfunc-xray-active/123456789012/us-east-1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "stepfunc-xray-active" {
		t.Errorf("payload.recommendation_kind = %v, want stepfunc-xray-active", pay["recommendation_kind"])
	}
	if pay["provider"] != "aws" {
		t.Errorf("payload.provider = %v, want aws", pay["provider"])
	}
	if pay["account_id"] != "123456789012" {
		t.Errorf("payload.account_id = %v, want 123456789012", pay["account_id"])
	}
}

// TestWebhook_WorkflowsKinds_RouteToGCP — §11 acceptance test 14.
// The two GCP Workflows kinds route through the webhook receiver as
// provider=gcp with the parsed project_id surfaced on the audit
// payload (and account_id explicitly empty per the v0.89.48 stable
// schema invariant).
func TestWebhook_WorkflowsKinds_RouteToGCP(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/workflows-trace-enable/my-prod-project/us-central1/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "workflows-trace-enable" {
		t.Errorf("payload.recommendation_kind = %v, want workflows-trace-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "gcp" {
		t.Errorf("payload.provider = %v, want gcp", pay["provider"])
	}
	if pay["project_id"] != "my-prod-project" {
		t.Errorf("payload.project_id = %v, want my-prod-project", pay["project_id"])
	}
	if pay["account_id"] != "" {
		t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
	}
}

// TestWebhook_LogicAppsKinds_RouteToAzure — §11 acceptance test 15.
// The two Azure Logic Apps kinds route through the webhook receiver
// as provider=azure with the parsed subscription_id surfaced.
func TestWebhook_LogicAppsKinds_RouteToAzure(t *testing.T) {
	audit := &discoveryRecordingAudit{}
	h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
	connectionID := seedConnection(t, store, "octo/widgets")

	body := makePREventBody(t, "closed", true, "octo/widgets", 42,
		"squadron/rec/logicapps-appinsights-enable/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/eastus/abc123",
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
	if e.TargetID != connectionID {
		t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
	}
	pay := e.Payload
	if pay["recommendation_kind"] != "logicapps-appinsights-enable" {
		t.Errorf("payload.recommendation_kind = %v, want logicapps-appinsights-enable", pay["recommendation_kind"])
	}
	if pay["provider"] != "azure" {
		t.Errorf("payload.provider = %v, want azure", pay["provider"])
	}
	if pay["subscription_id"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("payload.subscription_id = %v, want aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", pay["subscription_id"])
	}
}

// TestProviderFromRecommendationKind_OrchestrationTierExtension —
// pins the dispatch table for the 6 new orchestration kinds and
// reasserts prior-tier routing remains green. Boundary cases on
// "stepfunc" / "workflows" / "logicapps" alone (no trailing hyphen)
// fall through to AWS.
func TestProviderFromRecommendationKind_OrchestrationTierExtension(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{kind: "stepfunc-xray-active", want: "aws"},
		{kind: "stepfunc-logging-enable", want: "aws"},
		{kind: "workflows-trace-enable", want: "gcp"},
		{kind: "workflows-logging-enable", want: "gcp"},
		{kind: "logicapps-appinsights-enable", want: "azure"},
		{kind: "logicapps-diagnostics-enable", want: "azure"},
		// Prior-tier sanity.
		{kind: "lambda-xray-active", want: "aws"},
		{kind: "cloudrun-otel-sidecar", want: "gcp"},
		{kind: "azfunc-appinsights-enable", want: "azure"},
		{kind: "ocifunc-apm-enable", want: "oci"},
		// Boundary cases — bare prefixes without trailing hyphen fall
		// through to AWS via the switch default.
		{kind: "stepfunc", want: "aws"},
		{kind: "workflows", want: "aws"},
		{kind: "logicapps", want: "aws"},
	}
	for _, tc := range cases {
		if got := providerFromRecommendationKind(tc.kind); got != tc.want {
			t.Errorf("providerFromRecommendationKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// --- Event source tier slice 1 chunk 5 (v0.89.102, #738 Stream 136) -

// TestWebhook_EventBridgeKinds_RouteToAWS — §11 acceptance test 15.
// The three AWS EventBridge kinds (eventbridge-xray-enable,
// eventbridge-schemas-discover, eventbridge-logging-enable) must
// route through the webhook receiver as provider=aws with the parsed
// account_id surfaced on the audit payload.
func TestWebhook_EventBridgeKinds_RouteToAWS(t *testing.T) {
	for _, kind := range []string{
		"eventbridge-xray-enable",
		"eventbridge-schemas-discover",
		"eventbridge-logging-enable",
	} {
		t.Run(kind, func(t *testing.T) {
			audit := &discoveryRecordingAudit{}
			h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
			connectionID := seedConnection(t, store, "octo/widgets")
			branch := "squadron/rec/" + kind + "/123456789012/us-east-1/abc123"
			body := makePREventBody(t, "closed", true, "octo/widgets", 42,
				branch, "2026-06-22T12:34:56Z", "alice")
			sig := signGitHubWebhook(t, body, webhookTestSecret)

			w := doWebhookRequest(t, h, body, sig, "pull_request")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if len(audit.entries) != 1 {
				t.Fatalf("audit entries = %d, want 1", len(audit.entries))
			}
			e := audit.entries[0]
			if e.TargetID != connectionID {
				t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
			}
			pay := e.Payload
			if pay["recommendation_kind"] != kind {
				t.Errorf("payload.recommendation_kind = %v, want %s", pay["recommendation_kind"], kind)
			}
			if pay["provider"] != "aws" {
				t.Errorf("payload.provider = %v, want aws", pay["provider"])
			}
			if pay["account_id"] != "123456789012" {
				t.Errorf("payload.account_id = %v, want 123456789012", pay["account_id"])
			}
		})
	}
}

// TestWebhook_PubSubKinds_RouteToGCP — §11 acceptance test 16.
// The two GCP Pub/Sub kinds (pubsub-trace-enable, pubsub-schema-attach)
// route through the webhook receiver as provider=gcp with the parsed
// project_id surfaced on the audit payload.
func TestWebhook_PubSubKinds_RouteToGCP(t *testing.T) {
	for _, kind := range []string{
		"pubsub-trace-enable",
		"pubsub-schema-attach",
	} {
		t.Run(kind, func(t *testing.T) {
			audit := &discoveryRecordingAudit{}
			h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
			connectionID := seedConnection(t, store, "octo/widgets")
			branch := "squadron/rec/" + kind + "/my-prod-project/us-central1/abc123"
			body := makePREventBody(t, "closed", true, "octo/widgets", 42,
				branch, "2026-06-22T12:34:56Z", "alice")
			sig := signGitHubWebhook(t, body, webhookTestSecret)

			w := doWebhookRequest(t, h, body, sig, "pull_request")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if len(audit.entries) != 1 {
				t.Fatalf("audit entries = %d, want 1", len(audit.entries))
			}
			e := audit.entries[0]
			if e.TargetID != connectionID {
				t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
			}
			pay := e.Payload
			if pay["recommendation_kind"] != kind {
				t.Errorf("payload.recommendation_kind = %v, want %s", pay["recommendation_kind"], kind)
			}
			if pay["provider"] != "gcp" {
				t.Errorf("payload.provider = %v, want gcp", pay["provider"])
			}
			if pay["project_id"] != "my-prod-project" {
				t.Errorf("payload.project_id = %v, want my-prod-project", pay["project_id"])
			}
			if pay["account_id"] != "" {
				t.Errorf("payload.account_id = %v, want empty string", pay["account_id"])
			}
		})
	}
}

// TestWebhook_ServiceBusKinds_RouteToAzure — §11 acceptance test 17.
// The Azure Service Bus kind (servicebus-diagnostics-enable) routes
// through the webhook receiver as provider=azure with the parsed
// subscription_id surfaced.
func TestWebhook_ServiceBusKinds_RouteToAzure(t *testing.T) {
	for _, kind := range []string{
		"servicebus-diagnostics-enable",
	} {
		t.Run(kind, func(t *testing.T) {
			audit := &discoveryRecordingAudit{}
			h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
			connectionID := seedConnection(t, store, "octo/widgets")
			branch := "squadron/rec/" + kind + "/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/eastus/abc123"
			body := makePREventBody(t, "closed", true, "octo/widgets", 42,
				branch, "2026-06-22T12:34:56Z", "alice")
			sig := signGitHubWebhook(t, body, webhookTestSecret)

			w := doWebhookRequest(t, h, body, sig, "pull_request")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if len(audit.entries) != 1 {
				t.Fatalf("audit entries = %d, want 1", len(audit.entries))
			}
			e := audit.entries[0]
			if e.TargetID != connectionID {
				t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
			}
			pay := e.Payload
			if pay["recommendation_kind"] != kind {
				t.Errorf("payload.recommendation_kind = %v, want %s", pay["recommendation_kind"], kind)
			}
			if pay["provider"] != "azure" {
				t.Errorf("payload.provider = %v, want azure", pay["provider"])
			}
			if pay["subscription_id"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
				t.Errorf("payload.subscription_id = %v, want aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
					pay["subscription_id"])
			}
		})
	}
}

// TestWebhook_StreamingKinds_RouteToOCI — §11 acceptance test 18.
// The OCI Streaming kind (streaming-logging-enable) routes through
// the webhook receiver as provider=oci with the parsed tenancy_ocid
// surfaced.
func TestWebhook_StreamingKinds_RouteToOCI(t *testing.T) {
	for _, kind := range []string{
		"streaming-logging-enable",
	} {
		t.Run(kind, func(t *testing.T) {
			audit := &discoveryRecordingAudit{}
			h, store := newTestWebhookHandler(t, audit, webhookTestSecret)
			connectionID := seedConnection(t, store, "octo/widgets")
			branch := "squadron/rec/" + kind + "/ocid1.tenancy.oc1..aaaaaaaa/us-phoenix-1/abc123"
			body := makePREventBody(t, "closed", true, "octo/widgets", 42,
				branch, "2026-06-22T12:34:56Z", "alice")
			sig := signGitHubWebhook(t, body, webhookTestSecret)

			w := doWebhookRequest(t, h, body, sig, "pull_request")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if len(audit.entries) != 1 {
				t.Fatalf("audit entries = %d, want 1", len(audit.entries))
			}
			e := audit.entries[0]
			if e.TargetID != connectionID {
				t.Errorf("target_id = %q, want %q", e.TargetID, connectionID)
			}
			pay := e.Payload
			if pay["recommendation_kind"] != kind {
				t.Errorf("payload.recommendation_kind = %v, want %s", pay["recommendation_kind"], kind)
			}
			if pay["provider"] != "oci" {
				t.Errorf("payload.provider = %v, want oci", pay["provider"])
			}
			if pay["tenancy_ocid"] != "ocid1.tenancy.oc1..aaaaaaaa" {
				t.Errorf("payload.tenancy_ocid = %v, want ocid1.tenancy.oc1..aaaaaaaa", pay["tenancy_ocid"])
			}
		})
	}
}

// TestProviderFromRecommendationKind_EventSourceTierExtension — pins
// the dispatch table for the 7 new event source kinds and reasserts
// prior-tier routing remains green. Same shape as the orchestration
// extension test.
func TestProviderFromRecommendationKind_EventSourceTierExtension(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{kind: "eventbridge-xray-enable", want: "aws"},
		{kind: "eventbridge-schemas-discover", want: "aws"},
		{kind: "eventbridge-logging-enable", want: "aws"},
		{kind: "pubsub-trace-enable", want: "gcp"},
		{kind: "pubsub-schema-attach", want: "gcp"},
		{kind: "servicebus-diagnostics-enable", want: "azure"},
		{kind: "streaming-logging-enable", want: "oci"},
		// Prior-tier sanity — orchestration / serverless / trace
		// emission kinds still route correctly.
		{kind: "stepfunc-xray-active", want: "aws"},
		{kind: "workflows-trace-enable", want: "gcp"},
		{kind: "logicapps-appinsights-enable", want: "azure"},
		{kind: "ocifunc-apm-enable", want: "oci"},
		{kind: "lambda-otel-layer", want: "aws"},
		// Boundary cases — bare prefixes without trailing hyphen fall
		// through to AWS via the switch default.
		{kind: "eventbridge", want: "aws"},
		{kind: "pubsub", want: "aws"},
		{kind: "servicebus", want: "aws"},
		{kind: "streaming", want: "aws"},
	}
	for _, tc := range cases {
		if got := providerFromRecommendationKind(tc.kind); got != tc.want {
			t.Errorf("providerFromRecommendationKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}
