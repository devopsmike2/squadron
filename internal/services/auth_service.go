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
// hash is stored.
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
	Issue(ctx context.Context, label string) (token *APIToken, plaintext string, err error)
	List(ctx context.Context) ([]*APIToken, error)
	Revoke(ctx context.Context, id string) error

	// Validate checks a plaintext bearer value. Returns nil, nil if the
	// token is unknown OR revoked — callers shouldn't distinguish those
	// cases (both lead to a 401). Returns nil, err only on storage
	// failure.
	Validate(ctx context.Context, plaintext string) (*APIToken, error)
}

// APIToken is the service-layer view of applicationstore.APIToken. The
// plaintext is never on this struct — it lives on the stack at Issue
// time, gets sent to the operator once, and is discarded.
type APIToken struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// IsActive reports whether a token can still authenticate requests.
func (t *APIToken) IsActive() bool {
	return t != nil && t.RevokedAt == nil
}

// AuthActor is what the middleware stamps into the request context for
// authenticated requests. AuditService.Record reads from context and
// uses this as the audit actor when present, instead of the static
// AuditActorSystem.
type AuthActor struct {
	TokenID    string
	TokenLabel string
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
