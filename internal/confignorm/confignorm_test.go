// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package confignorm

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"crlf folded to lf", "a:\r\n  b: 1\r\n", "a:\n  b: 1"},
		{"trailing newlines trimmed", "a: 1\n\n\n", "a: 1"},
		{"leading blank line trimmed", "\n\na: 1", "a: 1"},
		{"trailing spaces trimmed", "a: 1   ", "a: 1"},
		{"leading spaces trimmed", "   a: 1", "a: 1"},
		{"interior indentation preserved", "a:\n  b: 1\n  c: 2", "a:\n  b: 1\n  c: 2"},
		{"whitespace-only becomes empty", "  \n\t\n  ", ""},
		{"already canonical unchanged", "a: 1\nb: 2", "a: 1\nb: 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalize_WhitespaceOnlyDifferencesCollapse is the core invariant the
// preview diff and drift detection both depend on: two configs that differ only
// by surrounding whitespace must normalize to the SAME string, so the preview
// can't show a change that drift will immediately call "synced".
func TestNormalize_WhitespaceOnlyDifferencesCollapse(t *testing.T) {
	base := "receivers:\n  otlp: {}"
	variants := []string{
		"\nreceivers:\n  otlp: {}",           // leading blank line
		"receivers:\n  otlp: {}\n",           // trailing newline
		"receivers:\n  otlp: {}   ",          // trailing spaces
		"  receivers:\n  otlp: {}",           // leading spaces
		"receivers:\r\n  otlp: {}\r\n",       // CRLF
		"\r\n\r\nreceivers:\r\n  otlp: {}\n", // mixed
	}
	want := Normalize(base)
	for _, v := range variants {
		if got := Normalize(v); got != want {
			t.Fatalf("Normalize(%q) = %q, want it to equal Normalize(base) = %q", v, got, want)
		}
	}
}

func TestHash(t *testing.T) {
	// Empty / whitespace-only content hashes to the empty sentinel, never to a
	// real digest — so "no effective config" can't collide with a content match.
	for _, empty := range []string{"", "   ", "\n\t\n"} {
		if got := Hash(empty); got != "" {
			t.Fatalf("Hash(%q) = %q, want empty sentinel", empty, got)
		}
	}

	// Non-empty content is a deterministic, lowercase-hex, 64-char SHA-256.
	h := Hash("a: 1")
	if len(h) != 64 {
		t.Fatalf("Hash length = %d, want 64", len(h))
	}
	if h != Hash("a: 1\n") || h != Hash("\r\na: 1  ") {
		t.Fatalf("Hash not stable across incidental-whitespace variants")
	}
	if Hash("a: 1") == Hash("a: 2") {
		t.Fatalf("Hash collision on distinct content")
	}
}
