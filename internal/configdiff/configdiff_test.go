// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package configdiff

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDiff_Identical(t *testing.T) {
	r := Diff("foo: bar\n", "foo: bar\n")
	assert.True(t, r.Identical)
	assert.Equal(t, 0, r.Added)
	assert.Equal(t, 0, r.Removed)
	assert.Empty(t, r.Unified)
}

func TestDiff_IdenticalIgnoresTrailingNewline(t *testing.T) {
	// "missing trailing newline" should not count as a diff; the
	// operator can't usually fix that and the noise drowns out the
	// real changes.
	r := Diff("foo: bar", "foo: bar\n")
	assert.True(t, r.Identical)
}

func TestDiff_AddOneLine(t *testing.T) {
	r := Diff("foo: bar\n", "foo: bar\nbaz: qux\n")
	assert.False(t, r.Identical)
	assert.Equal(t, 1, r.Added)
	assert.Equal(t, 0, r.Removed)
	assert.Contains(t, r.Unified, "+baz: qux")
}

func TestDiff_RemoveOneLine(t *testing.T) {
	r := Diff("foo: bar\nbaz: qux\n", "foo: bar\n")
	assert.Equal(t, 0, r.Added)
	assert.Equal(t, 1, r.Removed)
	assert.Contains(t, r.Unified, "-baz: qux")
}

func TestDiff_ReplaceLine_CountsBothSides(t *testing.T) {
	// "Changed one line" looks to difflib like a remove + add. That's
	// the desired wire shape — operators reading "1 added, 1 removed"
	// understand that better than "1 changed".
	r := Diff("foo: 1\n", "foo: 2\n")
	assert.Equal(t, 1, r.Added)
	assert.Equal(t, 1, r.Removed)
}

func TestDiff_HeaderLinesNotCounted(t *testing.T) {
	// The "--- current" / "+++ target" header lines start with - and
	// + but are metadata. The body shouldn't include them in the
	// add/remove counts. We verify by adding many lines and confirming
	// the count matches the actual content edits, not n+2.
	current := "a\nb\nc\n"
	target := "a\nb\nc\nd\ne\n"
	r := Diff(current, target)
	assert.Equal(t, 2, r.Added, "should only count the two new lines, not the header")
}

func TestDiff_EmptyCurrent(t *testing.T) {
	// New group with no current config — target is "everything is
	// new". Useful when the operator's first action against a group
	// is a rollout (rather than a manual config push).
	r := Diff("", "receivers:\n  otlp:\n    protocols:\n      grpc:\n")
	assert.False(t, r.Identical)
	assert.Equal(t, 4, r.Added)
	assert.Equal(t, 0, r.Removed)
}

func TestDiff_UnifiedDiffShape(t *testing.T) {
	// Smoke-check the unified-diff body is well-formed enough for a
	// Monaco diff viewer to consume without complaint.
	r := Diff("foo: 1\nbar: 2\n", "foo: 1\nbar: 3\n")
	require := assert.New(t)
	require.True(strings.Contains(r.Unified, "--- current"), "should have current header")
	require.True(strings.Contains(r.Unified, "+++ target"), "should have target header")
	require.True(strings.Contains(r.Unified, "@@"), "should have at least one hunk header")
}
