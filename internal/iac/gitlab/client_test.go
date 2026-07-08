// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gitlab

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

// newFakeGitLab spins up an httptest.Server with the supplied handler.
// The returned PATClient is pointed at it so callers exercise the
// wrapper end-to-end without touching real GitLab.
func newFakeGitLab(t *testing.T, handler http.HandlerFunc) (*PATClient, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	cli := NewPATClient("test-token-do-not-log").WithBaseURL(srv.URL)
	return cli, srv.Close
}

func TestGetRepo_round_trips_default_branch(t *testing.T) {
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		// GitLab URL-encodes owner/repo into a single :id segment; the
		// raw wire path must keep the slash percent-escaped (the
		// http.Server decodes it back into r.URL.Path).
		if esc := strings.ToUpper(r.URL.EscapedPath()); !strings.Contains(esc, "OCTO%2FWIDGETS") {
			t.Errorf("escaped path = %q, want owner/repo slash percent-escaped", r.URL.EscapedPath())
		}
		if r.URL.Path != "/projects/octo/widgets" {
			t.Errorf("decoded path = %q, want /projects/octo/widgets", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent = %q, want %q", got, UserAgent)
		}
		// The load-bearing GitLab auth-header assertion.
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "test-token-do-not-log" {
			t.Errorf("PRIVATE-TOKEN header missing or wrong: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("GitLab must not send an Authorization header, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
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
	refsCalled := false
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repository/branches") {
			refsCalled = true
		}
		if r.URL.Path == "/projects/octo/widgets" {
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
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
		t.Errorf("branches endpoint MUST NOT be called when branch = default")
	}

	err = cli.CreateBranch(context.Background(), "octo", "widgets", "refs/heads/main", "deadbeef")
	if !errors.Is(err, ErrDefaultBranchWriteRefused) {
		t.Fatalf("err with refs/heads/ prefix = %v, want ErrDefaultBranchWriteRefused", err)
	}
}

func TestPutFileContent_returns_typed_error_when_branch_equals_default(t *testing.T) {
	putCalled := false
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repository/files/") {
			putCalled = true
		}
		if r.URL.Path == "/projects/octo/widgets" {
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
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
		t.Errorf("files endpoint MUST NOT be called when branch = default")
	}
}

func TestOpenPR_returns_typed_error_when_head_equals_default(t *testing.T) {
	mrCalled := false
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/merge_requests") && r.Method == http.MethodPost {
			mrCalled = true
		}
		if r.URL.Path == "/projects/octo/widgets" {
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
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
	if mrCalled {
		t.Errorf("/merge_requests MUST NOT be called when head = default")
	}
}

func TestClient_401_returns_ErrAuthFailed_with_no_token_in_message(t *testing.T) {
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized: test-token-do-not-log"}`))
	})
	defer done()
	_, err := cli.GetRepo(context.Background(), "octo", "widgets")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if strings.Contains(err.Error(), "test-token-do-not-log") {
		t.Fatalf("token bytes leaked into error: %v", err)
	}
}

func TestClient_404_on_GetRepo_returns_ErrRepoNotFound(t *testing.T) {
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer done()
	_, err := cli.GetRepo(context.Background(), "octo", "vanished")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
}

func TestCreateBranch_happy_path_calls_correct_endpoint(t *testing.T) {
	var gotBranch, gotRef string
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
		case r.URL.Path == "/projects/octo/widgets/repository/branches" && r.Method == http.MethodPost:
			gotBranch = r.URL.Query().Get("branch")
			gotRef = r.URL.Query().Get("ref")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"squadron/rec-abc1234-0"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer done()

	if err := cli.CreateBranch(context.Background(), "octo", "widgets", "squadron/rec-abc1234-0", "fromsha123"); err != nil {
		t.Fatalf("CreateBranch error: %v", err)
	}
	if gotBranch != "squadron/rec-abc1234-0" {
		t.Errorf("branch = %q, want squadron/rec-abc1234-0", gotBranch)
	}
	if gotRef != "fromsha123" {
		t.Errorf("ref = %q, want fromsha123", gotRef)
	}
}

func TestPutFileContent_happy_path_base64_encodes_content(t *testing.T) {
	var putBody []byte
	var method string
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
		case strings.Contains(r.URL.Path, "/repository/files/modules/lambda/main.tf"):
			method = r.Method
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"file_path":"modules/lambda/main.tf","branch":"squadron/rec-abc1234-0"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer done()

	combined := []byte("resource \"aws_lambda_function\" \"existing\" {\n}\nresource \"otel\" \"x\" {}\n")
	_, err := cli.PutFileContent(context.Background(), PutFileOptions{
		Owner: "octo", Repo: "widgets",
		Path:    "modules/lambda/main.tf",
		Branch:  "squadron/rec-abc1234-0",
		Content: combined,
		Message: "Squadron: append lambda-otel-layer snippet (scan abc1234)",
	})
	if err != nil {
		t.Fatalf("PutFileContent error: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST for create (empty FileSHA)", method)
	}
	var raw map[string]any
	if err := json.Unmarshal(putBody, &raw); err != nil {
		t.Fatalf("decode put body: %v body=%s", err, string(putBody))
	}
	if raw["branch"] != "squadron/rec-abc1234-0" {
		t.Errorf("body.branch = %v", raw["branch"])
	}
	if raw["encoding"] != "base64" {
		t.Errorf("body.encoding = %v, want base64", raw["encoding"])
	}
	decoded, derr := base64.StdEncoding.DecodeString(raw["content"].(string))
	if derr != nil {
		t.Fatalf("base64 decode content: %v", derr)
	}
	if string(decoded) != string(combined) {
		t.Fatalf("content bytes mismatch.\n want: %q\n got:  %q", string(combined), string(decoded))
	}
}

