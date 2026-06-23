// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package checkrunprompt

import (
	"strings"
	"testing"
)

// TestComposeCreateSummary_HappyPath_RendersAllSections — the
// load-bearing happy path. With 2 merged + 1 closed + 1 excluded
// the summary MUST contain the kind, the scope tuple, the
// reasoning, all three verdict-context bullets in the documented
// order, the per-PR-URL markdown links, and the View in Squadron
// link. Mirrors design doc §13 acceptance test 1 from the
// summary-content perspective.
func TestComposeCreateSummary_HappyPath_RendersAllSections(t *testing.T) {
	in := SummaryInput{
		RecommendationKind:   "rds-pi-em",
		RecommendationReason: "Enable Performance Insights for prod-db so we can attribute QPS spikes.",
		AccountID:            "123456789012",
		Region:               "us-east-1",
		ConnectionID:         "conn-abc",
		PRURL:                "https://github.com/acme/infra/pull/142",
		RecommendationID:     "rec-xyz",
		SquadronHost:         "https://squadron.acme.example",
		VerdictsByState: map[string][]string{
			VerdictStateMerged: {
				"https://github.com/acme/infra/pull/138",
				"https://github.com/acme/infra/pull/142",
			},
			VerdictStateClosedNotMerged: {
				"https://github.com/acme/infra/pull/145",
			},
			VerdictStateOperatorExcluded: {"exclude-1"},
		},
	}
	title, summary := ComposeCreateSummary(in)
	if title != "Squadron recommendation: rds-pi-em" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{
		"**Squadron recommendation: rds-pi-em**",
		"account 123456789012",
		"region us-east-1",
		"connection conn-abc",
		"Enable Performance Insights",
		"**Verdict learning context**",
		"Informed by 2 prior accepted PRs",
		"[#138](https://github.com/acme/infra/pull/138)",
		"[#142](https://github.com/acme/infra/pull/142)",
		"Informed by 1 prior closed-without-merge PRs",
		"[#145](https://github.com/acme/infra/pull/145)",
		"1 operator-set exclusions in this scope",
		"[View in Squadron](https://squadron.acme.example/discovery/aws/conn-abc/recommendations#rec-xyz)",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q\n--- summary ---\n%s", want, summary)
		}
	}
}

// TestComposeCreateSummary_ColdStart_OmitsVerdictContext — when
// VerdictsByState is empty / nil the entire "Verdict learning
// context" header MUST be absent so the cold-start path renders
// cleanly. Mirrors design doc §13 acceptance test 12.
func TestComposeCreateSummary_ColdStart_OmitsVerdictContext(t *testing.T) {
	in := SummaryInput{
		RecommendationKind:   "rds-pi-em",
		RecommendationReason: "Enable PI.",
		AccountID:            "111111111111",
		Region:               "us-west-2",
		ConnectionID:         "conn-cold",
		PRURL:                "https://github.com/acme/infra/pull/1",
		RecommendationID:     "rec-cold",
		SquadronHost:         "https://squadron.acme.example",
		VerdictsByState:      nil,
	}
	_, summary := ComposeCreateSummary(in)
	if strings.Contains(summary, "Verdict learning context") {
		t.Errorf("cold-start summary contains 'Verdict learning context' section:\n%s", summary)
	}
	// Sanity: every other section still renders.
	for _, want := range []string{
		"**Squadron recommendation: rds-pi-em**",
		"account 111111111111",
		"**What this PR does**",
		"[View in Squadron]",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("cold-start summary missing %q\n--- summary ---\n%s", want, summary)
		}
	}

	// Also exercise the "every bucket present but empty" cold-start
	// shape — same gate, different input.
	in.VerdictsByState = map[string][]string{
		VerdictStateMerged:           {},
		VerdictStateClosedNotMerged:  {},
		VerdictStateOperatorExcluded: {},
	}
	_, summary = ComposeCreateSummary(in)
	if strings.Contains(summary, "Verdict learning context") {
		t.Errorf("empty-bucket cold-start summary contains 'Verdict learning context' section:\n%s", summary)
	}
}

// TestComposeCreateSummary_RedactsSecretsInReasoning — a
// token-shaped string (ghp_ prefix per ai.redact) in the reasoning
// MUST NOT appear verbatim. The placeholder text is what survives.
func TestComposeCreateSummary_RedactsSecretsInReasoning(t *testing.T) {
	tokenLiteral := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	in := SummaryInput{
		RecommendationKind:   "rds-pi-em",
		RecommendationReason: "The proposer's token was " + tokenLiteral + " — do not log.",
		AccountID:            "1", Region: "r", ConnectionID: "c",
		PRURL:        "https://github.com/a/b/pull/1",
		SquadronHost: "https://h.example",
	}
	_, summary := ComposeCreateSummary(in)
	if strings.Contains(summary, tokenLiteral) {
		t.Errorf("summary contains unredacted token:\n%s", summary)
	}
	if !strings.Contains(summary, "<redacted:github_token>") {
		// escapeMarkdown rewrites < and > so the placeholder is
		// emitted with escapes. Accept either the raw placeholder
		// (defensive) or the escaped form (actual path).
		if !strings.Contains(summary, `\<redacted:github\_token\>`) {
			t.Errorf("summary does not carry the redaction placeholder:\n%s", summary)
		}
	}
}

