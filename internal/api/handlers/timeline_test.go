// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/services"
)

// TestHumanizeEventType pins the v0.81.4 (#545) humanizer table.
// The cleanup-grade scope is "the prominent plan.* / rollout.* /
// proposal.* family the operator sees during an incident", with a
// fallback to the raw event_type for anything not in the table. New
// rows added to the humanizer should add a case here; the unknown-
// type fallback test catches accidental regressions where someone
// thinks they've added a row to the table but skipped a switch case.
func TestHumanizeEventType(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		action    string
		want      string
	}{
		// Plan family — the v0.69-v0.79 Move 3 surfaces.
		{"plan.created", "plan.created", "plan_created", "Plan created"},
		{"plan.approved", "plan.approved", "approve", "Plan approved"},
		{"plan.completed", "plan.completed", "complete", "Plan completed"},
		{"plan.step_started", "plan.step_started", "advance", "Plan step started"},
		{"plan.step_completed", "plan.step_completed", "complete", "Plan step completed"},
		{"plan.rolled_back", "plan.rolled_back", "rollback", "Plan rolled back"},
		// Rollout family.
		{"rollout.created", "rollout.created", "create", "Rollout created"},
		{"rollout.approved", "rollout.approved", "approve", "Rollout approved"},
		{"rollout.stage_applied", "rollout.stage_applied", "advance", "Rollout stage applied"},
		{"rollout.succeeded", "rollout.succeeded", "complete", "Rollout succeeded"},
		{"rollout.aborted", "rollout.aborted", "abort", "Rollout aborted"},
		{"rollout.rolled_back", "rollout.rolled_back", "rollback", "Rollout rolled back"},
		// Proposal family.
		{"proposal.created", "proposal.created", "propose", "AI proposal created"},
		{"proposal.declined", "proposal.declined", "decline", "AI proposal declined"},
		{"proposal.skipped", "proposal.skipped", "skip", "AI proposal skipped"},
		{"proposal.evidence_linked", "proposal.evidence_linked", "link", "AI evidence linked"},
		// v0.89.25 (#641) — humanizer coverage cleanup. The next 15
		// rows close the gap for event types that previously fell
		// through to the raw event_type display.
		// Agent lifecycle.
		{"agent.registered", "agent.registered", "register", "Agent registered"},
		{"agent.drift.synced", "agent.drift.synced", "sync", "Agent config synced"},
		{"agent.drift.drifted", "agent.drift.drifted", "drift", "Agent config drifted"},
		// Config lifecycle.
		{"config.stored", "config.stored", "store", "Config stored"},
		{"config.applied", "config.applied", "apply", "Config applied"},
		// Alert lifecycle.
		{"alert_rule.created", "alert_rule.created", "create", "Alert rule created"},
		{"alert_rule.updated", "alert_rule.updated", "update", "Alert rule updated"},
		{"alert_rule.deleted", "alert_rule.deleted", "delete", "Alert rule deleted"},
		{"alert.fired", "alert.fired", "fire", "Alert fired"},
		{"alert.resolved", "alert.resolved", "resolve", "Alert resolved"},
		// Incident drafter.
		{"incident.drafted", "incident.drafted", "draft", "Incident draft created"},
		{"incident.draft_declined", "incident.draft_declined", "decline", "Incident draft declined"},
		// Discovery flat-lifecycle events.
		{"discovery.aws.connection_created", "discovery.aws.connection_created", "create", "AWS connection created"},
		{"discovery.aws.scan_completed", "discovery.aws.scan_completed", "scan", "AWS scan completed"},
		{"discovery.aws.scan_all_completed", "discovery.aws.scan_all_completed", "scan", "Multi-account AWS scan completed"},
		// v0.89.234 — discovery lifecycle coverage for the events the
		// v0.89.25 cleanup missed + the GCP/Azure/OCI connectors.
		{"discovery.aws.scan_started", "discovery.aws.scan_started", "scan", "AWS scan started"},
		{"discovery.aws.recommendations_generated", "discovery.aws.recommendations_generated", "recommend", "AWS recommendations generated"},
		{"discovery.aws.connection_read", "discovery.aws.connection_read", "read", "AWS connection accessed"},
		{"discovery.gcp.connection_created", "discovery.gcp.connection_created", "create", "GCP connection created"},
		{"discovery.gcp.connection_deleted", "discovery.gcp.connection_deleted", "delete", "GCP connection deleted"},
		{"discovery.gcp.scan_started", "discovery.gcp.scan_started", "scan", "GCP scan started"},
		{"discovery.gcp.scan_completed", "discovery.gcp.scan_completed", "scan", "GCP scan completed"},
		{"discovery.gcp.scan_failed", "discovery.gcp.scan_failed", "scan", "GCP scan failed"},
		{"discovery.gcp.recommendations_generated", "discovery.gcp.recommendations_generated", "recommend", "GCP recommendations generated"},
		{"discovery.azure.connection_created", "discovery.azure.connection_created", "create", "Azure connection created"},
		{"discovery.azure.connection_deleted", "discovery.azure.connection_deleted", "delete", "Azure connection deleted"},
		{"discovery.azure.scan_started", "discovery.azure.scan_started", "scan", "Azure scan started"},
		{"discovery.azure.scan_completed", "discovery.azure.scan_completed", "scan", "Azure scan completed"},
		{"discovery.azure.scan_failed", "discovery.azure.scan_failed", "scan", "Azure scan failed"},
		{"discovery.azure.recommendations_generated", "discovery.azure.recommendations_generated", "recommend", "Azure recommendations generated"},
		{"discovery.oci.connection_created", "discovery.oci.connection_created", "create", "OCI connection created"},
		{"discovery.oci.connection_deleted", "discovery.oci.connection_deleted", "delete", "OCI connection deleted"},
		{"discovery.oci.scan_started", "discovery.oci.scan_started", "scan", "OCI scan started"},
		{"discovery.oci.scan_completed", "discovery.oci.scan_completed", "scan", "OCI scan completed"},
		{"discovery.oci.scan_failed", "discovery.oci.scan_failed", "scan", "OCI scan failed"},
		{"discovery.oci.recommendations_generated", "discovery.oci.recommendations_generated", "recommend", "OCI recommendations generated"},
		// v0.89.235 — humanizer completeness pass (lifecycle + discovery requests).
		{"agent.decommissioned", "agent.decommissioned", "decommission", "Agent decommissioned"},
		{"api_token.issued", "api_token.issued", "issue", "API token issued"},
		{"api_token.revoked", "api_token.revoked", "revoke", "API token revoked"},
		{"config.created", "config.created", "create", "Config created"},
		{"config.lint_evaluated", "config.lint_evaluated", "lint", "Config lint evaluated"},
		{"incident.published", "incident.published", "publish", "Incident published"},
		{"incident.dismissed", "incident.dismissed", "dismiss", "Incident dismissed"},
		{"plan.step_cancelled", "plan.step_cancelled", "cancel", "Plan step cancelled"},
		{"rollout.advanced", "rollout.advanced", "advance", "Rollout advanced"},
		{"rollout.rollback_requested", "rollout.rollback_requested", "request_rollback", "Rollout rollback requested"},
		{"discovery.summary.requested", "discovery.summary.requested", "request", "Discovery summary requested"},
		{"discovery.trace_coverage.requested", "discovery.trace_coverage.requested", "request", "Trace-coverage report requested"},
		{"discovery.span_quality.requested", "discovery.span_quality.requested", "request", "Span-quality report requested"},
		{"discovery.workload_health.requested", "discovery.workload_health.requested", "request", "Workload-health report requested"},
		// v0.89.26 (#642 Stream 43) — per-rollout exclude-from-
		// learning toggle. Table-test entry pins the default
		// (cold-path / payload-missing) text. The payload-aware
		// direction-sensitive wording lives in
		// TestExcludeFromLearningTitle below, which exercises
		// auditToEvent so the dispatch chain is in scope.
		{"rollout.excluded_from_learning", "rollout.excluded_from_learning", "exclude_from_learning", "AI proposal excluded from future learning"},
		// v0.89.28 (#643 slice 1) — discovery_proposal.created cold-
		// start fallback. The payload-aware humanizer at
		// handleIaCAuditEvent emits the enriched count title when
		// verdict_examples_used is non-empty; this table entry
		// renders the simple title when handleIaCAuditEvent returned
		// ok=false (e.g. payload absent entirely).
		{"discovery_proposal.created", "discovery_proposal.created", "discovery_proposal_created", "Discovery recommendations generated"},
		// v0.89.30 (#649) — webhook replay protection. Emitted when an
		// inbound GitHub webhook delivery passed HMAC verification but
		// collided with the dedupe table on the X-GitHub-Delivery UUID.
		// Pins the cold-path text so a future humanizer regression on
		// this event family is caught here.
		{"webhook.delivery_replayed", "webhook.delivery_replayed", "delivery_replayed", "Webhook delivery replayed"},
		// Fallback — unknown event_type returns the raw string so we
		// never lose information by humanizing. Pick a truly unknown
		// type that's NOT in any of the v0.89.25 cleanup additions.
		{"unknown event type falls through", "telemetry.flushed", "flush", "telemetry.flushed"},
		// Fallback — empty event_type returns action.
		{"empty event type uses action", "", "create", "create"},
		// Fallback — both empty returns empty.
		{"both empty", "", "", ""},
		// Fallback — whitespace stripped.
		{"trims whitespace from event_type", "  rollout.unknown_x  ", "y", "rollout.unknown_x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := humanizeEventType(tc.eventType, tc.action)
			if got != tc.want {
				t.Errorf("humanizeEventType(%q, %q) = %q, want %q",
					tc.eventType, tc.action, got, tc.want)
			}
		})
	}
}