func TestPutFileContent_400_on_create_returns_ErrFileAlreadyExists(t *testing.T) {
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
		case strings.Contains(r.URL.Path, "/repository/files/"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"A file with this name already exists"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer done()
	_, err := cli.PutFileContent(context.Background(), PutFileOptions{
		Owner: "octo", Repo: "widgets",
		Path:    "modules/lambda/main.tf",
		Branch:  "squadron/rec-abc1234-0",
		Content: []byte("x"),
		Message: "create",
	})
	if !errors.Is(err, ErrFileAlreadyExists) {
		t.Fatalf("err = %v, want ErrFileAlreadyExists", err)
	}
}

func TestGetFileContent_round_trips_and_decodes_base64(t *testing.T) {
	want := "resource \"aws_lambda_function\" \"existing\" {\n  function_name = \"existing\"\n}\n"
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/repository/files/modules/lambda/main.tf") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("ref query = %q, want main", r.URL.Query().Get("ref"))
		}
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
			"file_path": "modules/lambda/main.tf",
			"blob_id":   "existingblobsha",
			"encoding":  "base64",
			"size":      len(want),
			"content":   wrapped.String(),
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
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer done()
	_, err := cli.GetFileContent(context.Background(), "octo", "widgets", "modules/lambda/main.tf", "main")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
}

func TestOpenPR_happy_path_round_trips_payload(t *testing.T) {
	var mrBody []byte
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
		case strings.HasSuffix(r.URL.Path, "/merge_requests") && r.Method == http.MethodPost:
			mrBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"iid":42,"web_url":"https://gitlab.com/octo/widgets/-/merge_requests/42","sha":"headsha"}`))
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
	if pr.HTMLURL != "https://gitlab.com/octo/widgets/-/merge_requests/42" {
		t.Errorf("HTMLURL = %q", pr.HTMLURL)
	}
	var raw map[string]string
	_ = json.Unmarshal(mrBody, &raw)
	if raw["target_branch"] != "main" || raw["source_branch"] != "squadron/rec-abc1234-0" {
		t.Errorf("MR body refs wrong: %+v", raw)
	}
}

func TestAddLabels_refuses_when_mr_source_is_default(t *testing.T) {
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
		case r.URL.Path == "/projects/octo/widgets/merge_requests/9" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"iid":9,"source_branch":"main"}`))
		case r.Method == http.MethodPut:
			t.Errorf("MR update MUST NOT be called when source = default")
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

func TestAddLabels_happy_path_sends_add_labels(t *testing.T) {
	var putBody []byte
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/octo/widgets" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"path_with_namespace":"octo/widgets","default_branch":"main"}`))
		case r.URL.Path == "/projects/octo/widgets/merge_requests/9" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"iid":9,"source_branch":"squadron/rec-abc1234-0"}`))
		case r.URL.Path == "/projects/octo/widgets/merge_requests/9" && r.Method == http.MethodPut:
			putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"iid":9}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer done()
	if err := cli.AddLabels(context.Background(), "octo", "widgets", 9, []string{"squadron", "squadron/lambda-otel-layer"}); err != nil {
		t.Fatalf("AddLabels error: %v", err)
	}
	var raw map[string]any
	_ = json.Unmarshal(putBody, &raw)
	if raw["add_labels"] != "squadron,squadron/lambda-otel-layer" {
		t.Errorf("add_labels = %v", raw["add_labels"])
	}
}

func TestListTree_recursive_returns_blobs_across_pages(t *testing.T) {
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/projects/octo/widgets/repository/tree") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("recursive") != "true" {
			t.Errorf("recursive query missing: %q", r.URL.RawQuery)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("x-next-page", "2")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"path":"main.tf","type":"blob"},{"path":"modules","type":"tree"}]`))
		case "2":
			w.Header().Set("x-next-page", "")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"path":"modules/storage.tf","type":"blob"}]`))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})
	defer done()

	entries, err := cli.ListTree(context.Background(), "octo", "widgets", "main")
	if err != nil {
		t.Fatalf("ListTree error: %v", err)
	}
	var blobs []string
	for _, e := range entries {
		if e.Type == "blob" {
			blobs = append(blobs, e.Path)
		}
	}
	if len(blobs) != 2 || blobs[0] != "main.tf" || blobs[1] != "modules/storage.tf" {
		t.Errorf("blobs = %v, want [main.tf modules/storage.tf]", blobs)
	}
}

func TestListTree_404_maps_to_ErrRepoNotFound(t *testing.T) {
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
	})
	defer done()
	_, err := cli.ListTree(context.Background(), "octo", "widgets", "nope")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Errorf("err = %v, want ErrRepoNotFound", err)
	}
}

func TestGetBranchSHA_round_trips_commit_id(t *testing.T) {
	cli, done := newFakeGitLab(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/octo/widgets/repository/branches/main" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"deadbeefsha"}}`))
	})
	defer done()
	sha, err := cli.GetBranchSHA(context.Background(), "octo", "widgets", "main")
	if err != nil {
		t.Fatalf("GetBranchSHA error: %v", err)
	}
	if sha != "deadbeefsha" {
		t.Errorf("sha = %q, want deadbeefsha", sha)
	}
}

// compile-time assertion that PATClient satisfies Client.
var _ Client = (*PATClient)(nil)
