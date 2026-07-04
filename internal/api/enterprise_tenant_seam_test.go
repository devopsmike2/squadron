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

// enterprise_tenant_seam_test.go — ADR 0011 slice 3c. Pins the
// /api/v1/tenants/* route seam: OSS (no handler wired) returns 404; a wired
// handler serves and receives the path suffix. Mirrors
// enterprise_rbac_seam_test.go.

func newTenantSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountEnterpriseTenants(grp)
	return r
}

func TestEnterpriseTenantSeam_OSS404(t *testing.T) {
	// Zero-value Server = OSS edition: no tenant handler injected.
	s := &Server{}
	r := newTenantSeamRouter(s)

	// Paths carry a suffix so the /tenants/*path wildcard matches directly
	// (a bare /api/v1/tenants 301-redirects to /api/v1/tenants/ before the
	// wildcard is consulted — same as the RBAC seam, which also only exercises
	// suffixed paths).
	for _, path := range []string{"/api/v1/tenants/", "/api/v1/tenants/acme", "/api/v1/tenants/acme/tokens"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		assert.Contains(t, w.Body.String(), "enterprise feature", path)
	}
}

// stubTenantHandler stands in for the enterprise handler, recording the path
// suffix it received.
type stubTenantHandler struct{ sawPath string }

func (h *stubTenantHandler) HandleTenants(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseTenantSeam_ServesWhenWired(t *testing.T) {
	stub := &stubTenantHandler{}
	s := &Server{}
	s.SetEnterpriseTenantHandler(stub)
	r := newTenantSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/tenants/acme/tokens", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/acme/tokens", stub.sawPath, "handler must receive the path suffix after /tenants")
}
