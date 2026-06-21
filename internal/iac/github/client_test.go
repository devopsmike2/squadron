// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newFakeGitHub spins up an httptest.Server with the supplied
// handler. The returned PATClient is pointed at it so callers can
// exercise the wrapper end-to-end without touching real GitHub.
func newFakeGitHub(t *testing.T, handler http.HandlerFunc) (*PATClient, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	cli := NewPATClient("test-token-do-not-log").WithBaseURL(srv.URL)
	return cli, srv.Close
}

func TestGetRepo_round_trips_default_branch(t *testing.T) {
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/octo/widgets" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent = %q, want %q", got, UserAgent)
		}
		if got := r.Header.Get("Authorization"); got != "token test-token-do-not-log" {
			t.Errorf("Authorization header missing or wrong: %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"full_name":"octo/widgets","default_branch":"main"}`))
	})
	defer done()

	r, err := cli.GetRepo(context.Background(), "octo", "widgets")
	if err != nil {
		t.Fatalf("GetRepo error: %v", err)
	}
	if r.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want main", r.DefaultBranch)
	}
	if r.FullName != "octo/widgets" {
		t.Errorf("FullName = %q", r.FullName)
	}
}

func TestCreateBranch_returns_typed_error_when_branch_equals_default(t *testing.T) {
	// The /repos endpoint returns default_branch=main. A subsequent
	// CreateBranch with branchName=main MUST refuse BEFORE the
	// underlying /git/refs call lands.
	refsCalled := false
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git/refs") {
			refsCalled = true
		}
		if r.URL.Path == "/repos/octo/widgets" {
			_, _ = w.Write([]byte(`{"full_name":"octo/widgets","default_branch":"main"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer done()

	err := cli.CreateBranch(context.Background(), "octo", "widgets", "main", "deadbeef")
	if !errors.Is(err, ErrDefaultBranchWriteRefused) {
		t.Fatalf("err = %v, want ErrDefaultBranchWriteRefused", err)
	}
	if refsCalled {
		t.Errorf("/git/refs MUST NOT be called when branch = default")
	}

	// Same posture for the refs/heads/-prefixed form.
	err = cli.CreateBranch(context.Background(), "octo", "widgets", "refs/heads/main", "deadbeef")
	if !errors.Is(err, ErrDefaultBranchWriteRefused) {
		t.Fatalf("err with refs/heads/ prefix = %v, want ErrDefaultBranchWriteRefused", err)
	}
}

func TestPutFileContent_returns_typed_error_when_branch_equals_default(t *testing.T) {
	putCalled := false
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/") {
			putCalled = true
		}
		if r.URL.Path == "/repos/octo/widgets" {
			_, _ = w.Write([]byte(`{"full_name":"octo/widgets","default_branch":"main"}`))
			return
		}
	})
	defer done()
	_, err := cli.PutFileContent(context.Background(), PutFileOptions{
		Owner: "octo", Repo: "widgets",
		Path:    "modules/lambda/main.tf",
		Branch:  "main",
		Content: []byte("resource \"x\" \"y\" {}\n"),
		Message: "Squadron: append lambda otel layer snippet",
	})
	if !errors.Is(err, ErrDefaultBranchWriteRefused) {
		t.Fatalf("err = %v, want ErrDefaultBranchWriteRefused", err)
	}
	if putCalled {
		t.Errorf("PUT /contents/ MUST NOT be called when branch = default")
	}
}

func TestOpenPR_returns_typed_error_when_head_equals_default(t *testing.T) {
	prCalled := false
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls") {
			prCalled = true
		}
		if r.URL.Path == "/repos/octo/widgets" {
			_, _ = w.Write([]byte(`{"full_name":"octo/widgets","default_branch":"main"}`))
			return
		}
	})
	defer done()
	_, err := cli.OpenPR(context.Background(), OpenPROptions{
		Owner: "octo", Repo: "widgets",
		Title: "Squadron: instrument lambda-otel-layer for 3 resources (scan abc1234)",
		Body:  "irrelevant",
		Head:  "main",
		Base:  "main",
	})
	if !errors.Is(err, ErrDefaultBranchWriteRefused) {
		t.Fatalf("err = %v, want ErrDefaultBranchWriteRefused", err)
	}
	if prCalled {
		t.Errorf("/pulls MUST NOT be called when head = default")
	}
}

func TestClient_401_returns_ErrAuthFailed_with_no_token_in_message(t *testing.T) {
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		// GitHub typically echoes a Reasonably long JSON error body
		// on auth failure; the wrapper must NOT propagate any of it
		// into the returned error string.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials: test-token-do-not-log","documentation_url":"..."}`))
	})
	defer done()
	_, err := cli.GetRepo(context.Background(), "octo", "widgets")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	// The single most load-bearing assertion: the token bytes do not
	// land in the error string. If a future regression starts
	// propagating the response body, this test catches it.
	if strings.Contains(err.Error(), "test-token-do-not-log") {
		t.Fatalf("token bytes leaked into error: %v", err)
	}
}

