// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newFakeChecksGitHub mirrors newFakeGitHub but is named separately
// so a glance at the test file makes the Checks-API scope obvious.
// The PAT passed to the wrapper methods is supplied by each test;
// the constructor's "test-token-do-not-log" value is what the
// wrapper falls back to when the client.go default-branch helpers
// run, NOT what the Checks API path uses.
func newFakeChecksGitHub(t *testing.T, handler http.HandlerFunc) (*PATClient, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	cli := NewPATClient("test-token-do-not-log").WithBaseURL(srv.URL)
	return cli, srv.Close
}

// TestCreateCheckRun_HappyPath pins the success path: 201 with a
// numeric id + the canonical body shape lands a CheckRunRef with
// CheckID round-tripped. Also asserts the request body carries the
// expected name, status, and output.title fields so chunk-2
// regressions land cleanly here.
func TestCreateCheckRun_HappyPath(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   []byte
		gotAuth   string
		gotAccept string
	)
	cli, done := newFakeChecksGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":12345,"head_sha":"abc123","html_url":"https://github.com/octo/widgets/runs/12345"}`))
	})
	defer done()

	ref, err := cli.CreateCheckRun(context.Background(), "pat-chk-write", CheckRunCreate{
		Owner:     "octo",
		Repo:      "widgets",
		HeadSHA:   "abc123",
		Name:      "Squadron recommendation",
		Status:    CheckRunStatusInProgress,
		StartedAt: time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC),
		Output: CheckRunOutput{
			Title:   "Recommendation: rds-pi-em",
			Summary: "Squadron opened this PR.",
		},
	})
	if err != nil {
		t.Fatalf("CreateCheckRun error: %v", err)
	}
	if ref.CheckID != 12345 {
		t.Errorf("CheckID = %d, want 12345", ref.CheckID)
	}
	if ref.Owner != "octo" || ref.Repo != "widgets" {
		t.Errorf("Owner/Repo = %q/%q, want octo/widgets", ref.Owner, ref.Repo)
	}
	if ref.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q, want abc123", ref.HeadSHA)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/repos/octo/widgets/check-runs" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "token pat-chk-write" {
		t.Errorf("Authorization header = %q, want token pat-chk-write", gotAuth)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("decode body: %v body=%s", err, string(gotBody))
	}
	if raw["name"] != "Squadron recommendation" {
		t.Errorf("body.name = %v, want Squadron recommendation", raw["name"])
	}
	if raw["head_sha"] != "abc123" {
		t.Errorf("body.head_sha = %v, want abc123", raw["head_sha"])
	}
	if raw["status"] != CheckRunStatusInProgress {
		t.Errorf("body.status = %v, want %s", raw["status"], CheckRunStatusInProgress)
	}
	if out, ok := raw["output"].(map[string]any); !ok || out["title"] != "Recommendation: rds-pi-em" {
		t.Errorf("body.output = %v", raw["output"])
	}
}

// TestCreateCheckRun_MissingScope_Returns403Error pins the
// scope_missing discrimination: a 403 with GitHub's canonical
// "Resource not accessible by integration" message MUST surface as
// Kind=scope_missing, NOT as a generic network error. This is the
// load-bearing discrimination the chunk-2 audit emit fans out on.
func TestCreateCheckRun_MissingScope_Returns403Error(t *testing.T) {
	cli, done := newFakeChecksGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration","documentation_url":"..."}`))
	})
	defer done()
	_, err := cli.CreateCheckRun(context.Background(), "pat-no-scope", CheckRunCreate{
		Owner:   "octo",
		Repo:    "widgets",
		HeadSHA: "abc123",
		Status:  CheckRunStatusInProgress,
	})
	var cerr *CheckRunError
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *CheckRunError", err)
	}
	if cerr.Kind != CheckRunErrorKindScopeMissing {
		t.Errorf("Kind = %q, want %q", cerr.Kind, CheckRunErrorKindScopeMissing)
	}
	if cerr.Status != http.StatusForbidden {
		t.Errorf("Status = %d, want 403", cerr.Status)
	}
}

