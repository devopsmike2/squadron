// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// This file is the chunk-3 webhook-side integration for the GitHub
// Checks API back-signal arc (v0.89.44, #664 Stream 62). It rides on
// the existing IaCGitHubWebhookHandler in iac_github_webhook.go:
// AFTER the recommendation.pr_merged OR
// recommendation.pr_closed_not_merged audit event emits, this file's
// updateCheckRunWithConclusion helper looks up the durable check-run
// state via GetCheckRunForRecommendation, composes the update
// summary via internal/proposer/checkrunprompt.ComposeUpdateSummary,
// PATCHes the live check run on GitHub, persists the new
// status+conclusion via SetCheckRunForRecommendation, and emits the
// iac.check_run.updated audit event.
//
// Fail-open is load-bearing per design doc §5 + §7.2. Every failure
// path either silently no-ops (the unwired-deployment path) or emits
// iac.check_run.failed with a structured error_kind and returns. The
// original recommendation.pr_merged / .pr_closed_not_merged audit
// event has ALREADY completed by the time control reaches here.
//
// Helper layering:
//   - WebhookChecksAPI is the slim interface the webhook handler
//     depends on for UpdateCheckRun. It's a deliberate sibling to
//     ChecksAPI (chunk 2's create surface) so each chunk's helper can
//     evolve independently. A single *iacgithub.PATClient satisfies
//     both in production.
//   - WebhookCheckRunStore is the slim subset of
//     applicationstore.ApplicationStore the helper reads from + writes
//     to (Get + Set).
//   - checkRunUpdateArgs is the per-call payload — one struct so the
//     call site at the two audit-emit branches stays a single line.
//
// SummaryInput composition: per the chunk-3 brief, we deliberately
// pick option (c) — minimal SummaryInput with empty VerdictsByState.
// The verdict-learning section is OUT of the update summary because
// it's already in the audit log from the original PR-open audit
// event; recomputing it from storage would add query cost for no
// operator-visible benefit at the update moment. The
// ComposeUpdateSummary template handles an empty VerdictsByState
// via the same cold-start gate ComposeCreateSummary uses.

package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"

	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/proposer/checkrunprompt"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// WebhookChecksAPI is the slim interface the chunk-3 webhook-side
// follow-up depends on. Satisfied by *iacgithub.PATClient in
// production and by a recording fake in tests. Separate from chunk
// 2's ChecksAPI so the create and update wire-shapes can evolve
// independently — a single *iacgithub.PATClient satisfies both.
type WebhookChecksAPI interface {
	UpdateCheckRun(ctx context.Context, pat string, req iacgithub.CheckRunUpdate) error
}

// WebhookCheckRunStore is the slim subset of
// applicationstore.ApplicationStore the chunk-3 follow-up reads from
// and writes to. Mirrors chunk 2's CheckRunStore, plus the Get half:
// the webhook path looks up the existing check run BEFORE patching.
type WebhookCheckRunStore interface {
	GetCheckRunForRecommendation(ctx context.Context, recommendationID string) (ref types.CheckRunRef, status string, conclusion string, exists bool, err error)
	SetCheckRunForRecommendation(ctx context.Context,
		rec types.ExcludedRecommendation,
		ref types.CheckRunRef,
		status, conclusion string,
	) error
}

// checkRunUpdateArgs is the per-call payload the merge / close branch
// hands to updateCheckRunWithConclusion. One struct keeps the two
// call sites to a single line each; per-field positional args drift
// fast as the arc adds chunk-4 fields.
//
// RecommendationID is intentionally NOT carried in this struct — the
// webhook payload doesn't have it; the helper recovers it from the
// chunk-2 iac.check_run.created audit-log pivot.
type checkRunUpdateArgs struct {
	// ConnectionID is the scope tuple's connection identifier. Carried
	// into the iac.check_run.updated audit payload AND used to scope
	// the audit-log pivot that recovers recommendation_id.
	ConnectionID string

	// AccountID + Region are the rest of the scope tuple. Optional on
	// pre-v0.89.28 branches that didn't encode the segments; the
	// ExcludedRecommendation upsert preserves whatever the original
	// row carries (the storage layer's invariant is that scope fields
	// don't change once persisted — see types.go SetCheckRunForRecommendation).
	AccountID string
	Region    string

	// RecommendationKind is the proposer-emitted kind. Carried into
	// the audit payload AND into the update summary's title.
	RecommendationKind string

	// PRURL is the GitHub PR URL. The webhook payload carries this
	// verbatim from GitHub; the helper uses it as the pivot key when
	// scanning the audit log for the matching iac.check_run.created
	// event.
	PRURL string
}

