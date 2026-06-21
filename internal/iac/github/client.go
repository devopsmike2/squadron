// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package github is the slice-1 GitHub-API wrapper Squadron's IaC
// connect flow calls against. Slice 1 ships PAT auth only; the
// design doc (docs/proposals/603-connect-iac-repo.md §4) names a
// GitHub App path for slice 2 — the Client interface here is shaped
// so the App implementation can satisfy it without churning callers.
//
// Two cross-cutting invariants this package owns:
//
//  1. **Never write the default branch.** §9 of the design doc names
//     "compromised Squadron force-pushes the default branch" as a
//     defense-in-depth concern. The wrapper layer here refuses any
//     write whose ref equals the repo's default branch, BEFORE
//     issuing the underlying API call. The handler layer enforces the
//     same rule independently — belt-and-braces, on the principle
//     that the substrate must refuse the invariant violation even if
//     a future handler regression forgets to.
//
//  2. **Never log or echo the token.** The PAT is the secret. It
//     never lands in a log line, never lands in a returned error
//     string, never lands in any field this package exposes. On a 401
//     from GitHub the wrapper returns ErrAuthFailed with no body
//     bytes — the token is unrecoverable from the error.
package github

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
	"strings"
	"time"
)

// UserAgent is sent on every outbound request so the PR audit trail
// on the operator's repo identifies Squadron's writes. The version
// suffix tracks Squadron's release tag; bumping the suffix when a
// slice ships is harmless — GitHub does not parse the string.
const UserAgent = "Squadron/0.89.11 (Connect-IaC-Repo)"

// defaultBaseURL is the public GitHub.com API root. PATClient lets a
// caller override it for tests (httptest.NewServer) and, in a future
// slice, for GitHub Enterprise Server installs.
const defaultBaseURL = "https://api.github.com"

// defaultTimeout caps any single API call. Wraps the http.Client
// transport timeout so a slow GitHub never holds an open-PR handler
// past the operator's patience budget. 30s comfortably exceeds the
// happy-path RTT (sub-second for any single call here) while still
// bounding the worst case.
const defaultTimeout = 30 * time.Second

// Typed errors. Callers errors.Is against these sentinels — the
// handler layer maps them to humanized error envelopes for the UI.
var (
	// ErrDefaultBranchWriteRefused is returned by any write method
	// (CreateBranch, PutFileContent, OpenPR, AddLabels,
	// RequestReviewers) when the branch arg equals the repo's
	// default branch. The check fires at the wrapper layer BEFORE
	// the underlying HTTP request; no network call is made when this
	// fires. The handler layer enforces the same rule independently.
	ErrDefaultBranchWriteRefused = errors.New("github: refusing to write the repo's default branch")

	// ErrAuthFailed is returned on a GitHub 401. The error string
	// deliberately carries NO body bytes — a future GitHub error
	// page that quoted the token in plaintext would otherwise leak
	// the secret into our logs.
	ErrAuthFailed = errors.New("github: authentication failed")

	// ErrRepoNotFound is returned on a GitHub 404 from GetRepo. The
	// handler maps this to a humanized "the repo is no longer
	// reachable; re-run the IaC connect wizard" message.
	ErrRepoNotFound = errors.New("github: repository not found")

	// ErrFileNotFound is returned on a GitHub 404 from
	// GetFileContent. Distinct from ErrRepoNotFound so the wizard's
	// Validate step can surface "the repo is fine but this file
	// doesn't exist yet" — the placement-map row may legitimately
	// point at a not-yet-created file, in which case PutFileContent
	// will create it.
	ErrFileNotFound = errors.New("github: file not found at ref")

	// ErrFileAlreadyExists is returned by PutFileContent (and by
	// the slice-1.5 CreateFile variant in the handler) when the
	// caller asked to CREATE a file (FileSHA empty) but GitHub
	// returned 422 because a file already exists at that path on
	// the branch's base. v0.89.11 (#626 Stream 27) slice-1.5
	// disposition: the handler maps this to SquadronFileAlreadyExists
	// — the operator must close the prior open Squadron PR for the
	// same resource_kind before re-running Open PR. Slice 2 will
	// replace this with an HCL-aware merge.
	ErrFileAlreadyExists = errors.New("github: file already exists at path")
)

