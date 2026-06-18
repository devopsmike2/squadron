// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package incidents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Publisher is the plug-in contract for incident draft providers.
// Each provider (clipboard, GitHub Issues, Linear, Jira, generic
// webhook, ServiceNow, ...) implements this small surface; the
// handler picks the right one by the provider name on the publish
// request.
//
// Publishers should be safe to call concurrently. The handler does
// not serialize publish calls for the same draft, but the handler
// also only marks the draft published once, so a winning Publish is
// what gets stamped.
type Publisher interface {
	// Name returns the canonical provider name. Must match the
	// value the operator picks in the UI / the value in the publish
	// request body.
	Name() string

	// Publish ships the draft to the provider. Returns the
	// provider's external identifier (e.g. "LIN-123", "1234" for a
	// GitHub issue number) and a URL the operator can click. An
	// empty externalID is allowed for providers that have no
	// permanent reference (clipboard).
	Publish(ctx context.Context, draft *types.IncidentDraft) (externalID, externalURL string, err error)
}

// ClipboardPublisher is the default no-op publisher. The body is
// already rendered; the UI is responsible for the actual clipboard
// copy. We register this server-side so the publish path is
// uniform: every provider goes through Publisher even when there is
// no remote call.
type ClipboardPublisher struct{}

// NewClipboardPublisher returns a ClipboardPublisher.
func NewClipboardPublisher() *ClipboardPublisher { return &ClipboardPublisher{} }

// Name implements Publisher.
func (ClipboardPublisher) Name() string { return "clipboard" }

// Publish implements Publisher. Returns empty strings; the audit
// event records that the operator picked clipboard.
func (ClipboardPublisher) Publish(_ context.Context, _ *types.IncidentDraft) (string, string, error) {
	return "", "", nil
}

// GitHubIssuesConfig is the minimal credentials shape for the
// GitHub Issues publisher. The token is a fine-grained Issues:write
// PAT (or a GitHub App token, same wire format). Owner / repo
// target a single issue tracker; multi-repo routing is future work.
type GitHubIssuesConfig struct {
	Owner string
	Repo  string
	Token string

	// Optional labels to attach to every issue Squadron files.
	// Squadron does not invent labels; if a label does not exist
	// the API call still succeeds but the label silently drops.
	// Operators can pre-create labels like "squadron" so the
	// resulting issues are easy to filter.
	Labels []string

	// APIBaseURL lets tests redirect to httptest, and lets
	// GitHub Enterprise installs override the public API host.
	// Defaults to https://api.github.com when empty.
	APIBaseURL string

	// HTTPClient is overridable for tests. Defaults to a
	// 15 second timeout client.
	HTTPClient *http.Client
}

// GitHubIssuesPublisher posts incident drafts as new GitHub Issues
// in a single configured repository. The issue title is the draft
// title; the body is the rendered markdown (which already includes
// the audit references at the bottom).
type GitHubIssuesPublisher struct {
	cfg GitHubIssuesConfig
}

// NewGitHubIssuesPublisher constructs the publisher. Returns an
// error if the supplied config is incomplete; the all-in-one binary
// uses that to decide whether to register the publisher at all.
func NewGitHubIssuesPublisher(cfg GitHubIssuesConfig) (*GitHubIssuesPublisher, error) {
	if strings.TrimSpace(cfg.Owner) == "" {
		return nil, errors.New("github issues publisher: owner is required")
	}
	if strings.TrimSpace(cfg.Repo) == "" {
		return nil, errors.New("github issues publisher: repo is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("github issues publisher: token is required")
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://api.github.com"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &GitHubIssuesPublisher{cfg: cfg}, nil
}

// Name implements Publisher.
func (GitHubIssuesPublisher) Name() string { return "github" }

// Publish creates an issue and returns the issue number plus the
// html_url field from the response. Non 2xx responses become an
// error with a short excerpt of the response body so the operator
// can see what went wrong (most often a missing repo or a token
// without issues:write).
func (p *GitHubIssuesPublisher) Publish(ctx context.Context, draft *types.IncidentDraft) (string, string, error) {
	if draft == nil {
		return "", "", errors.New("nil draft")
	}
	if strings.TrimSpace(draft.Title) == "" {
		return "", "", errors.New("draft has empty title")
	}

	url := fmt.Sprintf("%s/repos/%s/%s/issues",
		strings.TrimRight(p.cfg.APIBaseURL, "/"),
		p.cfg.Owner,
		p.cfg.Repo,
	)
	body := map[string]any{
		"title": draft.Title,
		"body":  draft.BodyMarkdown,
	}
	if len(p.cfg.Labels) > 0 {
		body["labels"] = p.cfg.Labels
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		excerpt := strings.TrimSpace(string(respBytes))
		if len(excerpt) > 500 {
			excerpt = excerpt[:500] + "..."
		}
		return "", "", fmt.Errorf("github issues api: %d %s: %s", resp.StatusCode, resp.Status, excerpt)
	}

	var parsed struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}
	if parsed.Number == 0 {
		return "", "", errors.New("github issues api: response missing issue number")
	}
	return fmt.Sprintf("%d", parsed.Number), parsed.HTMLURL, nil
}

// PublisherRegistry is the map the handler consults when it
// receives a publish request. The handler resolves the provider
// name on the request body to the corresponding Publisher.
//
// Providers that exist as enums in the wire format but have no
// Publisher implementation (e.g. linear, jira) cause the handler to
// stamp the draft with the operator chosen external_id / URL but
// skip the remote call. That keeps the audit trail accurate without
// pretending to integrate.
type PublisherRegistry map[string]Publisher

// NewPublisherRegistry returns a registry seeded with the always
// available clipboard publisher.
func NewPublisherRegistry() PublisherRegistry {
	return PublisherRegistry{
		"clipboard": NewClipboardPublisher(),
	}
}

// Register adds a publisher to the registry. Overrides any existing
// publisher with the same name.
func (r PublisherRegistry) Register(p Publisher) {
	r[p.Name()] = p
}

// Lookup returns the publisher for the given provider name, or nil
// if none is registered.
func (r PublisherRegistry) Lookup(name string) Publisher {
	return r[name]
}
