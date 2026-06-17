// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"fmt"
	"strings"

	"github.com/devopsmike2/squadron/internal/services"
)

// topEvidence returns the first n evidence references from the
// slice. Used to cap how many refs flow into a notification body
// so Slack messages don't blow past the section-block field cap
// (10) or look noisy in a chat channel.
func topEvidence(refs []services.EvidenceRef, n int) []services.EvidenceRef {
	if len(refs) <= n {
		return refs
	}
	return refs[:n]
}

// aiProposalSlackBlocks renders Slack Block Kit JSON that webhook
// receivers speaking Slack can paste directly into chat.postMessage
// or use to enrich an Incoming Webhook payload. The blocks include:
//
//   - A header with the rollout name.
//   - A context block tagging the proposal as AI-originated with
//     the model identifier alongside the transition.
//   - A section block carrying the natural-language reasoning so
//     the approver has the AI's justification visible without
//     clicking through.
//   - A section block listing the top evidence refs as Slack
//     links when a URL is present, otherwise as plain bullets.
//   - An actions block with an "Open in Squadron" button when the
//     receiver wires it through.
//
// Receivers that don't speak Slack ignore this field. The base
// JSON payload still carries proposal_reasoning + evidence_refs
// so a Teams or PagerDuty formatter can render its own card from
// the same data.
func aiProposalSlackBlocks(r *services.Rollout, transition string) []map[string]any {
	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{
				"type":  "plain_text",
				"text":  truncate("AI proposed: "+r.Name, 150),
				"emoji": true,
			},
		},
		{
			"type": "context",
			"elements": []map[string]any{
				{
					"type": "mrkdwn",
					"text": fmt.Sprintf("Origin: *AI proposer* · Transition: `%s` · State: `%s`", transition, r.State),
				},
			},
		},
	}
	if r.ProposalReasoning != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": "*Reasoning*\n" + truncate(r.ProposalReasoning, 2500),
			},
		})
	}
	if len(r.EvidenceRefs) > 0 {
		var sb strings.Builder
		sb.WriteString("*Evidence*\n")
		for _, ref := range topEvidence(r.EvidenceRefs, 3) {
			sb.WriteString(formatEvidenceLine(ref))
			sb.WriteString("\n")
		}
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": strings.TrimRight(sb.String(), "\n"),
			},
		})
	}
	return blocks
}

// formatEvidenceLine renders one evidence ref as a Slack mrkdwn
// bullet. URL takes precedence so the approver can click straight
// to the alert / metric; otherwise the kind + id pair is shown so
// the operator can grep the audit log.
func formatEvidenceLine(ref services.EvidenceRef) string {
	label := ref.Description
	if label == "" {
		if ref.Kind != "" && ref.ID != "" {
			label = ref.Kind + ":" + ref.ID
		} else if ref.Kind != "" {
			label = ref.Kind
		} else {
			label = "evidence"
		}
	}
	if ref.URL != "" {
		return fmt.Sprintf("• <%s|%s>", ref.URL, label)
	}
	return "• " + label
}

// truncate keeps Slack-bound text inside its limits. Returns the
// input unchanged when below the cap; appends an ellipsis when
// over. Pulled out so the helper is unit-testable without touching
// the engine.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
