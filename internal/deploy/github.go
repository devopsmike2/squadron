// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"bytes"
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// GitHubBaseURL is the GitHub REST API base. Overridable for
// GitHub Enterprise (set it to https://github.your-co.com/api/v3
// when constructing the provider).
const GitHubBaseURL = "https://api.github.com"

// GitHubProvider implements Provider against api.github.com. The
// PAT needs `actions:write` on the target repo for Dispatch, and
// `actions:read` for GetRun/LatestRunSince. Tokens come from the
// caller per-request rather than being stashed on the provider
// instance — that keeps the secret short-lived in memory.
type GitHubProvider struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGitHubProvider constructs a provider with a 30s HTTP timeout.
// Pass a baseURL to override for GitHub Enterprise.
func NewGitHubProvider(baseURL string) *GitHubProvider {
	if baseURL == "" {
		baseURL = GitHubBaseURL
	}
	return &GitHubProvider{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Dispatch fires a workflow_dispatch event. GitHub's API returns
// 204 No Content with no body, so we don't get the run_id back —
// the service follows up with LatestRunSince to attach it.
//
// inputs are forwarded as the workflow's `inputs` block; the
// workflow file must declare each key under `on.workflow_dispatch.inputs`
// or GitHub will reject the dispatch with a 422.
func (g *GitHubProvider) Dispatch(
	ctx context.Context,
	target *apptypes.DeployTarget,
	pat string,
	inputs map[string]string,
) (string, error) {
	if target == nil {
		return "", fmt.Errorf("target nil")
	}
	if pat == "" {
		return "", fmt.Errorf("PAT required")
	}
	if target.GitHubOwner == "" || target.GitHubRepo == "" || target.GitHubWorkflow == "" {
		return "", fmt.Errorf("github_owner, github_repo, and github_workflow are required on the target")
	}
	branch := target.GitHubBranch
	if branch == "" {
		branch = "main"
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/dispatches",
		g.BaseURL,
		url.PathEscape(target.GitHubOwner),
		url.PathEscape(target.GitHubRepo),
		url.PathEscape(target.GitHubWorkflow))

	body := map[string]interface{}{
		"ref":    branch,
		"inputs": inputs,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("build dispatch request: %w", err)
	}
	g.setAuthHeaders(req, pat)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("dispatch request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return "", classifyError(resp)
	}
	// GitHub returns 204 with no run_id; LatestRunSince picks it up
	// on the follow-up.
	return "", nil
}

// GetRun fetches a single run's status by ID. Used by the polling
// fallback.
func (g *GitHubProvider) GetRun(
	ctx context.Context,
	target *apptypes.DeployTarget,
	pat string,
	runID int64,
) (*RunStatus, error) {
	if pat == "" {
		return nil, fmt.Errorf("PAT required")
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d",
		g.BaseURL,
		url.PathEscape(target.GitHubOwner),
		url.PathEscape(target.GitHubRepo),
		runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build run request: %w", err)
	}
	g.setAuthHeaders(req, pat)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("run request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp)
	}
	return parseRun(resp.Body)
}

// LatestRunSince finds the newest run on the target's workflow
// that started at or after `since`. Used immediately after dispatch
// to learn the run_id GitHub didn't return.
//
// Returns nil with no error when no qualifying run exists yet
// (workflow_dispatch is async; the run may not be visible for a
// second or two after dispatch returns).
func (g *GitHubProvider) LatestRunSince(
	ctx context.Context,
	target *apptypes.DeployTarget,
	pat string,
	since time.Time,
) (*RunStatus, error) {
	if pat == "" {
		return nil, fmt.Errorf("PAT required")
	}
	// We filter by event=workflow_dispatch + branch + the per_page=1
	// trick to grab just the newest. created>=since narrows it
	// further; GitHub honors ISO-8601 here.
	params := url.Values{}
	params.Set("event", "workflow_dispatch")
	params.Set("per_page", "5") // small page; we filter in-process
	params.Set("created", ">="+since.UTC().Format(time.RFC3339))
	branch := target.GitHubBranch
	if branch == "" {
		branch = "main"
	}
	params.Set("branch", branch)

	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/runs?%s",
		g.BaseURL,
		url.PathEscape(target.GitHubOwner),
		url.PathEscape(target.GitHubRepo),
		url.PathEscape(target.GitHubWorkflow),
		params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build runs request: %w", err)
	}
	g.setAuthHeaders(req, pat)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runs request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp)
	}

	var page struct {
		TotalCount   int `json:"total_count"`
		WorkflowRuns []struct {
			ID         int64     `json:"id"`
			Status     string    `json:"status"`
			Conclusion string    `json:"conclusion"`
			HTMLURL    string    `json:"html_url"`
			CreatedAt  time.Time `json:"created_at"`
			UpdatedAt  time.Time `json:"updated_at"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode runs: %w", err)
	}
	if len(page.WorkflowRuns) == 0 {
		return nil, nil
	}
	// Newest first: GitHub orders by created_at desc.
	newest := page.WorkflowRuns[0]
	rs := &RunStatus{
		GitHubRunID:  newest.ID,
		GitHubRunURL: newest.HTMLURL,
		Status:       newest.Status,
		Conclusion:   newest.Conclusion,
		StartedAt:    newest.CreatedAt,
	}
	if newest.Status == "completed" {
		t := newest.UpdatedAt
		rs.CompletedAt = &t
	}
	return rs, nil
}

// FetchFile pulls a file from the target's repo via the GitHub
// Contents API. Used by v0.34.1 to read inventory.ini at trigger
// time. The PAT needs `contents:read` (already granted alongside
// `actions:write` for the dispatch path, so no separate token
// configuration needed).
//
// Returns the decoded file bytes. Files larger than the Contents
// API's 1MB cap can't be fetched this way — GitHub serves them via
// the Git blob API instead — but inventory files are far below that
// limit so we don't fall back.
func (g *GitHubProvider) FetchFile(
	ctx context.Context,
	target *apptypes.DeployTarget,
	pat string,
	path string,
) ([]byte, error) {
	if pat == "" {
		return nil, fmt.Errorf("PAT required")
	}
	if path == "" {
		return nil, fmt.Errorf("path required")
	}
	branch := target.GitHubBranch
	if branch == "" {
		branch = "main"
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		g.BaseURL,
		url.PathEscape(target.GitHubOwner),
		url.PathEscape(target.GitHubRepo),
		// Path segments must NOT be URL-escaped wholesale (would
		// turn slashes into %2F and 404 the request) but the
		// individual segments do need encoding for spaces etc.
		encodePathSegments(path),
		url.QueryEscape(branch),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build contents request: %w", err)
	}
	g.setAuthHeaders(req, pat)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contents request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp)
	}
	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int64  `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode contents: %w", err)
	}
	if body.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected content encoding %q (expected base64)", body.Encoding)
	}
	// GitHub wraps the base64 content in newlines every 60 chars;
	// base64.StdEncoding.DecodeString tolerates them with the
	// strings.ReplaceAll. RawStdEncoding wouldn't.
	decoded, err := base64Decode(body.Content)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	return decoded, nil
}

