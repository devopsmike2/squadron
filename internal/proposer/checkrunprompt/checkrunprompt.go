// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package checkrunprompt renders the markdown summary block the
// GitHub Checks API check run displays on a Squadron-opened PR. It
// is the prompt-side companion to the iac/github CreateCheckRun /
// UpdateCheckRun wrappers shipped in chunk 1 (v0.89.42).
//
// Two cross-cutting invariants this package owns:
//
//  1. **Cold-start parity.** The verdict learning context section
//     is omitted entirely when every per-state bucket is empty so a
//     PR opened on a brand-new connection renders a coherent
//     check-run summary without an awkward "Informed by 0 prior
//     PRs" line. See design doc §13 acceptance test 12.
//
//  2. **Markdown injection escape pass.** The proposer's reasoning
//     string is operator-untrusted in some contexts (e.g. text the
//     proposer ingested from Slack via Ask Squadron). Even after
//     the existing ai.RedactSecrets pipeline strips token-shaped
//     bytes, markdown features (bold, image embeds, raw HTML)
//     could surface as injection vectors in the GitHub UI. This
//     package strips image markdown, strips raw HTML, and escapes
//     the remaining markdown special characters so the reasoning
//     renders as literal text. See design doc §12.2 and §13
//     acceptance test 11.
package checkrunprompt

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/devopsmike2/squadron/internal/ai"
)

// VerdictStateMerged is the per-state bucket key for prior PRs the
// operator merged. The discovery bridge's chunk-6
// verdict_examples_used_by_state map writes this key.
const VerdictStateMerged = "merged"

// VerdictStateClosedNotMerged is the per-state bucket key for prior
// PRs the operator closed without merging.
const VerdictStateClosedNotMerged = "closed_not_merged"

// VerdictStateOperatorExcluded is the per-state bucket key for prior
// operator-set exclusions in this scope (filtered before this scan).
const VerdictStateOperatorExcluded = "operator_excluded"

// SummaryInput is what the bridge passes to compose the check-run
// summary. All fields are optional except RecommendationKind +
// AccountID + Region + ConnectionID — the §9.1 template renders the
// scope tuple verbatim and a missing field would leave an "account
// , region" gap.
type SummaryInput struct {
	// RecommendationKind is the proposer-emitted kind (e.g.
	// "rds-pi-em", "eks-observability-addon"). Used in the title and
	// the body.
	RecommendationKind string

	// RecommendationReason is the raw reasoning string the proposer
	// emitted. Passed through ai.RedactSecrets then escapeMarkdown
	// before embedding.
	RecommendationReason string

	// AccountID + Region + ConnectionID + PRURL are the scope tuple
	// + PR coordinates the summary's first paragraph names.
	AccountID    string
	Region       string
	ConnectionID string
	PRURL        string

	// RecommendationID is the proposer-emitted ID; used to build the
	// "View in Squadron" deep-link's fragment.
	RecommendationID string

	// SquadronHost is the base URL the "View in Squadron" link
	// targets. Empty SquadronHost suppresses the link line entirely
	// rather than emitting a broken `(/)` href.
	SquadronHost string

	// VerdictsByState is chunk 6's verdict_examples_used_by_state
	// map. Keys are VerdictState* constants; values are PR URLs
	// (for merged / closed_not_merged) or short identifiers (for
	// operator_excluded). A nil / empty map triggers the cold-start
	// path: the entire "Verdict learning context" section is
	// omitted.
	VerdictsByState map[string][]string
}

// ComposeCreateSummary builds the markdown summary for a brand-new
// check run at PR open time. Returns title + summary strings ready
// to drop into iacgithub.CheckRunOutput.
//
// Title: "Squadron recommendation: <kind>".
//
// Summary layout (per §9.1):
//
//	**Squadron recommendation: <kind>**
//
//	This PR was opened by Squadron based on a discovery scan of
//	account <account>, region <region>, connection <connection_id>.
//
//	**What this PR does**
//	<recommendation reasoning, redacted + escaped>
//
//	**Verdict learning context**
//	- Informed by N prior accepted PRs in this scope: [#142, #138]
//	- Informed by M prior closed-without-merge PRs in this scope: [#145]
//	- P operator-set exclusions in this scope (filtered before this scan)
//
//	[View in Squadron](https://your-squadron-host/...)
//
// Cold-start (every VerdictsByState bucket empty / nil map) omits
// the entire "Verdict learning context" section so the summary
// still renders coherently on a brand-new connection.
func ComposeCreateSummary(in SummaryInput) (title, summary string) {
	title = "Squadron recommendation: " + in.RecommendationKind

	var b strings.Builder
	fmt.Fprintf(&b, "**Squadron recommendation: %s**\n\n", in.RecommendationKind)

	fmt.Fprintf(&b,
		"This PR was opened by Squadron based on a discovery scan of\naccount %s, region %s, connection %s.\n\n",
		in.AccountID, in.Region, in.ConnectionID)

	b.WriteString("**What this PR does**\n")
	b.WriteString(sanitizeReasoning(in.RecommendationReason))
	b.WriteString("\n\n")

	if hasAnyVerdicts(in.VerdictsByState) {
		writeVerdictContext(&b, in.VerdictsByState)
	}

	if link := viewInSquadronLink(in); link != "" {
		b.WriteString(link)
		b.WriteString("\n")
	}
	return title, b.String()
}

