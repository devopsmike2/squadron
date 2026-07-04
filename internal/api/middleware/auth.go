// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package middleware contains HTTP middleware that lives outside the
// per-handler logic — auth, request logging, metrics. The auth middleware
// in particular is the one place where bearer tokens are validated; every
// handler downstream can assume the request is authenticated (when auth
// is enabled) and read the authenticated actor from the request context.
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/services"
)

// AuthActorContextKey is the context key the auth middleware uses to
// stash the authenticated actor. Audit code reads from here so every
// state-changing operation gets attributed to the issuing token without
// each handler having to plumb the actor through manually.
//
// It's a Gin context key (string) rather than a typed context.Context
// key because Gin stores arbitrary values keyed by string. The audit
// service treats it as opaque — it just looks for this key, type-
// asserts to services.AuthActor, and uses the result if present.
const AuthActorContextKey = "squadron.auth.actor"

// AuthorizationHeader and BearerScheme are the canonical names used by
// the middleware. Pulled out as constants so tests can refer to them
// without re-typing magic strings.
const (
	AuthorizationHeader = "Authorization"
	BearerScheme        = "Bearer"
)

// RequireBearer returns Gin middleware that enforces a Bearer token on
// every protected request. The auth service validates the token; on
// success the AuthActor is stashed into the Gin context so downstream
// audit recordings can attribute the request to the issuing token.
//
// Pre-flight CORS (OPTIONS) requests are allowed through unauthenticated
// — browsers send OPTIONS before the first request to learn whether the
// real request will be permitted, and the spec doesn't carry credentials
// on the preflight. The CORS middleware sits in front of this one and
// handles the OPTIONS reply.
//
// Public endpoints (/healthz, /metrics) MUST NOT have this middleware
// applied — the caller is responsible for not mounting it on those.
func RequireBearer(auth services.AuthService, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}

		raw := c.GetHeader(AuthorizationHeader)
		if raw == "" {
			abortUnauthorized(c, "missing Authorization header")
			return
		}
		// Header must be "Bearer <token>". We accept any case for the
		// scheme name (RFC 7235 §2.1 says it's case-insensitive) but
		// require exactly one space separator.
		parts := strings.SplitN(raw, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], BearerScheme) {
			abortUnauthorized(c, "Authorization header must be: Bearer <token>")
			return
		}
		plaintext := strings.TrimSpace(parts[1])
		if plaintext == "" {
			abortUnauthorized(c, "bearer token is empty")
			return
		}

		token, err := auth.Validate(c.Request.Context(), plaintext)
		if err != nil {
			// Storage failure — we don't want to leak that as a 401
			// because clients with valid tokens would assume they're
			// rejected.
			logger.Error("auth: validate failed", zap.Error(err))
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "auth temporarily unavailable"})
			return
		}
		if token == nil {
			abortUnauthorized(c, "invalid or revoked token")
			return
		}

		actor := services.AuthActor{
			TokenID:    token.ID,
			TokenLabel: token.Label,
			Scopes:     token.Scopes,
		}
		c.Set(AuthActorContextKey, actor)
		// Also stash on the request context so service-layer audit
		// recordings (which don't see the Gin context) pick up the
		// authenticated actor automatically.
		c.Request = c.Request.WithContext(services.WithActor(c.Request.Context(), actor))
		c.Next()
	}
}

func abortUnauthorized(c *gin.Context, detail string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error":  "unauthorized",
		"detail": detail,
	})
}

// ActorFromGin returns the authenticated actor for a Gin request, or
// the zero value if the request was unauthenticated. Callers can pass
// the result to AuditEntry.Actor or check IsZero() for "no actor".
func ActorFromGin(c *gin.Context) services.AuthActor {
	if v, ok := c.Get(AuthActorContextKey); ok {
		if actor, ok := v.(services.AuthActor); ok {
			return actor
		}
	}
	return services.AuthActor{}
}

