// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "testing"

// TestSSOCrossTenantScope_InInventory pins that sso:cross_tenant is a grantable
// scope (present in AllScopes + IsValidScope). It's the OSS-inert prerequisite
// for the enterprise SSO-directory two-scope check (ADR 0016 + ADR 0020 D3): a
// cross-tenant GET /sso/directory/...?tenant=X requires sso:cross_tenant ON TOP
// of sso:read. Defining the scope changes nothing in OSS (no /sso routes) — this
// locks the constant + inventory so a regression fails a test.
func TestSSOCrossTenantScope_InInventory(t *testing.T) {
	if ScopeSSOCrossTenant != "sso:cross_tenant" {
		t.Fatalf("ScopeSSOCrossTenant = %q, want %q", ScopeSSOCrossTenant, "sso:cross_tenant")
	}
	if !IsValidScope(ScopeSSOCrossTenant) {
		t.Errorf("IsValidScope(%q) = false, want true", ScopeSSOCrossTenant)
	}
	found := false
	for _, s := range AllScopes() {
		if s == ScopeSSOCrossTenant {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AllScopes() is missing %q", ScopeSSOCrossTenant)
	}
}
