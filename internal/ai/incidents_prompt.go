// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"fmt"
	"strings"
	"time"
)

// draftIncidentSystem is the system prompt for DraftIncidentFromAction.
//
// Design notes:
//
//   - The drafter writes the ticket the operator hands to leadership,
//     to a customer, or to themselves on Monday morning. The tone is
//     factual and concise. No marketing language. No claims the data
//     does not support.
//   - The drafter does not include hostnames that look internal
//     (anything ending in .internal, .corp, .local, or with a leading
//     digit-prefixed segment that looks like a host id). It also
//     does not include raw IP addresses or any string that looks
//     like a secret. The renderer strips these defensively but the
//     prompt names them so the model does not produce them in the
//     first place.
//   - The drafter is allowed to decline. A dry-run-only event that
//     changed no state, an action.denied for a routine signature
//     expiry that the runner re-fetches automatically, an
//     action.failed that was a one-shot retry succeeding on the next
//     tick — none of those merit a ticket. Declining is the right
//     call when the operator would close the ticket without action.
//
// The output is strict JSON. The schema is described in the user
// message; the system prompt focuses on tone and content rules so
// the model can hold the format in working memory while writing.
const draftIncidentSystem = `You are Squadron's incident drafter. After an automated action runs on a node in the operator's fleet, you write a postmortem-style ticket draft the operator will review, edit, and publish through their team's ticketing system.

Write in the voice of an engineer summarizing what happened. Be factual. Use short paragraphs. Do not use emojis. Do not use hyphens in prose unless grammatically required.

Content rules:

  1. Title is one line, under 100 characters, no marketing language. State the affected component, the change applied, and either "success" or "failure" if relevant. Examples: "Restart and verify nginx on web canary, success" or "Pin hashing rounds on web group canary, mitigated cost spike".

  2. Summary is 2 to 4 short paragraphs. Cover: what triggered the action, what Squadron did, what changed, what the operator should look at next. Do not include hostnames ending in .internal, .corp, or .local. Do not include raw IP addresses. Do not include token IDs, secrets, or signing key fingerprints beyond what is in the input.

  3. Timeline is a chronological list. Each entry has an ISO 8601 timestamp (UTC, RFC 3339) and a one-line factual description. Include only events that are present in the input. Do not invent intermediate events.

  4. Resolution applied is one or two short sentences naming the actual change. Leave it empty when the input says state_changed=false (a dry-run that did not modify anything).

  5. Follow ups is a list of short prompts for the operator. Use the imperative voice: "Confirm the team owning the ML feature is aware", "Decide whether the new value is permanent", "Check the audit timeline for related rollouts in the past week". Three to five items. Do not include items the operator could not act on.

  6. Decline (declined=true with a one-sentence reason) when there is nothing useful to ticket: a dry-run that produced no signal, an action.denied that did not result in execution, a transient failure that is already self-recovering. Decline when an operator would close the resulting ticket without action.

Return exactly one JSON object matching the schema in the user message. No prose outside the JSON object. No markdown fences around the JSON.`

// buildDraftIncidentUserMessage renders the structured input into the
// model prompt. The user message carries the schema, the raw input,
// and the explicit instruction to return JSON.
func buildDraftIncidentUserMessage(in IncidentDraftInput) string {
	var b strings.Builder

	b.WriteString("Draft an incident ticket for the action below.\n\n")

	b.WriteString("Action context:\n")
	fmt.Fprintf(&b, "  action_request_id: %s\n", in.ActionRequestID)
	if in.RolloutID != "" {
		fmt.Fprintf(&b, "  rollout_id: %s\n", in.RolloutID)
	}
	if in.GroupName != "" {
		fmt.Fprintf(&b, "  group: %s\n", in.GroupName)
	}
	fmt.Fprintf(&b, "  action_type: %s\n", in.ActionType)
	fmt.Fprintf(&b, "  phase: %s\n", in.Phase)
	fmt.Fprintf(&b, "  status: %s\n", in.Status)
	fmt.Fprintf(&b, "  state_changed: %t\n", in.StateChanged)
	if !in.StartedAt.IsZero() {
		fmt.Fprintf(&b, "  started_at: %s\n", in.StartedAt.UTC().Format(time.RFC3339))
	}
	if !in.CompletedAt.IsZero() {
		fmt.Fprintf(&b, "  completed_at: %s\n", in.CompletedAt.UTC().Format(time.RFC3339))
	}
	if in.ActionSummary != "" {
		fmt.Fprintf(&b, "  action_summary: %s\n", in.ActionSummary)
	}
	b.WriteString("\n")

	if in.TriggerSummary != "" {
		b.WriteString("What triggered the action:\n")
		b.WriteString("  ")
		b.WriteString(in.TriggerSummary)
		b.WriteString("\n\n")
	}
	if in.ProposalReasoning != "" {
		b.WriteString("AI proposer reasoning (when the originating rollout was AI drafted):\n")
		b.WriteString("  ")
		b.WriteString(in.ProposalReasoning)
		b.WriteString("\n\n")
	}

	if len(in.OutcomeBullets) > 0 {
		b.WriteString("Outcome details:\n")
		for _, line := range in.OutcomeBullets {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		b.WriteString("\n")
	}

	b.WriteString(`Return JSON of this shape:

{
  "declined": false,
  "reason": "",
  "title": "string, one line, under 100 chars",
  "summary": "string, 2 to 4 short paragraphs",
  "timeline": [
    {"at": "2026-06-14T02:14:00Z", "text": "what happened"}
  ],
  "resolution_applied": "string, empty when state_changed was false",
  "follow_ups": ["short imperative prompt", "another"]
}

If the action does not merit a ticket, return:

{
  "declined": true,
  "reason": "one sentence explanation"
}
`)
	return b.String()
}
