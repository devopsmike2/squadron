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

// resource_test.go — ADR 0010 slice 2a. Pins the resource-resolution the scope
// middleware feeds the Authorizer, and the editions-contract property that this
// plumbing is INERT under the OSS ScopeAuthorizer (behavior unchanged).

func TestDeriveResourceType(t *testing.T) {
	cases := []struct {
		fullPath string
		want     string
	}{
		// Rollouts — plans are more specific than the bare rollout prefix.
		// The COLLECTION route (no :id) must also resolve to "rollout" so an
		// all_resources rollout-typed RBAC permission can authorize the LIST —
		// a trailing slash on the mapping prefix would (and did) break this.
		{"/api/v1/rollouts", "rollout"},
		{"/api/v1/rollouts/:id", "rollout"},
		{"/api/v1/rollouts/:id/abort", "rollout"},
		{"/api/v1/rollouts/plans/:id", "rollout-plan"},
		{"/api/v1/rollout-recipes/templates", "rollout-recipe"},
		// rollout-recipes must NOT be captured by the /rollouts prefix.
		{"/api/v1/rollout-recipes", "rollout-recipe"},
		// Discovery — the connection :id is the RBAC scope axis; nested
		// scans/recommendations still resolve to the connection class.
		{"/api/v1/discovery/aws/connections/:id", "discovery-connection"},
		{"/api/v1/discovery/gcp/connections/:id/scans/:scanID", "discovery-connection"},
		{"/api/v1/iac/github/connections/:id/open-pr", "iac-connection"},
		// Core resources.
		{"/api/v1/configs/:id", "config"},
		{"/api/v1/groups/:id/config", "group"},
		{"/api/v1/agents/:id/restart", "agent"},
		{"/api/v1/topology/agent/:id", "topology"},
		{"/api/v1/alerts/rules/:id", "alert-rule"},
		{"/api/v1/alerts/cost-spikes/:id/acknowledge", "cost-spike"},
		{"/api/v1/audit/:id/explain", "audit-event"},
		{"/api/v1/deploy/targets/:id", "deploy-target"},
		{"/api/v1/deploy/runs/:id/redeploy", "deploy-run"},
		{"/api/v1/siem/destinations/:id", "siem-destination"},
		{"/api/v1/runners/:id", "action-runner"},
		{"/api/v1/actions/:id/result", "action"},
		{"/api/v1/incidents/drafts/:id/publish", "incident-draft"},
		{"/api/v1/recommendations/:id/dismiss", "recommendation"},
		{"/api/v1/insights/volume/agents/:id", "insights"},
		{"/api/v1/auth/tokens/:id/revoke", "api-token"},
		{"/api/v1/inventory/expected/:hostname", "expected-agent"},
		{"/api/v1/telemetry/saved-queries/:id", "saved-query"},
		// Unmapped routes → "" (class-wide / action-level fallback).
		{"/api/v1/quickstart/backends", ""},
		{"/api/v1/telemetry/metrics/query", ""},
		{"/api/v1/pricing/config", ""},
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.fullPath, func(t *testing.T) {
			assert.Equal(t, c.want, deriveResourceType(c.fullPath))
		})
	}
}

// capturingAuthorizer records the Resource the middleware passed and delegates
// the decision to an inner authorizer (default OSS ScopeAuthorizer).
type capturingAuthorizer struct {
	inner identity.Authorizer
	got   identity.Resource
}

func (c *capturingAuthorizer) Authorize(ctx context.Context, p identity.Principal, scope string, r identity.Resource) identity.Decision {
	c.got = r
	return c.inner.Authorize(ctx, p, scope, r)
}

// routerFor mounts RequireBearer + RequireScope on a single route template, so
// c.FullPath() returns that template and resolveResource can key on it.
func routerFor(authSvc services.AuthService, template, required string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequireBearer(authSvc, zap.NewNop()))
	r.GET(template, RequireScope(required), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestRequireScope_ResolvesResourceFromRoute(t *testing.T) {
	cap := &capturingAuthorizer{inner: identity.ScopeAuthorizer{}}
	SetAuthorizer(cap)
	defer SetAuthorizer(identity.ScopeAuthorizer{})

	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "wildcard", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	r := routerFor(svc, "/api/v1/rollouts/:id", services.ScopeRolloutsRead)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/rollouts/r-123", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, identity.Resource{Type: "rollout", ID: "r-123"}, cap.got,
		"middleware must populate the Resource type+id from the matched route")
}

func TestRequireScope_UnmappedRouteYieldsZeroResource(t *testing.T) {
	cap := &capturingAuthorizer{inner: identity.ScopeAuthorizer{}}
	SetAuthorizer(cap)
	defer SetAuthorizer(identity.ScopeAuthorizer{})

	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "wildcard", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	r := routerFor(svc, "/api/v1/quickstart/backends", services.ScopeAgentsRead)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/quickstart/backends", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, identity.Resource{}, cap.got, "unmapped route must yield a zero Resource")
}

// TestRequireScope_ResourcePlumbingInertUnderOSS is the editions-contract for
// slice 2a: even though the middleware now hands the OSS ScopeAuthorizer a
// populated Resource, the OSS decision is unchanged — a matching scope still
// 200s and a non-matching scope still 403s, exactly as before resource
// plumbing existed. The OSS authorizer ignores the Resource; only the
// enterprise authorizer consumes it.
func TestRequireScope_ResourcePlumbingInertUnderOSS(t *testing.T) {
	// Default OSS authorizer (no SetAuthorizer override).
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, readerTok, err := svc.Issue(t.Context(), "reader", []string{services.ScopeRolloutsRead}, nil)
	require.NoError(t, err)

	// Matching scope on a resource-bearing route → 200 (resource ignored).
	rAllow := routerFor(svc, "/api/v1/rollouts/:id", services.ScopeRolloutsRead)
	wAllow := httptest.NewRecorder()
	reqAllow := httptest.NewRequest(http.MethodGet, "/api/v1/rollouts/r-1", nil)
	reqAllow.Header.Set("Authorization", "Bearer "+readerTok)
	rAllow.ServeHTTP(wAllow, reqAllow)
	assert.Equal(t, http.StatusOK, wAllow.Code, "matching scope must still 200 with a populated Resource")

	// Non-matching scope on the same route shape → 403 (resource ignored).
	rDeny := routerFor(svc, "/api/v1/rollouts/:id/abort", services.ScopeRolloutsWrite)
	wDeny := httptest.NewRecorder()
	reqDeny := httptest.NewRequest(http.MethodGet, "/api/v1/rollouts/r-1/abort", nil)
	reqDeny.Header.Set("Authorization", "Bearer "+readerTok)
	rDeny.ServeHTTP(wDeny, reqDeny)
	assert.Equal(t, http.StatusForbidden, wDeny.Code, "non-matching scope must still 403 with a populated Resource")
}
