// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

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
	_, plaintext, err := svc.Issue(t.Context(), "reader", []string{services.ScopeAgentsRead})
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
	_, plaintext, err := svc.Issue(t.Context(), "reader", []string{services.ScopeAgentsRead})
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
	_, plaintext, err := svc.Issue(t.Context(), "wildcard", []string{services.ScopeWildcard})
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
