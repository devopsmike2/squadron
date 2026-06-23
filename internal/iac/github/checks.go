// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// This file is the slice-1 wrapper for the GitHub Checks API the
// v0.89.42 (#662 Stream 60) back-signal arc surfaces in PR review
// flow. It rides on the same PAT-backed transport client.go already
// uses (per design-doc §3 option A — picked); the wrapper adds two
// methods (CreateCheckRun + UpdateCheckRun) and the structured-error
// vocabulary the chunks-2/3/4 audit-emit paths fan out on.
//
// Two cross-cutting invariants this file owns:
//
//  1. **Fail-open posture.** Every check-run API call wraps its
//     errors into a *CheckRunError with a small set of typed
//     discriminators. The caller MUST treat the failure as "drop the
//     check run, log the event, continue" — the existing PR open /
//     merge / close paths still complete normally. §8 of the design
//     doc names this as load-bearing: operators upgrading to the
//     checks-API release MUST NOT have their PR opens broken because
//     their PAT is at the old scope.
//
//  2. **Error-kind discrimination is the contract.** The four
//     error_kind strings (scope_missing, rate_limit, pr_not_found,
//     network) are what the audit-emit path matches against and
//     what SIEM consumers fan out on. The discrimination logic
//     below is deliberate, pinned to GitHub-documented headers and
//     status codes, and load-bearing for the chunks-2/3/4 wiring.
//     Specifically:
//       - rate_limit: 429 OR (4xx with X-RateLimit-Remaining=0).
//         GitHub uses 403 with rate-limit headers as their primary
//         rate-limit signal on the REST API; the wrapper checks
//         the header value BEFORE the 403/404 → scope_missing
//         branch.
//       - scope_missing: 403 with "Resource not accessible"
//         message OR 404 (GitHub's opaque "endpoint not found"
//         for missing-scope cases per §5).
//       - pr_not_found: 422 with PR-not-found / no-commit shape.
//         GitHub returns 422 on UpdateCheckRun when the addressed
//         check run no longer exists (PR deleted, force-push
//         orphaned the SHA, etc).
//       - network: transport-level errors AND any other 4xx/5xx
//         the wrapper doesn't classify. This is the "I don't
//         know, drop it" branch — SIEM dashboards group these as
//         "transient" and rely on Squadron's normal retry posture
//         (slice-1: no retry; the check run is best-effort).
//
// The PAT token is held by the PATClient and supplied here via the
// pat argument. Per the file-level invariant in client.go (never
// log, never echo), the wrapper here ALSO refuses to put body bytes
// from GitHub into any returned error's Message field on auth-class
// failures — see the 401 branch.

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Error kind discriminators — the load-bearing classification the
// chunks-2/3/4 audit-emit paths key on. See file-level comment for
// the discrimination rules.
const (
	CheckRunErrorKindScopeMissing = "scope_missing"
	CheckRunErrorKindRateLimit    = "rate_limit"
	CheckRunErrorKindPRNotFound   = "pr_not_found"
	CheckRunErrorKindNetwork      = "network"
)

// CheckRunStatus values per the GitHub Checks API (see
// https://docs.github.com/en/rest/checks/runs). Exposed as constants
// so callers stop typo-ing the strings.
const (
	CheckRunStatusQueued     = "queued"
	CheckRunStatusInProgress = "in_progress"
	CheckRunStatusCompleted  = "completed"
)

// CheckRunConclusion values per the GitHub Checks API. Slice 1 only
// emits the three the design doc §7 names; the fuller GitHub set
// (cancelled, skipped, timed_out, action_required) is reserved for
// future slices.
const (
	CheckRunConclusionSuccess = "success"
	CheckRunConclusionFailure = "failure"
	CheckRunConclusionNeutral = "neutral"
)

// CheckRunRef is the durable identity of one check run on one
// commit. Mirrors the storage-layer applicationstore.CheckRunRef
// (defined in the types package to avoid a storage→iac/github
// import); the two are field-compatible.
type CheckRunRef struct {
	Owner   string
	Repo    string
	CheckID int64
	HeadSHA string
}

