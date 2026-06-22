// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	"github.com/devopsmike2/squadron/internal/services"
)

// defaultSquadronBranchPrefix is the slice-1 default prefix the
// webhook receiver strips off the head branch when parsing the
// recommendation kind. Matches iacconnstore.DefaultBranchPrefix —
// the constant is repeated here (as a sane fallback for
// IaCGitHubWebhookHandler.branchPrefix) so the webhook surface owns
// its own copy and slice-2 per-connection prefixes can be threaded
// through without churning the substrate's default.
//
// The handler's branch-prefix field is the operative value: a
// constructor call may pass a different prefix (today nobody does;
// slice 1 ships one prefix per deployment). Substrate-side per-
// connection prefixes are a slice 2 concern.
const defaultSquadronBranchPrefix = "squadron/rec/"

// gitHubWebhookSecretEnvVar names the environment variable the
// deployment-time wiring reads to seed the webhook receiver's shared
// secret. The variable is surfaced in the 503-when-unconfigured
// response body so the operator reading the GitHub webhook delivery
// log sees exactly which knob to turn.
const gitHubWebhookSecretEnvVar = "SQUADRON_GITHUB_WEBHOOK_SECRET"

// IaCGitHubWebhookHandler serves POST /api/v1/webhooks/github — the
// GitHub-side delivery target the operator wires into their repo's
// webhook settings. Receives pull_request events, validates the
// X-Hub-Signature-256 HMAC against the deployment-wide secret, and
// records a recommendation.pr_merged audit event when the action is
// "closed" + merged == true. The handler is intentionally lenient
// about everything else — unknown event types, malformed branches,
// no matching connection — because GitHub's redelivery system
// punishes 5xx by retrying and 4xx is reserved for "the operator
// will see this in their webhook delivery log and recognize it as
// configuration drift Squadron can't recover from on its own".
//
// Slice 1 trade-offs (per the v0.89.23 plan):
//   - one shared deployment-wide secret via env var, not per-
//     connection rotation
//   - no X-GitHub-Delivery dedupe (replay protection is slice 2)
//   - no GitHub Checks API back-signal (Squadron only listens; it
//     doesn't talk back at the PR level)
//   - no UI for entering the secret (env var only)
//   - no backfill of pre-existing merges
type IaCGitHubWebhookHandler struct {
	auditService services.AuditService
	store        iacconnstore.Store
	// secret is the raw HMAC key bytes, cached at construct time so
	// HandleWebhook doesn't read the environment on every request.
	// An empty (nil or zero-length) secret means the handler is
	// configured-but-disabled — HandleWebhook 503s on every call
	// with a humanized body naming the env var.
	secret       []byte
	logger       *zap.Logger
	branchPrefix string
}

// NewIaCGitHubWebhookHandler constructs an IaCGitHubWebhookHandler.
// Callers seed `secret` from os.Getenv(gitHubWebhookSecretEnvVar) at
// startup; the constructor accepts a nil/empty slice and the
// resulting handler 503s — the deployment misconfiguration is
// surfaced via the GitHub webhook delivery log rather than a silent
// "the listener was never wired" no-op.
//
// The logger MUST be non-nil; we accept a zap.NewNop() in tests.
func NewIaCGitHubWebhookHandler(
	auditSvc services.AuditService,
	store iacconnstore.Store,
	secret []byte,
	logger *zap.Logger,
) *IaCGitHubWebhookHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	// Defensive copy of the secret so the caller can scrub their
	// own buffer post-construction. Even though the env-var case
	// strings the bytes through unchanged, an embedder wiring a
	// rotating-keys flow in slice 2 can pass a per-call slice
	// without worrying the handler retained the original backing
	// array.
	var stored []byte
	if len(secret) > 0 {
		stored = make([]byte, len(secret))
		copy(stored, secret)
	}
	return &IaCGitHubWebhookHandler{
		auditService: auditSvc,
		store:        store,
		secret:       stored,
		logger:       logger,
		branchPrefix: defaultSquadronBranchPrefix,
	}
}

// gitHubPullRequestEvent is the slice-1 subset of GitHub's
// pull_request webhook payload — only the fields the merge-detection
// + audit-emit path reads. We intentionally don't pull in a
// third-party GitHub library: the payload's shape is stable, the
// dependency surface stays tight, and the JSON struct doubles as a
// contract test in the test file.
//
// merged_by is a pointer because GitHub sends `null` when the PR is
// closed without being merged (the action is "closed" but
// pull_request.merged is false). The non-pointer reading path would
// crash on Login lookup; the pointer reading path is the obvious
// shape.
type gitHubPullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number   int    `json:"number"`
		Merged   bool   `json:"merged"`
		MergedAt string `json:"merged_at"`
		HTMLURL  string `json:"html_url"`
		Head     struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		MergedBy *struct {
			Login string `json:"login"`
		} `json:"merged_by"`
	} `json:"pull_request"`
}

