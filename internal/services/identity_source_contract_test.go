// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "testing"

// identity_source_contract_test.go — the editions-contract for ADR 0014 Arc C
// slice 4a's OSS-inert pieces in this package: the scim:read / scim:write scopes
// and the SetStrictIdentitySource toggle. These lock the OSS side so a
// regression fails a test rather than silently changing edition behavior.

// TestSCIMScopes_InInventory pins that scim:read / scim:write are grantable
// scopes (present in AllScopes + IsValidScope). Slice 4c guards the SCIM routes
// with RequireScope("scim:write"), so a token can carry them; the constants and
// the inventory entry are the OSS-inert prerequisite (defining a scope changes
// nothing until a route requires it, and OSS mounts no SCIM routes).
func TestSCIMScopes_InInventory(t *testing.T) {
	all := AllScopes()
	want := map[string]bool{ScopeSCIMRead: false, ScopeSCIMWrite: false}
	for _, s := range all {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for scope, found := range want {
		if !found {
			t.Errorf("AllScopes() is missing %q", scope)
		}
		if !IsValidScope(scope) {
			t.Errorf("IsValidScope(%q) = false, want true", scope)
		}
	}
	if ScopeSCIMRead != "scim:read" {
		t.Errorf("ScopeSCIMRead = %q, want %q", ScopeSCIMRead, "scim:read")
	}
	if ScopeSCIMWrite != "scim:write" {
		t.Errorf("ScopeSCIMWrite = %q, want %q", ScopeSCIMWrite, "scim:write")
	}
}

// TestStrictIdentitySource_OSSDefaultInert pins the OSS default: nothing calls
// SetStrictIdentitySource, so the backing var is false — the eventual slice-4d
// identity-source check is inert. Flipping it (enterprise) and back must be
// observable through the getter, but OSS never flips it.
func TestStrictIdentitySource_OSSDefaultInert(t *testing.T) {
	if StrictIdentitySource() {
		t.Fatal("StrictIdentitySource() = true at OSS default, want false (toggle is inert until the enterprise wire flips it)")
	}

	// Prove the toggle is a real setter (the enterprise wire path), then
	// restore the OSS-inert default so other tests see a clean state.
	t.Cleanup(func() { SetStrictIdentitySource(false) })
	SetStrictIdentitySource(true)
	if !StrictIdentitySource() {
		t.Fatal("StrictIdentitySource() = false after SetStrictIdentitySource(true), want true")
	}
	SetStrictIdentitySource(false)
	if StrictIdentitySource() {
		t.Fatal("StrictIdentitySource() = true after SetStrictIdentitySource(false), want false")
	}
}