// CheckRunOutput is the user-visible content of a check run. Per
// the GitHub API, Summary has a 65535-char cap; the wrapper does NOT
// truncate — the caller (chunk-2 bridge) is responsible for
// composing the summary within the cap. Title is required by the
// API; Text is optional.
type CheckRunOutput struct {
	Title   string
	Summary string // markdown, max 65535 chars per GitHub API
	Text    string // optional longer markdown
}

// CheckRunCreate is the input shape for CreateCheckRun.
//
// Name defaults to "Squadron recommendation" at the bridge layer
// per §11 Q2; the wrapper takes it verbatim. Status is one of the
// CheckRunStatus* constants — slice 1 always opens with
// CheckRunStatusInProgress.
type CheckRunCreate struct {
	Owner     string
	Repo      string
	HeadSHA   string
	Name      string
	Status    string
	StartedAt time.Time
	Output    CheckRunOutput
}

// CheckRunUpdate is the input shape for UpdateCheckRun. Ref carries
// the durable identity; Status + Conclusion + CompletedAt frame the
// transition; Output replaces the summary block.
type CheckRunUpdate struct {
	Ref         CheckRunRef
	Status      string
	Conclusion  string
	CompletedAt time.Time
	Output      CheckRunOutput
}

// CheckRunError is the structured error returned by the two
// methods. Kind is one of the CheckRunErrorKind* constants; Status
// is the HTTP status (0 for transport-level failures); Message is
// the wrapper's own diagnostic (NEVER GitHub response-body bytes on
// the auth-class branches, per the file-level invariant).
type CheckRunError struct {
	Kind    string
	Status  int
	Message string
}

func (e *CheckRunError) Error() string {
	return fmt.Sprintf("check_run error (kind=%s, status=%d): %s",
		e.Kind, e.Status, e.Message)
}