func TestClient_404_on_GetRepo_returns_ErrRepoNotFound(t *testing.T) {
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer done()
	_, err := cli.GetRepo(context.Background(), "octo", "vanished")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

func TestCreateBranch_happy_path_calls_correct_endpoint_with_correct_body(t *testing.T) {
	var refsBody []byte
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"full_name":"octo/widgets","default_branch":"main"}`))
		case r.URL.Path == "/repos/octo/widgets/git/refs" && r.Method == http.MethodPost:
			refsBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ref":"refs/heads/squadron/rec-abc1234-0","object":{"sha":"deadbeef"}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer done()

	if err := cli.CreateBranch(context.Background(), "octo", "widgets", "squadron/rec-abc1234-0", "fromsha123"); err != nil {
		t.Fatalf("CreateBranch error: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(refsBody, &got); err != nil {
		t.Fatalf("decode refs body: %v body=%s", err, string(refsBody))
	}
	if got["ref"] != "refs/heads/squadron/rec-abc1234-0" {
		t.Errorf("body.ref = %q, want refs/heads/squadron/rec-abc1234-0", got["ref"])
	}
	if got["sha"] != "fromsha123" {
		t.Errorf("body.sha = %q, want fromsha123", got["sha"])
	}
}

// TestPutFileContent_happy_path_appends_snippet_with_one_trailing_newline_and_no_other_changes
// pins down the slice-1 invariant: the snippet is appended to the file
// content with EXACTLY one trailing newline and no other modifications
// to the bytes the client sends to GitHub. Byte-exact assertion against
// the JSON body the wrapper produced.
func TestPutFileContent_happy_path_appends_snippet_with_one_trailing_newline_and_no_other_changes(t *testing.T) {
	var putBody []byte
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"full_name":"octo/widgets","default_branch":"main"}`))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/contents/modules/lambda/main.tf"):
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"content":{"sha":"newblob"},"commit":{"sha":"newcommit"}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer done()

	// The byte stream the wrapper sends to GitHub is the caller's
	// Content, base64-encoded. We hand it the original file bytes +
	// snippet + one trailing newline, exactly the way the handler
	// will assemble it. The test asserts byte-exact equality against
	// what the wrapper put on the wire.
	original := []byte("resource \"aws_lambda_function\" \"existing\" {\n  function_name = \"existing\"\n}\n")
	snippet := []byte("resource \"aws_lambda_function\" \"otel_layer\" {\n  layers = [\"otel\"]\n}")
	// The handler is responsible for adding exactly one trailing
	// newline. The wrapper here MUST forward the bytes verbatim.
	combined := append(append([]byte{}, original...), snippet...)
	combined = append(combined, '\n')

	res, err := cli.PutFileContent(context.Background(), PutFileOptions{
		Owner: "octo", Repo: "widgets",
		Path:    "modules/lambda/main.tf",
		Branch:  "squadron/rec-abc1234-0",
		Content: combined,
		Message: "Squadron: append lambda-otel-layer snippet (scan abc1234)",
		FileSHA: "existingblobsha",
	})
	if err != nil {
		t.Fatalf("PutFileContent error: %v", err)
	}
	if res.CommitSHA != "newcommit" {
		t.Errorf("CommitSHA = %q, want newcommit", res.CommitSHA)
	}

	var raw map[string]any
	if err := json.Unmarshal(putBody, &raw); err != nil {
		t.Fatalf("decode put body: %v body=%s", err, string(putBody))
	}
	// branch surfaces without the refs/heads/ prefix.
	if raw["branch"] != "squadron/rec-abc1234-0" {
		t.Errorf("body.branch = %v, want squadron/rec-abc1234-0", raw["branch"])
	}
	if raw["sha"] != "existingblobsha" {
		t.Errorf("body.sha = %v, want existingblobsha", raw["sha"])
	}
	if !strings.Contains(raw["message"].(string), "scan abc1234") {
		t.Errorf("body.message = %v, expected scan-id in message", raw["message"])
	}
	// The content field, base64-decoded, MUST equal combined byte-for-byte.
	decoded, derr := base64.StdEncoding.DecodeString(raw["content"].(string))
	if derr != nil {
		t.Fatalf("base64 decode content: %v", derr)
	}
	if string(decoded) != string(combined) {
		t.Fatalf("content bytes mismatch.\n want: %q\n got:  %q", string(combined), string(decoded))
	}
	// Exactly one trailing newline.
	if !strings.HasSuffix(string(decoded), "\n") {
		t.Errorf("content does not end with a newline")
	}
	if strings.HasSuffix(string(decoded), "\n\n") {
		t.Errorf("content ends with more than one trailing newline")
	}
}