// HandleWebhook is the POST /api/v1/webhooks/github entry point.
//
// Lifecycle:
//  1. 503 if no secret was configured (deployment misconfiguration).
//  2. Read the body bytes (needed for HMAC; can't re-encode JSON
//     and expect the signature to match GitHub's view).
//  3. Read X-Hub-Signature-256, strip "sha256=", hex-decode.
//  4. Constant-time HMAC compare via hmac.Equal — NOT bytes.Equal.
//  5. 200 + ignored for any non-pull_request event type.
//  6. Unmarshal the body into gitHubPullRequestEvent.
//  7. 200 + ignored for action != "closed" or merged == false.
//  8. Best-effort connection lookup by repo full name — nil result
//     is fine, we still emit the audit row with connection_id="".
//  9. Parse recommendation_kind from the head branch via
//     parseRecommendationKindFromBranch — empty string when the
//     branch isn't Squadron-shaped is honest reporting.
// 10. Emit recommendation.pr_merged via auditSvc.Record.
//
// Status codes:
//   - 200 — handled OR honestly ignored (unknown event, not merged,
//     no matching connection). GitHub's redelivery system reads 200
//     as "delivered" and doesn't retry.
//   - 400 — body unreadable or unmarshalable. Operator-facing
//     signal that the payload shape doesn't match what we expect;
//     not retriable.
//   - 401 — signature missing, malformed, or doesn't match. Same
//     posture as above — retrying with the same payload + same
//     headers is futile.
//   - 503 — secret unconfigured. Recoverable by the operator
//     setting the env var and restarting; GitHub will retry the
//     delivery, but it'll keep failing until the operator acts.
//     The body names the env var explicitly so the operator
//     reading the GitHub webhook delivery log sees exactly which
//     knob to turn.
func (h *IaCGitHubWebhookHandler) HandleWebhook(c *gin.Context) {
	if len(h.secret) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":  "webhook secret not configured",
			"detail": "set " + gitHubWebhookSecretEnvVar + " to enable the GitHub PR-merged listener",
		})
		return
	}

	body, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "request body could not be read"})
		return
	}

	sig := c.GetHeader("X-Hub-Signature-256")
	if !strings.HasPrefix(sig, "sha256=") {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}
	// Strip the algorithm prefix. The remainder is hex(HMAC-SHA256).
	hexSig := strings.TrimPrefix(sig, "sha256=")
	provided, err := hex.DecodeString(hexSig)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	mac := hmac.New(sha256.New, h.secret)
	mac.Write(body)
	expected := mac.Sum(nil)
	// hmac.Equal is the constant-time compare; bytes.Equal would
	// leak per-byte timing. Do NOT log either side of this
	// comparison — that's an attacker-visible side channel via the
	// SIEM forwarder.
	if !hmac.Equal(expected, provided) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	eventType := c.GetHeader("X-GitHub-Event")
	if eventType != "pull_request" {
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"ignored": true,
			"event":   eventType,
		})
		return
	}

	var ev gitHubPullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "pull_request payload could not be parsed"})
		return
	}

	if ev.Action != "closed" {
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"ignored": true,
			"reason":  "pr_action_not_closed",
		})
		return
	}
	if !ev.PullRequest.Merged {
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"ignored": true,
			"reason":  "pr_closed_not_merged",
		})
		return
	}

	// Best-effort connection lookup. nil result is fine: the merge
	// is real (the HMAC passed, GitHub said merged=true) so the
	// audit row goes out regardless. Log a warning so an operator
	// debugging "why didn't my webhook fire" can find the trail.
	var connectionID string
	if h.store != nil {
		conn, err := h.store.GetByRepoFullName(c.Request.Context(), ev.PullRequest.Base.Repo.FullName)
		switch {
		case err == nil && conn != nil:
			connectionID = conn.ConnectionID
		case errors.Is(err, iacconnstore.ErrConnectionNotFound):
			h.logger.Warn("iac github webhook: pr_merged received but no IaC connection matches repo",
				zap.String("repo_full_name", ev.PullRequest.Base.Repo.FullName),
				zap.Int("pr_number", ev.PullRequest.Number),
			)
		default:
			h.logger.Warn("iac github webhook: connection lookup failed; emitting audit with empty connection_id",
				zap.Error(err),
				zap.String("repo_full_name", ev.PullRequest.Base.Repo.FullName),
			)
		}
	}

	// Parse the recommendation kind off the branch suffix. Empty
	// string when the branch isn't a Squadron-shaped one is the
	// honest report — the operator may have merged a hand-authored
	// PR in a connected repo and we shouldn't pretend it carries a
	// Squadron recommendation kind. v0.89.28 (#643 slice 1) extends
	// the parse to also extract account_id + region from the new
	// 6-segment branch shape; older 4-segment branches still parse
	// the kind cleanly and produce empty account_id / region.
	branch := ev.PullRequest.Head.Ref
	kind, accountID, region, _ := parseRecommendationScopeFromBranch(branch, h.branchPrefix)

	var mergedBy string
	if ev.PullRequest.MergedBy != nil {
		mergedBy = ev.PullRequest.MergedBy.Login
	}

	if h.auditService != nil {
		payload := map[string]any{
			"repo_full_name":      ev.PullRequest.Base.Repo.FullName,
			"pr_number":           ev.PullRequest.Number,
			"pr_url":              ev.PullRequest.HTMLURL,
			"branch":              branch,
			"merged_at":           ev.PullRequest.MergedAt,
			"merged_by":           mergedBy,
			"recommendation_kind": kind,
			"connection_id":       connectionID,
			"recorded_at":         time.Now().UTC(),
		}
		// account_id + region are optional in the payload — omit on
		// the older 4-segment branch shape so SIEM consumers can tell
		// scope-encoded merges apart from pre-extension ones.
		if accountID != "" {
			payload["account_id"] = accountID
		}
		if region != "" {
			payload["region"] = region
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      "github_webhook",
			EventType:  services.AuditEventRecommendationPRMerged,
			TargetType: services.AuditTargetIaCRecommendation,
			TargetID:   connectionID,
			Action:     "pr_merged",
			Payload:    payload,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":                  true,
		"audit_event_emitted": true,
	})
}

