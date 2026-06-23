// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// This file is the chunk-2 bridge integration for the GitHub Checks
// API back-signal arc (v0.89.43, #663 Stream 61). It rides on the
// existing IaC GitHub PR-open handler in iac_github.go: AFTER the
// recommendation.pr_opened audit event emits, this file's
// emitCheckRunForOpenedPR helper composes the markdown summary via
// internal/proposer/checkrunprompt, posts the check run on the PR's
// head commit, persists the durable state via
// SetCheckRunForRecommendation, and emits the iac.check_run.created
// audit event.
//
// Fail-open is load-bearing here: every failure path emits
// iac.check_run.failed with a structured error_kind and returns. The
// PR open and its audit event have already completed; the check run
// is value-add. See design doc §5 + §8.
//
// Helper layering:
//   - checkRunOpenedPRArgs is the call-site payload (one struct so
//     the call site stays a single line).
//   - ChecksAPI is the slim interface the handler depends on,
//     satisfied by *iacgithub.PATClient in production and by a fake
//     in tests.
//   - CheckRunStore is the slim subset of the application store the
//     handler writes to.

package handlers

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/proposer/checkrunprompt"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// defaultCheckRunName — the slice-1 default check-run name per
// design doc §11 Q2. Operators wanting a different namespace can
// override via WithCheckRunName (operators sourcing from the
// SQUADRON_CHECK_RUN_NAME env var is the documented path in
// server.go wiring).
const defaultCheckRunName = "Squadron recommendation"

// ChecksAPI is the slim interface the chunk-2 PR-open follow-up
// depends on. Satisfied by *iacgithub.PATClient in production and by
// a recording fake in tests. Stated as an interface so the handler
// never carries a concrete PATClient field — the PAT is supplied at
// call time per design doc §3 option A.
//
// v0.89.44 (#665 Stream 63, slice 1 chunk 4): the interface grew the
// UpdateCheckRun method so the discovery-side exclusion handler can
// PATCH an in-flight check run to conclusion=neutral when an operator
// excludes a kind. The chunk-2 bridge does not call UpdateCheckRun;
// the fake in chunk-2 tests stays compatible because nil/zero is the
// safe default for unused methods on the interface.
type ChecksAPI interface {
	CreateCheckRun(ctx context.Context, pat string, req iacgithub.CheckRunCreate) (iacgithub.CheckRunRef, error)
	UpdateCheckRun(ctx context.Context, pat string, req iacgithub.CheckRunUpdate) error
}

// CheckRunStore is the slim subset of applicationstore.ApplicationStore
// the chunk-2 follow-up writes through. The real store satisfies
// this directly; tests substitute a recording fake.
//
// v0.89.44 (#665 Stream 63, slice 1 chunk 4): the interface grew the
// GetCheckRunForRecommendation read surface so the discovery-side
// exclusion handler can look up the in-flight check run for a
// recommendation_id before PATCHing it to neutral. The chunk-2
// bridge does not call Get; the fake in chunk-2 tests stays
// compatible by returning exists=false on the zero value.
type CheckRunStore interface {
	SetCheckRunForRecommendation(ctx context.Context,
		rec types.ExcludedRecommendation,
		ref types.CheckRunRef,
		status, conclusion string,
	) error

	GetCheckRunForRecommendation(ctx context.Context,
		recommendationID string,
	) (ref types.CheckRunRef, status string, conclusion string, exists bool, err error)
}

// checkRunOpenedPRArgs is the per-call payload the OpenPR handler
// hands to emitCheckRunForOpenedPR. One struct keeps the call site
// to a single line; field-by-field positional args drift fast as
// the arc adds chunk-3/4 fields.
type checkRunOpenedPRArgs struct {
	Connection                 *iacconnstore.IaCConnection
	Request                    *iacGitHubOpenPRRequest
	PRURL                      string
	HeadSHA                    string
	Owner                      string
	Repo                       string
	PAT                        string
	VerdictExamplesUsedByState map[string][]string
}

// emitCheckRunForOpenedPR is the chunk-2 follow-up on the existing
// recommendation.pr_opened path. It's fail-open: any failure to
// create a check run emits iac.check_run.failed audit and returns;
// the PR open and its existing audit event have already completed.
//
// Short-circuits silently when h.checksClient is unwired (operator
// hasn't enabled the Checks API integration yet — slice-1 fail-open
// posture for deployments upgrading PAT scope).
func (h *IaCGitHubHandlers) emitCheckRunForOpenedPR(ctx context.Context, args checkRunOpenedPRArgs) {
	if h.checksClient == nil {
		return
	}
	if args.Request == nil || args.Connection == nil {
		return
	}
	if args.PRURL == "" || args.HeadSHA == "" {
		return
	}

	in := checkrunprompt.SummaryInput{
		RecommendationKind:   args.Request.ResourceKind,
		RecommendationReason: args.Request.ProposerReasoning,
		AccountID:            args.Request.AccountID,
		Region:               args.Request.Region,
		ConnectionID:         args.Connection.ConnectionID,
		PRURL:                args.PRURL,
		RecommendationID:     args.Request.RecommendationID,
		SquadronHost:         h.squadronHost,
		VerdictsByState:      args.VerdictExamplesUsedByState,
	}
	title, summary := checkrunprompt.ComposeCreateSummary(in)

	name := h.checkRunNameVal
	if strings.TrimSpace(name) == "" {
		name = defaultCheckRunName
	}

	req := iacgithub.CheckRunCreate{
		Owner:     args.Owner,
		Repo:      args.Repo,
		HeadSHA:   args.HeadSHA,
		Name:      name,
		Status:    iacgithub.CheckRunStatusInProgress,
		StartedAt: time.Now().UTC(),
		Output: iacgithub.CheckRunOutput{
			Title:   title,
			Summary: summary,
		},
	}

	ref, err := h.checksClient.CreateCheckRun(ctx, args.PAT, req)
	if err != nil {
		h.emitCheckRunFailedAudit(ctx, args, err)
		return
	}

	// Persist the durable check-run state. Optional: when the
	// store is unwired (test surfaces or pre-chunk-2 deployments)
	// skip silently — the live check run on GitHub will reconcile
	// on next event.
	if h.checkRunStore != nil && strings.TrimSpace(args.Request.RecommendationID) != "" {
		rec := types.ExcludedRecommendation{
			RecommendationID:   args.Request.RecommendationID,
			ConnectionID:       args.Connection.ConnectionID,
			AccountID:          args.Request.AccountID,
			Region:             args.Request.Region,
			RecommendationKind: args.Request.ResourceKind,
		}
		storageRef := types.CheckRunRef{
			Owner:   ref.Owner,
			Repo:    ref.Repo,
			CheckID: ref.CheckID,
			HeadSHA: ref.HeadSHA,
		}
		if werr := h.checkRunStore.SetCheckRunForRecommendation(
			ctx, rec, storageRef, iacgithub.CheckRunStatusInProgress, "",
		); werr != nil {
			// Log but don't fail. The check run is live on GitHub;
			// the storage row will reconcile on the next event.
			if h.logger != nil {
				h.logger.Warn("iac github open-pr: check run created but storage write failed",
					zap.Error(werr), zap.Int64("check_run_id", ref.CheckID))
			}
		}
	}

	h.emitCheckRunCreatedAudit(ctx, args, ref)
}