// checkRunCreatedAuditLookback bounds how far back the helper looks
// in the audit log to recover recommendation_id from the chunk-2
// iac.check_run.created pivot. Bound exists for two reasons:
//
//  1. Bounded query cost — a deployment with millions of audit rows
//     shouldn't sweep the whole table on every inbound merge / close
//     event.
//  2. A PR sitting open for 90 days that gets merged is the slowest
//     reasonable case the brief's design doc names. 365 days
//     comfortably exceeds this; we pick it explicitly so a long-
//     running PR's merge / close still finds its create pivot.
//
// Set to a year — captures the long-tail PR lifecycle the slice-1
// design doc tolerates while keeping the audit-log scan O(audit-rows-
// per-connection-per-year), which is typically O(hundreds) on a
// healthy deployment.
const checkRunCreatedAuditLookback = 365 * 24 * time.Hour

// checkRunCreatedAuditLimit caps the page size of the audit-log scan
// the helper issues to recover recommendation_id. Larger than the
// expected per-connection check-run-created count over the lookback
// window; bounded so a misconfigured deployment with thousands of
// open PRs on one connection doesn't blow up the lookup.
const checkRunCreatedAuditLimit = 500

// updateCheckRunForPRMerged is the merge branch's entry point: maps
// to conclusion=success. The two-tier shape (updateCheckRunForPR* +
// shared updateCheckRunWithConclusion) keeps the two audit-emit call
// sites symmetric and gives the test surface two distinct functions
// to assert against per the chunk-3 brief's Step 1 contract.
func (h *IaCGitHubWebhookHandler) updateCheckRunForPRMerged(ctx context.Context, args checkRunUpdateArgs) {
	h.updateCheckRunWithConclusion(ctx, args, iacgithub.CheckRunConclusionSuccess)
}

// updateCheckRunForPRClosed is the close-without-merge branch's
// entry point: maps to conclusion=failure.
func (h *IaCGitHubWebhookHandler) updateCheckRunForPRClosed(ctx context.Context, args checkRunUpdateArgs) {
	h.updateCheckRunWithConclusion(ctx, args, iacgithub.CheckRunConclusionFailure)
}