// Repo is the projected GetRepo response the rest of this package
// (and the handler layer) needs. The full GitHub response carries
// dozens of fields; we surface only DefaultBranch because that's the
// only one the IaC flow keys off.
type Repo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
}

// FileContent is the projected GetFileContent response. SHA is what
// PutFileContent supplies on update; DecodedContent is the raw bytes
// the caller appends to. Encoding ("base64") and Size are surfaced
// so a future caller can validate the upstream's framing — slice 1
// uses neither.
type FileContent struct {
	Path           string `json:"path"`
	SHA            string `json:"sha"`
	Encoding       string `json:"encoding"`
	Size           int    `json:"size"`
	DecodedContent []byte `json:"-"`
}

// PullRequest is the projected create-PR / get-PR response. Number
// + HTMLURL are what the audit payload and the UI need; HeadSHA is
// surfaced so a future "show me the commit my PR points at" flow can
// pivot off it without a second round-trip.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	HeadSHA string `json:"head_sha"`
}

// CommitFileResult is the projected PutFileContent response. SHA is
// the new file blob's SHA — a future "what did Squadron write?"
// surface can pivot off it. CommitSHA is the commit Squadron's write
// produced; the audit payload carries this so an auditor can
// reconstruct the on-repo state without re-reading GitHub.
type CommitFileResult struct {
	BlobSHA   string `json:"blob_sha"`
	CommitSHA string `json:"commit_sha"`
}

// OpenPROptions bundles the create-PR call's required fields. Body
// is the proposer-emitted reasoning prose + snippet block; Labels
// the design-doc-mandated `squadron` + `squadron/<resource_kind>`
// pair. Draft is left out of slice 1 — every Squadron PR ships ready
// for review.
type OpenPROptions struct {
	Owner string
	Repo  string
	Title string
	Body  string
	Head  string // the new branch
	Base  string // typically the repo's default branch
}

// PutFileOptions bundles the create-or-update-file-contents call.
// FileSHA is empty on create, the existing blob SHA on update —
// GitHub's contents API distinguishes the two by the presence of the
// `sha` field.
type PutFileOptions struct {
	Owner   string
	Repo    string
	Path    string
	Branch  string
	Content []byte
	Message string
	FileSHA string // empty for create, blob SHA for update
}

// Client is the surface the Squadron handlers program against. Slice
// 1's PATClient satisfies it; slice 2's app-installation client will
// satisfy the same interface so handlers don't change.
type Client interface {
	// GetRepo round-trips the repo. Returns ErrRepoNotFound on 404,
	// ErrAuthFailed on 401.
	GetRepo(ctx context.Context, owner, repo string) (*Repo, error)

	// GetFileContent reads the file at path on ref. ref may be a
	// branch name, tag, or commit SHA. Returns ErrFileNotFound on
	// 404, ErrRepoNotFound is NOT distinguishable here because
	// GitHub returns 404 for both — callers that need the
	// distinction should GetRepo first.
	GetFileContent(ctx context.Context, owner, repo, path, ref string) (*FileContent, error)

	// CreateBranch creates a new ref off fromSHA. Refuses when
	// branchName equals the repo's default branch.
	CreateBranch(ctx context.Context, owner, repo, branchName, fromSHA string) error

	// PutFileContent creates or updates a file on branch. Refuses
	// when branch equals the repo's default branch.
	PutFileContent(ctx context.Context, opts PutFileOptions) (*CommitFileResult, error)

	// OpenPR opens a pull request with head = the new branch, base =
	// typically the default branch. Refuses when head equals the
	// repo's default branch (the wrapper does not check base — base
	// being default is the entire point of opening a PR).
	OpenPR(ctx context.Context, opts OpenPROptions) (*PullRequest, error)

	// AddLabels attaches labels to the PR. Refuses when the PR's
	// head branch (looked up by GitHub) equals default — defense in
	// depth in case a caller hand-crafts a PR-number reference at a
	// PR that already shipped a default-branch update.
	AddLabels(ctx context.Context, owner, repo string, prNumber int, labels []string) error

	// RequestReviewers asks the named team to review the PR. Same
	// default-branch refusal posture as AddLabels.
	RequestReviewers(ctx context.Context, owner, repo string, prNumber int, teamSlugs []string) error
}

