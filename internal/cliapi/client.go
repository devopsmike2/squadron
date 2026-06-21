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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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
//
// The HTTPClient's RoundTripper is wrapped with otelhttp so every
// outbound request automatically becomes a child span of the
// caller's context AND carries a W3C traceparent header injected via
// the global propagator. When tracing isn't initialized (no OTLP
// endpoint configured), otelhttp produces a no-op span and emits
// nothing — overhead is negligible and the wire shape is unchanged.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport,
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					return r.Method + " " + r.URL.Path
				}),
			),
		},
	}
}

// APIError is the structured error returned by Squadron handlers. The
// HTTP status is preserved on Status so callers can map specific codes
// to specific exit codes (e.g. 401 → "set SQUADRON_TOKEN").
//
// Squadron currently uses two error envelope shapes on the wire:
//
//  1. {"error": "<code>", "detail": "<prose>"} — legacy shape used
//     by the pre-v0.89 handlers (rollouts, configs, audit, auth).
//     Code → APIError.Code, Detail → APIError.Detail.
//
//  2. {"error": {"code": "...", "message": "...", "suggested_step":
//     "...", "doc_link": "..."}} — the humanized shape used by the
//     IaC GitHub handlers (v0.89.3+) and the AWS discovery handlers.
//     Decoded into the same APIError fields: Code → code, Detail →
//     message; the SuggestedStep and DocLink hints are preserved so
//     the command layer can render them on a follow-up line.
//
// Both shapes funnel into the same APIError so callers only have
// to handle one error type. The command layer reads SuggestedStep /
// DocLink when they're set and renders the extra lines; older
// callers just see Code + Detail and behave unchanged.
type APIError struct {
	Status        int    `json:"-"`
	Code          string `json:"error"`
	Detail        string `json:"detail,omitempty"`
	SuggestedStep string `json:"-"`
	DocLink       string `json:"-"`
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Detail)
	}
	return e.Code
}

// humanizedErrorEnvelope is the v0.89.3+ wire shape used by the IaC
// and discovery handlers. Kept as a package-private type so the
// command layer never sees it directly — it's funneled into APIError
// on decode.
type humanizedErrorEnvelope struct {
	Error struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		SuggestedStep string `json:"suggested_step,omitempty"`
		DocLink       string `json:"doc_link,omitempty"`
	} `json:"error"`
}

// decodeErrorBody tries the legacy {"error":"code","detail":"prose"}
// shape first; on failure (the IaC handlers wrap the error in an
// object, which can't decode into a string field), tries the
// humanized envelope. The first decode to populate a non-empty Code
// wins; if both miss, the caller's fallback path (raw body as
// Detail) kicks in.
func decodeErrorBody(status int, body []byte) *APIError {
	apiErr := &APIError{Status: status}
	if err := json.Unmarshal(body, apiErr); err == nil && apiErr.Code != "" {
		return apiErr
	}
	// Reset Code/Detail in case the legacy unmarshal half-filled
	// the struct before failing.
	apiErr.Code = ""
	apiErr.Detail = ""
	var humanized humanizedErrorEnvelope
	if err := json.Unmarshal(body, &humanized); err == nil && humanized.Error.Code != "" {
		apiErr.Code = humanized.Error.Code
		apiErr.Detail = humanized.Error.Message
		apiErr.SuggestedStep = humanized.Error.SuggestedStep
		apiErr.DocLink = humanized.Error.DocLink
		return apiErr
	}
	return apiErr
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
// ctx carries cancellation + the active OTel span context. The
// otelhttp-wrapped transport reads the span from ctx and injects a
// traceparent header on the outbound request, so the server-side
// otelgin middleware can extract it and make the API request span a
// child of the caller. Pass context.Background() in tests / scripts
// that don't care.
//
// Non-2xx responses are returned as *APIError so callers can inspect
// the status without parsing the message.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
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

	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
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
		// decodeErrorBody tries both the legacy and humanized
		// envelopes before giving up. Fall back to the raw payload
		// as Detail if both shapes miss (the server returned
		// something non-JSON — proxy 502, panic recovery page, etc).
		apiErr := decodeErrorBody(resp.StatusCode, respBody)
		if apiErr.Code == "" {
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