// ComposeUpdateSummary is the analog for the check-run UPDATE path
// on PR merge / close / operator-exclude. Chunk 3+ wires this in;
// shipping it now alongside ComposeCreateSummary keeps the package
// coherent and avoids the follow-on slice having to grow this
// package separately.
//
// Title: "Squadron recommendation: <kind> — <SUCCESS|FAILURE|NEUTRAL>".
// conclusion is one of iacgithub.CheckRunConclusion* — the package
// uppercases it for the title.
func ComposeUpdateSummary(in SummaryInput, conclusion string) (title, summary string) {
	title = fmt.Sprintf("Squadron recommendation: %s — %s",
		in.RecommendationKind, strings.ToUpper(conclusion))

	var b strings.Builder
	fmt.Fprintf(&b, "**Squadron recommendation: %s** — %s\n\n",
		in.RecommendationKind, strings.ToUpper(conclusion))
	fmt.Fprintf(&b,
		"Squadron's check run for account %s, region %s, connection %s.\n\n",
		in.AccountID, in.Region, in.ConnectionID)

	b.WriteString("**What this PR does**\n")
	b.WriteString(sanitizeReasoning(in.RecommendationReason))
	b.WriteString("\n\n")

	if hasAnyVerdicts(in.VerdictsByState) {
		writeVerdictContext(&b, in.VerdictsByState)
	}

	if link := viewInSquadronLink(in); link != "" {
		b.WriteString(link)
		b.WriteString("\n")
	}
	return title, b.String()
}

// hasAnyVerdicts is the cold-start gate. Cold-start (nil map OR
// every bucket empty) suppresses the entire Verdict learning
// context section. See §13 acceptance test 12.
func hasAnyVerdicts(m map[string][]string) bool {
	for _, v := range m {
		if len(v) > 0 {
			return true
		}
	}
	return false
}

// writeVerdictContext emits the three §9.1 bullets. The bullets
// fire only when their per-state bucket is non-empty; bullets with
// empty buckets are omitted entirely so the section reads as
// "informed by <only what we have>" rather than "informed by 0".
func writeVerdictContext(b *strings.Builder, byState map[string][]string) {
	b.WriteString("**Verdict learning context**\n")

	if merged := byState[VerdictStateMerged]; len(merged) > 0 {
		fmt.Fprintf(b, "- Informed by %d prior accepted PRs in this scope: %s\n",
			len(merged), formatPRURLList(merged))
	}
	if closed := byState[VerdictStateClosedNotMerged]; len(closed) > 0 {
		fmt.Fprintf(b, "- Informed by %d prior closed-without-merge PRs in this scope: %s\n",
			len(closed), formatPRURLList(closed))
	}
	if excluded := byState[VerdictStateOperatorExcluded]; len(excluded) > 0 {
		fmt.Fprintf(b, "- %d operator-set exclusions in this scope (filtered before this scan)\n",
			len(excluded))
	}
	b.WriteString("\n")
}

// prURLNumberRE matches the GitHub PR URL shape so we can derive
// the "#<N>" link text. PR URLs in the verdict_examples_used_by_state
// map came from the audit payload (chunk 6) — they were validated by
// the webhook receiver, NOT typed by an operator, so we trust the
// shape but still defensively pattern-match.
var prURLNumberRE = regexp.MustCompile(`/pull/(\d+)(?:[/?#]|$)`)

