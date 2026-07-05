// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"
)

// AuthService manages API tokens used for Bearer authentication.
//
// Issue creates a new token and returns BOTH the token row AND the
// plaintext value. Plaintext is shown to the operator once at creation
// time and is never persisted or retrievable later — only the sha256
// hash is stored. The scopes list is what the new token may do — see
// the Scope* constants below.
//
// Validate is what the auth middleware calls on every authenticated
// request. It returns the issuing token if the plaintext hashes to a
// known, non-revoked row; nil otherwise. The middleware never sees the
// hash directly.
//
// Revoke marks a token as revoked. Subsequent Validate calls with the
// same plaintext will return nil. The row stays in the store so
// historical audit entries can still resolve token IDs to labels.
type AuthService interface {
	// Issue creates a token. expiresAt is optional — pass nil for "never
	// expires", or a future timestamp for an auto-revoking token. The
	// service rejects expiresAt in the past so operators don't
	// accidentally ship a dead-on-arrival token.
	Issue(ctx context.Context, label string, scopes []string, expiresAt *time.Time) (token *APIToken, plaintext string, err error)
	List(ctx context.Context) ([]*APIToken, error)
	Revoke(ctx context.Context, id string) error

	// Validate checks a plaintext bearer value. Returns nil, nil if the
	// token is unknown, revoked, OR expired — callers shouldn't
	// distinguish those cases (all three lead to a 401, since leaking
	// "your token specifically is expired" would let a guesser learn
	// that the value used to be valid). Returns nil, err only on
	// storage failure.
	Validate(ctx context.Context, plaintext string) (*APIToken, error)
}

// Scope strings name the permissions a token can carry. They follow
// the convention <resource>:<action> with a "*" wildcard meaning "all
// scopes". Adding a scope means: declaring a constant here, listing it
// in AllScopes (so the UI picker can show it), and adding the
// middleware.RequireScope("...") to every protected route that needs it.
const (
	ScopeWildcard      = "*"
	ScopeAgentsRead    = "agents:read"
	ScopeAgentsWrite   = "agents:write"
	ScopeGroupsRead    = "groups:read"
	ScopeGroupsWrite   = "groups:write"
	ScopeConfigsRead   = "configs:read"
	ScopeConfigsWrite  = "configs:write"
	ScopeTelemetryRead = "telemetry:read"
	ScopeAlertsRead    = "alerts:read"
	ScopeAlertsWrite   = "alerts:write"
	ScopeRolloutsRead  = "rollouts:read"
	ScopeRolloutsWrite = "rollouts:write"
	// v0.48 — separation of duties for approval workflows.
	// rollouts:write covers Create/Abort/Pause/Resume; the new
	// rollouts:approve covers Approve/Reject. Splitting these
	// means a single stolen rollouts:write token can't bypass
	// the two-person rule by both creating and approving (from
	// distinct sessions). For NERC CIP-style controls, grant
	// rollouts:write to operators and rollouts:approve to a
	// distinct change-management group. A reviewer can hold
	// both — the runtime two-person rule still blocks
	// self-approval based on the persisted RequestedBy.
	ScopeRolloutsApprove = "rollouts:approve"
	ScopeAuditRead       = "audit:read"
	ScopeAuthRead        = "auth:read"
	ScopeAuthWrite       = "auth:write"
	// v0.34 deploy integration. Deploy:read shows targets + run
	// history; deploy:trigger is what's required to actually fire
	// a workflow on the operator's behalf — guarded narrowly so
	// rotating a write-only token doesn't accidentally regrant
	// deploy authority.
	ScopeDeployRead    = "deploy:read"
	ScopeDeployTrigger = "deploy:trigger"
	// v0.50 — SIEM destination management. siem:read returns the
	// list and per-destination view (no plaintext secrets, ever).
	// siem:write covers create / update / delete / test. Sized
	// like alerts: most operators need read; a smaller circle
	// touches write.
	ScopeSiemRead  = "siem:read"
	ScopeSiemWrite = "siem:write"
	// v0.53 — action runner management (Move 2). actions:read
	// covers the runner list, action request list, and runner
	// long-poll for pending requests. actions:write covers
	// runner registration, action dispatch (signing + persist),
	// runner revocation, and result reporting. These are split
	// because most operators only need to look; only the runner
	// daemon and a small change-management group need to fire
	// actions.
	ScopeActionsRead  = "actions:read"
	ScopeActionsWrite = "actions:write"

	// v0.54 — incident drafts (Move 3). Read covers listing and
	// viewing drafted tickets in the operator inbox. Write covers
	// editing, dismissing, and publishing through a provider plug
	// in. Kept separate from actions:write because the people who
	// review and publish incident tickets are not always the same
	// people who dispatch fleet actions.
	ScopeIncidentsRead  = "incidents:read"
	ScopeIncidentsWrite = "incidents:write"

	// ADR 0014 Arc C — SCIM 2.0 provisioning (slice 4a defines the scopes;
	// slice 4c guards the routes with RequireScope). scim:read covers
	// ServiceProviderConfig + Users/Groups reads; scim:write covers
	// provisioning (create/update/patch/delete + the active:false deprovision
	// signal). The IdP holds a reserved-label, tenant-bound token carrying
	// scim:* and provisions only into its own tenant (D3). Inert in OSS:
	// defining a scope changes nothing until a route requires it, and OSS
	// mounts no SCIM routes (handler nil → 404).
	ScopeSCIMRead  = "scim:read"
	ScopeSCIMWrite = "scim:write"
)

