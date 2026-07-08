// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package gitlab is the opt-in GitLab sibling of internal/iac/github.
// It is a faithful mirror of that package's Client surface against the
// GitLab REST API (v4). The two packages are deliberately independent
// (no shared types) so neither churns the other; the handler layer's
// adapter (internal/api/handlers/iac_gitlab.go) maps this package's
// projected types onto the GitHub-shaped ones the connect handlers
// already program against.
//
// The same two cross-cutting invariants the github package owns hold
// here verbatim:
//
//  1. **Never write the default branch.** Every write method refuses a
//     ref equal to the project's default branch BEFORE issuing the
//     underlying API call.
//
//  2. **Never log or echo the token.** The PAT is the secret. It never
//     lands in a log line, a returned error string, or any exposed
//     field. On a 401 the wrapper returns ErrAuthFailed with no body
//     bytes — the token is unrecoverable from the error.
package gitlab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UserAgent is sent on every outbound request so the MR audit trail on
// the operator's project identifies Squadron's writes. Mirrors the
// github package's UserAgent string — GitLab does not parse it.
const UserAgent = "Squadron/0.89.12 (Connect-IaC-Repo)"

// defaultBaseURL is the public gitlab.com API root. PATClient lets a
// caller override it (WithBaseURL) for tests and for self-managed
// GitLab installs (which serve the same v4 API under their own host).
const defaultBaseURL = "https://gitlab.com/api/v4"

// defaultTimeout caps any single API call. Mirrors the github wrapper's
// 30s budget.
const defaultTimeout = 30 * time.Second

// maxTreePages bounds ListTree's pagination follow so a pathologically
// large project cannot hold a handler open indefinitely. Tree listing
// is best-effort (a partial list is fine), matching the github
// wrapper's posture on GitHub's >100k-entry truncation.
const maxTreePages = 100

// Typed errors mirror the github package's sentinel set one-for-one so
// the handler-layer adapter can translate them to the GitHub-shaped
// sentinels the connect handlers already errors.Is against.
var (
	// ErrDefaultBranchWriteRefused is returned by any write method when
	// the branch arg equals the project's default branch. The check
	// fires BEFORE the underlying HTTP request.
	ErrDefaultBranchWriteRefused = errors.New("gitlab: refusing to write the repo's default branch")

	// ErrAuthFailed is returned on a GitLab 401. The error string
	// deliberately carries NO body bytes.
	ErrAuthFailed = errors.New("gitlab: authentication failed")

	// ErrRepoNotFound is returned on a GitLab 404 from GetRepo.
	ErrRepoNotFound = errors.New("gitlab: project not found")

	// ErrFileNotFound is returned on a GitLab 404 from GetFileContent.
	ErrFileNotFound = errors.New("gitlab: file not found at ref")

	// ErrFileAlreadyExists is returned by PutFileContent when the caller
	// asked to CREATE a file (FileSHA empty) but GitLab returned 400
	// because a file already exists at that path — the analog of the
	// github wrapper's 422-on-create mapping.
	ErrFileAlreadyExists = errors.New("gitlab: file already exists at path")
)

// Repo is the projected GET /projects/:id response. GitLab names the
// canonical "owner/repo" identifier path_with_namespace and the
// default branch default_branch.
type Repo struct {
	FullName      string `json:"path_with_namespace"`
	DefaultBranch string `json:"default_branch"`
}

// FileContent is the projected GET repository/files response. GitLab
// returns the blob's SHA as blob_id and the bytes base64-encoded in
// content.
type FileContent struct {
	Path           string
	SHA            string
	Encoding       string
	Size           int
	DecodedContent []byte
}

// TreeEntry is one node from a repository/tree listing. Type is "blob"
// (file) or "tree" (directory).
type TreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// PullRequest is the projected merge-request response. GitLab calls a
// PR a merge request and numbers it per-project as iid.
type PullRequest struct {
	Number  int
	HTMLURL string
	HeadSHA string
}

// CommitFileResult is the projected create/update-file response.
// GitLab's files endpoint returns only {file_path, branch}; it does not
// carry the blob or commit SHA, so those fields are left empty.
type CommitFileResult struct {
	BlobSHA   string
	CommitSHA string
}