// updateCheckRunWithConclusion is the shared body. The five-step
// dance the chunk-3 brief documents:
//
//  1. Short-circuit silently when h.checksClient is nil (Checks API
//     not wired for this deployment) OR h.checkRunStore is nil
//     (storage not wired) OR h.auditService is nil (audit-log pivot
//     unavailable, so we have no way to recover recommendation_id
//     from the webhook payload).
//  2. Recover recommendation_id from the chunk-2
//     iac.check_run.created audit event keyed by (connection_id,
//     pr_url). The webhook payload doesn't carry recommendation_id —
//     the branch-name encoding only round-trips scan_id+step_idx —
//     so the chunk-2 audit row is the authoritative pivot. Missing
//     pivot → fail-open silent skip (the chunk-2 path didn't fire on
//     this PR, possibly because Checks API was unwired at PR open).
//  3. GetCheckRunForRecommendation. If exists=false: no check run was
//     ever created for this PR. Emit nothing — fail-open per design
//     doc §5.
//  4. ComposeUpdateSummary via the chunk-2 prompt package. The
//     SummaryInput is minimal — see the file-level comment on the
//     option (c) decision.
//  5. Build iacgithub.CheckRunUpdate with the existing ref + new
//     status=completed + conclusion + completed_at=time.Now().UTC() +
//     Output{Title, Summary}.
//  6. Call h.checksClient.UpdateCheckRun. On error: emit
//     iac.check_run.failed with error_kind discriminator from the
//     CheckRunError, drop and continue. On success: update storage
//     via SetCheckRunForRecommendation with the new status +
//     conclusion, then emit iac.check_run.updated with payload that
//     includes previous_status + previous_conclusion + new_status +
//     new_conclusion per design doc §8.
func (h *IaCGitHubWebhookHandler) updateCheckRunWithConclusion(
	ctx context.Context,
	args checkRunUpdateArgs,
	conclusion string,
) {
	if h.checksClient == nil {
		// Slice-1 fail-open: deployment hasn't wired the Checks API
		// integration. The existing recommendation.pr_merged /
		// .pr_closed_not_merged audit event has already fired; the
		// check run is a value-add we silently skip.
		return
	}
	if h.checkRunStore == nil {
		// Storage substrate not wired — we cannot look up the
		// existing check run to know which check_run_id to PATCH.
		// Same fail-open silence as the unwired-client case.
		return
	}
	if h.auditService == nil {
		// Audit-log pivot is the only way the webhook handler can
		// recover recommendation_id from a webhook payload (the
		// branch encoding doesn't round-trip the id). Without it,
		// fail-open silent skip.
		return
	}
	if strings.TrimSpace(args.PRURL) == "" || strings.TrimSpace(args.ConnectionID) == "" {
		// Both fields are required for the audit-log pivot — pr_url
		// is the match key and connection_id is the scope filter.
		// The recommendation.pr_merged / .pr_closed_not_merged audit
		// emit path stamps both reliably; an empty value here means
		// we received a webhook whose branch didn't match a
		// connection (TargetID="" path) — fail-open silent skip.
		return
	}

	recommendationID := h.findRecommendationIDForPR(ctx, args.ConnectionID, args.PRURL)
	if recommendationID == "" {
		// No chunk-2 iac.check_run.created event found for this
		// (connection_id, pr_url) tuple — chunk-2 path didn't fire
		// or the audit log is gone. Fail-open silent skip per
		// design doc §5.
		return
	}

	ref, prevStatus, prevConclusion, exists, err := h.checkRunStore.GetCheckRunForRecommendation(ctx, recommendationID)
	if err != nil {
		// Storage read failure — log and fall through. The original
		// merge / close audit row already landed; the check run can
		// reconcile on slice-2's reconciliation job (design doc §11
		// Q4). Do NOT emit iac.check_run.failed for a STORAGE read
		// failure — that audit event is reserved for GitHub-side API
		// failures so SIEM consumers can cleanly partition the two.
		if h.logger != nil {
			h.logger.Warn("iac github webhook: check-run state lookup failed; skipping update",
				zap.String("recommendation_id", recommendationID),
				zap.Error(err),
			)
		}
		return
	}
	if !exists {
		// No check run was ever created for this PR. Chunk-2 path
		// didn't fire — possibly the PR was opened before Checks API
		// enablement. Fail-open silently.
		return
	}
	if ref.CheckID == 0 {
		// Row exists but no check_run_id is stored on it. This can
		// happen on the boundary case where chunk 4's
		// ExcludeRecommendation handler created the row before the
		// chunk-2 PR-open path got a chance to write the
		// check_run_id. Fail-open silently.
		return
	}

	// Compose the update summary via the chunk-2 prompt package.
	// Per the chunk-3 brief Step 2, this is the minimal-SummaryInput
	// option (c). RecommendationReason is empty; the
	// ComposeUpdateSummary template renders
	// "_(no reasoning provided)_" for the "What this PR does" line
	// (verified via the sanitizeReasoning code path in
	// checkrunprompt.go). The verdict-learning section is omitted
	// because VerdictsByState is nil (cold-start gate).
	in := checkrunprompt.SummaryInput{
		RecommendationKind: args.RecommendationKind,
		AccountID:          args.AccountID,
		Region:             args.Region,
		ConnectionID:       args.ConnectionID,
		PRURL:              args.PRURL,
		RecommendationID:   recommendationID,
		SquadronHost:       h.squadronHost,
	}
	title, summary := checkrunprompt.ComposeUpdateSummary(in, conclusion)

	// Convert the storage-layer types.CheckRunRef into the iac/github
	// CheckRunRef. The two are field-compatible by design (see types.go
	// CheckRunRef doc); copying field-by-field keeps the storage
	// package off the iac/github import.
	apiRef := iacgithub.CheckRunRef{
		Owner:   ref.Owner,
		Repo:    ref.Repo,
		CheckID: ref.CheckID,
		HeadSHA: ref.HeadSHA,
	}
	req := iacgithub.CheckRunUpdate{
		Ref:         apiRef,
		Status:      iacgithub.CheckRunStatusCompleted,
		Conclusion:  conclusion,
		CompletedAt: time.Now().UTC(),
		Output: iacgithub.CheckRunOutput{
			Title:   title,
			Summary: summary,
		},
	}

	if err := h.checksClient.UpdateCheckRun(ctx, h.pat, req); err != nil {
		h.emitCheckRunUpdateFailedAudit(ctx, args, recommendationID, ref, conclusion, err)
		return
	}

	// Persist the new state. The ExcludedRecommendation projection
	// carries the scope tuple verbatim — the storage layer's invariant
	// is to NOT overwrite the scope fields on the upsert path (see
	// types.go SetCheckRunForRecommendation contract), so the values
	// we hand in here are only used to satisfy the row-creation path
	// if (somehow) the row was deleted between the Get and the Set.
	// The chunk-2 path persisted the row on PR open, so in practice
	// we always upsert an existing row.
	rec := types.ExcludedRecommendation{
		RecommendationID:   recommendationID,
		ConnectionID:       args.ConnectionID,
		AccountID:          args.AccountID,
		Region:             args.Region,
		RecommendationKind: args.RecommendationKind,
	}
	if werr := h.checkRunStore.SetCheckRunForRecommendation(
		ctx, rec, ref, iacgithub.CheckRunStatusCompleted, conclusion,
	); werr != nil {
		// Storage write failed AFTER GitHub accepted the PATCH. The
		// live check run on GitHub is in the new state; our durable
		// view drifted. Log but emit the success audit anyway — the
		// audit row is the authoritative record of "we patched the
		// check run." Slice-2's reconciliation job will detect the
		// drift on startup.
		if h.logger != nil {
			h.logger.Warn("iac github webhook: check run updated on GitHub but storage write failed",
				zap.Error(werr),
				zap.Int64("check_run_id", ref.CheckID),
				zap.String("recommendation_id", recommendationID),
			)
		}
	}

	h.emitCheckRunUpdatedAudit(ctx, args, recommendationID, ref, prevStatus, prevConclusion, conclusion)
}