// PATClient is the slice-1 Client implementation. It uses the
// classic-PAT auth header and a plain net/http.Client — no
// go-github dep — because the API surface here is small enough that
// a 200-LOC wrapper costs less than the dep does.
type PATClient struct {
	token         string
	defaultBranch string // optional cache so write methods can refuse default-branch writes without a second GetRepo
	baseURL       string
	httpClient    *http.Client
}

// NewPATClient constructs a PATClient. token is the operator's
// classic PAT (scope `repo`). The token is held in memory for the
// lifetime of the client and never logged, never echoed in errors.
//
// The client is safe for concurrent use across multiple goroutines —
// http.Client already is, and PATClient only ever reads its own
// fields.
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
// URL; production callers do not call this. A future GitHub
// Enterprise Server slice will pass the operator's GHES URL.
func (c *PATClient) WithBaseURL(baseURL string) *PATClient {
	c.baseURL = strings.TrimRight(baseURL, "/")
	return c
}

// WithDefaultBranch caches the repo's default branch on the client
// so write methods can refuse default-branch writes without a fresh
// GetRepo round-trip. The handler layer calls this immediately after
// the first GetRepo. An empty string disables the cache (the wrapper
// then fetches the default branch on every write method).
func (c *PATClient) WithDefaultBranch(branch string) *PATClient {
	c.defaultBranch = branch
	return c
}

// WithHTTPClient lets tests inject an http.Client (e.g. one with a
// shorter timeout, or one wired to a recording transport). Production
// callers do not call this.
func (c *PATClient) WithHTTPClient(hc *http.Client) *PATClient {
	c.httpClient = hc
	return c
}

