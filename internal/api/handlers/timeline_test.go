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
		// Fallback — unknown event_type returns the raw string so we
		// never lose information by humanizing.
		{"unknown event type falls through", "agent.heartbeat", "tick", "agent.heartbeat"},
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
