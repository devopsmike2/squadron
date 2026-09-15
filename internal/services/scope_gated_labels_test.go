// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "testing"

// TestScopeGatedTokenLabelPrefixes_InertByDefault asserts the OSS default: no
// gated prefixes registered, so every label reports ungated (the public handler
// imposes no extra scope check).
func TestScopeGatedTokenLabelPrefixes_InertByDefault(t *testing.T) {
	SetScopeGatedTokenLabelPrefixes(nil)
	t.Cleanup(func() { SetScopeGatedTokenLabelPrefixes(nil) })

	for _, label := range []string{"pin:fleet-1", "oidc:sub", "ops", ""} {
		if scope, gated := RequiredScopeForTokenLabel(label); gated {
			t.Errorf("label %q: gated=%v scope=%q, want ungated in OSS default", label, gated, scope)
		}
	}
}

// TestScopeGatedTokenLabelPrefixes_Match asserts a registered prefix gates the
// matching labels (case-insensitively, prefix not whole-string) and returns the
// required scope, while non-matching labels stay ungated.
func TestScopeGatedTokenLabelPrefixes_Match(t *testing.T) {
	SetScopeGatedTokenLabelPrefixes(map[string]string{"pin:": ScopeAgentsWrite})
	t.Cleanup(func() { SetScopeGatedTokenLabelPrefixes(nil) })

	gated := []string{"pin:fleet-1", "PIN:fleet-2", "  pin:trimmed  "}
	for _, label := range gated {
		scope, ok := RequiredScopeForTokenLabel(label)
		if !ok {
			t.Errorf("label %q: want gated, got ungated", label)
			continue
		}
		if scope != ScopeAgentsWrite {
			t.Errorf("label %q: scope=%q, want %q", label, scope, ScopeAgentsWrite)
		}
	}

	for _, label := range []string{"ops", "oidc:sub", "pinned-but-no-colon", ""} {
		if scope, ok := RequiredScopeForTokenLabel(label); ok {
			t.Errorf("label %q: gated with scope %q, want ungated", label, scope)
		}
	}
}

// TestScopeGatedTokenLabelPrefixes_ClearAndSkipEmpty asserts empty/whitespace
// prefix or scope entries are dropped, and a nil argument clears the map.
func TestScopeGatedTokenLabelPrefixes_ClearAndSkipEmpty(t *testing.T) {
	SetScopeGatedTokenLabelPrefixes(map[string]string{
		"pin:": ScopeAgentsWrite,
		"":     ScopeAgentsWrite, // dropped: empty prefix
		"foo:": "",               // dropped: empty scope
	})
	t.Cleanup(func() { SetScopeGatedTokenLabelPrefixes(nil) })

	if _, ok := RequiredScopeForTokenLabel("foo:bar"); ok {
		t.Error("foo: had an empty required scope and must be dropped (ungated)")
	}
	if _, ok := RequiredScopeForTokenLabel("pin:x"); !ok {
		t.Error("pin: should remain gated")
	}

	SetScopeGatedTokenLabelPrefixes(nil)
	if _, ok := RequiredScopeForTokenLabel("pin:x"); ok {
		t.Error("nil argument must clear the gated map")
	}
}
