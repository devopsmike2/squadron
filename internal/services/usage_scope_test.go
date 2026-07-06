// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "testing"

// TestUsageScopes_InInventory pins usage:read / usage:cross_tenant as grantable
// scopes (AllScopes + IsValidScope). Inert in OSS (no /usage routes); the
// enterprise per-tenant usage handler enforces them (usage:read own-tenant,
// + usage:cross_tenant for another tenant — the ADR 0020 D3 two-scope pattern).
func TestUsageScopes_InInventory(t *testing.T) {
	if ScopeUsageRead != "usage:read" || ScopeUsageCrossTenant != "usage:cross_tenant" {
		t.Fatalf("scope constants wrong: %q / %q", ScopeUsageRead, ScopeUsageCrossTenant)
	}
	all := AllScopes()
	for _, want := range []string{ScopeUsageRead, ScopeUsageCrossTenant} {
		if !IsValidScope(want) {
			t.Errorf("IsValidScope(%q) = false", want)
		}
		found := false
		for _, s := range all {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("AllScopes() missing %q", want)
		}
	}
}