// TestCreateCheckRun_NotFound_Returns404Error pins the §5 mapping:
// fine-grained PATs that lack the scope return 404 (GitHub's
// opaque "endpoint not found" response). MUST surface as
// scope_missing, NOT pr_not_found — the recommendation here is
// "fix your PAT scope," not "the PR is gone."
func TestCreateCheckRun_NotFound_Returns404Error(t *testing.T) {
	cli, done := newFakeChecksGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found","documentation_url":"..."}`))
	})
	defer done()
	_, err := cli.CreateCheckRun(context.Background(), "pat-no-scope", CheckRunCreate{
		Owner:   "octo",
		Repo:    "widgets",
		HeadSHA: "abc123",
		Status:  CheckRunStatusInProgress,
	})
	var cerr *CheckRunError
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *CheckRunError", err)
	}
	if cerr.Kind != CheckRunErrorKindScopeMissing {
		t.Errorf("Kind = %q, want %q", cerr.Kind, CheckRunErrorKindScopeMissing)
	}
}

// TestCreateCheckRun_RateLimit_ReturnsRateLimitError pins the
// X-RateLimit-Remaining=0 discrimination. GitHub returns 403 (NOT
// 429) on rate-limit hits against the REST API; the wrapper MUST
// check the header value BEFORE falling through to the scope-
// missing branch. This is the most subtle of the four error_kinds
// because the same status code maps to two different discriminators
// based on a header.
func TestCreateCheckRun_RateLimit_ReturnsRateLimitError(t *testing.T) {
	cli, done := newFakeChecksGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1734567890")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded","documentation_url":"..."}`))
	})
	defer done()
	_, err := cli.CreateCheckRun(context.Background(), "pat", CheckRunCreate{
		Owner:   "octo",
		Repo:    "widgets",
		HeadSHA: "abc123",
		Status:  CheckRunStatusInProgress,
	})
	var cerr *CheckRunError
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *CheckRunError", err)
	}
	if cerr.Kind != CheckRunErrorKindRateLimit {
		t.Errorf("Kind = %q, want %q", cerr.Kind, CheckRunErrorKindRateLimit)
	}
	// The reset header value MUST appear in the message so the SIEM
	// dashboard can render the "when does this clear?" panel.
	if !strings.Contains(cerr.Message, "1734567890") {
		t.Errorf("Message = %q, want reset value embedded", cerr.Message)
	}
}

// TestCreateCheckRun_RateLimit_429StillDiscriminates pins the
// canonical 429 path: even without the X-RateLimit-Remaining header
// a 429 MUST surface as rate_limit (the secondary signal).
func TestCreateCheckRun_RateLimit_429StillDiscriminates(t *testing.T) {
	cli, done := newFakeChecksGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"Too many requests"}`))
	})
	defer done()
	_, err := cli.CreateCheckRun(context.Background(), "pat", CheckRunCreate{
		Owner:   "octo",
		Repo:    "widgets",
		HeadSHA: "abc123",
		Status:  CheckRunStatusInProgress,
	})
	var cerr *CheckRunError
	if !errors.As(err, &cerr) || cerr.Kind != CheckRunErrorKindRateLimit {
		t.Fatalf("Kind = %v, want rate_limit", cerr)
	}
}

// TestCreateCheckRun_NetworkError_ReturnsNetworkError pins the
// transport-level failure path: an unreachable server (closed
// listener) MUST surface as Kind=network. This is the "drop and
// keep going" branch — the chunk-2 audit emit groups these with
// the generic transient bucket.
func TestCreateCheckRun_NetworkError_ReturnsNetworkError(t *testing.T) {
	// Bind a listener, capture its URL, then close it. The wrapper's
	// HTTP client will fail to connect on the subsequent dial.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := "http://" + ln.Addr().String()
	_ = ln.Close()

	cli := NewPATClient("test-token-do-not-log").WithBaseURL(addr)
	_, err = cli.CreateCheckRun(context.Background(), "pat", CheckRunCreate{
		Owner:   "octo",
		Repo:    "widgets",
		HeadSHA: "abc123",
		Status:  CheckRunStatusInProgress,
	})
	var cerr *CheckRunError
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *CheckRunError", err)
	}
	if cerr.Kind != CheckRunErrorKindNetwork {
		t.Errorf("Kind = %q, want %q", cerr.Kind, CheckRunErrorKindNetwork)
	}
}

