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
	Issue(ctx context.Context, label string, scopes []string) (token *APIToken, plaintext string, err error)
	List(ctx context.Context) ([]*APIToken, error)
	Revoke(ctx context.Context, id string) error

	// Validate checks a plaintext bearer value. Returns nil, nil if the
	// token is unknown OR revoked — callers shouldn't distinguish those
	// cases (both lead to a 401). Returns nil, err only on storage
	// failure.
	Validate(ctx context.Context, plaintext string) (*APIToken, error)
}

// Scope strings name the permissions a token can carry. They follow
// the convention <resource>:<action> with a "*" wildcard meaning "all
// scopes". Adding a scope means: declaring a constant here, listing it
// in AllScopes (so the UI picker can show it), and adding the
// middleware.RequireScope("...") to every protected route that needs it.
const (
	ScopeWildcard       = "*"
	ScopeAgentsRead     = "agents:read"
	ScopeAgentsWrite    = "agents:write"
	ScopeGroupsRead     = "groups:read"
	ScopeGroupsWrite    = "groups:write"
	ScopeConfigsRead    = "configs:read"
	ScopeConfigsWrite   = "configs:write"
	ScopeTelemetryRead  = "telemetry:read"
	ScopeAlertsRead     = "alerts:read"
	ScopeAlertsWrite    = "alerts:write"
	ScopeRolloutsRead   = "rollouts:read"
	ScopeRolloutsWrite  = "rollouts:write"
	ScopeAuditRead      = "audit:read"
	ScopeAuthRead       = "auth:read"
	ScopeAuthWrite      = "auth:write"
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
		ScopeRolloutsRead, ScopeRolloutsWrite,
		ScopeAuditRead,
		ScopeAuthRead, ScopeAuthWrite,
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