// emitCheckRunCreatedAudit records iac.check_run.created with the
// payload shape design doc §8 names. Fail-open: nil auditService
// is a no-op (matches the slice-1 posture elsewhere in this
// handler).
func (h *IaCGitHubHandlers) emitCheckRunCreatedAudit(ctx context.Context, args checkRunOpenedPRArgs, ref iacgithub.CheckRunRef) {
	if h.auditService == nil {
		return
	}
	payload := map[string]any{
		"connection_id":       args.Connection.ConnectionID,
		"recommendation_id":   args.Request.RecommendationID,
		"recommendation_kind": args.Request.ResourceKind,
		"pr_url":              args.PRURL,
		"head_sha":            ref.HeadSHA,
		"check_run_id":        ref.CheckID,
		"owner":               ref.Owner,
		"repo":                ref.Repo,
		"status":              iacgithub.CheckRunStatusInProgress,
		"actor":               services.AuditActorSystem,
		"recorded_at":         time.Now().UTC(),
	}
	if strings.TrimSpace(args.Request.AccountID) != "" {
		payload["account_id"] = args.Request.AccountID
	}
	if strings.TrimSpace(args.Request.Region) != "" {
		payload["region"] = args.Request.Region
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventIaCCheckRunCreated,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   args.Connection.ConnectionID,
		Action:     "check_run_created",
		Payload:    payload,
	})
}

// emitCheckRunFailedAudit records iac.check_run.failed when the
// Create call fails. The structured error_kind discriminator
// (scope_missing | rate_limit | pr_not_found | network) drives
// SIEM dashboards and the humanizer's fix-it copy per design doc §8.
func (h *IaCGitHubHandlers) emitCheckRunFailedAudit(ctx context.Context, args checkRunOpenedPRArgs, err error) {
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
		"connection_id":       args.Connection.ConnectionID,
		"recommendation_id":   args.Request.RecommendationID,
		"recommendation_kind": args.Request.ResourceKind,
		"pr_url":              args.PRURL,
		"head_sha":            args.HeadSHA,
		"error_kind":          errKind,
		"http_status":         httpStatus,
		"error_message":       msg,
		"actor":               services.AuditActorSystem,
		"recorded_at":         time.Now().UTC(),
	}
	if strings.TrimSpace(args.Request.AccountID) != "" {
		payload["account_id"] = args.Request.AccountID
	}
	if strings.TrimSpace(args.Request.Region) != "" {
		payload["region"] = args.Request.Region
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventIaCCheckRunFailed,
		TargetType: services.AuditTargetIaCRecommendation,
		TargetID:   args.Connection.ConnectionID,
		Action:     "check_run_failed",
		Payload:    payload,
	})
	if h.logger != nil {
		h.logger.Info("iac github open-pr: check run create failed (fail-open)",
			zap.String("error_kind", errKind),
			zap.Int("http_status", httpStatus),
			zap.String("pr_url", args.PRURL))
	}
}

// parseOwnerRepoFromPRURL extracts owner + repo from a GitHub PR
// URL. Returns empty strings on malformed input — the call site
// short-circuits the check-run create when either is empty.
//
// Defensive helper for slices 3+/test scaffolding that need to
// reconstruct (owner, repo) from a stored PR URL. The chunk-2
// happy path uses splitRepoFullName on the connection's
// RepoFullName instead so it never traverses URL parsing here.
//
// Format expected: https://github.com/<owner>/<repo>/pull/<n>
func parseOwnerRepoFromPRURL(prURL string) (owner, repo string) {
	trimmed := strings.TrimSpace(prURL)
	if trimmed == "" {
		return "", ""
	}
	const marker = "github.com/"
	idx := strings.Index(trimmed, marker)
	if idx < 0 {
		return "", ""
	}
	tail := trimmed[idx+len(marker):]
	parts := strings.SplitN(tail, "/", 4)
	if len(parts) < 3 {
		return "", ""
	}
	if parts[2] != "pull" {
		return "", ""
	}
	return parts[0], parts[1]
}
