// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"

	"github.com/devopsmike2/squadron/extension/identity"
)

// tenant_scope.go — the ADR 0011 M2 (query-layer scoping) seam for the Postgres
// application store, a faithful mirror of the sqlite backend's tenant_scope.go.
//
// The Postgres port is OSS single-tenant: most entities (agents, configs,
// rollouts, saved queries, alert rules) deliberately omit tenant scoping
// because their types carry no tenant field. The AUDIT chain is the exception —
// its hash covers the tenant string and its seq is per-tenant — so the audit
// methods resolve the tenant EXACTLY the way the sqlite backend does (through
// tenantScope, falling back to identity.DefaultTenant on an unstamped context).
// This keeps append + verify byte-identical across the two backends and gives
// per-tenant chain isolation.
//
// In the OSS build this is INERT: nothing calls SetStrictTenantScoping, so
// strictTenantScoping stays false and every unstamped context lands on
// identity.DefaultTenant ("default"), which is also what every row is written
// with — so the predicate is a no-op and behavior matches the sqlite store.

// strictTenantScoping gates the fail-fast behavior on an unstamped context.
// Defaults to false; the enterprise wire flips it true at startup. OSS never
// calls SetStrictTenantScoping, leaving the legacy DefaultTenant fallback.
var strictTenantScoping bool

// SetStrictTenantScoping toggles strict tenant scoping. Mirrors the sqlite
// backend's identically-named seam; OSS never calls it.
func SetStrictTenantScoping(v bool) { strictTenantScoping = v }

// ErrTenantContextRequired is returned by a tenant-scoped store operation when
// strict scoping is enabled (enterprise) and the caller's context was never
// stamped with a tenant. Mirrors the sqlite backend's sentinel.
var ErrTenantContextRequired = errors.New("postgres: tenant-scoped operation on an unstamped context (strict tenant scoping enabled)")

// tenantScope resolves how a scoped query should be tenanted (ADR 0011 M2).
// apply=false  → system context: no tenant predicate (all tenants).
// apply=true   → add a "tenant_id = $n" predicate with tenant (DefaultTenant
//
//	when unstamped and strict scoping is off; error when strict scoping is on).
//
// Byte-identical in semantics to the sqlite backend's tenantScope.
func tenantScope(ctx context.Context) (tenant string, apply bool, err error) {
	if identity.IsSystemContext(ctx) {
		return "", false, nil
	}
	t, ok := identity.TenantFromContextOK(ctx)
	if !ok && strictTenantScoping {
		return "", false, ErrTenantContextRequired
	}
	return t, true, nil // t is DefaultTenant when !ok
}
