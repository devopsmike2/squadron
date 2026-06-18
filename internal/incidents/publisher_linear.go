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

// LinearConfig holds the credentials and routing for the Linear
// publisher. Linear is GraphQL-only; there is no REST equivalent for
// IssueCreate as of the v0.55 work. We talk to a single fixed
// endpoint and route every Squadron incident draft into a single
// team. Per-group team routing is future work.
//
// The APIKey is what Linear calls a "Personal API Key" (issued under
// the user's account settings, format lin_api_xxxx). Linear sends
// the key in the Authorization header without a Bearer prefix; the
// publisher mirrors that wire shape so operators can paste the raw
// key from Linear into SQUADRON_LINEAR_API_KEY.
type LinearConfig struct {
	APIKey string
	TeamID string

	// Optional label IDs to attach to every issue. Linear labels
	// are referenced by ID, not name, because two labels in two
	// teams can share a name. Operators look up label IDs once
	// from their workspace settings or via Linear's API. Empty is
	// fine.
	LabelIDs []string

	// APIEndpoint lets tests redirect to httptest, and lets an
	// enterprise install (rare for Linear) override the public
	// endpoint. Defaults to https://api.linear.app/graphql.
	APIEndpoint string

	// HTTPClient is overridable for tests. Defaults to a 15 second
	// timeout client.
	HTTPClient *http.Client
}

// LinearPublisher posts incident drafts as new issues to a single
// configured Linear team. Linear's API is GraphQL; we send a single
// IssueCreate mutation. The response carries the issue identifier
// (the human-facing string like ENG-123) and the issue URL, which is
// what we stamp back onto the incident draft.
type LinearPublisher struct {
	cfg LinearConfig
}

// NewLinearPublisher constructs the publisher. Returns an error if
// the supplied config is incomplete; the all-in-one binary uses that
// to decide whether to register the publisher.
func NewLinearPublisher(cfg LinearConfig) (*LinearPublisher, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("linear publisher: api key is required")
	}
	if strings.TrimSpace(cfg.TeamID) == "" {
		return nil, errors.New("linear publisher: team id is required")
	}
	if cfg.APIEndpoint == "" {
		cfg.APIEndpoint = "https://api.linear.app/graphql"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &LinearPublisher{cfg: cfg}, nil
}

// Name implements Publisher.
func (LinearPublisher) Name() string { return "linear" }

// linearIssueCreateMutation is the smallest IssueCreate that gives
// us back the fields we need to stamp the draft: the user-facing
// identifier (ENG-123 style), the canonical URL, and a success flag
// so we can distinguish a partial failure from a clean create.
const linearIssueCreateMutation = `mutation IssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue {
      id
      identifier
      url
    }
  }
}`

type linearRequestBody struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type linearGraphQLError struct {
	Message string `json:"message"`
}

type linearIssueCreateResponse struct {
	Data struct {
		IssueCreate struct {
			Success bool `json:"success"`
			Issue   struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				URL        string `json:"url"`
			} `json:"issue"`
		} `json:"issueCreate"`
	} `json:"data"`
	Errors []linearGraphQLError `json:"errors,omitempty"`
}

// Publish creates a Linear issue and returns the identifier (e.g.
// "ENG-123") plus the issue URL. Non 2xx responses, GraphQL-level
// errors, or success=false from the mutation all become a publisher
// error so the handler can surface them to the operator.
func (p *LinearPublisher) Publish(ctx context.Context, draft *types.IncidentDraft) (string, string, error) {
	if draft == nil {
		return "", "", errors.New("nil draft")
	}
	if strings.TrimSpace(draft.Title) == "" {
		return "", "", errors.New("draft has empty title")
	}

	input := map[string]interface{}{
		"teamId":      p.cfg.TeamID,
		"title":       draft.Title,
		"description": draft.BodyMarkdown,
	}
	if len(p.cfg.LabelIDs) > 0 {
		input["labelIds"] = p.cfg.LabelIDs
	}

	body := linearRequestBody{
		Query:     linearIssueCreateMutation,
		Variables: map[string]interface{}{"input": input},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.APIEndpoint, bytes.NewReader(buf))
	if err != nil {
		return "", "", err
	}
	// Linear takes the raw key in the Authorization header. No
	// Bearer prefix. This matches the docs at
	// https://developers.linear.app/docs/graphql/working-with-the-graphql-api/authentication
	req.Header.Set("Authorization", p.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

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
		return "", "", fmt.Errorf("linear api: %d %s: %s", resp.StatusCode, resp.Status, excerpt)
	}

	var parsed linearIssueCreateResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		msgs := make([]string, 0, len(parsed.Errors))
		for _, e := range parsed.Errors {
			msgs = append(msgs, e.Message)
		}
		return "", "", fmt.Errorf("linear graphql: %s", strings.Join(msgs, "; "))
	}
	if !parsed.Data.IssueCreate.Success {
		return "", "", errors.New("linear graphql: issueCreate returned success=false")
	}
	if parsed.Data.IssueCreate.Issue.Identifier == "" {
		return "", "", errors.New("linear graphql: response missing issue identifier")
	}
	return parsed.Data.IssueCreate.Issue.Identifier, parsed.Data.IssueCreate.Issue.URL, nil
}