// authorizer is the authorization decision-maker the scope middleware
// consults. It defaults to the OSS flat-scope authorizer, whose allow/deny
// is byte-identical to the historical services.AuthActor.HasScope semantics.
// main.go overrides it once at startup via SetAuthorizer with the edition's
// wired identity.Authorizer (ADR 0006 — the enterprise edition supplies a
// role-based, deny-by-default authorizer here). The RequireScope closure
// reads this at request time, so a startup-time SetAuthorizer takes effect
// for every route.
var authorizer identity.Authorizer = identity.ScopeAuthorizer{}

// SetAuthorizer installs the process-wide authorizer the scope middleware
// consults. Called once from main.go with the edition's identity provider
// bundle (idSeam.Authorizer). A nil argument is ignored, keeping the OSS
// default. Not safe for concurrent use with in-flight requests — call it
// during startup, before the server accepts traffic.
func SetAuthorizer(a identity.Authorizer) {
	if a != nil {
		authorizer = a
	}
}

// tenantResolver is the resolver the ResolveTenant middleware consults to
// map a request to its tenant. It defaults to the OSS single-tenant resolver
// (every request runs under identity.DefaultTenant). main.go overrides it once
// at startup via SetTenantResolver with the edition's wired resolver (ADR 0006
// — the enterprise edition derives a real tenant from the authenticated
// principal).
var tenantResolver identity.TenantResolver = identity.SingleTenantResolver{}

// SetTenantResolver installs the process-wide tenant resolver the
// ResolveTenant middleware consults. Called once from main.go with
// idSeam.TenantResolver. A nil argument is ignored, keeping the OSS default.
// Not safe for concurrent use with in-flight requests — call it during
// startup, before the server accepts traffic.
func SetTenantResolver(r identity.TenantResolver) {
	if r != nil {
		tenantResolver = r
	}
}

// ResolveTenant returns Gin middleware that resolves the request's tenant via
// the wired identity.TenantResolver and stamps it onto the request context
// (identity.WithTenant), so downstream service/store layers can scope by it.
// OSS resolves the single implicit identity.DefaultTenant; the enterprise
// edition derives a real tenant from the authenticated principal. Mount it
// AFTER RequireBearer so an enterprise resolver can see the actor; it is safe
// (and a no-op-shaped default) to mount even when auth is disabled.
func ResolveTenant() gin.HandlerFunc {
	return func(c *gin.Context) {
		tenant := tenantResolver.Resolve(c.Request.Context())
		c.Request = c.Request.WithContext(identity.WithTenant(c.Request.Context(), tenant))
		c.Next()
	}
}

// RequireScope returns Gin middleware that enforces a specific scope
// on the authenticated actor. Must run AFTER RequireBearer — that's
// the middleware that puts the actor on the context. If no actor is
// present (auth disabled server-side) the scope check is skipped —
// auth-disabled deployments behave as before, and operators who turn
// auth on get authorization with the same flag flip.
//
// The decision is delegated to the wired identity.Authorizer (ADR 0006).
// The OSS default (identity.ScopeAuthorizer) reproduces the historical
// flat-scope check exactly; the enterprise edition can supply a role-based,
// resource-aware, deny-by-default authorizer without any change here. The
// 403 response body is unchanged regardless of the authorizer.
//
// 401 vs 403:
//   - 401 unauthorized = "I don't know who you are" (no/bad token).
//     Returned by RequireBearer.
//   - 403 forbidden    = "I know who you are but you can't do this".
//     Returned here. Distinct status so the CLI can branch — e.g.
//     a 403 means "your token is fine, but it lacks 'rollouts:write'".
func RequireScope(required string) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor := ActorFromGin(c)
		if actor.IsZero() {
			// Auth is disabled server-side; nothing to authorize against.
			c.Next()
			return
		}
		// resolveResource inspects the matched route to populate the Resource
		// the Authorizer scopes against (ADR 0010). INERT under the OSS
		// ScopeAuthorizer, which ignores it — the enterprise role-based
		// Authorizer uses it for resource-aware decisions.
		decision := authorizer.Authorize(c.Request.Context(), identity.Principal{
			ID:     actor.TokenID,
			Label:  actor.TokenLabel,
			Scopes: actor.Scopes,
		}, required, resolveResource(c))
		if !decision.Allow {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":          "forbidden",
				"detail":         "token does not have the required scope",
				"required_scope": required,
			})
			return
		}
		c.Next()
	}
}
