// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// AzureDevOpsBaseURL is the public Azure DevOps Services REST API
// host. Override with NewAzureDevOpsProvider("https://tfs.your-co.com")
// for self-hosted Azure DevOps Server (formerly TFS) installations.
const AzureDevOpsBaseURL = "https://dev.azure.com"

// AzureDevOpsProvider implements Provider against the Azure DevOps
// REST API (api-version=7.1). PATs need scopes:
//
//   - Build (Read & execute)  — for run/dispatch
//   - Code (Read)              — for FetchFile/FetchFileAtRef
//
// Field mapping from the shared DeployTarget shape onto Azure
// DevOps terminology:
//
//   - GitHubOwner    → Azure DevOps "organization" (the URL segment)
//   - GitHubRepo     → Azure DevOps "project" (or "project/repo" if
//                       the repo name differs from the project)
//   - GitHubWorkflow → Azure DevOps "pipeline ID" as a string
//   - GitHubBranch   → branch to dispatch on
//
// Reusing the existing columns saves a schema migration in v0.41.0;
// the field semantics are documented here and surfaced in the UI
// labels when the operator picks "Azure DevOps" as the provider.
//
// Added in v0.41.0 (connectors part 1).
type AzureDevOpsProvider struct {
	BaseURL string
	HTTP    *http.Client
}