// TestHumanizeIaCAuditEvent pins the v0.89.4 [#612] payload-aware
// humanizer for the Stream 19 IaC + recommendation.pr_* family.
// Each event type gets one happy-path assertion (Summary + Detail
// rendered from a representative payload) and one defensive
// assertion (a required payload field missing → ok=false so the
// caller falls back to the v0.81.4 path and we never emit empty
// placeholders).
func TestHumanizeIaCAuditEvent(t *testing.T) {
	cases := []struct {
		name      string
		event     *services.AuditEvent
		wantTitle string
		wantSub   string
		wantOK    bool
	}{
		// --- iac.github.connection_created ---
		{
			name: "connection_created happy path",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCGitHubConnectionCreated,
				Payload: map[string]any{
					"connection_id":  "conn-abc",
					"repo_full_name": "acme/infra",
					"default_branch": "main",
					"auth_kind":      "pat",
					"placement_map": []any{
						map[string]any{"provider": "aws", "resource_kind": "rds_instance", "file_path": "rds.tf"},
						map[string]any{"provider": "aws", "resource_kind": "s3_bucket", "file_path": "s3.tf"},
						map[string]any{"provider": "aws", "resource_kind": "iam_role", "file_path": "iam.tf"},
					},
				},
			},
			wantTitle: "Connected github.com/acme/infra to Squadron",
			wantSub:   "3 placement rows configured (pat)",
			wantOK:    true,
		},
		{
			name: "connection_created missing repo_full_name falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCGitHubConnectionCreated,
				Payload: map[string]any{
					"auth_kind":     "pat",
					"placement_map": []any{},
				},
			},
			wantOK: false,
		},

		// --- iac.github.connection_validated ---
		{
			name: "connection_validated happy path",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCGitHubConnectionValidated,
				Payload: map[string]any{
					"repo_full_name": "acme/infra",
					"default_branch": "main",
					"preflight_results": []any{
						map[string]any{"provider": "aws", "resource_kind": "rds_instance", "file_path": "rds.tf", "exists": true, "sha_short": "abc1234"},
						map[string]any{"provider": "aws", "resource_kind": "s3_bucket", "file_path": "s3.tf", "exists": true, "sha_short": "def5678"},
						map[string]any{"provider": "aws", "resource_kind": "iam_role", "file_path": "iam.tf", "exists": false},
					},
				},
			},
			wantTitle: "Validated IaC connection to github.com/acme/infra",
			wantSub:   "2 of 3 placement files reachable on main",
			wantOK:    true,
		},
		{
			name: "connection_validated missing default_branch falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCGitHubConnectionValidated,
				Payload: map[string]any{
					"repo_full_name":    "acme/infra",
					"preflight_results": []any{},
				},
			},
			wantOK: false,
		},

		// --- iac.github.placement_map_updated ---
		{
			name: "placement_map_updated happy path",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCGitHubPlacementMapUpdated,
				Payload: map[string]any{
					"connection_id":  "conn-abc",
					"repo_full_name": "acme/infra",
					"placement_map": []any{
						map[string]any{"provider": "aws", "resource_kind": "rds_instance", "file_path": "rds.tf"},
						map[string]any{"provider": "aws", "resource_kind": "s3_bucket", "file_path": "s3.tf"},
						map[string]any{"provider": "aws", "resource_kind": "iam_role", "file_path": "iam.tf"},
						map[string]any{"provider": "aws", "resource_kind": "ec2_instance", "file_path": "ec2.tf"},
					},
				},
			},
			wantTitle: "Updated placement map for github.com/acme/infra",
			wantSub:   "4 placement rows now configured",
			wantOK:    true,
		},
		{
			name: "placement_map_updated missing placement_map falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCGitHubPlacementMapUpdated,
				Payload: map[string]any{
					"repo_full_name": "acme/infra",
				},
			},
			wantOK: false,
		},

		// --- recommendation.pr_opened ---
		{
			name: "pr_opened happy path",
			event: &services.AuditEvent{
				EventType: services.AuditEventRecommendationPROpened,
				Payload: map[string]any{
					"scan_id":        "scan-1",
					"step_idx":       float64(0),
					"resource_kind":  "rds_instance",
					"repo_full_name": "acme/infra",
					"pr_number":      float64(42),
					"pr_url":         "https://github.com/acme/infra/pull/42",
					"branch":         "squadron/rec-rds-1",
					"commit_sha":     "9f9f9f9f",
					"file_path":      "rds.tf",
					"actor":          "system",
				},
			},
			wantTitle: "Opened PR #42 in github.com/acme/infra for rds_instance",
			wantSub:   "Branch squadron/rec-rds-1, file rds.tf",
			wantOK:    true,
		},
		{
			name: "pr_opened missing pr_number falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventRecommendationPROpened,
				Payload: map[string]any{
					"resource_kind":  "rds_instance",
					"repo_full_name": "acme/infra",
					"branch":         "squadron/rec-rds-1",
					"file_path":      "rds.tf",
				},
			},
			wantOK: false,
		},

		// --- recommendation.pr_open_failed ---
		{
			name: "pr_open_failed happy path uses humanized_message verbatim",
			event: &services.AuditEvent{
				EventType: services.AuditEventRecommendationPROpenFailed,
				Payload: map[string]any{
					"scan_id":           "scan-1",
					"step_idx":          float64(0),
					"resource_kind":     "rds_instance",
					"repo_full_name":    "acme/infra",
					"error_code":        "github.branch_already_exists",
					"humanized_message": "Branch squadron/rec-rds-1 already exists on github.com/acme/infra. Delete the stale branch and retry.",
					"actor":             "system",
				},
			},
			wantTitle: "Could not open PR in github.com/acme/infra for rds_instance",
			wantSub:   "Branch squadron/rec-rds-1 already exists on github.com/acme/infra. Delete the stale branch and retry.",
			wantOK:    true,
		},
		{
			name: "pr_open_failed missing humanized_message falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventRecommendationPROpenFailed,
				Payload: map[string]any{
					"resource_kind":  "rds_instance",
					"repo_full_name": "acme/infra",
					"error_code":     "github.branch_already_exists",
				},
			},
			wantOK: false,
		},

		// --- recommendation.pr_merged (v0.89.24 #640) ---
		// Pins the humanizer for the v0.89.23 (#639) webhook event.
		// Cases verify happy path with all four required fields, the
		// kindless-suffix fallback when the PR didn't come from a
		// Squadron-shaped branch, and the missing-merged_by defensive
		// path that falls through to the v0.81.4 raw-event_type
		// renderer.
		{
			name: "pr_merged happy path with recommendation_kind",
			event: &services.AuditEvent{
				EventType: services.AuditEventRecommendationPRMerged,
				Payload: map[string]any{
					"repo_full_name":      "acme/infra",
					"pr_number":           float64(42),
					"pr_url":              "https://github.com/acme/infra/pull/42",
					"branch":              "squadron/rec/rds-pi-em/abc123",
					"merged_at":           "2026-06-22T10:00:00Z",
					"merged_by":           "alice",
					"recommendation_kind": "rds-pi-em",
					"connection_id":       "conn-abc",
				},
			},
			wantTitle: "Merged PR #42 in github.com/acme/infra for rds-pi-em",
			wantSub:   "Branch squadron/rec/rds-pi-em/abc123, merged by alice",
			wantOK:    true,
		},
		{
			// PR that landed under Squadron's webhook receiver but
			// from a non-Squadron-shaped branch (operator hand-pushed
			// or a custom prefix). The audit event still records the
			// merge; the title omits "for <kind>" rather than render
			// "Merged PR #5 in foo/bar for ".
			name: "pr_merged without recommendation_kind drops kind suffix",
			event: &services.AuditEvent{
				EventType: services.AuditEventRecommendationPRMerged,
				Payload: map[string]any{
					"repo_full_name": "acme/infra",
					"pr_number":      float64(5),
					"pr_url":         "https://github.com/acme/infra/pull/5",
					"branch":         "feature/hand-pushed",
					"merged_at":      "2026-06-22T10:00:00Z",
					"merged_by":      "bob",
					// recommendation_kind intentionally absent
				},
			},
			wantTitle: "Merged PR #5 in github.com/acme/infra",
			wantSub:   "Branch feature/hand-pushed, merged by bob",
			wantOK:    true,
		},
		{
			name: "pr_merged missing merged_by falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventRecommendationPRMerged,
				Payload: map[string]any{
					"repo_full_name": "acme/infra",
					"pr_number":      float64(42),
					"branch":         "squadron/rec/rds-pi-em/abc123",
					// merged_by intentionally absent — the receiver
					// guarantees this field on a real merge, so if
					// it's absent we treat the row as malformed and
					// fall through to the v0.81.4 raw-event_type
					// renderer rather than print "merged by ".
				},
			},
			wantOK: false,
		},

		// --- discovery_proposal.created (v0.89.28 #643 slice 1) ---
		// Pins the payload-aware humanizer for the new event. When
		// verdict_examples_used is non-empty the title surfaces the
		// count; when empty the title falls through to the simple
		// cold-start phrasing.
		{
			name: "discovery_proposal.created with prior accepted PRs",
			event: &services.AuditEvent{
				EventType: services.AuditEventDiscoveryProposalCreated,
				Payload: map[string]any{
					"scan_id":              "scan-1",
					"account_id":           "111111111111",
					"region":               "us-east-1",
					"recommendation_count": float64(3),
					"verdict_examples_used": []any{
						"https://github.com/octo/widgets/pull/142",
						"https://github.com/octo/widgets/pull/138",
					},
				},
			},
			wantTitle: "Discovery recommendations generated (informed by 2 prior accepted PRs)",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "discovery_proposal.created cold-start: empty examples list",
			event: &services.AuditEvent{
				EventType: services.AuditEventDiscoveryProposalCreated,
				Payload: map[string]any{
					"scan_id":               "scan-1",
					"verdict_examples_used": []any{},
				},
			},
			wantTitle: "Discovery recommendations generated",
			wantSub:   "",
			wantOK:    true,
		},

		// --- iac.check_run.created (v0.89.44 #665 Stream 63 slice 1
		// chunk 4) — humanizer for the chunk-2 PR-open follow-up emit.
		// Payload comes from internal/api/handlers/iac_github_checkrun.go's
		// emitCheckRunCreatedAudit shape.
		{
			name: "check_run.created happy path",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunCreated,
				Payload: map[string]any{
					"connection_id":       "conn-abc",
					"recommendation_id":   "rec-xyz",
					"recommendation_kind": "rds-pi-em",
					"pr_url":              "https://github.com/octo/widgets/pull/142",
					"head_sha":            "abc123",
					"check_run_id":        float64(9001),
					"owner":               "octo",
					"repo":                "widgets",
					"status":              "in_progress",
				},
			},
			wantTitle: "Squadron posted a check run on PR #142 in octo/widgets (kind=rds-pi-em)",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "check_run.created missing recommendation_kind falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunCreated,
				Payload: map[string]any{
					"pr_url": "https://github.com/octo/widgets/pull/142",
					"owner":  "octo",
					"repo":   "widgets",
				},
			},
			wantOK: false,
		},

		// --- iac.check_run.updated transitions (v0.89.44 #665 Stream
		// 63 slice 1 chunk 4) — humanizer for the chunks 3 + 4 emit.
		// Three pinned transitions + a generic fallback when
		// new_conclusion is unrecognized.
		{
			name: "check_run.updated in_progress -> success (merge)",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunUpdated,
				Payload: map[string]any{
					"pr_url":              "https://github.com/octo/widgets/pull/142",
					"previous_status":     "in_progress",
					"previous_conclusion": "",
					"new_status":          "completed",
					"new_conclusion":      "success",
					"recommendation_kind": "rds-pi-em",
				},
			},
			wantTitle: "Squadron's check run marked SUCCESS on PR #142 (operator merged).",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "check_run.updated in_progress -> failure (closed without merge)",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunUpdated,
				Payload: map[string]any{
					"pr_url":              "https://github.com/octo/widgets/pull/142",
					"previous_status":     "in_progress",
					"previous_conclusion": "",
					"new_status":          "completed",
					"new_conclusion":      "failure",
					"recommendation_kind": "rds-pi-em",
				},
			},
			wantTitle: "Squadron's check run marked FAILURE on PR #142 (operator closed without merging).",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "check_run.updated in_progress -> neutral (operator excluded)",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunUpdated,
				Payload: map[string]any{
					"pr_url":              "https://github.com/octo/widgets/pull/142",
					"previous_status":     "in_progress",
					"previous_conclusion": "",
					"new_status":          "completed",
					"new_conclusion":      "neutral",
					"recommendation_kind": "rds-pi-em",
				},
			},
			wantTitle: "Squadron's check run marked NEUTRAL on PR #142 (operator excluded this kind from future recommendations).",
			wantSub:   "",
			wantOK:    true,
		},
		{
			// Unknown new_conclusion → generic fallback that surfaces
			// the raw status + conclusion so SIEM consumers don't lose
			// the signal even on a future-shape transition.
			name: "check_run.updated unknown conclusion falls back to generic",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunUpdated,
				Payload: map[string]any{
					"pr_url":         "https://github.com/octo/widgets/pull/142",
					"new_status":     "completed",
					"new_conclusion": "skipped",
				},
			},
			wantTitle: "Squadron's check run updated on PR #142 (status=completed conclusion=skipped).",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "check_run.updated missing pr_url falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunUpdated,
				Payload: map[string]any{
					"new_status":     "completed",
					"new_conclusion": "success",
				},
			},
			wantOK: false,
		},

		// --- iac.check_run.failed error_kinds (v0.89.44 #665 Stream
		// 63 slice 1 chunk 4) — humanizer for chunks 2/3/4's failed
		// emit. Four pinned error_kinds: scope_missing, rate_limit,
		// pr_not_found, network. Unknown error_kinds fall back to the
		// network-style phrasing with the raw error_message attached.
		{
			name: "check_run.failed scope_missing",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunFailed,
				Payload: map[string]any{
					"pr_url":              "https://github.com/octo/widgets/pull/142",
					"error_kind":          "scope_missing",
					"http_status":         float64(403),
					"error_message":       "PAT lacks checks:write scope",
					"recommendation_kind": "rds-pi-em",
				},
			},
			wantTitle: "Squadron couldn't post a check run on PR #142: your IaC PAT is missing the checks:write scope.",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "check_run.failed rate_limit",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunFailed,
				Payload: map[string]any{
					"pr_url":        "https://github.com/octo/widgets/pull/142",
					"error_kind":    "rate_limit",
					"http_status":   float64(429),
					"error_message": "GitHub API rate limit exceeded (reset=1750000000)",
				},
			},
			wantTitle: "Squadron couldn't post a check run on PR #142: GitHub API rate limit exceeded.",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "check_run.failed pr_not_found",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunFailed,
				Payload: map[string]any{
					"pr_url":      "https://github.com/octo/widgets/pull/142",
					"error_kind":  "pr_not_found",
					"http_status": float64(422),
				},
			},
			wantTitle: "Squadron couldn't post a check run on PR #142: the PR was not found on GitHub.",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "check_run.failed network",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunFailed,
				Payload: map[string]any{
					"pr_url":        "https://github.com/octo/widgets/pull/142",
					"error_kind":    "network",
					"error_message": "context deadline exceeded",
				},
			},
			wantTitle: "Squadron couldn't post a check run on PR #142: context deadline exceeded.",
			wantSub:   "",
			wantOK:    true,
		},
		{
			name: "check_run.failed missing error_kind falls back",
			event: &services.AuditEvent{
				EventType: services.AuditEventIaCCheckRunFailed,
				Payload: map[string]any{
					"pr_url": "https://github.com/octo/widgets/pull/142",
				},
			},
			wantOK: false,
		},

		// --- nil-safe + unknown event type ---
		{
			name:   "nil event falls through",
			event:  nil,
			wantOK: false,
		},
		{
			name: "unknown event type falls through",
			event: &services.AuditEvent{
				EventType: "plan.created",
				Payload:   map[string]any{"x": "y"},
			},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTitle, gotSub, gotOK := humanizeIaCAuditEvent(tc.event)
			if gotOK != tc.wantOK {
				t.Fatalf("humanizeIaCAuditEvent ok = %v, want %v (title=%q sub=%q)",
					gotOK, tc.wantOK, gotTitle, gotSub)
			}
			if !tc.wantOK {
				// On fall-through, the caller is responsible for the
				// title/subtitle — we don't make a claim either way.
				return
			}
			if gotTitle != tc.wantTitle {
				t.Errorf("title = %q, want %q", gotTitle, tc.wantTitle)
			}
			if gotSub != tc.wantSub {
				t.Errorf("subtitle = %q, want %q", gotSub, tc.wantSub)
			}
		})
	}
}

