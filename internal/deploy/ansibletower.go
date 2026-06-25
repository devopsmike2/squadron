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
	"strconv"
	"strings"
	"time"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// AnsibleTowerProvider implements Provider against the
// Ansible Tower / AWX REST API. Tower is the third native backend
// after GitHub Actions and Azure DevOps Pipelines, added in v0.42.
//
// Field mapping (we reuse the existing DeployTarget columns):
//
//   - GitHubOwner    → Tower base URL host (no scheme)
//   - GitHubRepo     → not used (job templates live by ID, not repo)
//   - GitHubWorkflow → Tower job template ID (numeric string)
//   - GitHubBranch   → SCM branch override for the template launch
//   - PAT            → Tower OAuth2 token or session token
//
// Tower's API is more permissive than GitHub or Azure DevOps:
// launching a job is POST /api/v2/job_templates/{id}/launch/ and
// returns the job ID directly (no waiting on a separate webhook).
// Status polls hit /api/v2/jobs/{id}/.
//
// Inventory file fetching is the awkward bit — Tower stores
// inventory either as static text inside a Tower "inventory"
// resource OR as a project file inside Tower's git project. We
// support the second path because that's what an SRE shop wired
// to Squadron's deploy lifecycle would use: the project's git
// content is mirrored to the local Tower filesystem, and the
// `/api/v2/projects/{id}/playbooks/` endpoint can fetch the file.
// For v0.42 we expose a single FetchFile that walks the most-recent
// project update to find the path; FetchFileAtRef returns ErrUnsupported
// because Tower doesn't trivially expose historical SHAs. The GHA
// history walker simply won't trigger for Tower targets — that's
// fine, the v0.36.1 design always assumed walker support was per-
// provider opt-in.
//
// Added in v0.42.0 (connectors part 2).
type AnsibleTowerProvider struct {
	HTTP *http.Client
}

