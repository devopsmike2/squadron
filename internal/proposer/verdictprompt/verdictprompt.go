// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package verdictprompt renders the shared verdict block in the
// proposer prompt. It is the prompt-side companion to verdictsel:
// callers from each surface pass the curated approved+rejected slices
// (already produced by verdictsel.Select) plus surface-specific
// header and instruction copy; Render returns the prompt block text
// or "" when both slices are empty.
//
// See docs/proposals/531-proposer-learning-slice2.md §7 for the line
// stanza format and the surface-specific header / instruction copy.
package verdictprompt

import (
	"fmt"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/proposer/verdictsel"
)

// Surface tags the rendered output for surface-specific identifier
// formatting in the `reference:` line.
type Surface int

const (
	// SurfaceCostSpike formats `reference:` as `rollout_id=<ID>`
	// regardless of state. Used by the #531 cost-spike proposer.
	SurfaceCostSpike Surface = iota
	// SurfaceDiscovery selects the discovery-side reference shape:
	// pr_url= for merged, pr_closed= for closed_not_merged,
	// operator_excluded=<date> for operator_excluded. Used by the
	// #643 discovery proposer.
	SurfaceDiscovery
)

// RenderOpts carries the surface-specific copy that varies between
// cost-spike and discovery callers. The default headers and
// instruction tails are exposed as package-level constants
// (CostSpikeHeader, CostSpikeInstructionTail, DiscoveryHeader,
// DiscoveryInstructionTail) so callers can use them verbatim or
// override.
type RenderOpts struct {
	Surface         Surface
	Now             time.Time // for the relative `when:` lines
	Header          string    // appears once at top of block
	InstructionTail string    // appears once at bottom
}

// CostSpikeHeader is the §7.2 cost-spike header line.
const CostSpikeHeader = "Prior verdicts for this group (operator decisions on past AI proposals):"

// CostSpikeInstructionTail is the §7.2 cost-spike instruction tail.
// Load-bearing: tells the model to use as signal, NOT to cite
// rollout IDs as evidence.
const CostSpikeInstructionTail = "Use these as preference signal. Match the shape of approved\n" +
	"proposals; avoid the shape of rejected ones. Do NOT cite these\n" +
	"rollout_ids in your evidence — they're operator history, not\n" +
	"evidence for this spike."

// DiscoveryHeader is the §7.2 discovery header line.
const DiscoveryHeader = "Recent operator decisions on past Squadron recommendations for this scope:"

// DiscoveryInstructionTail is the §7.2 discovery instruction tail.
// Load-bearing: tells the model how to interpret closed_not_merged
// (rejected signal with recovery affordance) and operator_excluded
// (drop the entire kind).
const DiscoveryInstructionTail = "Use these as preference signal. Do NOT re-propose the same\n" +
	"kind+resource against the same scope that the operator has\n" +
	"already accepted or explicitly excluded within the window. For\n" +
	"closed-without-merge entries, the operator engaged but\n" +
	"declined this specific shape — you may propose a different\n" +
	"variation if you have evidence; cite the divergence in your\n" +
	"reasoning. If the operator-excluded entry applies to this\n" +
	"scope, drop the entire kind from your recommendations."

// reasonMaxChars bounds the per-stanza `reason:` field after
// redaction. Long reasons are truncated with a trailing ellipsis so
// the prompt body stays inside the model's effective attention
// budget on operators with verbose reasoning history.
const reasonMaxChars = 240

// Render returns the §7 prompt block. Empty pool (both slices
// empty) returns "" — the caller drops the entire block, preserving
// slice 1 cold-start parity.
//
// Block layout:
//
//	<Header>
//
//	<rejected stanzas...>
//	<approved stanzas...>
//	<InstructionTail>
func Render(approved, rejected []verdictsel.Verdict, opts RenderOpts) string {
	if len(approved) == 0 && len(rejected) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(opts.Header)
	b.WriteString("\n\n")
	for _, v := range rejected {
		writeStanza(&b, v, opts)
		b.WriteString("\n")
	}
	for _, v := range approved {
		writeStanza(&b, v, opts)
		b.WriteString("\n")
	}
	b.WriteString(opts.InstructionTail)
	return b.String()
}