// TestAuditToEventUsesIaCHumanizer covers the integration seam — a
// payload-rich IaC event reaches auditToEvent and emerges with the
// payload-derived (Title, Subtitle), NOT the v0.81.4 fallback
// "actor · target_type" subtitle. A matching defensive case
// confirms a payload-missing event falls back so we never emit
// half-rendered placeholders to the Timeline page.
func TestAuditToEventUsesIaCHumanizer(t *testing.T) {
	t.Run("pr_opened uses payload-derived title and subtitle", func(t *testing.T) {
		ev := &services.AuditEvent{
			ID:         "audit-1",
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventRecommendationPROpened,
			TargetType: services.AuditTargetIaCRecommendation,
			TargetID:   "conn-abc",
			Action:     "pr_opened",
			Payload: map[string]any{
				"resource_kind":  "rds_instance",
				"repo_full_name": "acme/infra",
				"pr_number":      float64(42),
				"branch":         "squadron/rec-rds-1",
				"file_path":      "rds.tf",
			},
		}
		got := auditToEvent(ev)
		if got.Title != "Opened PR #42 in github.com/acme/infra for rds_instance" {
			t.Errorf("title = %q", got.Title)
		}
		if got.Subtitle != "Branch squadron/rec-rds-1, file rds.tf" {
			t.Errorf("subtitle = %q", got.Subtitle)
		}
	})

	t.Run("pr_opened missing payload falls back to actor/target_type subtitle", func(t *testing.T) {
		ev := &services.AuditEvent{
			ID:         "audit-2",
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventRecommendationPROpened,
			TargetType: services.AuditTargetIaCRecommendation,
			TargetID:   "conn-abc",
			Action:     "pr_opened",
			// No payload — must NOT emit "Opened PR # in github.com/ for ".
			Payload: nil,
		}
		got := auditToEvent(ev)
		// Title falls back to the raw event_type so we don't lose
		// information.
		if got.Title != services.AuditEventRecommendationPROpened {
			t.Errorf("title = %q, want raw event_type", got.Title)
		}
		// Subtitle must contain the target_type (the v0.81.4 path),
		// proving the fallback executed.
		if got.Subtitle == "" || got.Subtitle == "Branch , file " {
			t.Errorf("subtitle = %q — fallback path should have set actor + target_type", got.Subtitle)
		}
	})
}

