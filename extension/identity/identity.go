// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package identity defines the boundary between the open-core auth model
// — bearer tokens + flat per-token scopes, a single implicit tenant — and
// the enterprise edition's identity features: SSO (SAML/OIDC) + SCIM,
// role-based access control, and multi-tenant isolation.
//
// The boundary lives under extension/ (not internal/) so the private
// enterprise repo can import it across module boundaries, and it holds NO
// internal/ dependency — the shapes here (Principal, Resource, Decision)
// are adapted copies, not aliases of internal/services types. This mirrors
// extension/detectors, extension/policy, extension/changewindow, and
// extension/siem.
//
// The OSS binary wires OSSProviders(): a bearer authenticator, a flat
// ScopeAuthorizer whose allow/deny is byte-identical to the historical
// middleware.RequireScope / services.AuthActor.HasScope, and a
// SingleTenantResolver that always returns DefaultTenant. Observable OSS
// behavior is therefore unchanged. The enterprise edition wires its own
// Providers (real SSO/RBAC/multi-tenancy) against the same interfaces. See
// cmd/all-in-one/wire_identity_oss.go (default) and
// wire_identity_enterprise.go (build tag: enterprise), ADR 0006, and
// docs/build.md.
package identity

import "context"

// Wildcard is the scope that grants all permissions. It mirrors
// internal/services.ScopeWildcard; the constant is duplicated here rather
// than imported because extension/ must not depend on internal/.
const Wildcard = "*"

// DefaultTenant is the single implicit tenant every request runs under in
// the OSS build. The enterprise TenantResolver derives a real tenant from
// the authenticated identity instead.
const DefaultTenant = "default"

// SystemTenant is the reserved marker (ADR 0011) that a background job stamps
// on its context to mean "operate across ALL tenants" — retention GCs, the
// rollout engine tick, the deploy poller, the alert evaluator, etc. It is NOT
// a real tenant: the enterprise scoped store treats a system context as
// "apply no tenant predicate", never as a literal WHERE tenant_id = '__system__'.
// It is deliberately distinct from DefaultTenant so that a MISSED stamp on a
// fleet-wide job degrades to DefaultTenant (visibly-empty results, or an
// enterprise fail-fast) rather than silently scoping to — or corrupting — the
// default tenant.
const SystemTenant = "__system__"

// Principal is the adapted, extension-safe identity of a request's caller.
// It mirrors the fields of internal/services.AuthActor WITHOUT importing
// it, keeping this package free of internal/ dependencies.
type Principal struct {
	// ID is a stable identifier for the caller (OSS: the API token id).
	ID string
	// Label is the human-facing name (OSS: the API token label).
	Label string
	// Scopes are the permissions the principal carries (OSS: token scopes).
	Scopes []string
}

// Resource identifies the object an action targets, for resource-level
// authorization. The OSS ScopeAuthorizer ignores it (scopes are
// action-level, not resource-scoped); the enterprise RBAC Authorizer may
// scope a decision to a specific resource.
type Resource struct {
	// Type is the resource kind, e.g. "rollout" or "config"; "" when the
	// action is not resource-scoped.
	Type string
	// ID is the resource identifier; "" when the action is not
	// resource-scoped.
	ID string
}

// Decision is the outcome of an authorization check.
type Decision struct {
	// Allow reports whether the action is permitted.
	Allow bool
	// Reason is a human-readable denial reason for audit/debug; empty when
	// Allow is true.
	Reason string
}

// Authenticator resolves a request credential into a Principal. The OSS
// build keeps performing bearer-token validation in the existing auth
// middleware; this interface exists so the enterprise edition can resolve
// richer principals (SSO/OIDC sessions, SCIM-provisioned users) against a
// stable seam.
type Authenticator interface {
	// Name identifies the active authentication scheme for logs and the
	// /metrics build identity. OSS: "bearer".
	Name() string
}

// Authorizer decides whether a principal may perform an action. main.go
// wires exactly one, and the auth middleware consults it (from slice 2 of
// ADR 0006). A nil Authorizer must be treated by callers as "allow"
// (auth-disabled posture), matching today's middleware.RequireScope, which
// skips the check when no actor is present.
type Authorizer interface {
	// Authorize reports whether principal p is allowed requiredScope for
	// the (optional) resource. The OSS ScopeAuthorizer implements the
	// historical flat-scope semantics exactly; the enterprise Authorizer is
	// role-based, deny-by-default, and may consider the resource.
	Authorize(ctx context.Context, p Principal, requiredScope string, resource Resource) Decision
}

// TenantResolver maps a request context to the tenant it runs under. The
// OSS SingleTenantResolver always returns DefaultTenant; the enterprise
// resolver derives the tenant from the authenticated identity.
type TenantResolver interface {
	Resolve(ctx context.Context) string
}

// Providers bundles the identity seam's three providers so main.go wires
// them in a single call, mirroring the single-call-site pattern of the
// other extension seams.
type Providers struct {
	Authenticator  Authenticator
	Authorizer     Authorizer
	TenantResolver TenantResolver
}