// writeStanza emits the 4-line stanza for a single verdict.
func writeStanza(b *strings.Builder, v verdictsel.Verdict, opts RenderOpts) {
	label := stateLabel(v.State)
	fmt.Fprintf(b, "[%s] kind=%s\n", label, v.Kind)
	fmt.Fprintf(b, "  reference: %s\n", referenceLine(v, opts.Surface))
	fmt.Fprintf(b, "  when: %s\n", whenLine(v, opts.Now))
	fmt.Fprintf(b, "  reason: %s\n", reasonField(v.Body))
}

// stateLabel maps the State* constants to their human-readable
// bracket labels. StateMerged maps to ACCEPTED to preserve the
// slice 1 discovery-side label semantics.
func stateLabel(state string) string {
	switch state {
	case verdictsel.StateApproved:
		return "APPROVED"
	case verdictsel.StateRejected:
		return "REJECTED"
	case verdictsel.StateMerged:
		return "ACCEPTED"
	case verdictsel.StateClosedNotMerged:
		return "CLOSED_NOT_MERGED"
	case verdictsel.StateOperatorExcluded:
		return "OPERATOR_EXCLUDED"
	default:
		return strings.ToUpper(state)
	}
}

// referenceLine emits the surface-specific identifier line.
func referenceLine(v verdictsel.Verdict, surface Surface) string {
	switch surface {
	case SurfaceCostSpike:
		return "rollout_id=" + v.ID
	case SurfaceDiscovery:
		switch v.State {
		case verdictsel.StateMerged:
			return "pr_url=" + v.ID
		case verdictsel.StateClosedNotMerged:
			return "pr_closed=" + v.ID
		case verdictsel.StateOperatorExcluded:
			return "operator_excluded=" + v.Timestamp.UTC().Format("2006-01-02")
		default:
			// Defensive: any non-discovery-shaped state falls
			// through to a generic id= line so the prompt block
			// still renders rather than emitting empty.
			return "id=" + v.ID
		}
	default:
		return "id=" + v.ID
	}
}

// whenLine builds "<verb> <relative>" for the verdict, e.g.
// "approved 2 days ago".
func whenLine(v verdictsel.Verdict, now time.Time) string {
	return stateVerb(v.State) + " " + relative(v.Timestamp, now)
}

// stateVerb maps the State* constants to the past-tense verb used
// in the `when:` line.
func stateVerb(state string) string {
	switch state {
	case verdictsel.StateApproved:
		return "approved"
	case verdictsel.StateRejected:
		return "rejected"
	case verdictsel.StateMerged:
		return "merged"
	case verdictsel.StateClosedNotMerged:
		return "closed"
	case verdictsel.StateOperatorExcluded:
		return "excluded"
	default:
		return state
	}
}

// relative renders the difference between t and now as a coarse
// human-readable phrase: "today" / "yesterday" / "<N> days ago" /
// "<N> weeks ago". The phrasing matches the §7 spec examples and is
// deliberately coarse — the model reads recency, not exact deltas.
func relative(t, now time.Time) string {
	delta := now.Sub(t)
	if delta < 0 {
		delta = -delta
	}
	day := 24 * time.Hour
	days := int(delta / day)
	switch {
	case days <= 0:
		return "today"
	case days == 1:
		return "yesterday"
	case days < 14:
		return fmt.Sprintf("%d days ago", days)
	default:
		weeks := days / 7
		return fmt.Sprintf("%d weeks ago", weeks)
	}
}

// reasonField applies ai.RedactSecrets to the verdict Body and
// truncates to reasonMaxChars. An empty body (or one that redacts
// to empty) yields "(no reason given)".
func reasonField(body string) string {
	redacted := ai.RedactSecrets(body)
	redacted = strings.TrimSpace(redacted)
	if redacted == "" {
		return "(no reason given)"
	}
	if len(redacted) > reasonMaxChars {
		// Reserve 3 chars for the ellipsis so the total line stays
		// at the documented bound.
		return redacted[:reasonMaxChars-3] + "..."
	}
	return redacted
}