// do is the single I/O chokepoint. Sets auth headers, User-Agent,
// JSON Accept; reads the body once; maps GitHub status codes to typed
// errors. The token never lands in a returned error string.
func (c *PATClient) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, []byte, error) {
	if !strings.HasPrefix(path, "http") {
		path = c.baseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, path, body)
	if err != nil {
		// Defensive: NewRequest errors only on a malformed method or
		// URL — both of which would be programmer error here.
		return nil, nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport-level error (DNS, TCP, TLS, timeout). The error
		// string from http.Client.Do never carries the request
		// headers, so the token cannot leak this way — but we still
		// wrap to a stable message so callers don't depend on the
		// upstream's wording.
		return nil, nil, fmt.Errorf("github: transport: %w", err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

// resolveDefaultBranch returns the cached default branch when set;
// otherwise round-trips GetRepo. The write methods call this before
// the default-branch refusal check.
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

// refuseIfDefault is the wrapper-layer guard. branch is what the
// caller wants to write; if it equals the repo's default branch the
// method returns ErrDefaultBranchWriteRefused with NO underlying API
// call.
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

// branchEquals normalizes both forms a caller might supply:
// "main" and "refs/heads/main" should both compare equal to a
// default-branch value of "main". The check is intentionally strict —
// case-sensitive — because Git refs are case-sensitive.
func branchEquals(a, b string) bool {
	return stripRefHeads(a) == stripRefHeads(b)
}

func stripRefHeads(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// GetRepo round-trips the repo. Maps 404 → ErrRepoNotFound, 401 →
// ErrAuthFailed.
func (c *PATClient) GetRepo(ctx context.Context, owner, repo string) (*Repo, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	resp, body, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var r Repo
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("github: decode repo: %w", err)
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

// GetFileContent reads the file at path on ref. Returns
// ErrFileNotFound on 404 — slice 1's Validate path treats a not-yet-
// created file as legitimate (PutFileContent will create it on the
// new branch).
func (c *PATClient) GetFileContent(ctx context.Context, owner, repo, path, ref string) (*FileContent, error) {
	apiPath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/" + escapeFilePath(path)
	if ref != "" {
		apiPath += "?ref=" + url.QueryEscape(ref)
	}
	resp, body, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// GitHub returns either an object (single file) or an array
		// (directory). Slice 1 only ever reads files; an array
		// response means the caller asked for a directory, which we
		// treat as ErrFileNotFound since there's no single file blob
		// for the PR builder to update.
		if len(body) > 0 && body[0] == '[' {
			return nil, ErrFileNotFound
		}
		var raw struct {
			Path     string `json:"path"`
			SHA      string `json:"sha"`
			Encoding string `json:"encoding"`
			Size     int    `json:"size"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("github: decode file content: %w", err)
		}
		// GitHub wraps the base64 with newlines every 60 chars per
		// the MIME convention. base64.StdEncoding tolerates internal
		// whitespace via a manual strip.
		cleaned := strings.NewReplacer("\n", "", "\r", "").Replace(raw.Content)
		decoded, derr := base64.StdEncoding.DecodeString(cleaned)
		if derr != nil {
			return nil, fmt.Errorf("github: decode base64 file content: %w", derr)
		}
		return &FileContent{
			Path:           raw.Path,
			SHA:            raw.SHA,
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

// CreateBranch creates refs/heads/<branchName> pointing at fromSHA.
// Refuses if branchName equals the repo's default branch.
func (c *PATClient) CreateBranch(ctx context.Context, owner, repo, branchName, fromSHA string) error {
	if err := c.refuseIfDefault(ctx, owner, repo, branchName); err != nil {
		return err
	}
	payload := map[string]string{
		"ref": "refs/heads/" + stripRefHeads(branchName),
		"sha": fromSHA,
	}
	b, _ := json.Marshal(payload)
	apiPath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/git/refs"
	resp, _, err := c.do(ctx, http.MethodPost, apiPath, bytes.NewReader(b))
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

// PutFileContent creates or updates a file at opts.Path on
// opts.Branch. Refuses when opts.Branch equals the repo's default
// branch.
func (c *PATClient) PutFileContent(ctx context.Context, opts PutFileOptions) (*CommitFileResult, error) {
	if err := c.refuseIfDefault(ctx, opts.Owner, opts.Repo, opts.Branch); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"message": opts.Message,
		"content": base64.StdEncoding.EncodeToString(opts.Content),
		"branch":  stripRefHeads(opts.Branch),
	}
	if opts.FileSHA != "" {
		payload["sha"] = opts.FileSHA
	}
	b, _ := json.Marshal(payload)
	apiPath := "/repos/" + url.PathEscape(opts.Owner) + "/" + url.PathEscape(opts.Repo) + "/contents/" + escapeFilePath(opts.Path)
	resp, body, err := c.do(ctx, http.MethodPut, apiPath, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var raw struct {
			Content struct {
				SHA string `json:"sha"`
			} `json:"content"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("github: decode put-file response: %w", err)
		}
		return &CommitFileResult{
			BlobSHA:   raw.Content.SHA,
			CommitSHA: raw.Commit.SHA,
		}, nil
	case http.StatusUnauthorized:
		return nil, ErrAuthFailed
	case http.StatusNotFound:
		return nil, ErrRepoNotFound
	case http.StatusUnprocessableEntity:
		// 422 on create (FileSHA empty) almost always means a file
		// already exists at the path. v0.89.11 (#626 Stream 27)
		// surfaces this as a typed sentinel so the handler can
		// map it to the slice-1.5 SquadronFileAlreadyExists humanized
		// error. On UPDATE (FileSHA non-empty) a 422 is the
		// "sha mismatch" / "branch protection" class — leave the
		// generic statusError path.
		if opts.FileSHA == "" {
			return nil, ErrFileAlreadyExists
		}
		return nil, statusError("PutFileContent", resp.StatusCode)
	default:
		return nil, statusError("PutFileContent", resp.StatusCode)
	}
}

// OpenPR opens a pull request. Refuses if opts.Head equals the
// repo's default branch.
func (c *PATClient) OpenPR(ctx context.Context, opts OpenPROptions) (*PullRequest, error) {
	if err := c.refuseIfDefault(ctx, opts.Owner, opts.Repo, opts.Head); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"title": opts.Title,
		"body":  opts.Body,
		"head":  stripRefHeads(opts.Head),
		"base":  stripRefHeads(opts.Base),
	}
	b, _ := json.Marshal(payload)
	apiPath := "/repos/" + url.PathEscape(opts.Owner) + "/" + url.PathEscape(opts.Repo) + "/pulls"
	resp, body, err := c.do(ctx, http.MethodPost, apiPath, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var raw struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("github: decode pr response: %w", err)
		}
		return &PullRequest{
			Number:  raw.Number,
			HTMLURL: raw.HTMLURL,
			HeadSHA: raw.Head.SHA,
		}, nil
	case http.StatusUnauthorized:
		return nil, ErrAuthFailed
	case http.StatusNotFound:
		return nil, ErrRepoNotFound
	default:
		return nil, statusError("OpenPR", resp.StatusCode)
	}
}

// AddLabels attaches labels to a PR (which is also an Issue on
// GitHub's data model — the /issues/:n/labels endpoint serves both).
//
// The default-branch refusal here is precautionary: a caller that
// hand-crafts a prNumber pointing at a PR whose head IS default
// branch (impossible in slice 1 — we don't open such PRs — but the
// invariant must hold against future regressions) is refused before
// the call lands.
func (c *PATClient) AddLabels(ctx context.Context, owner, repo string, prNumber int, labels []string) error {
	// Resolve the PR's head branch and refuse if it matches default.
	head, err := c.getPRHeadRef(ctx, owner, repo, prNumber)
	if err != nil {
		return err
	}
	if err := c.refuseIfDefault(ctx, owner, repo, head); err != nil {
		return err
	}
	payload := map[string]any{"labels": labels}
	b, _ := json.Marshal(payload)
	apiPath := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", url.PathEscape(owner), url.PathEscape(repo), prNumber)
	resp, _, err := c.do(ctx, http.MethodPost, apiPath, bytes.NewReader(b))
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

// RequestReviewers asks the named teams to review the PR. teamSlugs
// is the bare team slug (the org is the repo's owner). Same
// default-branch refusal posture as AddLabels.
func (c *PATClient) RequestReviewers(ctx context.Context, owner, repo string, prNumber int, teamSlugs []string) error {
	head, err := c.getPRHeadRef(ctx, owner, repo, prNumber)
	if err != nil {
		return err
	}
	if err := c.refuseIfDefault(ctx, owner, repo, head); err != nil {
		return err
	}
	payload := map[string]any{"team_reviewers": teamSlugs}
	b, _ := json.Marshal(payload)
	apiPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/requested_reviewers", url.PathEscape(owner), url.PathEscape(repo), prNumber)
	resp, _, err := c.do(ctx, http.MethodPost, apiPath, bytes.NewReader(b))
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusUnprocessableEntity:
		// 422 from this endpoint typically means "no such team" or
		// "team already a reviewer" — neither is a hard failure for
		// the slice-1 flow. The handler treats 422 as soft so the PR
		// still ships.
		return nil
	case http.StatusUnauthorized:
		return ErrAuthFailed
	case http.StatusNotFound:
		return ErrRepoNotFound
	default:
		return statusError("RequestReviewers", resp.StatusCode)
	}
}

// getPRHeadRef pulls just the head.ref off a PR so the default-branch
// refusal check has a string to compare against.
func (c *PATClient) getPRHeadRef(ctx context.Context, owner, repo string, prNumber int) (string, error) {
	apiPath := fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(owner), url.PathEscape(repo), prNumber)
	resp, body, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var raw struct {
			Head struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return "", fmt.Errorf("github: decode pr: %w", err)
		}
		return raw.Head.Ref, nil
	case http.StatusUnauthorized:
		return "", ErrAuthFailed
	case http.StatusNotFound:
		return "", ErrRepoNotFound
	default:
		return "", statusError("getPRHeadRef", resp.StatusCode)
	}
}