// CreateCheckRun posts a new check run on the given commit. Returns
// the durable CheckRunRef on success; on failure returns
// *CheckRunError with Kind set to one of the CheckRunErrorKind*
// constants. Callers MUST treat the failure as fail-open (drop the
// check run, log via the iac.check_run.failed audit event, continue).
func (c *PATClient) CreateCheckRun(ctx context.Context, pat string, req CheckRunCreate) (CheckRunRef, error) {
	if req.Owner == "" || req.Repo == "" || req.HeadSHA == "" {
		return CheckRunRef{}, &CheckRunError{
			Kind:    CheckRunErrorKindNetwork,
			Message: "missing owner / repo / head_sha",
		}
	}
	name := req.Name
	if name == "" {
		name = "Squadron recommendation"
	}
	payload := map[string]any{
		"name":     name,
		"head_sha": req.HeadSHA,
	}
	if req.Status != "" {
		payload["status"] = req.Status
	}
	if !req.StartedAt.IsZero() {
		payload["started_at"] = req.StartedAt.UTC().Format(time.RFC3339)
	}
	if req.Output.Title != "" || req.Output.Summary != "" || req.Output.Text != "" {
		payload["output"] = checkRunOutputPayload(req.Output)
	}
	body, _ := json.Marshal(payload)
	apiPath := fmt.Sprintf("/repos/%s/%s/check-runs",
		url.PathEscape(req.Owner), url.PathEscape(req.Repo))
	resp, respBytes, err := c.doCheckRun(ctx, pat, http.MethodPost, apiPath, bytes.NewReader(body))
	if err != nil {
		// Transport-level: classified as network. The token never
		// lands in this branch (http.Client.Do strips request
		// headers from its error string).
		return CheckRunRef{}, &CheckRunError{
			Kind:    CheckRunErrorKindNetwork,
			Message: err.Error(),
		}
	}
	if cerr := classifyCheckRunResponse(resp, respBytes); cerr != nil {
		return CheckRunRef{}, cerr
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var raw struct {
			ID      int64  `json:"id"`
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(respBytes, &raw); err != nil {
			return CheckRunRef{}, &CheckRunError{
				Kind:    CheckRunErrorKindNetwork,
				Status:  resp.StatusCode,
				Message: fmt.Sprintf("decode check-run response: %v", err),
			}
		}
		// Prefer the response's head_sha (GitHub may canonicalize
		// the SHA case); fall back to the request's value if the
		// response shape didn't carry it (defensive).
		headSHA := raw.HeadSHA
		if headSHA == "" {
			headSHA = req.HeadSHA
		}
		return CheckRunRef{
			Owner:   req.Owner,
			Repo:    req.Repo,
			CheckID: raw.ID,
			HeadSHA: headSHA,
		}, nil
	default:
		// Unclassified status — classify as network and surface the
		// status so the audit row shows it.
		return CheckRunRef{}, &CheckRunError{
			Kind:    CheckRunErrorKindNetwork,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("unexpected status %d", resp.StatusCode),
		}
	}
}

// UpdateCheckRun patches an existing check run identified by
// req.Ref. Same fail-open posture as CreateCheckRun; same error-kind
// vocabulary on failure.
func (c *PATClient) UpdateCheckRun(ctx context.Context, pat string, req CheckRunUpdate) error {
	if req.Ref.Owner == "" || req.Ref.Repo == "" || req.Ref.CheckID == 0 {
		return &CheckRunError{
			Kind:    CheckRunErrorKindNetwork,
			Message: "missing owner / repo / check_id",
		}
	}
	payload := map[string]any{}
	if req.Status != "" {
		payload["status"] = req.Status
	}
	if req.Conclusion != "" {
		payload["conclusion"] = req.Conclusion
	}
	if !req.CompletedAt.IsZero() {
		payload["completed_at"] = req.CompletedAt.UTC().Format(time.RFC3339)
	}
	if req.Output.Title != "" || req.Output.Summary != "" || req.Output.Text != "" {
		payload["output"] = checkRunOutputPayload(req.Output)
	}
	body, _ := json.Marshal(payload)
	apiPath := fmt.Sprintf("/repos/%s/%s/check-runs/%d",
		url.PathEscape(req.Ref.Owner), url.PathEscape(req.Ref.Repo), req.Ref.CheckID)
	resp, respBytes, err := c.doCheckRun(ctx, pat, http.MethodPatch, apiPath, bytes.NewReader(body))
	if err != nil {
		return &CheckRunError{
			Kind:    CheckRunErrorKindNetwork,
			Message: err.Error(),
		}
	}
	if cerr := classifyCheckRunResponse(resp, respBytes); cerr != nil {
		return cerr
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	default:
		return &CheckRunError{
			Kind:    CheckRunErrorKindNetwork,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("unexpected status %d", resp.StatusCode),
		}
	}
}

// checkRunOutputPayload renders the CheckRunOutput in the shape the
// GitHub API expects. Title is required when output is set; the
// caller is responsible for supplying it (the bridge composes
// "Recommendation: <kind>" per §9.1 of the design doc).
func checkRunOutputPayload(o CheckRunOutput) map[string]any {
	out := map[string]any{
		"title":   o.Title,
		"summary": o.Summary,
	}
	if o.Text != "" {
		out["text"] = o.Text
	}
	return out
}

// doCheckRun is the single I/O chokepoint for the Checks API calls.
// Sets auth headers (separate pat argument, NOT the PATClient's
// stored token — the chunk-2 bridge supplies the connection-scoped
// PAT explicitly), Accept, X-GitHub-Api-Version, and User-Agent.
// Reads the body once and returns it alongside the response so the
// classification helper can pattern-match without a second read.
//
// We deliberately do NOT route through (*PATClient).do — that
// helper uses the stored c.token, and the chunk-2/3/4 bridge wires
// in a PAT that's keyed per-connection at call time. Reusing the
// transport (c.httpClient) gets us the same timeout + injection
// posture for tests while keeping the credential model explicit.
func (c *PATClient) doCheckRun(ctx context.Context, pat, method, path string, body io.Reader) (*http.Response, []byte, error) {
	if !strings.HasPrefix(path, "http") {
		path = c.baseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, path, body)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+pat)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	return resp, respBytes, nil
}