// findRecommendationIDForPR scans the audit log for the chunk-2
// iac.check_run.created event matching (connection_id, pr_url) and
// returns the recommendation_id stamped on its payload. Returns ""
// when no matching event exists OR the audit-list call errors.
//
// The query is scoped by TargetID=connection_id and a lookback
// window (checkRunCreatedAuditLookback). Within those bounds the
// helper iterates newest-first (the auditService.List contract)
// looking for the matching pr_url in the payload — the newest
// matching event wins (handles the edge case where a PR was
// re-opened: the most recent chunk-2 emit reflects the live check
// run).
func (h *IaCGitHubWebhookHandler) findRecommendationIDForPR(
	ctx context.Context,
	connectionID, prURL string,
) string {
	if h.auditService == nil {
		return ""
	}
	since := time.Now().UTC().Add(-checkRunCreatedAuditLookback)
	events, err := h.auditService.List(ctx, services.AuditEventFilter{
		EventType:  services.AuditEventIaCCheckRunCreated,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   connectionID,
		Since:      since,
		Limit:      checkRunCreatedAuditLimit,
	})
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("iac github webhook: audit-log pivot for check_run.created failed; skipping update",
				zap.String("connection_id", connectionID),
				zap.String("pr_url", prURL),
				zap.Error(err),
			)
		}
		return ""
	}
	for _, e := range events {
		if e == nil || e.Payload == nil {
			continue
		}
		if u, _ := e.Payload["pr_url"].(string); u != prURL {
			continue
		}
		if rid, _ := e.Payload["recommendation_id"].(string); rid != "" {
			return rid
		}
	}
	return ""
}