// TestProposalCreatedTitle pins the v0.89.22 (#638) payload-aware
// humanizer for the v0.89.17 (#633) #531 slice 1 feedback loop. The
// guarantee is two-sided: cold-start proposals MUST keep the v0.81.4
// hardcoded title byte-for-byte (the v0.79 humanizer table test
// already asserts this — we don't want to regress it through the
// payload-aware enrichment path), and proposals that cited prior
// verdicts MUST surface the count in the title so an operator
// scanning the timeline can see at a glance which proposals were
// shaped by past operator decisions.
//
// What we do NOT surface in the title (intentional, per the
// proposalCreatedTitle comment): the per-entry rejected/approved
// state. v0.89.17's audit payload carries just the rollout IDs;
// looking up the state per entry would require an N+1 query
// against the rollouts table at humanize time. The expanded
// payload row still shows the IDs so an operator who needs to
// know the split can click in and look them up.
func TestProposalCreatedTitle(t *testing.T) {
	t.Run("cold start parity — empty verdict_examples_used falls through", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"group_id":              "web-staging",
				"spike_attribute":       "k8s.pod.uid",
				"verdict_examples_used": []any{},
			},
		}
		got := auditToEvent(ev)
		// MUST match the v0.81.4 hardcoded title byte-for-byte. If
		// this breaks, an operator who's used to seeing "AI proposal
		// created" in their timeline will see "AI proposal created
		// (cited 0 prior verdicts)" — confusing, and wrong: an
		// empty array IS the cold-start signal.
		if got.Title != "AI proposal created" {
			t.Errorf("cold-start title = %q, want %q", got.Title, "AI proposal created")
		}
	})

	t.Run("verdict_examples_used absent — falls through", func(t *testing.T) {
		// The v0.79 audit shape predates v0.89.17 — old events in the
		// database don't have the field. Must still render as cold
		// start so backfilled timelines don't regress.
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"group_id": "web-staging",
			},
		}
		got := auditToEvent(ev)
		if got.Title != "AI proposal created" {
			t.Errorf("absent-field title = %q, want %q", got.Title, "AI proposal created")
		}
	})

	t.Run("single citation — singular form", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []any{"rlt_6m08"},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created (cited 1 prior verdict)"
		if got.Title != want {
			t.Errorf("single-citation title = %q, want %q", got.Title, want)
		}
	})

	t.Run("multiple citations — plural form", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []any{
					"rlt_6m08", "rlt_8ax9", "rlt_7q12",
				},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created (cited 3 prior verdicts)"
		if got.Title != want {
			t.Errorf("multi-citation title = %q, want %q", got.Title, want)
		}
	})

	t.Run("typed []string array also handled", func(t *testing.T) {
		// The audit payload usually deserializes through JSON to []any,
		// but the bridge can also hand the humanizer a typed []string
		// when called in-process (e.g. tests that bypass JSON). Both
		// shapes MUST count consistently.
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []string{"rlt_6m08", "rlt_8ax9"},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created (cited 2 prior verdicts)"
		if got.Title != want {
			t.Errorf("typed-slice title = %q, want %q", got.Title, want)
		}
	})

	t.Run("non-proposal.created event_type never enriches", func(t *testing.T) {
		// Defensive: the dispatch must NOT mis-apply the proposal
		// humanizer to a different event type even if that event
		// happens to carry a verdict_examples_used key (unlikely but
		// possible in a downstream extension).
		ev := &services.AuditEvent{
			EventType: "proposal.declined",
			Actor:     "operator",
			Payload: map[string]any{
				"verdict_examples_used": []any{"rlt_6m08"},
			},
		}
		got := auditToEvent(ev)
		if got.Title != "AI proposal declined" {
			t.Errorf("declined title = %q, want %q", got.Title, "AI proposal declined")
		}
	})
}