// OpenPROptions bundles the create-merge-request call's fields.
type OpenPROptions struct {
	Owner string
	Repo  string
	Title string
	Body  string
	Head  string // the new (source) branch
	Base  string // the target branch (typically the default branch)
}

// PutFileOptions bundles the create-or-update-file call. FileSHA is
// empty on create (POST) and the existing blob SHA on update (PUT) —
// GitLab distinguishes create from update by HTTP method.
type PutFileOptions struct {
	Owner   string
	Repo    string
	Path    string
	Branch  string
	Content []byte
	Message string
	FileSHA string // empty for create, blob SHA for update
}

// Client is the GitLab surface, a method-for-method mirror of
// github.Client. ListTree and GetBranchSHA are intentionally kept OFF
// this interface (as in the github package) — they live on the
// concrete PATClient and callers reach them by type assertion.
type Client interface {
	// GetRepo round-trips the project. Returns ErrRepoNotFound on 404,
	// ErrAuthFailed on 401.
	GetRepo(ctx context.Context, owner, repo string) (*Repo, error)

	// GetFileContent reads the file at path on ref. Returns
	// ErrFileNotFound on 404.
	GetFileContent(ctx context.Context, owner, repo, path, ref string) (*FileContent, error)

	// CreateBranch creates a new branch off fromSHA. Refuses when
	// branchName equals the project's default branch.
	CreateBranch(ctx context.Context, owner, repo, branchName, fromSHA string) error

	// PutFileContent creates or updates a file on branch. Refuses when
	// branch equals the project's default branch.
	PutFileContent(ctx context.Context, opts PutFileOptions) (*CommitFileResult, error)

	// OpenPR opens a merge request with source = the new branch, target
	// = typically the default branch. Refuses when head equals the
	// project's default branch.
	OpenPR(ctx context.Context, opts OpenPROptions) (*PullRequest, error)

	// AddLabels attaches labels to the merge request. Refuses when the
	// MR's source branch equals the default branch.
	AddLabels(ctx context.Context, owner, repo string, prNumber int, labels []string) error

	// RequestReviewers asks the named teams to review the MR. Same
	// default-branch refusal posture as AddLabels.
	RequestReviewers(ctx context.Context, owner, repo string, prNumber int, teamSlugs []string) error
}

// PATClient is the Client implementation. It uses GitLab's PRIVATE-TOKEN
// auth header and a plain net/http.Client — no go-gitlab dep — matching
// the github wrapper's construction pattern.
type PATClient struct {
	token         string
	defaultBranch string // optional cache so write methods can refuse without a second GetRepo
	baseURL       string
	httpClient    *http.Client
}

// NewPATClient constructs a PATClient. token is the operator's GitLab
// personal access token (scope api). The token is held in memory for
// the lifetime of the client and never logged, never echoed in errors.
func NewPATClient(token string) *PATClient {
	return &PATClient{
		token:   token,
		baseURL: defaultBaseURL,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
	}
}

// WithBaseURL overrides the API root. Tests pass an httptest server
// URL; self-managed GitLab callers pass their host's /api/v4 root.
func (c *PATClient) WithBaseURL(baseURL string) *PATClient {
	c.baseURL = strings.TrimRight(baseURL, "/")
	return c
}

// WithDefaultBranch caches the project's default branch so write
// methods can refuse default-branch writes without a fresh GetRepo.
func (c *PATClient) WithDefaultBranch(branch string) *PATClient {
	c.defaultBranch = branch
	return c
}

// WithHTTPClient lets tests inject an http.Client.
func (c *PATClient) WithHTTPClient(hc *http.Client) *PATClient {
	c.httpClient = hc
	return c
}

// projectID URL-encodes the "owner/repo" identifier into the single
// path segment GitLab's :id parameter expects (the slash becomes %2F).
func projectID(owner, repo string) string {
	return url.PathEscape(owner + "/" + repo)
}

