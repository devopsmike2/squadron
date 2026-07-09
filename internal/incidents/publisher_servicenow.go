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

// ServiceNowConfig holds the credentials and routing for the
// ServiceNow Table API publisher. ServiceNow uses Basic auth where
// the username is a service account login and the password is that
// account's password (or an equivalent API token). The wire format
// is:
//
//	Authorization: Basic <base64(username:password)>
//
// This mirrors the Jira publisher's Basic auth shape; call it out at
// the wire layer so an operator reading the publisher source can see
// the auth shape immediately.
//
// Instance is the ServiceNow instance subdomain, e.g. "acme" for the
// tenant https://acme.service-now.com. Squadron appends
// /api/now/table/incident to the instance base for the create call.
//
// DefaultUrgency / DefaultImpact default to "3" (ServiceNow's
// "low/moderate" middle value on the 1..3 scale) when empty.
// Operators routing into a dedicated priority tier will usually want
// to override through SQUADRON_SERVICENOW_URGENCY /
// SQUADRON_SERVICENOW_IMPACT.
type ServiceNowConfig struct {
	Instance       string
	Username       string
	Password       string
	DefaultUrgency string
	DefaultImpact  string

	// BaseURL overrides the derived https://<instance>.service-now.com
	// origin. It lets tests redirect to httptest and lets operators on
	// a custom domain point at the right host. Defaults to
	// https://<instance>.service-now.com when empty.
	BaseURL string

	// HTTPClient is overridable for tests. Defaults to a 15 second
	// timeout client.
	HTTPClient *http.Client
}

// ServiceNowPublisher posts incident drafts as new records in the
// ServiceNow "incident" table via the Table API. The record's
// short_description is the draft title; the description is the
// draft's markdown body (ServiceNow renders description as plain
// text, so unlike Linear / GitHub the markdown lands flat, mirroring
// the trade the Jira publisher accepts with ADF).
type ServiceNowPublisher struct {
	cfg ServiceNowConfig
}

// NewServiceNowPublisher constructs the publisher. Returns an error
// if the supplied config is incomplete; the all-in-one binary uses
// that to decide whether to register the publisher.
func NewServiceNowPublisher(cfg ServiceNowConfig) (*ServiceNowPublisher, error) {
	if strings.TrimSpace(cfg.Instance) == "" {
		return nil, errors.New("servicenow publisher: instance is required")
	}
	if strings.TrimSpace(cfg.Username) == "" {
		return nil, errors.New("servicenow publisher: username is required")
	}
	if strings.TrimSpace(cfg.Password) == "" {
		return nil, errors.New("servicenow publisher: password is required")
	}
	if strings.TrimSpace(cfg.DefaultUrgency) == "" {
		cfg.DefaultUrgency = "3"
	}
	if strings.TrimSpace(cfg.DefaultImpact) == "" {
		cfg.DefaultImpact = "3"
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = fmt.Sprintf("https://%s.service-now.com", cfg.Instance)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &ServiceNowPublisher{cfg: cfg}, nil
}

// Name implements Publisher.
func (ServiceNowPublisher) Name() string { return "servicenow" }

type serviceNowIncidentCreateResponse struct {
	Result struct {
		SysID  string `json:"sys_id"`
		Number string `json:"number"`
	} `json:"result"`
}

type serviceNowErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
	} `json:"error"`
	Status string `json:"status"`
}

// Publish creates a ServiceNow incident and returns the human
// readable incident number (e.g. "INC0010023") plus the user-facing
// nav_to URL constructed from the base URL and the record's sys_id.
// Non 2xx responses become an error with a short excerpt of the
// response body so operators can see exactly what ServiceNow
// complained about (most often a bad credential or an ACL that
// blocks the incident table).
func (p *ServiceNowPublisher) Publish(ctx context.Context, draft *types.IncidentDraft) (string, string, error) {
	if draft == nil {
		return "", "", errors.New("nil draft")
	}
	if strings.TrimSpace(draft.Title) == "" {
		return "", "", errors.New("draft has empty title")
	}

	base := strings.TrimRight(p.cfg.BaseURL, "/")
	url := fmt.Sprintf("%s/api/now/table/incident", base)

	body := map[string]any{
		"short_description": draft.Title,
		"description":       draft.BodyMarkdown,
		"urgency":           p.cfg.DefaultUrgency,
		"impact":            p.cfg.DefaultImpact,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return "", "", err
	}
	// ServiceNow Basic auth: username is the service account login,
	// password is that account's password (or API token). Same wire
	// shape as the Jira publisher.
	credentials := base64.StdEncoding.EncodeToString([]byte(p.cfg.Username + ":" + p.cfg.Password))
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
		// ServiceNow surfaces helpful detail under error.message /
		// error.detail; try to flatten that into a short
		// human-readable line. If parsing fails, fall back to the
		// truncated raw body.
		var parsed serviceNowErrorResponse
		if json.Unmarshal(respBytes, &parsed) == nil {
			if parsed.Error.Message != "" {
				excerpt = parsed.Error.Message
				if parsed.Error.Detail != "" {
					excerpt = parsed.Error.Message + ": " + parsed.Error.Detail
				}
			}
		}
		if len(excerpt) > 500 {
			excerpt = excerpt[:500] + "..."
		}
		return "", "", fmt.Errorf("servicenow api: %d %s: %s", resp.StatusCode, resp.Status, excerpt)
	}

	var parsed serviceNowIncidentCreateResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}
	if parsed.Result.Number == "" {
		return "", "", errors.New("servicenow api: response missing incident number")
	}
	// Build the nav_to URL the operator clicks. The Table API
	// returns record data, not a human UI link, so we construct the
	// user-facing URL ourselves from the sys_id.
	navURL := fmt.Sprintf("%s/nav_to.do?uri=incident.do?sys_id=%s", base, parsed.Result.SysID)
	return parsed.Result.Number, navURL, nil
}