// TestExcludeFromLearningTitle pins the v0.89.26 (#642 Stream 43)
// payload-aware humanizer for the per-rollout exclude-from-learning
// toggle (#531 slice 2 §10 Q3). Two cases, both exercised through
// auditToEvent so the dispatch chain is part of the contract:
//
//  1. new_state=true → "AI proposal excluded from future learning"
//     (matches the default table entry; we exercise it through the
//     payload-aware path so a regression in the dispatch wiring is
//     caught here, not just in TestHumanizeEventType).
//  2. new_state=false → "AI proposal re-included in future learning"
//     so the timeline reads honestly when the operator toggles
//     back on.
//
// Defensive third case: when the payload is missing entirely (a
// shape the service should never emit but the dispatch chain has
// to handle gracefully) we fall back to the default table text.
func TestExcludeFromLearningTitle(t *testing.T) {
	t.Run("new_state=true — excluded wording", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: "rollout.excluded_from_learning",
			Actor:     "operator:alice@example.com",
			Payload: map[string]any{
				"rollout_id":     "rlt_8ax9",
				"previous_state": false,
				"new_state":      true,
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal excluded from future learning"
		if got.Title != want {
			t.Errorf("excluded title = %q, want %q", got.Title, want)
		}
	})

	t.Run("new_state=false — re-included wording", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: "rollout.excluded_from_learning",
			Actor:     "operator:alice@example.com",
			Payload: map[string]any{
				"rollout_id":     "rlt_8ax9",
				"previous_state": true,
				"new_state":      false,
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal re-included in future learning"
		if got.Title != want {
			t.Errorf("re-included title = %q, want %q", got.Title, want)
		}
	})

	t.Run("payload missing — falls back to default table text", func(t *testing.T) {
		// Defensive — the service contract emits new_state on every
		// row, so this should never happen in practice. But the
		// humanizer falls through to the v0.81.4 table when the
		// field is missing so a corrupt or backfilled-without-the-
		// field row still renders something meaningful.
		ev := &services.AuditEvent{
			EventType: "rollout.excluded_from_learning",
			Actor:     "operator:alice@example.com",
			Payload:   map[string]any{},
		}
		got := auditToEvent(ev)
		want := "AI proposal excluded from future learning"
		if got.Title != want {
			t.Errorf("payload-missing title = %q, want %q", got.Title, want)
		}
	})
}

