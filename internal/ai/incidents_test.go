// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDraftIncidentFromAction_CleanDraft is the happy path: the
// model returns a well-formed JSON draft and the service parses
// every field into the result.
func TestDraftIncidentFromAction_CleanDraft(t *testing.T) {
	reply := anthropicReply(`{
  "declined": false,
  "title": "Restart and verify nginx on web canary, success",
  "summary": "A cost spike detected on the web group at 02:14 UTC traced to a new ML attribution workload. The proposer drafted a rollout pinning hashing.rounds to 6 for the canary tier. Squadron's action runner restarted nginx on the canary host after the config landed.\n\nIngest volume returned to baseline at 02:38.",
  "timeline": [
    {"at": "2026-06-14T02:14:00Z", "text": "cost spike detected"},
    {"at": "2026-06-14T02:21:00Z", "text": "operator approved the AI proposal"},
    {"at": "2026-06-14T02:25:00Z", "text": "runner restarted nginx on the canary host"},
    {"at": "2026-06-14T02:38:00Z", "text": "ingest volume returned to baseline"}
  ],
  "resolution_applied": "receivers.otlp.protocols.hashing.rounds: 12 to 6 on the web group canary tier; nginx restarted.",
  "follow_ups": [
    "Confirm the ML feature owner is aware of the change",
    "Decide whether rounds=6 is the new permanent value",
    "Check the audit timeline for related rollouts in the past week"
  ]
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	res, err := svc.DraftIncidentFromAction(context.Background(), IncidentDraftInput{
		ActionRequestID:   "ar-7c11",
		RolloutID:         "rl-4b29",
		GroupName:         "Web Group",
		ActionType:        "restart-systemd-service",
		Phase:             "execute",
		Status:            "success",
		StartedAt:         time.Date(2026, 6, 14, 2, 24, 30, 0, time.UTC),
		CompletedAt:       time.Date(2026, 6, 14, 2, 25, 12, 0, time.UTC),
		ActionSummary:     "systemctl restart nginx.service",
		TriggerSummary:    "cost spike +312 percent on web group from hashing.rounds=12",
		ProposalReasoning: "Pin hashing.rounds=6 on canary first to absorb the ML attribution.",
		OutcomeBullets:    []string{"ran_command: systemctl restart nginx.service", "exit_code: 0"},
		AuditReferences:   map[string]string{"audit_url": "/audit?action=ar-7c11"},
		StateChanged:      true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.False(t, res.Declined)
	assert.Equal(t, "Restart and verify nginx on web canary, success", res.Title)
	assert.Contains(t, res.Summary, "cost spike")
	assert.Len(t, res.Timeline, 4)
	assert.Equal(t, "cost spike detected", res.Timeline[0].Text)
	assert.Contains(t, res.ResolutionApplied, "hashing.rounds")
	assert.Len(t, res.FollowUps, 3)
	// Metering plumbed through from the Anthropic envelope.
	assert.Equal(t, 123, res.TokensIn)
	assert.Equal(t, 456, res.TokensOut)
	// The bridge passes audit references in; the drafter mirrors
	// them onto the result so the renderer can surface them.
	assert.Equal(t, "/audit?action=ar-7c11", res.AuditReferences["audit_url"])
}

// TestDraftIncidentFromAction_Declined verifies the model declining
// to draft propagates cleanly. The bridge uses Declined to skip
// persisting a draft for routine no-op events.
func TestDraftIncidentFromAction_Declined(t *testing.T) {
	reply := anthropicReply(`{
  "declined": true,
  "reason": "dry-run-only check that produced no state change and no signal worth reviewing"
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	res, err := svc.DraftIncidentFromAction(context.Background(), IncidentDraftInput{
		ActionRequestID: "ar-9999",
		ActionType:      "restart-systemd-service",
		Phase:           "dry_run",
		Status:          "success",
		StateChanged:    false,
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.True(t, res.Declined)
	assert.Contains(t, res.Reason, "dry-run")
	assert.Empty(t, res.Title)
	assert.Empty(t, res.Summary)
}

// TestDraftIncidentFromAction_RejectsMalformedJSON ensures a model
// response that is not valid JSON produces an error rather than a
// partial draft. The bridge logs and skips on error.
func TestDraftIncidentFromAction_RejectsMalformedJSON(t *testing.T) {
	reply := anthropicReply(`I'd love to help! Here's the draft: oh wait, not JSON.`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	_, err := svc.DraftIncidentFromAction(context.Background(), IncidentDraftInput{
		ActionRequestID: "ar-broken",
		ActionType:      "restart-systemd-service",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}

// TestDraftIncidentFromAction_RejectsEmptyTitle catches the model
// returning declined=false but no title. The bridge should treat
// this as an error (not a silent malformed draft).
func TestDraftIncidentFromAction_RejectsEmptyTitle(t *testing.T) {
	reply := anthropicReply(`{
  "declined": false,
  "title": "",
  "summary": "stuff"
}`)
	srv := proposerTestServer(t, reply)
	defer srv.Close()

	svc := proposerServiceForTest(srv.URL)
	_, err := svc.DraftIncidentFromAction(context.Background(), IncidentDraftInput{
		ActionRequestID: "ar-empty-title",
		ActionType:      "restart-systemd-service",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty title")
}

// TestDraftIncidentFromAction_DisabledServiceErrors ensures calling
// the drafter on a service that has AI disabled returns the
// canonical disabled error so the bridge can skip cleanly.
func TestDraftIncidentFromAction_DisabledServiceErrors(t *testing.T) {
	svc := NewService(Config{Enabled: false}, nil)
	_, err := svc.DraftIncidentFromAction(context.Background(), IncidentDraftInput{
		ActionRequestID: "ar-1",
		ActionType:      "restart-systemd-service",
	})
	require.ErrorIs(t, err, ErrDisabled)
}

// TestRenderIncidentMarkdown verifies the renderer produces the
// expected sections in order with no markdown surprises. We pin the
// shape so a UI consumer can rely on the headings.
func TestRenderIncidentMarkdown(t *testing.T) {
	r := &IncidentDraftResult{
		Title:   "Restart nginx on web canary, success",
		Summary: "Brief postmortem text.",
		Timeline: []IncidentTimelineEntry{
			{At: time.Date(2026, 6, 14, 2, 14, 0, 0, time.UTC), Text: "cost spike detected"},
			{At: time.Date(2026, 6, 14, 2, 25, 0, 0, time.UTC), Text: "runner executed restart"},
		},
		ResolutionApplied: "Restarted nginx.",
		FollowUps:         []string{"Confirm owner", "Decide next steps"},
		AuditReferences:   map[string]string{"audit_url": "/a/x"},
	}
	md := RenderIncidentMarkdown(r, IncidentDraftInput{
		ActionRequestID: "ar-7c11",
		RolloutID:       "rl-4b29",
	})

	// Heading order: Title, Summary, Timeline, Resolution applied,
	// Follow up, Audit references.
	require.Less(t, strings.Index(md, "## Summary"), strings.Index(md, "## Timeline"))
	require.Less(t, strings.Index(md, "## Timeline"), strings.Index(md, "## Resolution applied"))
	require.Less(t, strings.Index(md, "## Resolution applied"), strings.Index(md, "## Follow up"))
	require.Less(t, strings.Index(md, "## Follow up"), strings.Index(md, "## Audit references"))

	// Timeline entries render with HH:MM:SS prefixes.
	assert.Contains(t, md, "02:14:00  cost spike detected")
	assert.Contains(t, md, "02:25:00  runner executed restart")

	// Audit references include the action_request and rollout IDs
	// from the input as well as any extras the drafter passed.
	assert.Contains(t, md, "action_request: ar-7c11")
	assert.Contains(t, md, "rollout: rl-4b29")
	assert.Contains(t, md, "audit_url: /a/x")
}