// do is the single I/O chokepoint. Sets the PRIVATE-TOKEN auth header,
// User-Agent, JSON Accept; reads the body once. The token never lands
// in a returned error string.
func (c *PATClient) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, []byte, error) {
	if !strings.HasPrefix(path, "http") {
		path = c.baseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, path, body)
	if err != nil {
		return nil, nil, fmt.Errorf("gitlab: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("gitlab: transport: %w", err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

// resolveDefaultBranch returns the cached default branch when set;
// otherwise round-trips GetRepo.
func (c *PATClient) resolveDefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	if c.defaultBranch != "" {
		return c.defaultBranch, nil
	}
	r, err := c.GetRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

// refuseIfDefault is the wrapper-layer guard.
func (c *PATClient) refuseIfDefault(ctx context.Context, owner, repo, branch string) error {
	defaultBranch, err := c.resolveDefaultBranch(ctx, owner, repo)
	if err != nil {
		return err
	}
	if branchEquals(branch, defaultBranch) {
		return ErrDefaultBranchWriteRefused
	}
	return nil
}

func branchEquals(a, b string) bool {
	return stripRefHeads(a) == stripRefHeads(b)
}

func stripRefHeads(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// GetRepo round-trips the project. Maps 404 → ErrRepoNotFound, 401 →
// ErrAuthFailed.
func (c *PATClient) GetRepo(ctx context.Context, owner, repo string) (*Repo, error) {
	path := "/projects/" + projectID(owner, repo)
	resp, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var r Repo
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("gitlab: decode project: %w", err)
		}
		return &r, nil
	case http.StatusUnauthorized:
		return nil, ErrAuthFailed
	case http.StatusNotFound:
		return nil, ErrRepoNotFound
	default:
		return nil, statusError("GetRepo", resp.StatusCode)
	}
}

// GetFileContent reads the file at path on ref via
// GET /projects/:id/repository/files/:file_path?ref=. GitLab requires a
// ref; an empty ref defaults to HEAD. The content field is base64 and
// is decoded here.
func (c *PATClient) GetFileContent(ctx context.Context, owner, repo, path, ref string) (*FileContent, error) {
	if ref == "" {
		ref = "HEAD"
	}
	apiPath := "/projects/" + projectID(owner, repo) +
		"/repository/files/" + url.PathEscape(path) + "?ref=" + url.QueryEscape(ref)
	resp, body, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var raw struct {
			Path     string `json:"file_path"`
			BlobID   string `json:"blob_id"`
			Encoding string `json:"encoding"`
			Size     int    `json:"size"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("gitlab: decode file content: %w", err)
		}
		// GitLab may wrap the base64 with newlines; strip before decode.
		cleaned := strings.NewReplacer("\n", "", "\r", "").Replace(raw.Content)
		decoded, derr := base64.StdEncoding.DecodeString(cleaned)
		if derr != nil {
			return nil, fmt.Errorf("gitlab: decode base64 file content: %w", derr)
		}
		return &FileContent{
			Path:           raw.Path,
			SHA:            raw.BlobID,
			Encoding:       raw.Encoding,
			Size:           raw.Size,
			DecodedContent: decoded,
		}, nil
	case http.StatusUnauthorized:
		return nil, ErrAuthFailed
	case http.StatusNotFound:
		return nil, ErrFileNotFound
	default:
		return nil, statusError("GetFileContent", resp.StatusCode)
	}
}

// ListTree returns every entry under the project at ref using the
// recursive repository/tree API, following GitLab's keyset/offset
// pagination (x-next-page header) up to maxTreePages. ref is optional
// (GitLab defaults to the default branch). Callers filter by
// Type=="blob". A 404 maps to ErrRepoNotFound.
func (c *PATClient) ListTree(ctx context.Context, owner, repo, ref string) ([]TreeEntry, error) {
	pid := projectID(owner, repo)
	var out []TreeEntry
	page := "1"
	for i := 0; i < maxTreePages; i++ {
		apiPath := "/projects/" + pid + "/repository/tree?recursive=true&per_page=100&page=" + url.QueryEscape(page)
		if ref != "" {
			apiPath += "&ref=" + url.QueryEscape(ref)
		}
		resp, body, err := c.do(ctx, http.MethodGet, apiPath, nil)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusOK:
			var batch []TreeEntry
			if err := json.Unmarshal(body, &batch); err != nil {
				return nil, fmt.Errorf("gitlab: decode tree: %w", err)
			}
			out = append(out, batch...)
		case http.StatusUnauthorized:
			return nil, ErrAuthFailed
		case http.StatusNotFound:
			return nil, ErrRepoNotFound
		default:
			return nil, statusError("ListTree", resp.StatusCode)
		}
		next := resp.Header.Get("x-next-page")
		if next == "" || next == page {
			break
		}
		page = next
	}
	return out, nil
}

// CreateBranch creates branchName pointing at fromSHA via
// POST /projects/:id/repository/branches?branch=&ref=. Refuses if
// branchName equals the project's default branch.
func (c *PATClient) CreateBranch(ctx context.Context, owner, repo, branchName, fromSHA string) error {
	if err := c.refuseIfDefault(ctx, owner, repo, branchName); err != nil {
		return err
	}
	apiPath := "/projects/" + projectID(owner, repo) + "/repository/branches" +
		"?branch=" + url.QueryEscape(stripRefHeads(branchName)) +
		"&ref=" + url.QueryEscape(fromSHA)
	resp, _, err := c.do(ctx, http.MethodPost, apiPath, nil)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusUnauthorized:
		return ErrAuthFailed
	case http.StatusNotFound:
		return ErrRepoNotFound
	default:
		return statusError("CreateBranch", resp.StatusCode)
	}
}

// PutFileContent creates (POST, FileSHA empty) or updates (PUT) a file
// at opts.Path on opts.Branch. Refuses when opts.Branch equals the
// project's default branch. GitLab's files endpoint does not return the
// blob/commit SHA, so CommitFileResult's fields are left empty.
func (c *PATClient) PutFileContent(ctx context.Context, opts PutFileOptions) (*CommitFileResult, error) {
	if err := c.refuseIfDefault(ctx, opts.Owner, opts.Repo, opts.Branch); err != nil {
		return nil, err
	}
	method := http.MethodPut
	if opts.FileSHA == "" {
		method = http.MethodPost
	}
	payload := map[string]any{
		"branch":         stripRefHeads(opts.Branch),
		"content":        base64.StdEncoding.EncodeToString(opts.Content),
		"encoding":       "base64",
		"commit_message": opts.Message,
	}
	b, _ := json.Marshal(payload)
	apiPath := "/projects/" + projectID(opts.Owner, opts.Repo) +
		"/repository/files/" + url.PathEscape(opts.Path)
	resp, _, err := c.do(ctx, method, apiPath, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return &CommitFileResult{}, nil
	case http.StatusUnauthorized:
		return nil, ErrAuthFailed
	case http.StatusNotFound:
		return nil, ErrRepoNotFound
	case http.StatusBadRequest:
		// A 400 on create (FileSHA empty) is GitLab's "a file with this
		// name already exists" — the analog of GitHub's 422-on-create.
		if opts.FileSHA == "" {
			return nil, ErrFileAlreadyExists
		}
		return nil, statusError("PutFileContent", resp.StatusCode)
	default:
		return nil, statusError("PutFileContent", resp.StatusCode)
	}
}

// OpenPR opens a merge request via POST /projects/:id/merge_requests.
// Refuses if opts.Head equals the project's default branch.
func (c *PATClient) OpenPR(ctx context.Context, opts OpenPROptions) (*PullRequest, error) {
	if err := c.refuseIfDefault(ctx, opts.Owner, opts.Repo, opts.Head); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"source_branch": stripRefHeads(opts.Head),
		"target_branch": stripRefHeads(opts.Base),
		"title":         opts.Title,
		"description":   opts.Body,
	}
	b, _ := json.Marshal(payload)
	apiPath := "/projects/" + projectID(opts.Owner, opts.Repo) + "/merge_requests"
	resp, body, err := c.do(ctx, http.MethodPost, apiPath, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var raw struct {
			IID    int    `json:"iid"`
			WebURL string `json:"web_url"`
			SHA    string `json:"sha"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("gitlab: decode merge request: %w", err)
		}
		return &PullRequest{
			Number:  raw.IID,
			HTMLURL: raw.WebURL,
			HeadSHA: raw.SHA,
		}, nil
	case http.StatusUnauthorized:
		return nil, ErrAuthFailed
	case http.StatusNotFound:
		return nil, ErrRepoNotFound
	default:
		return nil, statusError("OpenPR", resp.StatusCode)
	}
}

// AddLabels attaches labels to a merge request via
// PUT /projects/:id/merge_requests/:iid with add_labels. Refuses when
// the MR's source branch equals the default branch.
func (c *PATClient) AddLabels(ctx context.Context, owner, repo string, prNumber int, labels []string) error {
	head, err := c.getMRSourceBranch(ctx, owner, repo, prNumber)
	if err != nil {
		return err
	}
	if err := c.refuseIfDefault(ctx, owner, repo, head); err != nil {
		return err
	}
	payload := map[string]any{"add_labels": strings.Join(labels, ",")}
	b, _ := json.Marshal(payload)
	apiPath := "/projects/" + projectID(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber)
	resp, _, err := c.do(ctx, http.MethodPut, apiPath, bytes.NewReader(b))
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusUnauthorized:
		return ErrAuthFailed
	case http.StatusNotFound:
		return ErrRepoNotFound
	default:
		return statusError("AddLabels", resp.StatusCode)
	}
}

