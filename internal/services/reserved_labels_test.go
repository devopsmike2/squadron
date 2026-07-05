// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "testing"

// reserved_labels_test.go — covers the ADR 0014 D9 reserved-label PREFIX
// defense layered on top of the existing exact whole-string match. The prefix
// set is EMPTY in OSS (inert); these tests register it directly to exercise the
// enterprise-side behavior without an enterprise build.

// TestReservedLabelPrefix_Rejects pins that once a prefix (e.g. `oidc:`) is
// registered, any label carrying it is reserved — this closes the D9 hole where
// an auth:write holder could POST a `oidc:foo` label through the public handler
// and forge an OIDC identity.
func TestReservedLabelPrefix_Rejects(t *testing.T) {
	t.Cleanup(func() {
		SetReservedTokenLabelPrefixes(nil)
		SetReservedTokenLabels(nil)
	})
	SetReservedTokenLabelPrefixes([]string{"oidc:", "scim:"})

	cases := []struct {
		label string
		want  bool
	}{
		{"oidc:foo", true},
		{"OIDC:Foo", true}, // case-insensitive
		{"  oidc:bar  ", true},
		{"scim:ext-1", true},
		{"oidc:", true},
		{"team-a", false},
		{"nonoidc:foo", false}, // prefix must be at the start
	}
	for _, tc := range cases {
		if got := IsReservedTokenLabel(tc.label); got != tc.want {
			t.Errorf("IsReservedTokenLabel(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
}

// TestReservedLabelExact_StillWorks pins that the pre-existing exact-match path
// (the bootstrap break-glass label) is unaffected by adding the prefix set.
func TestReservedLabelExact_StillWorks(t *testing.T) {
	t.Cleanup(func() {
		SetReservedTokenLabelPrefixes(nil)
		SetReservedTokenLabels(nil)
	})
	SetReservedTokenLabels([]string{"bootstrap"})
	SetReservedTokenLabelPrefixes([]string{"oidc:"})

	if !IsReservedTokenLabel("bootstrap") {
		t.Errorf("exact bootstrap label should be reserved")
	}
	if !IsReservedTokenLabel("Bootstrap") {
		t.Errorf("exact bootstrap label should match case-insensitively")
	}
	if !IsReservedTokenLabel("oidc:x") {
		t.Errorf("prefix and exact sets should both apply")
	}
}

// TestReservedLabelPrefix_InertWhenEmpty pins the OSS default: with no prefixes
// (and no exact labels) registered, nothing is reserved — the seam is inert.
func TestReservedLabelPrefix_InertWhenEmpty(t *testing.T) {
	t.Cleanup(func() {
		SetReservedTokenLabelPrefixes(nil)
		SetReservedTokenLabels(nil)
	})
	SetReservedTokenLabelPrefixes(nil)
	SetReservedTokenLabels(nil)

	for _, l := range []string{"oidc:foo", "scim:bar", "bootstrap", "anything"} {
		if IsReservedTokenLabel(l) {
			t.Errorf("IsReservedTokenLabel(%q) = true with empty sets; want inert", l)
		}
	}
}