// TestProposalCreatedByStateTitle pins the v0.89.37 (#657 Stream 55,
// #531 slice 2 chunk 6) extension to the proposal.created humanizer.
// When the audit payload carries verdict_examples_used_by_state with
// at least one non-empty bucket, the title surfaces the per-state mix
// ("informed by N approved + M rejected verdicts") instead of the
// v0.89.22 flat-count phrasing. Single-bucket → "informed by N
// approved verdicts" or "informed by N rejected verdicts"; zero
// buckets are dropped.
//
// Backward compat: when by-state is absent or all buckets are empty
// the humanizer falls back to the v0.89.22 flat-count phrasing on the
// existing verdict_examples_used field, so the slice 1
// TestProposalCreatedTitle table above continues to pin the legacy
// shape.
func TestProposalCreatedByStateTitle(t *testing.T) {
	t.Run("all approved — single-bucket plural", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []any{"rlt_a0", "rlt_a1"},
				"verdict_examples_used_by_state": map[string]any{
					"approved": []any{"rlt_a0", "rlt_a1"},
					"rejected": []any{},
				},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created (informed by 2 approved verdicts)"
		if got.Title != want {
			t.Errorf("all-approved title = %q, want %q", got.Title, want)
		}
	})

	t.Run("all rejected — single-bucket plural", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []any{"rlt_r0", "rlt_r1"},
				"verdict_examples_used_by_state": map[string]any{
					"approved": []any{},
					"rejected": []any{"rlt_r0", "rlt_r1"},
				},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created (informed by 2 rejected verdicts)"
		if got.Title != want {
			t.Errorf("all-rejected title = %q, want %q", got.Title, want)
		}
	})

	t.Run("mixed approved + rejected — multi-bucket plus-join", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []any{"rlt_a0", "rlt_r0", "rlt_r1"},
				"verdict_examples_used_by_state": map[string]any{
					"approved": []any{"rlt_a0"},
					"rejected": []any{"rlt_r0", "rlt_r1"},
				},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created (informed by 1 approved + 2 rejected verdicts)"
		if got.Title != want {
			t.Errorf("mixed title = %q, want %q", got.Title, want)
		}
	})

	t.Run("by-state absent but verdict_examples_used populated — falls back to v0.89.22 flat-count", func(t *testing.T) {
		// Legacy fixture shape: slice 1 audit rows + SIEM consumers
		// that haven't ingested the slice 2 extension yet. The
		// humanizer MUST surface the v0.89.22 phrasing so backfilled
		// timelines render consistently.
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []any{"rlt_6m08", "rlt_8ax9"},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created (cited 2 prior verdicts)"
		if got.Title != want {
			t.Errorf("fallback title = %q, want %q", got.Title, want)
		}
	})

	t.Run("by-state present but all buckets empty — falls back via flat-count cold-start path", func(t *testing.T) {
		// Defensive: the audit emit omits the by-state field when every
		// bucket is empty, but a forward-compat payload that emitted it
		// anyway must still fall through to the cold-start title via
		// the v0.89.22 path so we don't render "informed by  verdicts".
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []any{},
				"verdict_examples_used_by_state": map[string]any{
					"approved": []any{},
					"rejected": []any{},
				},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created"
		if got.Title != want {
			t.Errorf("empty-buckets title = %q, want %q", got.Title, want)
		}
	})

	t.Run("typed map[string][]string also handled", func(t *testing.T) {
		// In-process emits (bridge → audit fake in tests) hand the
		// humanizer a typed shape that bypasses the JSON round-trip.
		// Both shapes MUST surface the same mix.
		ev := &services.AuditEvent{
			EventType: "proposal.created",
			Actor:     "ai",
			Payload: map[string]any{
				"verdict_examples_used": []string{"rlt_a0", "rlt_r0"},
				"verdict_examples_used_by_state": map[string][]string{
					"approved": {"rlt_a0"},
					"rejected": {"rlt_r0"},
				},
			},
		}
		got := auditToEvent(ev)
		want := "AI proposal created (informed by 1 approved + 1 rejected verdicts)"
		if got.Title != want {
			t.Errorf("typed-map title = %q, want %q", got.Title, want)
		}
	})
}