// ListSuccessfulRuns enumerates past successful workflow_dispatch
// runs of the target's workflow that ran on or after `since`. The
// GHA history walker uses this to find historical inventory
// snapshots to backfill.
//
// Pagination: GitHub's runs API supports `per_page` up to 100.
// We request 100 here; callers wanting a larger window iterate
// with the `page` query param (TODO when we have a real install
// that needs > 100 runs in the lookback).
func (g *GitHubProvider) ListSuccessfulRuns(
	ctx context.Context,
	target *apptypes.DeployTarget,
	pat string,
	since time.Time,
) ([]WorkflowRunSummary, error) {
	if pat == "" {
		return nil, fmt.Errorf("PAT required")
	}
	branch := target.GitHubBranch
	if branch == "" {
		branch = "main"
	}
	params := url.Values{}
	params.Set("event", "workflow_dispatch")
	params.Set("status", "success")
	params.Set("per_page", "100")
	params.Set("branch", branch)
	if !since.IsZero() {
		params.Set("created", ">="+since.UTC().Format(time.RFC3339))
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s/runs?%s",
		g.BaseURL,
		url.PathEscape(target.GitHubOwner),
		url.PathEscape(target.GitHubRepo),
		url.PathEscape(target.GitHubWorkflow),
		params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build runs request: %w", err)
	}
	g.setAuthHeaders(req, pat)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runs request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classifyError(resp)
	}
	var page struct {
		WorkflowRuns []struct {
			ID         int64     `json:"id"`
			HeadSHA    string    `json:"head_sha"`
			HeadBranch string    `json:"head_branch"`
			CreatedAt  time.Time `json:"created_at"`
			HTMLURL    string    `json:"html_url"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode runs: %w", err)
	}
	out := make([]WorkflowRunSummary, 0, len(page.WorkflowRuns))
	for _, r := range page.WorkflowRuns {
		out = append(out, WorkflowRunSummary{
			RunID:     r.ID,
			HeadSHA:   r.HeadSHA,
			Branch:    r.HeadBranch,
			CreatedAt: r.CreatedAt,
			URL:       r.HTMLURL,
		})
	}
	return out, nil
}

// FetchFileAtRef is FetchFile but at an arbitrary git ref (branch,
// tag, or commit SHA). Used by the GHA walker to pull historical
// inventory.ini snapshots.
func (g *GitHubProvider) FetchFileAtRef(
	ctx context.Context,
	target *apptypes.DeployTarget,
	pat string,
	path string,
	ref string,
) ([]byte, error) {
	// FetchFile takes the branch off the target. Swap the target's
	// branch for the requested ref via a shallow copy so we don't
	// duplicate the request-building logic.
	clone := *target
	clone.GitHubBranch = ref
	return g.FetchFile(ctx, &clone, pat, path)
}

// ProbeAuth verifies the PAT can read the repo. Issues
// GET /repos/{owner}/{repo} which is the cheapest authenticated
// call against a private repo. 200 = good, 401 = revoked, 404 =
// PAT can't see the repo (most likely fine-grained PAT scoped
// to a different repo).
func (g *GitHubProvider) ProbeAuth(
	ctx context.Context,
	target *apptypes.DeployTarget,
	pat string,
) error {
	if pat == "" {
		return fmt.Errorf("PAT required")
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s",
		g.BaseURL,
		url.PathEscape(target.GitHubOwner),
		url.PathEscape(target.GitHubRepo))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build auth probe: %w", err)
	}
	g.setAuthHeaders(req, pat)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("auth probe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyError(resp)
	}
	return nil
}

// ProbeWorkflow confirms the workflow file exists at the configured
// branch. GET /repos/{owner}/{repo}/actions/workflows/{file}.
// Returns the standard classifyError shape on 404 so the UI surfaces
// the actual GitHub error message rather than a generic "not found."
func (g *GitHubProvider) ProbeWorkflow(
	ctx context.Context,
	target *apptypes.DeployTarget,
	pat string,
) error {
	if pat == "" {
		return fmt.Errorf("PAT required")
	}
	if target.GitHubWorkflow == "" {
		return fmt.Errorf("workflow file not configured")
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/workflows/%s",
		g.BaseURL,
		url.PathEscape(target.GitHubOwner),
		url.PathEscape(target.GitHubRepo),
		url.PathEscape(target.GitHubWorkflow))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build workflow probe: %w", err)
	}
	g.setAuthHeaders(req, pat)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("workflow probe: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return classifyError(resp)
	}
	return nil
}

// encodePathSegments escapes each path segment between '/'s but
// preserves the slashes themselves. url.PathEscape on the whole
// path would turn slashes into %2F and produce a 404.
func encodePathSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// base64Decode strips the GitHub Contents API's wrap-newlines and
// decodes. Separated out so we don't pull a heavy import alias into
// every other function.
func base64Decode(s string) ([]byte, error) {
	cleaned := strings.ReplaceAll(strings.ReplaceAll(s, "\n", ""), "\r", "")
	return b64.StdEncoding.DecodeString(cleaned)
}

// setAuthHeaders sets the standard set of headers GitHub expects:
// Accept (versioned), Authorization (Bearer), X-GitHub-Api-Version.
// PAT is taken via parameter rather than instance to keep the
// secret short-lived in memory.
func (g *GitHubProvider) setAuthHeaders(req *http.Request, pat string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Squadron-deploy/1.0")
}

// parseRun decodes a single workflow run response.
func parseRun(body io.Reader) (*RunStatus, error) {
	var run struct {
		ID         int64     `json:"id"`
		Status     string    `json:"status"`
		Conclusion string    `json:"conclusion"`
		HTMLURL    string    `json:"html_url"`
		CreatedAt  time.Time `json:"created_at"`
		UpdatedAt  time.Time `json:"updated_at"`
	}
	if err := json.NewDecoder(body).Decode(&run); err != nil {
		return nil, fmt.Errorf("decode run: %w", err)
	}
	rs := &RunStatus{
		GitHubRunID:  run.ID,
		GitHubRunURL: run.HTMLURL,
		Status:       run.Status,
		Conclusion:   run.Conclusion,
		StartedAt:    run.CreatedAt,
	}
	if run.Status == "completed" {
		t := run.UpdatedAt
		rs.CompletedAt = &t
	}
	return rs, nil
}

// classifyError turns a non-2xx GitHub response into a typed error
// with the API's message. The body is read up to 4 KiB so a huge
// HTML error page doesn't blow the log.
func classifyError(resp *http.Response) error {
	const cap = 4096
	body, _ := io.ReadAll(io.LimitReader(resp.Body, cap))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("github 401 unauthorized — token revoked or wrong scope: %s", string(body))
	case http.StatusForbidden:
		return fmt.Errorf("github 403 forbidden — token lacks actions:write or rate-limited: %s", string(body))
	case http.StatusNotFound:
		return fmt.Errorf("github 404 not found — owner/repo/workflow incorrect or token can't see them: %s", string(body))
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("github 422 — workflow input mismatch (check on.workflow_dispatch.inputs declarations): %s", string(body))
	default:
		return fmt.Errorf("github %d: %s", resp.StatusCode, string(body))
	}
}