// classifyCheckRunResponse is the load-bearing discrimination
// logic. Returns a typed *CheckRunError when the response should be
// surfaced as a structured failure; returns nil to let the caller
// keep going on the success / unrecognized-status paths.
//
// Order matters: rate-limit checks fire BEFORE scope_missing
// because a 403 with X-RateLimit-Remaining=0 is a rate-limit
// failure, NOT a scope failure. The header check is the canonical
// GitHub signal per their API docs.
func classifyCheckRunResponse(resp *http.Response, body []byte) *CheckRunError {
	// Rate-limit: 429, OR 4xx with X-RateLimit-Remaining=0. The
	// remaining-header check is the GitHub-documented primary
	// rate-limit signal on REST endpoints (they routinely return
	// 403 with the headers populated rather than 429).
	if resp.StatusCode == http.StatusTooManyRequests {
		return &CheckRunError{
			Kind:    CheckRunErrorKindRateLimit,
			Status:  resp.StatusCode,
			Message: rateLimitMessage(resp),
		}
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return &CheckRunError{
				Kind:    CheckRunErrorKindRateLimit,
				Status:  resp.StatusCode,
				Message: rateLimitMessage(resp),
			}
		}
	}

	// Scope-missing: 403 with the canonical "Resource not
	// accessible" wording, OR 404 (GitHub's opaque endpoint-not-
	// found response when a fine-grained PAT lacks the scope; see
	// design doc §5).
	if resp.StatusCode == http.StatusForbidden {
		msg := bodyMessageField(body)
		if strings.Contains(msg, "Resource not accessible") {
			return &CheckRunError{
				Kind:    CheckRunErrorKindScopeMissing,
				Status:  resp.StatusCode,
				Message: "PAT lacks checks:write scope",
			}
		}
		// Bare 403 without the resource-not-accessible message:
		// also a scope-class failure on the Checks API endpoint.
		return &CheckRunError{
			Kind:    CheckRunErrorKindScopeMissing,
			Status:  resp.StatusCode,
			Message: "PAT scope insufficient for Checks API",
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		return &CheckRunError{
			Kind:    CheckRunErrorKindScopeMissing,
			Status:  resp.StatusCode,
			Message: "Checks API endpoint not found (likely missing PAT scope)",
		}
	}

	// PR-not-found: 422 from the Checks API typically means the
	// addressed check run no longer exists (PR deleted, force-push
	// orphaned the SHA, etc). Surfaced as pr_not_found so the
	// audit row is honest.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return &CheckRunError{
			Kind:    CheckRunErrorKindPRNotFound,
			Status:  resp.StatusCode,
			Message: "check run target not found (PR may have been deleted or force-pushed)",
		}
	}

	// 401: auth failed. Map to scope_missing — the PAT is either
	// expired or never had the scope; the humanizer surfaces the
	// same fix-it copy in both cases.
	if resp.StatusCode == http.StatusUnauthorized {
		// Body bytes deliberately NOT included — the auth-failed
		// branch of GitHub's error pages can echo the token in
		// pathological cases.
		return &CheckRunError{
			Kind:    CheckRunErrorKindScopeMissing,
			Status:  resp.StatusCode,
			Message: "PAT authentication failed",
		}
	}

	// 5xx: classified as network so SIEM dashboards group with
	// transient failures.
	if resp.StatusCode >= 500 {
		return &CheckRunError{
			Kind:    CheckRunErrorKindNetwork,
			Status:  resp.StatusCode,
			Message: fmt.Sprintf("GitHub server error %d", resp.StatusCode),
		}
	}

	return nil
}

// rateLimitMessage assembles a humanized message that surfaces the
// reset timestamp (if present) so the SIEM dashboard's "when does
// this clear?" panel can read it without parsing prose. The reset
// header is a Unix timestamp; we keep the raw value rather than
// formatting because the audit emit path will re-format with the
// operator's timezone.
func rateLimitMessage(resp *http.Response) string {
	reset := resp.Header.Get("X-RateLimit-Reset")
	if reset == "" {
		return "GitHub API rate limit exceeded"
	}
	return fmt.Sprintf("GitHub API rate limit exceeded (reset=%s)", reset)
}

// bodyMessageField pulls the GitHub error response's "message"
// field. Returns "" on any decode failure; the classifier treats
// empty as "no specific signal, fall through to the generic 403
// branch."
func bodyMessageField(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var raw struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	return raw.Message
}