// RequestReviewers mirrors github.Client.RequestReviewers. GitLab has
// no team-slug reviewer request in its MR REST API (reviewers are set
// by numeric user id, and group review is an approval-rule concern), so
// this method preserves the interface and the default-branch refusal
// invariant, then treats the reviewer request as a best-effort soft
// success — the same posture the github wrapper takes on a 422 from
// GitHub's request-reviewers endpoint. It does not invent a GitLab
// call the platform does not offer.
func (c *PATClient) RequestReviewers(ctx context.Context, owner, repo string, prNumber int, teamSlugs []string) error {
	head, err := c.getMRSourceBranch(ctx, owner, repo, prNumber)
	if err != nil {
		return err
	}
	if err := c.refuseIfDefault(ctx, owner, repo, head); err != nil {
		return err
	}
	_ = teamSlugs
	return nil
}

// getMRSourceBranch pulls just the source_branch off a merge request so
// the default-branch refusal check has a string to compare against.
func (c *PATClient) getMRSourceBranch(ctx context.Context, owner, repo string, prNumber int) (string, error) {
	apiPath := "/projects/" + projectID(owner, repo) + "/merge_requests/" + strconv.Itoa(prNumber)
	resp, body, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var raw struct {
			SourceBranch string `json:"source_branch"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return "", fmt.Errorf("gitlab: decode merge request: %w", err)
		}
		return raw.SourceBranch, nil
	case http.StatusUnauthorized:
		return "", ErrAuthFailed
	case http.StatusNotFound:
		return "", ErrRepoNotFound
	default:
		return "", statusError("getMRSourceBranch", resp.StatusCode)
	}
}

// GetBranchSHA returns the SHA the named branch tip points at via
// GET /projects/:id/repository/branches/:branch. NOT part of the Client
// interface (mirrors the github package) — the handler asserts the
// concrete type for it.
func (c *PATClient) GetBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	branch = stripRefHeads(branch)
	apiPath := "/projects/" + projectID(owner, repo) +
		"/repository/branches/" + url.PathEscape(branch)
	resp, body, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var raw struct {
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return "", fmt.Errorf("gitlab: decode branch: %w", err)
		}
		return raw.Commit.ID, nil
	case http.StatusUnauthorized:
		return "", ErrAuthFailed
	case http.StatusNotFound:
		return "", ErrRepoNotFound
	default:
		return "", statusError("GetBranchSHA", resp.StatusCode)
	}
}

// statusError builds a stable error for unexpected status codes. The
// error string carries the operation name + the status — NEVER the
// response body (which GitLab can echo request data into).
func statusError(op string, status int) error {
	return fmt.Errorf("gitlab: %s: unexpected status %d", op, status)
}