// formatPRURLList renders a list of GitHub PR URLs as a
// comma-separated set of "[#N](url)" markdown links. URLs that don't
// match the expected pull-request pattern fall back to a bare
// "[link](url)" form so the summary still renders.
//
// The URLs themselves are validated GitHub URLs from the audit
// payload (per §12.4) — no escape needed on the href — but the
// link text always carries the PR number so the reader gets the
// most signal-dense possible representation.
func formatPRURLList(urls []string) string {
	out := make([]string, 0, len(urls))
	for _, raw := range urls {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		// Defensive: skip clearly-malformed URLs rather than dropping
		// them in as bare text.
		if _, err := url.Parse(trimmed); err != nil {
			continue
		}
		text := "#?"
		if m := prURLNumberRE.FindStringSubmatch(trimmed); len(m) == 2 {
			text = "#" + m[1]
		}
		out = append(out, fmt.Sprintf("[%s](%s)", text, trimmed))
	}
	return strings.Join(out, ", ")
}

// viewInSquadronLink builds the "View in Squadron" deep link or
// empty-string when SquadronHost is unset. The link targets the
// discovery recommendations page anchored on the recommendation_id
// when both are present.
func viewInSquadronLink(in SummaryInput) string {
	host := strings.TrimSpace(in.SquadronHost)
	if host == "" {
		return ""
	}
	host = strings.TrimRight(host, "/")
	conn := strings.TrimSpace(in.ConnectionID)
	if conn == "" {
		return fmt.Sprintf("[View in Squadron](%s)", host)
	}
	link := fmt.Sprintf("%s/discovery/aws/%s/recommendations", host, conn)
	if rid := strings.TrimSpace(in.RecommendationID); rid != "" {
		link += "#" + rid
	}
	return fmt.Sprintf("[View in Squadron](%s)", link)
}

// sanitizeReasoning is the markdown-injection escape pass per §12.2
// of the design doc. Four steps in order:
//
//  1. Strip image markdown via stripImageMarkdownRE. §12.2 names
//     `![alt](http://exfil/...)` as an exfiltration vector even
//     though GitHub does not auto-load images in check-run
//     summaries; belt-and-suspenders.
//
//  2. Strip raw HTML via stripHTMLRE. GitHub disallows raw HTML in
//     check-run summaries already; belt-and-suspenders. This MUST
//     run BEFORE RedactSecrets — the redactor's `<redacted:X>`
//     placeholders are tag-shaped, and a strip-after-redact pass
//     would excise them along with the operator-supplied HTML.
//
//  3. ai.RedactSecrets strips the documented token-shaped patterns
//     (anthropic / openai / github tokens etc). Runs AFTER the HTML
//     strip but BEFORE escapeMarkdown so the placeholder's `<` and
//     `>` get escaped along with the rest.
//
//  4. escapeMarkdown handles the remaining markdown special
//     characters so leftover `*` / `_` / “ ` “ / `[]()` etc
//     render as literal characters.
func sanitizeReasoning(s string) string {
	if s == "" {
		return "_(no reasoning provided)_"
	}
	s = stripImageMarkdownRE.ReplaceAllString(s, "")
	s = stripHTMLRE.ReplaceAllString(s, "")
	s = ai.RedactSecrets(s)
	s = escapeMarkdown(s)
	return s
}

// stripImageMarkdownRE matches the `![alt](url)` markdown image
// syntax. Anchored on the literal `!` so it does NOT match plain
// `[text](url)` link syntax — links survive the strip (escaped via
// escapeMarkdown).
var stripImageMarkdownRE = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)

// stripHTMLRE matches any `<...>` tag-shaped sequence so raw HTML
// is excised before escapeMarkdown ever sees it. The pattern is
// conservative — it will excise text that happens to look like a
// tag (e.g. "a < b > c") but that's the right tradeoff against
// XSS injection.
var stripHTMLRE = regexp.MustCompile(`<[^>]+>`)

// escapeMarkdown rewrites the documented markdown special
// characters as their backslash-escaped forms. The set comes from
// the brief: `*`, `_`, backtick, `[`, `]`, `(`, `)`, `<`, `>`,
// backslash. The replacer rewrites in a single pass.
//
// Order: backslash MUST come first in the replacer pairs so the
// subsequent replacements' newly-emitted backslashes don't get
// double-escaped on a second pass.
func escapeMarkdown(s string) string {
	return markdownEscapeReplacer.Replace(s)
}

var markdownEscapeReplacer = strings.NewReplacer(
	`\`, `\\`,
	"*", `\*`,
	"_", `\_`,
	"`", "\\`",
	"[", `\[`,
	"]", `\]`,
	"(", `\(`,
	")", `\)`,
	"<", `\<`,
	">", `\>`,
)
