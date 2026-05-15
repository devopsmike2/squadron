// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package cliapi is the minimal HTTP client the squadronctl binary
// uses to talk to a Squadron server.
//
// It's intentionally NOT a thin wrapper around the server's services
// package. The CLI ships as a standalone binary that needs to run
// without GCC, without SQLite, without DuckDB — so it can't transitively
// import storage code. Instead this package re-declares only the wire
// shapes the CLI actually reads, encoded as plain structs with json
// tags. The server is the source of truth; this package follows.
//
// Keep this list small. If you find yourself reaching for a third type
// from services here, prefer adding a curl-equivalent helper to the
// CLI rather than expanding the surface area.
package cliapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is the canonical Squadron HTTP client used by the CLI.
//
// BaseURL points at the Squadron HTTP listener (e.g.
// "http://localhost:8080"). Token is the Bearer credential; if empty,
// requests go out unauthenticated and the server's response decides
// whether that's OK (Squadron itself allows unauthed requests when
// auth.enabled is false).
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// New constructs a Client with sensible defaults. Pass an empty token
// for an unauthenticated client (useful for dev instances with
// auth.enabled=false).
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Token:      token,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is the structured error returned by Squadron handlers. The
// HTTP status is preserved on Status so callers can map specific codes
// to specific exit codes (e.g. 401 → "set SQUADRON_TOKEN").
type APIError struct {
	Status int    `json:"-"`
	Code   string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return e.Code
}

// Is401 reports whether an error is a 401 Unauthorized. Used by the
// CLI to print a "set SQUADRON_TOKEN" hint instead of a raw stack.
func Is401(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusUnauthorized
	}
	return false
}

// Do executes an HTTP request and decodes the response into out. out
// may be nil for endpoints that return 204. method is one of the
// http.Method* constants. path is the API path starting with /api/v1.
// query is optional URL query params (nil for none). body, if non-nil,
// is JSON-marshaled.
//
// Non-2xx responses are returned as *APIError so callers can inspect
// the status without parsing the message.
func (c *Client) Do(method, path string, query url.Values, body any, out any) error {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, u, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("call %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		apiErr := &APIError{Status: resp.StatusCode}
		// Try to parse the structured error body; fall back to the
		// raw payload as Detail if the server returned something
		// non-JSON (e.g. a panic recovery page).
		if jsonErr := json.Unmarshal(respBody, apiErr); jsonErr != nil || apiErr.Code == "" {
			apiErr.Code = http.StatusText(resp.StatusCode)
			if len(respBody) > 0 && len(respBody) < 500 {
				apiErr.Detail = strings.TrimSpace(string(respBody))
			}
		}
		return apiErr
	}

	if out == nil || resp.StatusCode == http.StatusNoContent || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w (body: %s)", err, snippet(respBody))
	}
	return nil
}

func snippet(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