// NewAzureDevOpsProvider builds a provider with a 30s HTTP timeout.
// Pass a baseURL to override for self-hosted Azure DevOps Server.
func NewAzureDevOpsProvider(baseURL string) *AzureDevOpsProvider {
	if baseURL == "" {
		baseURL = AzureDevOpsBaseURL
	}
	return &AzureDevOpsProvider{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Project/repo path helper. Azure DevOps lets you pack repo into the
// project name like "MyProject/MyRepo" — we split on the first "/"
// and treat the suffix as the repo when present. Without a suffix,
// project name == repo name (the common default).
func (p *AzureDevOpsProvider) splitProjectRepo(t *apptypes.DeployTarget) (project, repo string) {
	parts := strings.SplitN(t.GitHubRepo, "/", 2)
	project = parts[0]
	if len(parts) == 2 && parts[1] != "" {
		repo = parts[1]
	} else {
		repo = parts[0]
	}
	return
}

// authHeader builds the Basic-auth header value. Azure DevOps PATs
// authenticate as Basic with an empty username and the PAT as the
// password. The pattern is identical to what az-cli emits.
func authHeader(pat string) string {
	creds := ":" + pat
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

// do is the inner HTTP helper. Adds the auth header and JSON
// content-type, runs the request, and decodes JSON into `out` when
// non-nil. A non-2xx response returns a typed error with the body
// truncated to 2KB — enough to surface Azure DevOps's error code
// strings without flooding logs.
func (p *AzureDevOpsProvider) do(ctx context.Context, method, urlStr, pat string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, bodyReader)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", authHeader(pat))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		// Cap to 2KB so a giant HTML response from a misconfigured
		// proxy doesn't fill the logs.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("azure devops %d %s: %s", resp.StatusCode, method, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// runResponse is the shape Azure DevOps returns for a single pipeline
// run. We pluck just the fields Squadron stores — state/result/
// timestamps — and ignore the rest of the (very large) payload.
type adoRunResponse struct {
	ID           int64      `json:"id"`
	URL          string     `json:"url"`
	Links        adoLinks   `json:"_links"`
	State        string     `json:"state"`  // "inProgress" | "completed" | "canceling"
	Result       string     `json:"result"` // "succeeded" | "failed" | "canceled" | "skipped"
	CreatedDate  time.Time  `json:"createdDate"`
	FinishedDate *time.Time `json:"finishedDate,omitempty"`
}

type adoLinks struct {
	Web struct {
		Href string `json:"href"`
	} `json:"web"`
}

func (r adoRunResponse) toRunStatus() *RunStatus {
	return &RunStatus{
		GitHubRunID:  r.ID,        // shared shape — UI labels this neutrally
		GitHubRunURL: r.Links.Web.Href,
		Status:       mapADOState(r.State),
		Conclusion:   mapADOResult(r.Result),
		StartedAt:    r.CreatedDate,
		CompletedAt:  r.FinishedDate,
	}
}

func mapADOState(s string) string {
	switch s {
	case "inProgress":
		return "in_progress"
	case "completed":
		return "completed"
	default:
		// "canceling" and unknown states map to queued — Squadron
		// will catch up on the next sync poll.
		return "queued"
	}
}

func mapADOResult(s string) string {
	switch s {
	case "succeeded":
		return "success"
	case "failed":
		return "failure"
	case "canceled":
		return "cancelled"
	case "skipped":
		return "skipped"
	default:
		return ""
	}
}

// Dispatch fires a pipeline run.
//
// API: POST https://dev.azure.com/{org}/{project}/_apis/pipelines/{pipelineId}/runs?api-version=7.1
// Body: { "resources": { "repositories": { "self": { "refName": "refs/heads/<branch>" } } },
//         "templateParameters": { ...inputs... } }
//
// Returns the run ID synchronously — unlike GitHub Actions, Azure
// DevOps gives you the run identifier in the response body, so the
// "LatestRunSince fallback" dance isn't needed.
func (p *AzureDevOpsProvider) Dispatch(ctx context.Context, target *apptypes.DeployTarget, pat string, inputs map[string]string) (string, error) {
	project, _ := p.splitProjectRepo(target)
	branch := target.GitHubBranch
	if branch == "" {
		branch = "main"
	}
	body := map[string]any{
		"resources": map[string]any{
			"repositories": map[string]any{
				"self": map[string]any{
					"refName": "refs/heads/" + branch,
				},
			},
		},
		"templateParameters": inputs,
	}
	u := fmt.Sprintf("%s/%s/%s/_apis/pipelines/%s/runs?api-version=7.1",
		p.BaseURL, url.PathEscape(target.GitHubOwner), url.PathEscape(project),
		url.PathEscape(target.GitHubWorkflow))
	var run adoRunResponse
	if err := p.do(ctx, http.MethodPost, u, pat, body, &run); err != nil {
		return "", err
	}
	if run.ID == 0 {
		return "", fmt.Errorf("azure devops returned no run id")
	}
	return strconv.FormatInt(run.ID, 10), nil
}

// GetRun fetches the current state of a known run.
func (p *AzureDevOpsProvider) GetRun(ctx context.Context, target *apptypes.DeployTarget, pat string, runID int64) (*RunStatus, error) {
	project, _ := p.splitProjectRepo(target)
	u := fmt.Sprintf("%s/%s/%s/_apis/pipelines/%s/runs/%d?api-version=7.1",
		p.BaseURL, url.PathEscape(target.GitHubOwner), url.PathEscape(project),
		url.PathEscape(target.GitHubWorkflow), runID)
	var run adoRunResponse
	if err := p.do(ctx, http.MethodGet, u, pat, nil, &run); err != nil {
		return nil, err
	}
	return run.toRunStatus(), nil
}

// LatestRunSince scans the most-recent N runs and returns the newest
// one created after `since`. Azure DevOps responds with the most-
// recent first by default; we walk forward until we find one that
// predates `since`, then return the most recent candidate we saw.
func (p *AzureDevOpsProvider) LatestRunSince(ctx context.Context, target *apptypes.DeployTarget, pat string, since time.Time) (*RunStatus, error) {
	project, _ := p.splitProjectRepo(target)
	u := fmt.Sprintf("%s/%s/%s/_apis/pipelines/%s/runs?api-version=7.1",
		p.BaseURL, url.PathEscape(target.GitHubOwner), url.PathEscape(project),
		url.PathEscape(target.GitHubWorkflow))
	var resp struct {
		Value []adoRunResponse `json:"value"`
	}
	if err := p.do(ctx, http.MethodGet, u, pat, nil, &resp); err != nil {
		return nil, err
	}
	for _, r := range resp.Value {
		if r.CreatedDate.Before(since) {
			break
		}
		return r.toRunStatus(), nil // most-recent matching
	}
	return nil, nil
}

// FetchFile reads a file from the project's default branch (or the
// branch on the target). Used by inventory parsing.
//
// API: GET .../{project}/_apis/git/repositories/{repo}/items?path={path}&versionDescriptor.version={branch}&api-version=7.1
func (p *AzureDevOpsProvider) FetchFile(ctx context.Context, target *apptypes.DeployTarget, pat string, path string) ([]byte, error) {
	branch := target.GitHubBranch
	if branch == "" {
		branch = "main"
	}
	return p.fetchFileAt(ctx, target, pat, path, "branch", branch)
}

// FetchFileAtRef reads a file at a specific commit SHA — used by the
// history walker so each past deploy gets the inventory.ini snapshot
// that was actually deployed.
func (p *AzureDevOpsProvider) FetchFileAtRef(ctx context.Context, target *apptypes.DeployTarget, pat string, path string, ref string) ([]byte, error) {
	return p.fetchFileAt(ctx, target, pat, path, "commit", ref)
}

// fetchFileAt does the actual fetch with a configurable version
// descriptor. `versionType` is "branch" or "commit" per the Azure
// DevOps versionDescriptor model.
func (p *AzureDevOpsProvider) fetchFileAt(ctx context.Context, target *apptypes.DeployTarget, pat string, path string, versionType string, version string) ([]byte, error) {
	project, repo := p.splitProjectRepo(target)
	q := url.Values{}
	q.Set("path", path)
	q.Set("api-version", "7.1")
	q.Set("$format", "text")
	q.Set("versionDescriptor.version", version)
	q.Set("versionDescriptor.versionType", versionType)
	u := fmt.Sprintf("%s/%s/%s/_apis/git/repositories/%s/items?%s",
		p.BaseURL, url.PathEscape(target.GitHubOwner), url.PathEscape(project),
		url.PathEscape(repo), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", authHeader(pat))
	req.Header.Set("Accept", "text/plain")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("azure devops fetch %d: %s", resp.StatusCode, string(raw))
	}
	return io.ReadAll(resp.Body)
}

// ProbeAuth issues a cheap authenticated request (project metadata).
// Used by the v0.35 Validate endpoint as the first pre-flight check.
func (p *AzureDevOpsProvider) ProbeAuth(ctx context.Context, target *apptypes.DeployTarget, pat string) error {
	project, _ := p.splitProjectRepo(target)
	u := fmt.Sprintf("%s/%s/_apis/projects/%s?api-version=7.1",
		p.BaseURL, url.PathEscape(target.GitHubOwner), url.PathEscape(project))
	return p.do(ctx, http.MethodGet, u, pat, nil, nil)
}

// ProbeWorkflow confirms the configured pipeline ID exists. Wrong
// pipeline ID is the most common Azure DevOps setup mistake — the
// IDs aren't visible in the URL the way GitHub workflow filenames
// are, and operators commonly grab the wrong number.
func (p *AzureDevOpsProvider) ProbeWorkflow(ctx context.Context, target *apptypes.DeployTarget, pat string) error {
	project, _ := p.splitProjectRepo(target)
	u := fmt.Sprintf("%s/%s/%s/_apis/pipelines/%s?api-version=7.1",
		p.BaseURL, url.PathEscape(target.GitHubOwner), url.PathEscape(project),
		url.PathEscape(target.GitHubWorkflow))
	return p.do(ctx, http.MethodGet, u, pat, nil, nil)
}

// ListSuccessfulRuns enumerates successful pipeline runs since the
// given timestamp. Used by the v0.36.1 history walker.
//
// Azure DevOps doesn't expose a commit SHA in the runs list the way
// GitHub does — the pipeline run's source ref is in resources.
// repositories.self.version. We populate HeadSHA from there when
// present; missing values fall through as empty, which the walker
// silently skips.
func (p *AzureDevOpsProvider) ListSuccessfulRuns(ctx context.Context, target *apptypes.DeployTarget, pat string, since time.Time) ([]WorkflowRunSummary, error) {
	project, _ := p.splitProjectRepo(target)
	u := fmt.Sprintf("%s/%s/%s/_apis/pipelines/%s/runs?api-version=7.1",
		p.BaseURL, url.PathEscape(target.GitHubOwner), url.PathEscape(project),
		url.PathEscape(target.GitHubWorkflow))
	var resp struct {
		Value []struct {
			adoRunResponse
			Resources struct {
				Repositories map[string]struct {
					Version string `json:"version"`
				} `json:"repositories"`
			} `json:"resources"`
		} `json:"value"`
	}
	if err := p.do(ctx, http.MethodGet, u, pat, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]WorkflowRunSummary, 0, len(resp.Value))
	for _, r := range resp.Value {
		if r.CreatedDate.Before(since) {
			continue
		}
		if r.Result != "succeeded" {
			continue
		}
		sha := ""
		if self, ok := r.Resources.Repositories["self"]; ok {
			sha = self.Version
		}
		out = append(out, WorkflowRunSummary{
			RunID:     r.ID,
			HeadSHA:   sha,
			Branch:    target.GitHubBranch,
			CreatedAt: r.CreatedDate,
			URL:       r.Links.Web.Href,
		})
	}
	return out, nil
}
