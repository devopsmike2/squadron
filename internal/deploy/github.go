// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
