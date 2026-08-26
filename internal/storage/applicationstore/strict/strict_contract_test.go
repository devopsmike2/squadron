// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package strict_test

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/strict"

	// Blank-import every application-store backend so its init() registers its
	// strict toggler. This is the whole point of the contract: a backend that
	// forgets to register (a new, un-armable seam — the ADR 0043 bug) makes the
	// assertion below fail, so it can never ship silently.
	_ "github.com/devopsmike2/squadron/internal/storage/applicationstore/postgres"
	_ "github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// wantBackends is every application-store backend that MUST arm strict tenant
// scoping under the enterprise wire. Add a backend here when you add one to the
// tree; if its package forgot to call strict.Register in init(), this test fails.
var wantBackends = []string{"postgres", "sqlite"}

func TestStrictRegistry_EveryBackendRegistered(t *testing.T) {
	got := strict.Registered()
	set := map[string]bool{}
	for _, n := range got {
		set[n] = true
	}
	for _, want := range wantBackends {
		if !set[want] {
			t.Fatalf("backend %q did not register a strict-scoping toggler (registered: %v). "+
				"Every store backend must call strict.Register in its init() so the enterprise "+
				"wire's strict.EnableAll() arms it — an un-armed backend is the ADR 0043 CVE.", want, got)
		}
	}
}

// TestStrictRegistry_EnableDisableAll proves EnableAll()/DisableAll() drive the
// registered togglers. It flips every backend on, then off, and relies on the
// backends' own strict-flag tests for the per-backend behavior; here we only
// assert EnableAll/DisableAll do not panic and are callable once armed.
func TestStrictRegistry_EnableDisableAll(t *testing.T) {
	strict.EnableAll()
	strict.DisableAll() // restore OFF so sibling tests see the OSS default
}
