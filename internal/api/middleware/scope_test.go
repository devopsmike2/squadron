// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// newScopedRouter wires Bearer + RequireScope in front of a single
// handler that just returns 200 OK. Lets each test focus on the
// scope-check decision.
func newScopedRouter(authSvc services.AuthService, required string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequireBearer(authSvc, zap.NewNop()))
	r.GET("/scoped", RequireScope(required), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestRequireScope_TokenWithMatchingScope_200(t *testing.T) {
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "reader", []string{services.ScopeAgentsRead}, nil)
	require.NoError(t, err)

	r := newScopedRouter(svc, services.ScopeAgentsRead)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/scoped", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireScope_TokenWithDifferentScope_403(t *testing.T) {
	// Token has agents:read but the route needs rollouts:write.
	// Must be 403 (not 401) — auth was OK, permission was the problem.
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "reader", []string{services.ScopeAgentsRead}, nil)
	require.NoError(t, err)

	r := newScopedRouter(svc, services.ScopeRolloutsWrite)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/scoped", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "required_scope")
	assert.Contains(t, w.Body.String(), services.ScopeRolloutsWrite)
}

func TestRequireScope_WildcardToken_200_OnAnyRoute(t *testing.T) {
	// A wildcard-scoped token (the bootstrap shape) authorizes
	// every endpoint regardless of which RequireScope guards it.
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "wildcard", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	for _, scope := range []string{
		services.ScopeAgentsRead,
		services.ScopeRolloutsWrite,
		services.ScopeAuthWrite,
	} {
		t.Run(scope, func(t *testing.T) {
			r := newScopedRouter(svc, scope)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/scoped", nil)
			req.Header.Set("Authorization", "Bearer "+plaintext)
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
}

func TestRequireScope_AuthDisabledPassesThrough(t *testing.T) {
	// When RequireBearer is NOT mounted (auth.enabled=false in the
	// server), no actor lands on the context. RequireScope must let
	// the request through so dev/staging instances with auth off
	// behave the same as before this feature.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/scoped", RequireScope(services.ScopeRolloutsWrite), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/scoped", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "auth-disabled deploys must pass through scope checks")
}

// (The empty-scopes legacy-full-access branch is pinned at the authorizer
// unit level in extension/identity/identity_test.go — tokens can no longer be
// issued with empty scopes, so it can't be exercised through the issue path
// here.)

// denyAllAuthorizer is a test double proving RequireScope consults the wired
// identity.Authorizer rather than a hardcoded scope check.
type denyAllAuthorizer struct{}

func (denyAllAuthorizer) Authorize(context.Context, identity.Principal, string, identity.Resource) identity.Decision {
	return identity.Decision{Allow: false, Reason: "denied by test authorizer"}
}

// TestRequireScope_ConsultsWiredAuthorizer proves slice 2's indirection:
// SetAuthorizer swaps the decision-maker, so a deny-all authorizer 403s even a
// wildcard token that the OSS default would allow. Restores the default so
// sibling tests are unaffected (the authorizer is process-global).
func TestRequireScope_ConsultsWiredAuthorizer(t *testing.T) {
	SetAuthorizer(denyAllAuthorizer{})
	defer SetAuthorizer(identity.ScopeAuthorizer{})

	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "wildcard", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	r := newScopedRouter(svc, services.ScopeAgentsRead)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/scoped", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code, "wired deny-all authorizer must override the default")
}

// tenantEchoRouter mounts ResolveTenant in front of a handler that echoes the
// resolved tenant read back from the request context.
func tenantEchoRouter(seen *string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ResolveTenant())
	r.GET("/t", func(c *gin.Context) {
		*seen = identity.TenantFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	return r
}

// TestResolveTenant_StampsDefault confirms the OSS ResolveTenant middleware
// stamps identity.DefaultTenant onto the request context (single-tenant OSS).
func TestResolveTenant_StampsDefault(t *testing.T) {
	var seen string
	r := tenantEchoRouter(&seen)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/t", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, identity.DefaultTenant, seen)
}

// fixedTenantResolver is a test double returning a constant tenant.
type fixedTenantResolver struct{ tenant string }

func (f fixedTenantResolver) Resolve(context.Context) string { return f.tenant }

// TestResolveTenant_ConsultsWiredResolver confirms SetTenantResolver swaps the
// resolver, so the middleware stamps the wired tenant. Restores the OSS
// default (the resolver is process-global).
func TestResolveTenant_ConsultsWiredResolver(t *testing.T) {
	SetTenantResolver(fixedTenantResolver{tenant: "acme"})
	defer SetTenantResolver(identity.SingleTenantResolver{})

	var seen string
	r := tenantEchoRouter(&seen)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/t", nil))
	assert.Equal(t, "acme", seen)
}
