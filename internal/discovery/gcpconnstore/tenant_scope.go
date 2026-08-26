// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcpconnstore

import (
	"context"
	"errors"

	"github.com/devopsmike2/squadron/extension/identity"
)

// tenant_scope.go — the ADR 0043 query-layer tenant-scoping seam for the
// GCP connection substrate, mirroring the proven application-store
// tenantScope pattern (internal/storage/applicationstore/sqlite/
// tenant_scope.go). Every per-tenant read / write / delete routes through
// tenantScope to decide whether to add a `tenant_id = ?` predicate (the
// gcp_connections owner-tenant column). In the OSS build this is INERT:
// nothing calls SetStrictTenantScoping, so strictTenantScoping stays false
// and an unstamped context falls back to identity.DefaultTenant
// ("default"). Since OSS writes every row with tenant_id='default' too,
// the predicate is a no-op and OSS behavior is byte-identical to the
// pre-scoping store. The enterprise wire flips the flag on at startup and
// wires a real TenantResolver, at which point the same predicate becomes
// real per-tenant isolation of the sealed GCP service-account credentials.

// strictTenantScoping gates the fail-fast behavior on an unstamped
// context. Defaults to false. The enterprise wire calls
// SetStrictTenantScoping(true) at startup so any tenant-scoped operation
// that reaches the store on a context which was never stamped (neither
// WithTenant nor WithSystemContext) is rejected rather than silently
// degrading to DefaultTenant. OSS never calls it, so the legacy
// DefaultTenant fallback stays in force.
var strictTenantScoping bool

// SetStrictTenantScoping toggles strict tenant scoping. The enterprise
// wire sets it true at startup; OSS never calls it, leaving it false →
// legacy DefaultTenant fallback for unstamped contexts.
func SetStrictTenantScoping(v bool) { strictTenantScoping = v }

// ErrTenantContextRequired is returned by a tenant-scoped store operation
// when strict scoping is enabled (enterprise) and the caller's context was
// never stamped with a tenant. It forces every enterprise code path to be
// explicit about its tenant (WithTenant for a request, WithSystemContext
// for a fleet-wide background job) instead of silently landing on
// DefaultTenant.
var ErrTenantContextRequired = errors.New("gcpconnstore: tenant-scoped operation on an unstamped context (strict tenant scoping enabled)")

// tenantScope resolves how a scoped query should be tenanted.
// apply=false → system context: no tenant predicate (all tenants).
// apply=true  → add "tenant_id = ?" with tenant (DefaultTenant when
// unstamped and strict scoping is off; error when strict scoping is on).
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