// emitCheckRunUpdatedAudit records iac.check_run.updated with the
// transition payload design doc §8 names: previous_status +
// previous_conclusion (nullable strings) + new_status +
// new_conclusion. Nil auditService is a no-op (matches the slice-1
// posture elsewhere in this handler).
func (h *IaCGitHubWebhookHandler) emitCheckRunUpdatedAudit(
	ctx context.Context,
	args checkRunUpdateArgs,
	recommendationID string,
	ref types.CheckRunRef,
	previousStatus, previousConclusion, newConclusion string,
) {
	if h.auditService == nil {
		return
	}
	payload := map[string]any{
		"connection_id":       args.ConnectionID,
		"recommendation_id":   recommendationID,
		"recommendation_kind": args.RecommendationKind,
		"pr_url":              args.PRURL,
		"head_sha":            ref.HeadSHA,
		"check_run_id":        ref.CheckID,
		"owner":               ref.Owner,
		"repo":                ref.Repo,
		"previous_status":     previousStatus,
		"previous_conclusion": previousConclusion,
		"new_status":          iacgithub.CheckRunStatusCompleted,
		"new_conclusion":      newConclusion,
		"actor":               services.AuditActorSystem,
		"recorded_at":         time.Now().UTC(),
	}
	if strings.TrimSpace(args.AccountID) != "" {
		payload["account_id"] = args.AccountID
	}
	if strings.TrimSpace(args.Region) != "" {
		payload["region"] = args.Region
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventIaCCheckRunUpdated,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   args.ConnectionID,
		Action:     "check_run_updated",
		Payload:    payload,
	})
}

// emitCheckRunUpdateFailedAudit records iac.check_run.failed when
// the UpdateCheckRun PATCH fails. The structured error_kind
// discriminator (scope_missing | rate_limit | pr_not_found |
// network) drives SIEM dashboards and the humanizer's fix-it copy
// per design doc §8.
//
// Sibling helper to chunk 2's emitCheckRunFailedAudit on the IaC
// GitHub handler — the webhook handler owns its own copy because the
// two handlers' state isn't shared and the payload's failure scope
// (update vs create) is slightly different. The wire shape stays
// aligned so SIEM dashboards can union the two.
func (h *IaCGitHubWebhookHandler) emitCheckRunUpdateFailedAudit(
	ctx context.Context,
	args checkRunUpdateArgs,
	recommendationID string,
	ref types.CheckRunRef,
	intendedConclusion string,
	err error,
) {
	if h.auditService == nil {
		return
	}
	errKind := iacgithub.CheckRunErrorKindNetwork
	httpStatus := 0
	msg := err.Error()
	var cre *iacgithub.CheckRunError
	if errors.As(err, &cre) {
		errKind = cre.Kind
		httpStatus = cre.Status
		msg = cre.Message
	}
	payload := map[string]any{
		"connection_id":       args.ConnectionID,
		"recommendation_id":   recommendationID,
		"recommendation_kind": args.RecommendationKind,
		"pr_url":              args.PRURL,
		"head_sha":            ref.HeadSHA,
		"check_run_id":        ref.CheckID,
		"owner":               ref.Owner,
		"repo":                ref.Repo,
		// intended_conclusion records which transition we were trying
		// to drive — so SIEM consumers can tell apart a failed merge
		// PATCH from a failed close PATCH on the same check run.
		"intended_conclusion": intendedConclusion,
		"error_kind":          errKind,
		"http_status":         httpStatus,
		"error_message":       msg,
		"actor":               services.AuditActorSystem,
		"recorded_at":         time.Now().UTC(),
	}
	if strings.TrimSpace(args.AccountID) != "" {
		payload["account_id"] = args.AccountID
	}
	if strings.TrimSpace(args.Region) != "" {
		payload["region"] = args.Region
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventIaCCheckRunFailed,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   args.ConnectionID,
		Action:     "check_run_update_failed",
		Payload:    payload,
	})
	if h.logger != nil {
		h.logger.Info("iac github webhook: check run update failed (fail-open)",
			zap.String("error_kind", errKind),
			zap.Int("http_status", httpStatus),
			zap.String("intended_conclusion", intendedConclusion),
			zap.String("pr_url", args.PRURL),
		)
	}
}