func TestGetFileContent_round_trips_and_decodes_base64(t *testing.T) {
	want := "resource \"aws_lambda_function\" \"existing\" {\n  function_name = \"existing\"\n}\n"
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/contents/modules/lambda/main.tf") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// GitHub wraps base64 with newlines every 60 chars; emulate that
		// to make sure the wrapper strips them before decoding.
		enc := base64.StdEncoding.EncodeToString([]byte(want))
		var wrapped strings.Builder
		for i := 0; i < len(enc); i += 60 {
			end := i + 60
			if end > len(enc) {
				end = len(enc)
			}
			wrapped.WriteString(enc[i:end])
			wrapped.WriteByte('\n')
		}
		body := map[string]any{
			"path":     "modules/lambda/main.tf",
			"sha":      "existingblobsha",
			"encoding": "base64",
			"size":     len(want),
			"content":  wrapped.String(),
		}
		b, _ := json.Marshal(body)
		_, _ = w.Write(b)
	})
	defer done()

	fc, err := cli.GetFileContent(context.Background(), "octo", "widgets", "modules/lambda/main.tf", "main")
	if err != nil {
		t.Fatalf("GetFileContent error: %v", err)
	}
	if string(fc.DecodedContent) != want {
		t.Errorf("DecodedContent = %q, want %q", string(fc.DecodedContent), want)
	}
	if fc.SHA != "existingblobsha" {
		t.Errorf("SHA = %q", fc.SHA)
	}
}

func TestGetFileContent_404_returns_ErrFileNotFound(t *testing.T) {
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer done()
	_, err := cli.GetFileContent(context.Background(), "octo", "widgets", "modules/lambda/main.tf", "main")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
}

func TestOpenPR_happy_path_round_trips_payload(t *testing.T) {
	var prBody []byte
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"full_name":"octo/widgets","default_branch":"main"}`))
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodPost:
			prBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/octo/widgets/pull/42","head":{"sha":"headsha"}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer done()

	pr, err := cli.OpenPR(context.Background(), OpenPROptions{
		Owner: "octo", Repo: "widgets",
		Title: "Squadron: instrument lambda-otel-layer for 3 resources (scan abc1234)",
		Body:  "see proposer reasoning + snippet",
		Head:  "squadron/rec-abc1234-0",
		Base:  "main",
	})
	if err != nil {
		t.Fatalf("OpenPR error: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("Number = %d, want 42", pr.Number)
	}
	if pr.HTMLURL != "https://github.com/octo/widgets/pull/42" {
		t.Errorf("HTMLURL = %q", pr.HTMLURL)
	}
	var raw map[string]string
	_ = json.Unmarshal(prBody, &raw)
	if raw["base"] != "main" || raw["head"] != "squadron/rec-abc1234-0" {
		t.Errorf("PR body refs wrong: %+v", raw)
	}
}

func TestAddLabels_refuses_when_pr_head_is_default(t *testing.T) {
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"full_name":"octo/widgets","default_branch":"main"}`))
		case r.URL.Path == "/repos/octo/widgets/pulls/9" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"number":9,"head":{"ref":"main"}}`))
		case strings.HasSuffix(r.URL.Path, "/labels"):
			t.Errorf("/labels MUST NOT be called when PR head = default")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer done()
	err := cli.AddLabels(context.Background(), "octo", "widgets", 9, []string{"squadron"})
	if !errors.Is(err, ErrDefaultBranchWriteRefused) {
		t.Fatalf("err = %v, want ErrDefaultBranchWriteRefused", err)
	}
}