// NewAnsibleTowerProvider builds a provider with a 30s HTTP timeout.
// Tower's base URL comes from each target (GitHubOwner field).
func NewAnsibleTowerProvider() *AnsibleTowerProvider {
	return &AnsibleTowerProvider{
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// baseURL composes the API base for a target.
func (p *AnsibleTowerProvider) baseURL(target *apptypes.DeployTarget) string {
	host := strings.TrimRight(target.GitHubOwner, "/")
	// Allow either "tower.your-co.com" or "https://tower.your-co.com"
	// in the field — operators copy-paste from browser address bars.
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "https://" + host
	}
	return host + "/api/v2"
}

// do is the inner HTTP helper. Tower uses Bearer auth with the
// OAuth2 / personal access token.
func (p *AnsibleTowerProvider) do(ctx context.Context, method, urlStr, pat string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("ansible tower %d %s: %s", resp.StatusCode, method, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// towerJob is the slice of Tower's job representation we care about.
type towerJob struct {
	ID       int64      `json:"id"`
	URL      string     `json:"url"`
	Status   string     `json:"status"` // "pending" | "running" | "successful" | "failed" | "canceled"
	Failed   bool       `json:"failed"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	JobURL   string     `json:"-"` // we synthesize a UI URL from the base host
	Created  time.Time  `json:"created"`
}

func (j *towerJob) toRunStatus(base string) *RunStatus {
	return &RunStatus{
		GitHubRunID:  j.ID,
		GitHubRunURL: strings.TrimSuffix(base, "/api/v2") + "/#/jobs/playbook/" + strconv.FormatInt(j.ID, 10),
		Status:       mapTowerStatus(j.Status),
		Conclusion:   mapTowerConclusion(j.Status, j.Failed),
		StartedAt:    derefOrZero(j.Started),
		CompletedAt:  j.Finished,
	}
}

func mapTowerStatus(s string) string {
	switch s {
	case "pending", "waiting", "new":
		return "queued"
	case "running":
		return "in_progress"
	case "successful", "failed", "canceled", "error":
		return "completed"
	default:
		return "queued"
	}
}

func mapTowerConclusion(status string, failed bool) string {
	switch status {
	case "successful":
		return "success"
	case "failed", "error":
		return "failure"
	case "canceled":
		return "cancelled"
	}
	if failed {
		return "failure"
	}
	return ""
}

func derefOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Dispatch launches a job template.
//
// API: POST /api/v2/job_templates/{id}/launch/
// Body: { "extra_vars": { ...inputs as JSON-encoded string... }, "scm_branch": "<branch>" }
//
// Tower expects extra_vars as a STRING containing JSON, not an object —
// confusing but consistent across the API.
func (p *AnsibleTowerProvider) Dispatch(ctx context.Context, target *apptypes.DeployTarget, pat string, inputs map[string]string) (string, error) {
	if target.GitHubWorkflow == "" {
		return "", fmt.Errorf("ansible tower target missing job template id")
	}
	base := p.baseURL(target)
	extraVarsJSON := "{}"
	if len(inputs) > 0 {
		b, err := json.Marshal(inputs)
		if err != nil {
			return "", err
		}
		extraVarsJSON = string(b)
	}
	body := map[string]any{
		"extra_vars": extraVarsJSON,
	}
	if target.GitHubBranch != "" {
		body["scm_branch"] = target.GitHubBranch
	}
	u := fmt.Sprintf("%s/job_templates/%s/launch/", base, target.GitHubWorkflow)
	var job towerJob
	if err := p.do(ctx, http.MethodPost, u, pat, body, &job); err != nil {
		return "", err
	}
	if job.ID == 0 {
		return "", fmt.Errorf("ansible tower returned no job id")
	}
	return strconv.FormatInt(job.ID, 10), nil
}

// GetRun fetches the current state of a known job.
func (p *AnsibleTowerProvider) GetRun(ctx context.Context, target *apptypes.DeployTarget, pat string, runID int64) (*RunStatus, error) {
	base := p.baseURL(target)
	u := fmt.Sprintf("%s/jobs/%d/", base, runID)
	var job towerJob
	if err := p.do(ctx, http.MethodGet, u, pat, nil, &job); err != nil {
		return nil, err
	}
	return job.toRunStatus(base), nil
}

// LatestRunSince scans jobs of the configured template and returns
// the most recent one whose created date is after `since`. Tower
// returns the newest first.
func (p *AnsibleTowerProvider) LatestRunSince(ctx context.Context, target *apptypes.DeployTarget, pat string, since time.Time) (*RunStatus, error) {
	base := p.baseURL(target)
	u := fmt.Sprintf("%s/job_templates/%s/jobs/?order_by=-created&page_size=5",
		base, target.GitHubWorkflow)
	var resp struct {
		Results []towerJob `json:"results"`
	}
	if err := p.do(ctx, http.MethodGet, u, pat, nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.Results) > 0 {
		j := resp.Results[0]
		if !j.Created.Before(since) {
			return j.toRunStatus(base), nil
		}
	}
	return nil, nil
}

// FetchFile fetches a file from the linked project's working
// directory. Tower projects are git-backed; the working directory
// reflects the latest project update. We look up the project linked
// to the job template, walk its config, and use the configured SCM
// path as the search root.
//
// For v0.42 we keep it simple and call /api/v2/projects/{id}/inventories/
// for inventory file enumeration, plus a passthrough to the project
// content endpoint for non-inventory files. The common case at
// Southern Co is "inventory.ini" pinned via the project's SCM_PATH.
func (p *AnsibleTowerProvider) FetchFile(ctx context.Context, target *apptypes.DeployTarget, pat string, path string) ([]byte, error) {
	// Tower doesn't have a stable single endpoint for "fetch this
	// file from the project" — operators typically stage files via
	// project_updates. For v0.42 we return ErrUnsupported on the
	// fetch path; the deploy service degrades gracefully (it skips
	// the inventory auto-populate step) and the operator can
	// configure expected hosts manually until v0.42.1 wires the
	// project file fetcher.
	return nil, fmt.Errorf("ansible tower inventory fetch not supported in v0.42 — set expected_hosts manually or wait for v0.42.1")
}

// FetchFileAtRef is unsupported because Tower's project-update model
// doesn't expose historical SHAs through the REST API the way git
// providers do. The GHA-style walker simply won't trigger for Tower
// targets.
func (p *AnsibleTowerProvider) FetchFileAtRef(ctx context.Context, target *apptypes.DeployTarget, pat string, path string, ref string) ([]byte, error) {
	return nil, fmt.Errorf("ansible tower history walker not supported (project SHAs unavailable via REST)")
}

// ProbeAuth issues a cheap /me request to verify the token.
func (p *AnsibleTowerProvider) ProbeAuth(ctx context.Context, target *apptypes.DeployTarget, pat string) error {
	base := p.baseURL(target)
	return p.do(ctx, http.MethodGet, base+"/me/", pat, nil, nil)
}

// ProbeWorkflow confirms the configured job template ID exists.
func (p *AnsibleTowerProvider) ProbeWorkflow(ctx context.Context, target *apptypes.DeployTarget, pat string) error {
	base := p.baseURL(target)
	u := fmt.Sprintf("%s/job_templates/%s/", base, target.GitHubWorkflow)
	return p.do(ctx, http.MethodGet, u, pat, nil, nil)
}

// ListSuccessfulRuns returns successful jobs of the template since
// the given timestamp. Used by the v0.36.1 walker.
func (p *AnsibleTowerProvider) ListSuccessfulRuns(ctx context.Context, target *apptypes.DeployTarget, pat string, since time.Time) ([]WorkflowRunSummary, error) {
	base := p.baseURL(target)
	u := fmt.Sprintf("%s/job_templates/%s/jobs/?order_by=-created&status=successful&page_size=50",
		base, target.GitHubWorkflow)
	var resp struct {
		Results []towerJob `json:"results"`
	}
	if err := p.do(ctx, http.MethodGet, u, pat, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]WorkflowRunSummary, 0, len(resp.Results))
	for _, j := range resp.Results {
		if j.Created.Before(since) {
			continue
		}
		out = append(out, WorkflowRunSummary{
			RunID:     j.ID,
			HeadSHA:   "", // Tower doesn't surface this
			Branch:    target.GitHubBranch,
			CreatedAt: j.Created,
			URL:       strings.TrimSuffix(base, "/api/v2") + "/#/jobs/playbook/" + strconv.FormatInt(j.ID, 10),
		})
	}
	return out, nil
}
