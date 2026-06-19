// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"testing"
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