// AllScopes returns every grantable scope, in the canonical order the
// UI's checkbox grid renders them. The list is server-of-record so
// adding a new scope is a code change.
func AllScopes() []string {
	return []string{
		ScopeAgentsRead, ScopeAgentsWrite,
		ScopeGroupsRead, ScopeGroupsWrite,
		ScopeConfigsRead, ScopeConfigsWrite,
		ScopeTelemetryRead,
		ScopeAlertsRead, ScopeAlertsWrite,
		ScopeRolloutsRead, ScopeRolloutsWrite, ScopeRolloutsApprove,
		ScopeAuditRead,
		ScopeAuthRead, ScopeAuthWrite,
		ScopeDeployRead, ScopeDeployTrigger,
		ScopeSiemRead, ScopeSiemWrite,
		ScopeActionsRead, ScopeActionsWrite,
		ScopeIncidentsRead, ScopeIncidentsWrite,
		ScopeSCIMRead, ScopeSCIMWrite,
	}
}

// IsValidScope reports whether s is a known scope or the wildcard.
// Service Issue() rejects unknown scopes so a typo doesn't silently
// produce a token that grants nothing.
func IsValidScope(s string) bool {
	if s == ScopeWildcard {
		return true
	}
	for _, k := range AllScopes() {
		if s == k {
			return true
		}
	}
	return false
}

// APIToken is the service-layer view of applicationstore.APIToken. The
// plaintext is never on this struct — it lives on the stack at Issue
// time, gets sent to the operator once, and is discarded.
//
// Scopes is the permission list. An empty Scopes is treated as full
// access by the middleware, but only for tokens created before v0.10.
// New tokens are required to declare scopes explicitly; an Issue()
// call with an empty list returns a validation error.
type APIToken struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`

	// TenantID is the tenant this token authenticates into (ADR 0011).
	// Set into AuthActor.Tenant in RequireBearer so the enterprise
	// TenantResolver can derive a request's tenant. Inert in OSS.
	TenantID string `json:"tenant_id,omitempty"`
}

// IsExpired reports whether the token has an expiry and that expiry
// has passed. Tokens with no expiry never report as expired.
func (t *APIToken) IsExpired() bool {
	return t != nil && t.ExpiresAt != nil && !time.Now().Before(*t.ExpiresAt)
}

// HasScope reports whether this token may exercise the given scope.
// Wildcard "*" matches every scope; empty scopes (the legacy
// pre-v0.10 token shape) also matches every scope. New tokens are
// required to specify scopes so the empty case is bounded to
// existing rows.
func (t *APIToken) HasScope(required string) bool {
	if t == nil {
		return false
	}
	if len(t.Scopes) == 0 {
		return true // legacy full-access
	}
	for _, s := range t.Scopes {
		if s == ScopeWildcard || s == required {
			return true
		}
	}
	return false
}

// IsActive reports whether a token can still authenticate requests.
func (t *APIToken) IsActive() bool {
	return t != nil && t.RevokedAt == nil
}

// AuthActor is what the middleware stamps into the request context for
// authenticated requests. AuditService.Record reads from context and
// uses this as the audit actor when present, instead of the static
// AuditActorSystem.
//
// Scopes are carried so middleware downstream of RequireBearer (e.g.
// RequireScope) can authorize without re-querying the store. Same
// semantics as APIToken.Scopes — empty means legacy full-access.
type AuthActor struct {
	TokenID    string
	TokenLabel string
	Scopes     []string

	// Tenant is the tenant the authenticated token belongs to (ADR 0011).
	// Set in RequireBearer from APIToken.TenantID (default identity.DefaultTenant
	// when empty). The enterprise TenantResolver reads this off
	// ActorFromContext(ctx) to scope the request; inert in OSS, where the
	// SingleTenantResolver always returns DefaultTenant regardless.
	Tenant string
}

// HasScope mirrors APIToken.HasScope. Defined on the actor so the
// middleware can decide without holding the full token row.
func (a AuthActor) HasScope(required string) bool {
	if len(a.Scopes) == 0 {
		return true // legacy full-access
	}
	for _, s := range a.Scopes {
		if s == ScopeWildcard || s == required {
			return true
		}
	}
	return false
}

// String returns the canonical actor string ("operator:<label>") used
// in audit entries. Mirrors the audit_log.md spec.
func (a AuthActor) String() string {
	if a.TokenLabel == "" {
		return AuditActorSystem
	}
	return "operator:" + a.TokenLabel
}

// IsZero reports whether this is the zero-value actor (no
// authentication present on the request).
func (a AuthActor) IsZero() bool {
	return a.TokenID == "" && a.TokenLabel == ""
}

// actorContextKey is the typed key used to stash an AuthActor on a
// context.Context. Auth middleware sets it; the audit service reads it.
// We use an unexported defined type so external packages can't collide
// or pollute the key namespace.
type actorContextKey struct{}

// WithActor returns a new context that carries the given actor. Used
// by the auth middleware after a successful Bearer validation.
func WithActor(ctx context.Context, actor AuthActor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// ActorFromContext returns the actor on the context, or the zero value
// if none was set. AuditService.Record uses this to override the entry's
// Actor field when an authenticated actor is present.
func ActorFromContext(ctx context.Context) AuthActor {
	if v, ok := ctx.Value(actorContextKey{}).(AuthActor); ok {
		return v
	}
	return AuthActor{}
}
