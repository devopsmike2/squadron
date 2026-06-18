// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package incidents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// JiraConfig holds the credentials and routing for the Jira Cloud
// publisher. Jira Cloud uses Basic auth where the username is the
// account email and the password is an API token issued from the
// Atlassian account settings page. The wire format is:
//
//	Authorization: Basic <base64(email:api_token)>
//
// This is different from Linear (raw API key in the Authorization
// header) and GitHub (Bearer token); call it out at the wire layer
// so an operator reading the publisher source can see the auth
// shape immediately.
//
// BaseURL is the tenant URL, e.g. https://acme.atlassian.net.
// Squadron appends /rest/api/3/issue to that.
//
// IssueType defaults to "Task" when empty. Operators routing into a
// dedicated incident-response project will usually want to override
// to "Incident" or "Bug" through SQUADRON_JIRA_ISSUE_TYPE.
type JiraConfig struct {
	BaseURL    string
	Email      string
	APIToken   string
	ProjectKey string
	IssueType  string

	// Optional labels to attach to every issue. Jira labels are
	// referenced by name; missing labels still cause the issue to
	// create successfully (Jira silently creates new labels) so
	// operators who type SQUADRON_JIRA_LABELS=squadron,automation
	// will see those exact labels attached.
	Labels []string

	// HTTPClient is overridable for tests. Defaults to a 15 second
	// timeout client.
	HTTPClient *http.Client
}

// JiraPublisher posts incident drafts as new issues in a single
// configured Jira Cloud project. The issue summary is the draft
// title; the description is an Atlassian Document Format (ADF)
// document built from the draft's markdown body.
//
// Why ADF instead of plain text or wiki markup:
// Jira Cloud REST API v3 requires ADF for the description field.
// API v2 still accepts wiki markup but is on the deprecated path;
// we target v3 so the publisher stays supported as Atlassian
// continues to evolve the API. The trade is that markdown formatting
// like headers, bold, and bullet lists do not render in Jira; the
// text is visible but flat. Paragraph breaks are preserved because
// each blank-line-separated chunk of the draft becomes its own ADF
// paragraph node. Operators who need rich rendering should adopt the
// Linear or GitHub Issues publishers instead, both of which preserve
// markdown.
type JiraPublisher struct {
	cfg JiraConfig
}

// NewJiraPublisher constructs the publisher. Returns an error if the
// supplied config is incomplete; the all-in-one binary uses that to
// decide whether to register the publisher.
func NewJiraPublisher(cfg JiraConfig) (*JiraPublisher, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("jira publisher: base url is required")
	}
	if strings.TrimSpace(cfg.Email) == "" {
		return nil, errors.New("jira publisher: email is required")
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil, errors.New("jira publisher: api token is required")
	}
	if strings.TrimSpace(cfg.ProjectKey) == "" {
		return nil, errors.New("jira publisher: project key is required")
	}
	if strings.TrimSpace(cfg.IssueType) == "" {
		cfg.IssueType = "Task"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &JiraPublisher{cfg: cfg}, nil
}

// Name implements Publisher.
func (JiraPublisher) Name() string { return "jira" }

// buildADFDescription wraps the markdown body as a minimal ADF
// document. Each blank-line-separated chunk becomes its own
// paragraph node so paragraph breaks survive the round trip. The
// trade we accept is that inline markdown (bold, code, links) lands
// as literal characters; rich rendering is a Linear or GitHub
// concern, not a Jira one.
func buildADFDescription(markdown string) map[string]any {
	chunks := strings.Split(markdown, "\n\n")
	content := make([]map[string]any, 0, len(chunks))
	for _, raw := range chunks {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		content = append(content, map[string]any{
			"type": "paragraph",
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
		})
	}
	if len(content) == 0 {
		// Empty draft is still valid ADF as long as the body has
		// at least one paragraph node; pin a single empty
		// paragraph so the API does not reject the request for a
		// malformed document.
		content = append(content, map[string]any{
			"type":    "paragraph",
			"content": []map[string]any{},
		})
	}
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": content,
	}
}

type jiraIssueCreateResponse struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Self string `json:"self"`
}

type jiraErrorResponse struct {
	ErrorMessages []string          `json:"errorMessages"`
	Errors        map[string]string `json:"errors"`
}

// Publish creates a Jira issue and returns the human readable issue
// key (e.g. "SQUAD-42") plus the user-facing URL constructed from
// the base URL and key. Non 2xx responses become an error with a
// short excerpt of the response body so operators can see exactly
// what Jira complained about (most often a bad project key or an
// expired API token).
func (p *JiraPublisher) Publish(ctx context.Context, draft *types.IncidentDraft) (string, string, error) {
	if draft == nil {
		return "", "", errors.New("nil draft")
	}
	if strings.TrimSpace(draft.Title) == "" {
		return "", "", errors.New("draft has empty title")
	}

	url := fmt.Sprintf("%s/rest/api/3/issue", strings.TrimRight(p.cfg.BaseURL, "/"))

	fields := map[string]any{
		"project":     map[string]any{"key": p.cfg.ProjectKey},
		"summary":     draft.Title,
		"description": buildADFDescription(draft.BodyMarkdown),
		"issuetype":   map[string]any{"name": p.cfg.IssueType},
	}
	if len(p.cfg.Labels) > 0 {
		fields["labels"] = p.cfg.Labels
	}
	body := map[string]any{"fields": fields}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", "", err
	}
	// Jira Cloud Basic auth: email is the username, API token is
	// the password. Atlassian docs:
	// https://developer.atlassian.com/cloud/jira/platform/basic-auth-for-rest-apis/
	credentials := base64.StdEncoding.EncodeToString([]byte(p.cfg.Email + ":" + p.cfg.APIToken))
	req.Header.Set("Authorization", "Basic "+credentials)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		excerpt := strings.TrimSpace(string(respBytes))
		// Jira surfaces helpful detail under errorMessages /
		// errors; try to flatten the first one into a short
		// human-readable line. If parsing fails, fall back to
		// the truncated raw body.
		var parsed jiraErrorResponse
		if json.Unmarshal(respBytes, &parsed) == nil {
			if len(parsed.ErrorMessages) > 0 {
				excerpt = parsed.ErrorMessages[0]
			} else if len(parsed.Errors) > 0 {
				for k, v := range parsed.Errors {
					excerpt = k + ": " + v
					break
				}
			}
		}
		if len(excerpt) > 500 {
			excerpt = excerpt[:500] + "..."
		}
		return "", "", fmt.Errorf("jira api: %d %s: %s", resp.StatusCode, resp.Status, excerpt)
	}

	var parsed jiraIssueCreateResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}
	if parsed.Key == "" {
		return "", "", errors.New("jira api: response missing issue key")
	}
	// Build the browse URL the operator clicks. The self link
	// returned by the API points at the REST endpoint, not the
	// human UI, so we construct the user-facing URL ourselves.
	browseURL := fmt.Sprintf("%s/browse/%s",
		strings.TrimRight(p.cfg.BaseURL, "/"),
		parsed.Key,
	)
	return parsed.Key, browseURL, nil
}
