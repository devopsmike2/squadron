// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"testing"
)

// TestScopeAuthorizer_MirrorsHasScope pins the OSS authorizer to the exact
// truth table of internal/services.AuthActor.HasScope, which
// middleware.RequireScope enforces today:
//   - empty scope set  => allow (legacy full-access)
//   - wildcard present => allow
//   - required present => allow
//   - otherwise        => deny
//
// Slice 2 routes the middleware through this authorizer, so any drift here
// would silently change OSS authorization behavior.
func TestScopeAuthorizer_MirrorsHasScope(t *testing.T) {
	cases := []struct {
		name     string
		scopes   []string
		required string
		want     bool
	}{
		{"empty scopes = legacy full access", nil, "rollouts:write", true},
		{"empty slice = legacy full access", []string{}, "agents:read", true},
		{"wildcard grants anything", []string{"*"}, "rollouts:approve", true},
		{"exact scope match", []string{"agents:read", "rollouts:write"}, "rollouts:write", true},
		{"missing scope denied", []string{"agents:read"}, "rollouts:write", false},
		{"non-empty without match or wildcard denied", []string{"configs:read"}, "configs:write", false},
	}

	var az ScopeAuthorizer
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := az.Authorize(context.Background(), Principal{Scopes: tc.scopes}, tc.required, Resource{})
			if d.Allow != tc.want {
				t.Fatalf("Authorize(scopes=%v, required=%q).Allow = %v, want %v",
					tc.scopes, tc.required, d.Allow, tc.want)
			}
			if !d.Allow && d.Reason == "" {
				t.Errorf("denied decision must carry a Reason for audit/debug")
			}
			if d.Allow && d.Reason != "" {
				t.Errorf("allowed decision must not carry a Reason, got %q", d.Reason)
			}
		})
	}
}

// TestScopeAuthorizer_IgnoresResource confirms the OSS authorizer is
// action-level: the same scopes yield the same decision regardless of the
// resource (resource-level authz is an enterprise-reserved capability).
func TestScopeAuthorizer_IgnoresResource(t *testing.T) {
	var az ScopeAuthorizer
	p := Principal{Scopes: []string{"rollouts:write"}}
	a := az.Authorize(context.Background(), p, "rollouts:write", Resource{})
	b := az.Authorize(context.Background(), p, "rollouts:write", Resource{Type: "rollout", ID: "r-123"})
	if a.Allow != b.Allow {
		t.Fatalf("resource must not change an OSS decision: %v vs %v", a.Allow, b.Allow)
	}
}

// TestSingleTenantResolver_AlwaysDefault confirms the OSS resolver is a
// single implicit tenant regardless of context.
func TestSingleTenantResolver_AlwaysDefault(t *testing.T) {
	var r SingleTenantResolver
	if got := r.Resolve(context.Background()); got != DefaultTenant {
		t.Fatalf("Resolve = %q, want %q", got, DefaultTenant)
	}
	// A different context yields the same tenant — the resolver never reads
	// the context (single implicit tenant in OSS).
	if got := r.Resolve(context.TODO()); got != DefaultTenant {
		t.Fatalf("Resolve(other ctx) = %q, want %q", got, DefaultTenant)
	}
}

// TestOSSProviders_WiresDefaults confirms the OSS bundle wires all three
// providers with the expected concrete types. The editions contract (slice
// 5) will assert the OSS binary uses exactly these.
func TestOSSProviders_WiresDefaults(t *testing.T) {
	p := OSSProviders()
	if _, ok := p.Authenticator.(BearerAuthenticator); !ok {
		t.Errorf("Authenticator = %T, want BearerAuthenticator", p.Authenticator)
	}
	if _, ok := p.Authorizer.(ScopeAuthorizer); !ok {
		t.Errorf("Authorizer = %T, want ScopeAuthorizer", p.Authorizer)
	}
	if _, ok := p.TenantResolver.(SingleTenantResolver); !ok {
		t.Errorf("TenantResolver = %T, want SingleTenantResolver", p.TenantResolver)
	}
	if name := p.Authenticator.Name(); name != "bearer" {
		t.Errorf("Authenticator.Name() = %q, want %q", name, "bearer")
	}
}

// TestTenantContext_Roundtrip confirms WithTenant/TenantFromContext carry a
// tenant id through a context.
func TestTenantContext_Roundtrip(t *testing.T) {
	ctx := WithTenant(context.Background(), "acme")
	if got := TenantFromContext(ctx); got != "acme" {
		t.Fatalf("TenantFromContext = %q, want %q", got, "acme")
	}
}

// TestTenantFromContext_DefaultsWhenAbsentOrEmpty confirms a reader always
// gets a usable tenant: DefaultTenant when the context carries none (e.g. a
// background context that never passed through the stamping middleware) or an
// empty string. This keeps single-tenant OSS and later store decorators safe.
func TestTenantFromContext_DefaultsWhenAbsentOrEmpty(t *testing.T) {
	if got := TenantFromContext(context.Background()); got != DefaultTenant {
		t.Fatalf("absent tenant = %q, want %q", got, DefaultTenant)
	}
	if got := TenantFromContext(WithTenant(context.Background(), "")); got != DefaultTenant {
		t.Fatalf("empty tenant = %q, want %q", got, DefaultTenant)
	}
}
