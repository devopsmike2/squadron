// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "testing"

// strict_identity_source_test.go — covers the ADR 0014 slice 4d
// IdentitySourceValidated predicate + its inertness. The reserved sets are EMPTY
// in OSS (inert); these tests register them directly to exercise the
// enterprise-side allow-set without an enterprise build, then assert that with
// the strict toggle OFF the whole gate is bypassed.

// TestIdentitySourceValidated_ReservedLabelsPass pins that once the enterprise
// wire has populated the reserved sets, a bootstrap exact label and any
// oidc:/scim: prefixed label count as validated identity sources, while a raw
// operator label (ci-bot) does not. This is the allow-set the strict RequireBearer
// gate consults.
func TestIdentitySourceValidated_ReservedLabelsPass(t *testing.T) {
	t.Cleanup(func() {
		SetReservedTokenLabelPrefixes(nil)
		SetReservedTokenLabels(nil)
	})
	// Mirror the enterprise wire: bootstrap exact-set + oidc:/scim: prefixes.
	SetReservedTokenLabels([]string{"bootstrap"})
	SetReservedTokenLabelPrefixes([]string{"oidc:", "scim:"})

	if !IdentitySourceValidated("bootstrap") {
		t.Errorf("IdentitySourceValidated(bootstrap) = false; want true (exact reserved label)")
	}
	if !IdentitySourceValidated("oidc:alice@example.com") {
		t.Errorf("IdentitySourceValidated(oidc:...) = false; want true (reserved prefix)")
	}
	if !IdentitySourceValidated("scim:ext-123") {
		t.Errorf("IdentitySourceValidated(scim:...) = false; want true (reserved prefix)")
	}
	if IdentitySourceValidated("ci-bot") {
		t.Errorf("IdentitySourceValidated(ci-bot) = true; want false (raw operator label)")
	}
}

// TestIdentitySourceValidated_TracksReservedSubstrate pins the thin-wrapper
// contract: IdentitySourceValidated is exactly IsReservedTokenLabel, so the
// strict predicate can never drift from the reserved-label substrate.
func TestIdentitySourceValidated_TracksReservedSubstrate(t *testing.T) {
	t.Cleanup(func() {
		SetReservedTokenLabelPrefixes(nil)
		SetReservedTokenLabels(nil)
	})
	SetReservedTokenLabels([]string{"bootstrap"})
	SetReservedTokenLabelPrefixes([]string{"oidc:"})

	for _, label := range []string{"bootstrap", "Bootstrap", "oidc:x", "ci-bot", ""} {
		if got, want := IdentitySourceValidated(label), IsReservedTokenLabel(label); got != want {
			t.Errorf("IdentitySourceValidated(%q) = %v; want %v (must equal IsReservedTokenLabel)", label, got, want)
		}
	}
}

// TestIdentitySourceValidated_InertWhenReservedEmpty pins the OSS default: with
// no reserved sets registered, nothing is a validated identity source. Combined
// with StrictIdentitySource() being false in OSS, the RequireBearer gate is
// bypassed entirely — a raw label authenticates (proven at the middleware level
// in TestRequireBearer_StrictOff_RawTokenAuthenticates).
func TestIdentitySourceValidated_InertWhenReservedEmpty(t *testing.T) {
	t.Cleanup(func() {
		SetReservedTokenLabelPrefixes(nil)
		SetReservedTokenLabels(nil)
	})
	SetReservedTokenLabelPrefixes(nil)
	SetReservedTokenLabels(nil)

	for _, label := range []string{"bootstrap", "oidc:x", "scim:y", "ci-bot"} {
		if IdentitySourceValidated(label) {
			t.Errorf("IdentitySourceValidated(%q) = true with empty reserved sets; want false (inert)", label)
		}
	}
	// And the strict toggle itself is off by default in OSS.
	if StrictIdentitySource() {
		t.Errorf("StrictIdentitySource() = true in OSS default; want false")
	}
}
