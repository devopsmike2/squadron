// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// editions_contract_test.go pins the open-core edition's wiring contract
// (ADR 0006 slice 5). It asserts that the default (OSS) build wires the
// identity seam's OSS-default providers, that the scoped-store wire is an
// identity pass-through, and that the build identity is "squadron-oss". These
// are the guarantees the enterprise edition overrides via its own build-tagged
// wire files; keeping them pinned here means a regression in the OSS wiring
// fails a test instead of silently changing edition behavior.

package main

import (
	"testing"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// TestOSSEdition_IdentityProviders confirms the OSS build wires the identity
// seam's default providers: bearer authenticator, flat-scope authorizer, and
// single-tenant resolver. The enterprise wire returns its own bundle.
func TestOSSEdition_IdentityProviders(t *testing.T) {
	p := identityProviders()
	if _, ok := p.Authenticator.(identity.BearerAuthenticator); !ok {
		t.Errorf("Authenticator = %T, want identity.BearerAuthenticator", p.Authenticator)
	}
	if _, ok := p.Authorizer.(identity.ScopeAuthorizer); !ok {
		t.Errorf("Authorizer = %T, want identity.ScopeAuthorizer", p.Authorizer)
	}
	if _, ok := p.TenantResolver.(identity.SingleTenantResolver); !ok {
		t.Errorf("TenantResolver = %T, want identity.SingleTenantResolver", p.TenantResolver)
	}
}

// TestOSSEdition_ScopedStoreIsPassthrough confirms the OSS scoped-store wire
// returns exactly the store it was given, so the optional store interfaces
// main.go type-asserts (CostSpikeStore, retention GCs, observation readers, …)
// stay intact. A wrapping decorator here would silently disable those features.
func TestOSSEdition_ScopedStoreIsPassthrough(t *testing.T) {
	var s applicationstore.ApplicationStore = memory.NewStore()
	if got := scopedApplicationStore(s); got != s {
		t.Error("scopedApplicationStore must be an identity pass-through in the OSS build")
	}
}

// TestOSSEdition_BuildIdentity confirms the OSS edition build identity is
// "squadron-oss" (the value exposed on /metrics as squadron_build_info).
func TestOSSEdition_BuildIdentity(t *testing.T) {
	if got := wireExtensions(nil); got != "squadron-oss" {
		t.Errorf("build edition = %q, want %q", got, "squadron-oss")
	}
}
