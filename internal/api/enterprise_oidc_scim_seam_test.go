// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// enterprise_oidc_scim_seam_test.go — ADR 0014 Arc C slice 4a. Pins the two new
// enterprise route seams: SCIM under the bearer group (/api/v1/scim/v2/*) and
// OIDC pre-bearer on the root router (/auth/oidc/*). OSS (no handler wired)
// returns 404 on every route — including /scim/v2/ServiceProviderConfig, which
// OSS deliberately does NOT special-case (OSS has no SCIM). A wired handler
// serves and receives the path suffix. Mirrors enterprise_rbac_seam_test.go and
// enterprise_tenant_seam_test.go.

// --- SCIM (under /api/v1 bearer group) ---------------------------------------

func newSCIMSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountEnterpriseSCIM(grp)
	return r
}

func TestEnterpriseSCIMSeam_OSS404(t *testing.T) {
	// Zero-value Server = OSS edition: no SCIM handler injected.
	s := &Server{}
	r := newSCIMSeamRouter(s)

	// ServiceProviderConfig included on purpose: OSS 404s ALL SCIM routes,
	// discovery too — the public-vs-authed ServiceProviderConfig question is an
	// enterprise (slice 4c) decision (ADR 0014 Part E).
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/scim/v2/ServiceProviderConfig"},
		{http.MethodPost, "/api/v1/scim/v2/Users"},
		{http.MethodGet, "/api/v1/scim/v2/Users/abc"},
		{http.MethodPost, "/api/v1/scim/v2/Groups"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, tc.path)
		assert.Contains(t, w.Body.String(), "enterprise feature", tc.path)
	}
}

// stubSCIMHandler stands in for the enterprise handler, recording the path
// suffix it received.
type stubSCIMHandler struct{ sawPath string }

func (h *stubSCIMHandler) HandleSCIM(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseSCIMSeam_ServesWhenWired(t *testing.T) {
	stub := &stubSCIMHandler{}
	s := &Server{}
	s.SetEnterpriseSCIMHandler(stub)
	r := newSCIMSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/scim/v2/Users", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/Users", stub.sawPath, "handler must receive the path suffix after /scim/v2")
}

// --- OIDC (pre-bearer, root router) ------------------------------------------

func newOIDCSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// OIDC mounts on the ROOT router (pre-bearer), not under /api/v1.
	s.mountEnterpriseOIDC(r)
	return r
}

func TestEnterpriseOIDCSeam_OSS404(t *testing.T) {
	// Zero-value Server = OSS edition: no OIDC handler injected.
	s := &Server{}
	r := newOIDCSeamRouter(s)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/auth/oidc/login"},
		{http.MethodPost, "/auth/oidc/login"},
		{http.MethodGet, "/auth/oidc/callback"},
		{http.MethodPost, "/auth/oidc/callback"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, tc.path)
		assert.Contains(t, w.Body.String(), "enterprise feature", tc.path)
	}
}

// stubOIDCHandler stands in for the enterprise handler, recording the path
// suffix it received.
type stubOIDCHandler struct{ sawPath string }

func (h *stubOIDCHandler) HandleOIDC(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseOIDCSeam_ServesWhenWired(t *testing.T) {
	stub := &stubOIDCHandler{}
	s := &Server{}
	s.SetEnterpriseOIDCHandler(stub)
	r := newOIDCSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/login", stub.sawPath, "handler must receive the path suffix after /auth/oidc")
}
