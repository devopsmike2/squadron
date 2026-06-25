// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package configdiff computes line-level diffs between two YAML config
// strings. It's a thin wrapper around go-difflib that returns both the
// rendered unified-diff text (for display) and structured line counts
// (for audit-log summaries and "X lines added, Y removed" badges).
//
// Lives in its own package because the rollout preview, the rollout
// detail view, and post-mortem rendering all want the same diff
// summary shape — and keeping the difflib import in one place makes it
// easier to swap the underlying engine later (e.g. for a token-level
// diff) without touching every caller.
package configdiff

import (
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Result is the diff between two config strings.
//
// Unified is the rfc-style unified-diff body, suitable for display in
// a monospace viewer. Added/Removed count the +/- lines (excluding the
// context lines and the @@ hunk headers); they're what the UI shows in
// the "X lines added, Y lines removed" badge and what the audit log
// stores as a fingerprint of how big this rollout's change is.
//
// Identical reports whether the inputs were byte-identical (after a
// trailing-newline normalization). A no-op rollout is rarely useful but
// not invalid; the UI surfaces it as a warning rather than blocking.
type Result struct {
	Unified   string `json:"unified"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Identical bool   `json:"identical"`
}

// Diff renders a unified diff with three lines of context on each side.
// Three lines is a common default and is enough that an operator can
// spot which YAML block a change lives in without dumping the whole
// file. Increase later if review feedback says it's too tight.
//
// Empty inputs are valid (a brand-new group has no current config). An
// empty current with a non-empty target shows up as "everything is new";
// an empty target is unusual but not a crash case.
func Diff(current, target string) Result {
	// Normalize trailing newlines so a YAML file that's missing its
	// terminator doesn't render as a phantom "missing newline at end of
	// file" diff line. Operators rarely fix that and the noise drowns
	// out real changes.
	currentNorm := normalize(current)
	targetNorm := normalize(target)

	if currentNorm == targetNorm {
		return Result{Identical: true}
	}

	// Special-case an empty current: render the whole target as
	// additions. SplitLines on an empty string returns [""] which
	// difflib treats as a single "removed empty line", inflating the
	// removed count by one for every empty-current diff — confusing
	// when the operator sees "1 line removed" for a brand-new group.
	if currentNorm == "" {
		lines := difflib.SplitLines(targetNorm)
		var b strings.Builder
		b.WriteString("--- current\n+++ target\n@@ -0,0 +1,")
		b.WriteString(itoa(len(lines)))
		b.WriteString(" @@\n")
		for _, l := range lines {
			b.WriteString("+")
			b.WriteString(l)
			if !strings.HasSuffix(l, "\n") {
				b.WriteString("\n")
			}
		}
		return Result{
			Unified: b.String(),
			Added:   len(lines),
			Removed: 0,
		}
	}

	d := difflib.UnifiedDiff{
		A:        difflib.SplitLines(currentNorm),
		B:        difflib.SplitLines(targetNorm),
		FromFile: "current",
		ToFile:   "target",
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(d)
	if err != nil {
		// difflib can't actually fail with these inputs (no I/O), but
		// returning the error would make every caller handle a
		// no-real-error case. Fall back to an empty unified body and
		// still surface the line counts so the caller has SOMETHING
		// useful.
		text = ""
	}

	added, removed := countChanges(text)
	return Result{
		Unified: text,
		Added:   added,
		Removed: removed,
	}
}

// normalize strips the trailing newline if present so files that differ
// only by trailing-whitespace are treated as identical.
func normalize(s string) string {
	return strings.TrimRight(s, "\n")
}

// itoa is a tiny helper to avoid importing strconv just for one call
// site in the empty-current path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	if neg {
		out = "-" + out
	}
	return out
}

// countChanges walks a unified-diff body and returns the +/- line
// counts. Skips the file header lines ("--- current", "+++ target") and
// the hunk headers ("@@ ..."). Lines that start with " " (context),
// "\\" (no-newline marker), or anything else are ignored.
func countChanges(unified string) (added, removed int) {
	for _, line := range strings.Split(unified, "\n") {
		// File headers are "+++ target" and "--- current". They start
		// with the change markers but they're metadata, not content
		// edits. Skip by length + prefix.
		if strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			continue
		}
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}