// --- OSS default implementations -----------------------------------------

// BearerAuthenticator is the OSS default authenticator. Authentication
// itself stays in the existing bearer middleware; this marker names the
// active scheme for logs and the build identity.
type BearerAuthenticator struct{}

// Name reports the OSS authentication scheme.
func (BearerAuthenticator) Name() string { return "bearer" }

// ScopeAuthorizer is the OSS default authorizer. It implements the exact
// flat-scope semantics of the historical middleware.RequireScope /
// services.AuthActor.HasScope:
//
//   - an empty scope set is legacy full-access (pre-scopes tokens);
//   - otherwise the principal must carry the wildcard or the required scope.
//
// Keeping this byte-identical is what lets slice 2 route the auth
// middleware through the Authorizer without changing OSS behavior.
type ScopeAuthorizer struct{}

// Authorize applies the flat-scope check. The resource is ignored — OSS
// scopes are action-level, not resource-scoped.
func (ScopeAuthorizer) Authorize(_ context.Context, p Principal, requiredScope string, _ Resource) Decision {
	if len(p.Scopes) == 0 {
		// Legacy full-access: pre-scopes tokens carry no scopes and retain
		// full access, exactly as services.AuthActor.HasScope grants it.
		return Decision{Allow: true}
	}
	for _, s := range p.Scopes {
		if s == Wildcard || s == requiredScope {
			return Decision{Allow: true}
		}
	}
	// Denial reason mirrors the historical RequireScope 403 detail so slice
	// 2 can preserve the exact response body.
	return Decision{Allow: false, Reason: "token does not have the required scope"}
}

// SingleTenantResolver is the OSS default: every request runs under the
// single implicit DefaultTenant. Enterprise multi-tenancy replaces this
// with a resolver that derives the tenant from the authenticated identity.
type SingleTenantResolver struct{}

// Resolve always returns DefaultTenant in the OSS build.
func (SingleTenantResolver) Resolve(context.Context) string { return DefaultTenant }

// tenantContextKey is the unexported key under which the resolved tenant id
// rides on a context.Context. An unexported defined type keeps external
// packages from colliding with or reading the key directly.
type tenantContextKey struct{}

// WithTenant returns a context carrying the given tenant id. The auth
// middleware stamps this once per request from the wired TenantResolver
// (ADR 0006 slice 3), so downstream service/store layers can scope by tenant
// without re-resolving it.
func WithTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantContextKey{}, tenant)
}

// TenantFromContext returns the tenant id carried by ctx, or DefaultTenant
// when none is present. The default keeps single-tenant OSS behavior and
// background contexts (which never pass through the stamping middleware)
// safe: a reader always gets a usable tenant, never an empty string.
func TenantFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tenantContextKey{}).(string); ok && v != "" {
		return v
	}
	return DefaultTenant
}

// TenantFromContextOK is like TenantFromContext but reports whether a tenant
// was EXPLICITLY stamped on the context (ADR 0011). The bool distinguishes
// "unstamped background context" (ok=false) from "stamped as DefaultTenant"
// (ok=true) — a distinction TenantFromContext erases by always returning
// DefaultTenant. OSS keeps using TenantFromContext (legacy fallback); the
// enterprise scoped store uses this to fail fast when a resolvable path runs on
// an unstamped context, so every enterprise background job is forced to stamp
// its tenant (WithSystemContext for all-tenant, WithTenant for per-owner).
// ResolveTenant stamps even the OSS default, so a real HTTP request is always
// ok=true.
func TenantFromContextOK(ctx context.Context) (string, bool) {
	if v, ok := ctx.Value(tenantContextKey{}).(string); ok && v != "" {
		return v, true
	}
	return DefaultTenant, false
}

// WithSystemContext stamps ctx as a system (all-tenant) context (ADR 0011) — a
// background job declaring "operate across every tenant". Implemented as
// WithTenant(ctx, SystemTenant) so it flows through the same context key; the
// enterprise scoped store recognizes SystemTenant via IsSystemContext and
// applies no tenant predicate.
func WithSystemContext(ctx context.Context) context.Context {
	return WithTenant(ctx, SystemTenant)
}

// IsSystemContext reports whether ctx was stamped as a system (all-tenant)
// context. The enterprise scoped store uses this to skip tenant filtering for
// fleet-wide background work (retention GC, the rollout engine, the deploy
// poller, the alert evaluator, …).
func IsSystemContext(ctx context.Context) bool {
	v, ok := ctx.Value(tenantContextKey{}).(string)
	return ok && v == SystemTenant
}

// OSSProviders returns the open-core default provider bundle. The OSS wire
// file (cmd/all-in-one/wire_identity_oss.go) returns this; the enterprise
// wire file returns its own bundle. Exposed as a constructor so the wire
// layer and tests share one definition of "the OSS defaults".
func OSSProviders() Providers {
	return Providers{
		Authenticator:  BearerAuthenticator{},
		Authorizer:     ScopeAuthorizer{},
		TenantResolver: SingleTenantResolver{},
	}
}