// TestDiscoveryProposalCreatedByStateTitle pins the v0.89.37 (#657
// Stream 55, #531 slice 2 chunk 6) extension to the
// discovery_proposal.created humanizer. Mirrors
// TestProposalCreatedByStateTitle but with the discovery-surface
// vocabulary ("accepted" / "closed" / "excluded"). Single-bucket
// merged-only intentionally renders the v0.89.28 phrasing
// ("informed by N accepted PRs") so the slice 1 humanizer
// invariant holds when an operator's pool happens to be merged-only.
func TestDiscoveryProposalCreatedByStateTitle(t *testing.T) {
	t.Run("all merged — single-bucket matches v0.89.28 phrasing", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: services.AuditEventDiscoveryProposalCreated,
			Payload: map[string]any{
				"scan_id":              "scan-1",
				"account_id":           "111111111111",
				"region":               "us-east-1",
				"recommendation_count": float64(3),
				"verdict_examples_used": []any{
					"https://github.com/octo/widgets/pull/142",
					"https://github.com/octo/widgets/pull/138",
				},
				"verdict_examples_used_by_state": map[string]any{
					"merged": []any{
						"https://github.com/octo/widgets/pull/142",
						"https://github.com/octo/widgets/pull/138",
					},
					"closed_not_merged": []any{},
					"operator_excluded": []any{},
				},
			},
		}
		got := auditToEvent(ev)
		want := "Discovery recommendations generated (informed by 2 accepted PRs)"
		if got.Title != want {
			t.Errorf("all-merged title = %q, want %q", got.Title, want)
		}
	})

	t.Run("mixed merged + closed + excluded — multi-bucket plus-join", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: services.AuditEventDiscoveryProposalCreated,
			Payload: map[string]any{
				"scan_id":              "scan-1",
				"account_id":           "111111111111",
				"region":               "us-east-1",
				"recommendation_count": float64(2),
				"verdict_examples_used": []any{
					"https://github.com/octo/widgets/pull/145",
					"rec_id_abc",
					"https://github.com/octo/widgets/pull/142",
				},
				"verdict_examples_used_by_state": map[string]any{
					"merged":            []any{"https://github.com/octo/widgets/pull/142"},
					"closed_not_merged": []any{"https://github.com/octo/widgets/pull/145"},
					"operator_excluded": []any{"rec_id_abc"},
				},
			},
		}
		got := auditToEvent(ev)
		want := "Discovery recommendations generated (informed by 1 accepted + 1 closed + 1 excluded)"
		if got.Title != want {
			t.Errorf("mixed title = %q, want %q", got.Title, want)
		}
	})

	t.Run("by-state absent — falls back to v0.89.28 flat-count phrasing", func(t *testing.T) {
		// Legacy slice 1 audit rows + cold-start rows that omit the
		// by-state field per the v0.89.37 emit gate must keep the
		// v0.89.28 humanizer output exactly.
		ev := &services.AuditEvent{
			EventType: services.AuditEventDiscoveryProposalCreated,
			Payload: map[string]any{
				"scan_id":              "scan-1",
				"account_id":           "111111111111",
				"region":               "us-east-1",
				"recommendation_count": float64(3),
				"verdict_examples_used": []any{
					"https://github.com/octo/widgets/pull/142",
					"https://github.com/octo/widgets/pull/138",
				},
			},
		}
		got := auditToEvent(ev)
		want := "Discovery recommendations generated (informed by 2 prior accepted PRs)"
		if got.Title != want {
			t.Errorf("fallback title = %q, want %q", got.Title, want)
		}
	})

	t.Run("by-state present but all buckets empty — falls back to cold-start phrasing", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: services.AuditEventDiscoveryProposalCreated,
			Payload: map[string]any{
				"scan_id":               "scan-1",
				"verdict_examples_used": []any{},
				"verdict_examples_used_by_state": map[string]any{
					"merged":            []any{},
					"closed_not_merged": []any{},
					"operator_excluded": []any{},
				},
			},
		}
		got := auditToEvent(ev)
		want := "Discovery recommendations generated"
		if got.Title != want {
			t.Errorf("empty-buckets title = %q, want %q", got.Title, want)
		}
	})

	t.Run("typed map[string][]string also handled", func(t *testing.T) {
		ev := &services.AuditEvent{
			EventType: services.AuditEventDiscoveryProposalCreated,
			Payload: map[string]any{
				"scan_id": "scan-1",
				"verdict_examples_used": []string{
					"https://github.com/octo/widgets/pull/145",
					"https://github.com/octo/widgets/pull/142",
				},
				"verdict_examples_used_by_state": map[string][]string{
					"merged":            {"https://github.com/octo/widgets/pull/142"},
					"closed_not_merged": {"https://github.com/octo/widgets/pull/145"},
				},
			},
		}
		got := auditToEvent(ev)
		want := "Discovery recommendations generated (informed by 1 accepted + 1 closed)"
		if got.Title != want {
			t.Errorf("typed-map title = %q, want %q", got.Title, want)
		}
	})
}