// TestComposeCreateSummary_EscapesMarkdownInReasoning — the §12.2
// load-bearing test for the injection escape pass:
//   - asterisks MUST be escaped so they don't render as bold
//   - image markdown MUST be stripped entirely
//   - raw HTML MUST be stripped entirely
//
// Mirrors design doc §13 acceptance test 11.
func TestComposeCreateSummary_EscapesMarkdownInReasoning(t *testing.T) {
	in := SummaryInput{
		RecommendationKind:   "rds-pi-em",
		RecommendationReason: "**bold** and ![img](http://attacker.com/exfil.png) and <script>alert(1)</script>",
		AccountID:            "1", Region: "r", ConnectionID: "c",
		PRURL:        "https://github.com/a/b/pull/1",
		SquadronHost: "https://h.example",
	}
	_, summary := ComposeCreateSummary(in)

	// Asterisks from the reasoning render as the escaped \* form.
	// The summary header lines (**Squadron recommendation: ...**)
	// still contain raw asterisks — they're emitted by the template,
	// not from operator input — so we look for the specific
	// "bold" word with surrounding escaped asterisks.
	if !strings.Contains(summary, `\*\*bold\*\*`) {
		t.Errorf("summary did not escape ** in reasoning:\n%s", summary)
	}

	// Image markdown stripped entirely.
	if strings.Contains(summary, "attacker.com/exfil.png") {
		t.Errorf("summary still contains image URL from stripped image markdown:\n%s", summary)
	}
	if strings.Contains(summary, "![img]") {
		t.Errorf("summary still contains raw image markdown:\n%s", summary)
	}

	// Raw HTML stripped entirely.
	if strings.Contains(summary, "<script>") {
		t.Errorf("summary still contains raw HTML:\n%s", summary)
	}
	if strings.Contains(summary, "alert(1)") {
		// stripHTMLRE excises `<...>` tags — the alert() body lives
		// BETWEEN the tags so it survives as escaped text. Accept
		// the escaped form (escapeMarkdown rewrites the parens).
		if !strings.Contains(summary, `alert\(1\)`) {
			t.Errorf("alert body neither stripped nor escaped:\n%s", summary)
		}
	}
}

// TestComposeCreateSummary_EmbedsPRURLsAsMarkdownLinks — the
// per-PR-URL markdown link rendering: each URL appears as a
// "[#N](url)" link derived from the URL's /pull/<N>/ path segment.
func TestComposeCreateSummary_EmbedsPRURLsAsMarkdownLinks(t *testing.T) {
	in := SummaryInput{
		RecommendationKind:   "rds-pi-em",
		RecommendationReason: "ok",
		AccountID:            "1", Region: "r", ConnectionID: "c",
		PRURL:        "https://github.com/a/b/pull/1",
		SquadronHost: "https://h.example",
		VerdictsByState: map[string][]string{
			VerdictStateMerged: {
				"https://github.com/acme/infra/pull/200",
				"https://github.com/acme/infra/pull/201",
			},
		},
	}
	_, summary := ComposeCreateSummary(in)
	if !strings.Contains(summary, "[#200](https://github.com/acme/infra/pull/200)") {
		t.Errorf("summary missing markdown link for pull/200:\n%s", summary)
	}
	if !strings.Contains(summary, "[#201](https://github.com/acme/infra/pull/201)") {
		t.Errorf("summary missing markdown link for pull/201:\n%s", summary)
	}
}

// TestComposeUpdateSummary_RendersConclusion — sanity check on the
// chunk-3+ helper: the title and body carry the uppercased
// conclusion string verbatim. Wired now to keep the package
// coherent; chunks-3 / 4 will assert deeper behavior.
func TestComposeUpdateSummary_RendersConclusion(t *testing.T) {
	in := SummaryInput{
		RecommendationKind:   "rds-pi-em",
		RecommendationReason: "ok",
		AccountID:            "1", Region: "r", ConnectionID: "c",
		PRURL:        "https://github.com/a/b/pull/1",
		SquadronHost: "https://h.example",
	}
	title, summary := ComposeUpdateSummary(in, "success")
	if !strings.Contains(title, "rds-pi-em — SUCCESS") {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(summary, "— SUCCESS") {
		t.Errorf("summary missing uppercased conclusion:\n%s", summary)
	}
}
