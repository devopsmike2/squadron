// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/extension/identity"
)

// resource.go — resource resolution for authorization (ADR 0010).
//
// The scope-enforcement middleware (RequireScope) historically passed an EMPTY
// identity.Resource{} to the Authorizer, so authorization was purely
// action-level (does the principal carry the scope?). A role-based enterprise
// Authorizer wants to make *resource-aware* decisions — "this on-call may
// abort THIS group's rollout, not every rollout" — which needs the resource
// type + id of the request.
//
// Rather than change the hundreds of RequireScope call sites in server.go, we
// resolve the resource here, at request time, from the matched Gin route
// (c.FullPath()) and its path params. This keeps the plumbing in one place and
// is INERT in the OSS build: identity.ScopeAuthorizer ignores the Resource
// entirely, so OSS authorization behavior is byte-for-byte unchanged. The
// enterprise Authorizer (squadron-enterprise) consumes the populated Resource
// to evaluate resource-scope predicates.
//
// The resolver is deliberately best-effort. An unmapped route, or a route with
// no resource id, yields a zero Resource — which the enterprise Authorizer
// treats as a class-wide (action-level) check, i.e. the same coarse decision
// OSS makes. Correctness is therefore monotonic: mapping a route can only make
// the enterprise decision *more* specific, never break the OSS default.

// routeResourceType maps a matched route template prefix to the RBAC resource
// class it operates on. The list is checked most-specific-prefix first (see
// deriveResourceType), so nested/more-specific prefixes must precede their
// parents. The type strings are the contract the enterprise role/permission
// model (ADR 0010) scopes predicates against; keep them stable.
//
// Note the coarse-scope reality (survey 2026-07-04): the agents:read/write
// scope alone gates ~15 distinct resource classes (agents, all four clouds'
// discovery connections, IaC, insights, recommendations, pricing,
// pipeline-health, inventory, quickstart, AI). The Resource.Type resolved here
// is what lets the enterprise Authorizer tell those classes apart under one
// scope — which the scope string alone cannot do.
var routeResourceType = []struct {
	prefix string // matched against c.FullPath() (the route template, e.g. "/api/v1/rollouts/:id")
	typ    string
}{
	// Discovery — the connection :id is the tenant/account scope dimension.
	// Scans/recommendations/observability nest under a connection, so they all
	// resolve to the connection class (the axis RBAC predicates scope on).
	// NOTE: no trailing slash — deriveResourceType picks the LONGEST matching
	// prefix, so a trailing slash is unnecessary AND buggy: it would exclude the
	// bare collection route (e.g. "/api/v1/rollouts" list) from matching, leaving
	// it type-"" so a rollout-typed RBAC permission could never authorize the
	// LIST. (Caught by SCIM/RBAC e2e: a rollout-operator role couldn't GET
	// /rollouts.) Keep these prefixes slash-free.
	{"/api/v1/discovery", "discovery-connection"},
	{"/api/v1/iac/github/connections", "iac-connection"},

	// Rollouts — plans are a distinct sub-resource; longest-prefix-wins resolves
	// /rollouts/plans/... to rollout-plan even though /rollouts also matches.
	{"/api/v1/rollouts/plans", "rollout-plan"},
	{"/api/v1/rollouts", "rollout"},
	{"/api/v1/rollout-recipes", "rollout-recipe"},

	{"/api/v1/configs", "config"},
	{"/api/v1/groups", "group"},
	{"/api/v1/agents", "agent"},
	{"/api/v1/topology", "topology"},

	{"/api/v1/alerts/cost-spikes", "cost-spike"},
	{"/api/v1/alerts/rules", "alert-rule"},

	{"/api/v1/audit", "audit-event"},

	{"/api/v1/deploy/targets", "deploy-target"},
	{"/api/v1/deploy/runs", "deploy-run"},

	{"/api/v1/siem/destinations", "siem-destination"},

	{"/api/v1/runners", "action-runner"},
	{"/api/v1/actions", "action"},

	{"/api/v1/incidents/drafts", "incident-draft"},

	{"/api/v1/recommendations", "recommendation"},
	{"/api/v1/insights", "insights"},

	{"/api/v1/auth/tokens", "api-token"},

	{"/api/v1/inventory/expected", "expected-agent"},

	{"/api/v1/telemetry/saved-queries", "saved-query"},
}

// deriveResourceType returns the RBAC resource class for a matched route
// template, or "" when the route isn't mapped (class-wide / action-level).
// Longest-matching prefix wins so nested routes resolve to the most specific
// class registered above.
func deriveResourceType(fullPath string) string {
	if fullPath == "" {
		return ""
	}
	best := ""
	bestLen := 0
	for _, r := range routeResourceType {
		if strings.HasPrefix(fullPath, r.prefix) && len(r.prefix) > bestLen {
			best = r.typ
			bestLen = len(r.prefix)
		}
	}
	return best
}

// resolveResource builds the identity.Resource the Authorizer scopes its
// decision against, from the matched route and its path params. Returns a zero
// Resource for unmapped routes or routes without a resource id — the
// action-level fallback. INERT under the OSS ScopeAuthorizer (ADR 0010).
func resolveResource(c *gin.Context) identity.Resource {
	typ := deriveResourceType(c.FullPath())
	if typ == "" {
		return identity.Resource{}
	}
	// The conventional resource-id param is :id across the route table; a few
	// routes key the resource on :hostname (inventory) instead. We take :id
	// first, then :hostname. Secondary params (:scanID, :agentID) intentionally
	// do NOT override :id — for nested discovery routes the connection :id is
	// the RBAC scope axis, not the nested resource.
	id := c.Param("id")
	if id == "" {
		id = c.Param("hostname")
	}
	return identity.Resource{Type: typ, ID: id}
}
