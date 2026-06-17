// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ai

import (
	"fmt"
	"strings"
)

// proposeFromCostSpikeSystem is the system prompt for the cost-spike
// proposer. Three jobs:
//
//   - Frame the model as a senior SRE proposing a fix, not a
//     chatbot. Concrete language, no markdown, no "I think".
//   - Constrain the output to a strict JSON shape so the parser
//     succeeds without prompt-engineering tricks at parse time.
//   - Establish the safety posture: every proposal is a draft that
//     a human approves; the model can decline (Declined=true) when
//     no good action exists.
//
// Tune this prompt in this constant; the rest of the code path
// stays stable.
const proposeFromCostSpikeSystem = `You are a senior site reliability engineer responsible ` +
	`for an OpenTelemetry collector fleet. Squadron just detected a cost spike on this fleet. ` +
	`Your job is to draft a staged rollout that would address the spike, ` +
	`then return a JSON object describing it.` + "\n\n" +

	`Rules for the proposal:` + "\n" +
	`  - Start with a small canary (10% of the fleet) and dwell at ` +
	`least 600 seconds (10 minutes) before the next stage. Two ` +
	`stages minimum: canary then full.` + "\n" +
	`  - Set require_approval to true. Every AI-originated proposal ` +
	`must be approved by a human before it advances.` + "\n" +
	`  - Set abort_criteria with max_drifted_agents at 5% of the ` +
	`fleet (round up) and min_dwell_seconds_before_abort at 120. ` +
	`Set max_error_logs_per_minute to 50 unless you have a reason ` +
	`to set it differently.` + "\n" +
	`  - The proposal MUST set group_id to the group_id provided in ` +
	`the user message. Do not invent a new group.` + "\n" +
	`  - The proposal MUST set target_config_id to a config you can ` +
	`identify from the context. If no clearly correct ` +
	`target_config_id exists, decline.` + "\n" +
	`  - You may decline (declined: true) if the spike is small, ` +
	`the attribution is ambiguous, or the action would require ` +
	`judgment a model cannot make. State the reason briefly.` + "\n\n" +

	`Reasoning field requirements:` + "\n" +
	`  - 2 to 4 sentences in plain prose, no markdown.` + "\n" +
	`  - State the suspected root cause (what attribute or pipeline ` +
	`is driving the spike), the proposed action (drop, sample, batch ` +
	`differently), and why a 10% canary is the right risk envelope.` + "\n" +
	`  - Write as a peer engineer would on Slack: direct, ` +
	`hedged where appropriate, no chatbot phrases.` + "\n\n" +

	`Evidence field requirements:` + "\n" +
	`  - Each entry kind MUST be one of: alert, metric, configlint, ` +
	`recommendation, audit_event, url.` + "\n" +
	`  - Include the cost spike itself as the first entry, kind ` +
	`"alert", id set to the spike_id.` + "\n" +
	`  - Cite any lint findings or recommendations from the context ` +
	`that informed the proposal.` + "\n\n" +

	`Output ONLY a JSON object, no preface, no code fences. Schema:` + "\n" +
	`{` + "\n" +
	`  "declined": false,` + "\n" +
	`  "reason": "",` + "\n" +
	`  "proposal": {` + "\n" +
	`    "name": "AI: drop high-cardinality container.id from metrics",` + "\n" +
	`    "group_id": "<from user message>",` + "\n" +
	`    "target_config_id": "<from context>",` + "\n" +
	`    "require_approval": true,` + "\n" +
	`    "stages": [` + "\n" +
	`      {"mode":"percentage","percentage":10,"dwell_seconds":600},` + "\n" +
	`      {"mode":"percentage","percentage":100,"dwell_seconds":0}` + "\n" +
	`    ],` + "\n" +
	`    "abort_criteria": {` + "\n" +
	`      "max_drifted_agents": 5,` + "\n" +
	`      "max_error_logs_per_minute": 50,` + "\n" +
	`      "min_dwell_seconds_before_abort": 120` + "\n" +
	`    }` + "\n" +
	`  },` + "\n" +
	`  "reasoning": "Two-to-four sentences here.",` + "\n" +
	`  "evidence": [` + "\n" +
	`    {"kind":"alert","id":"<spike_id>","description":"Cost spike fired at <when>"}` + "\n" +
	`  ]` + "\n" +
	`}` + "\n\n" +

	`When declining, omit "proposal" and "evidence" and set:` + "\n" +
	`{ "declined": true, "reason": "Short sentence." }`

// buildProposeUserMessage assembles the user-side message Claude
// receives. Kept here so the prompt template is in one place and
// tunable without touching the call site.
func buildProposeUserMessage(in CostSpikeContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cost spike detected on Squadron-managed fleet.\n\n")
	fmt.Fprintf(&b, "spike_id: %s\n", in.SpikeID)
	if in.Severity != "" {
		fmt.Fprintf(&b, "severity: %s\n", in.Severity)
	}
	if in.Signal != "" {
		fmt.Fprintf(&b, "signal: %s\n", in.Signal)
	}
	if !in.StartedAt.IsZero() {
		fmt.Fprintf(&b, "started_at: %s\n", in.StartedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	fmt.Fprintf(&b, "baseline_monthly_usd: $%.2f\n", in.BaselineMonthlyUSD)
	fmt.Fprintf(&b, "peak_monthly_usd: $%.2f\n", in.PeakMonthlyUSD)
	fmt.Fprintf(&b, "peak_pct_above_baseline: %.0f%%\n", in.PeakPctAboveBaseline)
	b.WriteString("\n")

	fmt.Fprintf(&b, "Target group:\n")
	fmt.Fprintf(&b, "  group_id: %s\n", in.GroupID)
	if in.GroupName != "" {
		fmt.Fprintf(&b, "  group_name: %s\n", in.GroupName)
	}
	b.WriteString("\n")

	if len(in.TopAgents) > 0 {
		fmt.Fprintf(&b, "Top contributing agents (descending):\n")
		for _, a := range in.TopAgents {
			fmt.Fprintf(&b, "  - %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(in.TopAttributes) > 0 {
		fmt.Fprintf(&b, "Top contributing attributes:\n")
		for _, a := range in.TopAttributes {
			fmt.Fprintf(&b, "  - %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(in.RecentLintFindings) > 0 {
		fmt.Fprintf(&b, "Recent configlint findings on this group's configs:\n")
		for _, f := range in.RecentLintFindings {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
		b.WriteString("\n")
	}
	if len(in.RecentRecommendations) > 0 {
		fmt.Fprintf(&b, "Open recommendations for this group:\n")
		for _, r := range in.RecentRecommendations {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
		b.WriteString("\n")
	}

	b.WriteString("Return your proposal as the JSON object described in the system prompt. ")
	b.WriteString("group_id MUST equal the value above. Set require_approval to true.\n")
	return b.String()
}