// TestUpdateCheckRun_HappyPath pins the success path: PATCH on the
// existing check-run endpoint with the conclusion + completed_at
// fields, 200 response, nil error returned. Asserts the wire body
// carries status + conclusion + completed_at fields so chunk-3
// regressions land here.
func TestUpdateCheckRun_HappyPath(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   []byte
	)
	cli, done := newFakeChecksGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":12345,"status":"completed","conclusion":"success"}`))
	})
	defer done()

	err := cli.UpdateCheckRun(context.Background(), "pat", CheckRunUpdate{
		Ref: CheckRunRef{
			Owner: "octo", Repo: "widgets", CheckID: 12345, HeadSHA: "abc123",
		},
		Status:      CheckRunStatusCompleted,
		Conclusion:  CheckRunConclusionSuccess,
		CompletedAt: time.Date(2026, 6, 22, 13, 0, 0, 0, time.UTC),
		Output: CheckRunOutput{
			Title:   "Recommendation: rds-pi-em — SUCCESS",
			Summary: "Operator merged.",
		},
	})
	if err != nil {
		t.Fatalf("UpdateCheckRun error: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/repos/octo/widgets/check-runs/12345" {
		t.Errorf("path = %q", gotPath)
	}
	var raw map[string]any
	if err := json.Unmarshal(gotBody, &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if raw["status"] != CheckRunStatusCompleted {
		t.Errorf("body.status = %v", raw["status"])
	}
	if raw["conclusion"] != CheckRunConclusionSuccess {
		t.Errorf("body.conclusion = %v", raw["conclusion"])
	}
	if raw["completed_at"] == nil {
		t.Errorf("body.completed_at missing")
	}
}

// TestUpdateCheckRun_PRNotFound_Returns422Error pins the
// pr_not_found discrimination: a 422 on the PATCH path MUST surface
// as pr_not_found, NOT as a generic network error. This is the
// "the PR is gone" branch — chunk-3 webhook handlers see this when
// an operator deletes a PR before the merge / close PATCH lands.
func TestUpdateCheckRun_PRNotFound_Returns422Error(t *testing.T) {
	cli, done := newFakeChecksGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Check run not found","errors":[{"resource":"CheckRun","code":"missing"}]}`))
	})
	defer done()
	err := cli.UpdateCheckRun(context.Background(), "pat", CheckRunUpdate{
		Ref: CheckRunRef{
			Owner: "octo", Repo: "widgets", CheckID: 99999, HeadSHA: "abc123",
		},
		Status:     CheckRunStatusCompleted,
		Conclusion: CheckRunConclusionFailure,
	})
	var cerr *CheckRunError
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *CheckRunError", err)
	}
	if cerr.Kind != CheckRunErrorKindPRNotFound {
		t.Errorf("Kind = %q, want %q", cerr.Kind, CheckRunErrorKindPRNotFound)
	}
}

// TestCreateCheckRun_AuthFailed_ReturnsScopeMissing pins the 401
// branch: auth-class failures are MAPPED to scope_missing (PAT
// either expired or never had the scope; the humanizer surfaces the
// same fix-it copy in both cases). The single most load-bearing
// assertion: the token MUST NOT leak into the returned error's
// Message field even if the upstream's response body echoes it.
func TestCreateCheckRun_AuthFailed_ReturnsScopeMissing(t *testing.T) {
	cli, done := newFakeChecksGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// Pathological upstream body that contains the token bytes.
		_, _ = w.Write([]byte(`{"message":"Bad credentials: pat-leak-canary"}`))
	})
	defer done()
	_, err := cli.CreateCheckRun(context.Background(), "pat-leak-canary", CheckRunCreate{
		Owner:   "octo",
		Repo:    "widgets",
		HeadSHA: "abc123",
		Status:  CheckRunStatusInProgress,
	})
	var cerr *CheckRunError
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want *CheckRunError", err)
	}
	if cerr.Kind != CheckRunErrorKindScopeMissing {
		t.Errorf("Kind = %q, want %q", cerr.Kind, CheckRunErrorKindScopeMissing)
	}
	if strings.Contains(cerr.Error(), "pat-leak-canary") {
		t.Fatalf("token bytes leaked into error: %v", cerr)
	}
}