// parseRecommendationKindFromBranch is the v0.89.23 entry point that
// callers still use to extract just the recommendation_kind. It now
// delegates to parseRecommendationScopeFromBranch so the parsing logic
// lives in one place; this wrapper preserves the original return
// signature for callers that don't need account_id + region.
func parseRecommendationKindFromBranch(branch, prefix string) (string, bool) {
	kind, _, _, ok := parseRecommendationScopeFromBranch(branch, prefix)
	return kind, ok
}

// parseRecommendationScopeFromBranch — v0.89.28 (#643 slice 1) —
// extracts the recommendation kind, account_id, and region segments
// from a Squadron-shaped branch name.
//
// The v0.89.28 branch encoding is
//   "<prefix><kind>/<account_id>/<region>/<short_id>"
// where prefix is the trailing-slash variant ("squadron/rec/"). Older
// branches that pre-date the encoding extension are
//   "<prefix><kind>/<short_id>"
// (no account_id / region segments); the parser accepts both shapes
// so a webhook fired against a PR opened on the previous release
// still parses the kind cleanly. account_id and region come back
// empty for the older shape — the webhook handler treats them as
// optional in the audit payload.
//
// Return contract:
//   - branch doesn't start with prefix → ("", "", "", false)
//   - branch starts with prefix but the first segment after prefix is
//     empty (e.g. "squadron/rec/") → ("", "", "", false)
//   - 4-segment shape "squadron/rec/<kind>/<short_id>" or anything
//     with 1 segment after prefix → (kind, "", "", true)
//   - 6-segment shape
//     "squadron/rec/<kind>/<account_id>/<region>/<short_id>" →
//     (kind, account_id, region, true)
func parseRecommendationScopeFromBranch(branch, prefix string) (kind, accountID, region string, ok bool) {
	if prefix == "" || !strings.HasPrefix(branch, prefix) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(branch, prefix)
	if rest == "" {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", "", false
	}
	kind = parts[0]
	// New 6-segment encoding: kind / account_id / region / short_id.
	// We DO NOT require exactly 4 trailing segments — anything 3+ past
	// the kind passes; the spec's encoding is the common case but a
	// hand-pushed branch with an extra path component shouldn't bin
	// out as kind-only.
	if len(parts) >= 4 && parts[1] != "" && parts[2] != "" {
		accountID = parts[1]
		region = parts[2]
	}
	return kind, accountID, region, true
}