// GetBranchSHA returns the SHA the named branch tip points at. The
// handler reads this to use as the parent SHA for the new branch
// CreateBranch creates. NOT part of the Client interface in slice 1
// — the handler asserts the concrete type — so that adding the
// method does not churn future Client implementations.
func (c *PATClient) GetBranchSHA(ctx context.Context, owner, repo, branch string) (string, error) {
	branch = stripRefHeads(branch)
	apiPath := fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(branch))
	resp, body, err := c.do(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var raw struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return "", fmt.Errorf("github: decode branch ref: %w", err)
		}
		return raw.Object.SHA, nil
	case http.StatusUnauthorized:
		return "", ErrAuthFailed
	case http.StatusNotFound:
		return "", ErrRepoNotFound
	default:
		return "", statusError("GetBranchSHA", resp.StatusCode)
	}
}

// escapeFilePath escapes each segment of a repo-relative file path
// but preserves slashes (the contents API takes /<owner>/<repo>/
// contents/<path/with/slashes/preserved>).
func escapeFilePath(p string) string {
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}

// statusError builds a stable error for unexpected status codes. The
// error string carries the operation name + the status — NEVER the
// response body. GitHub error pages can quote request data in
// pathological cases; we won't propagate that to our callers.
func statusError(op string, status int) error {
	return fmt.Errorf("github: %s: unexpected status %d", op, status)
}
