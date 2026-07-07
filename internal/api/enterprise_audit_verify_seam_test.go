// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enterprise_audit_verify_seam_test.go — ADR 0027 slice 1. Pins the INERT
// /api/v1/audit-verify/tenants/* cross-tenant seam: OSS (no handler wired)
// returns 404; a wired handler serves and receives the path suffix. Also
// verifies the exact OSS self route GET /api/v1/audit-verify coexists with the
// wildcard seam without a gin route-registration panic.

func newAuditVerifySeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountEnterpriseAuditVerify(grp)
	return r
}

func TestEnterpriseAuditVerifySeam_OSS404(t *testing.T) {
	// Zero-value Server = OSS edition: no cross-tenant verify handler injected.
	s := &Server{}
	r := newAuditVerifySeamRouter(s)

	for _, path := range []string{"/api/v1/audit-verify/tenants/", "/api/v1/audit-verify/tenants/acme"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		assert.Contains(t, w.Body.String(), "enterprise feature", path)
	}
}

type stubAuditVerifyHandler struct{ sawPath string }

func (h *stubAuditVerifyHandler) HandleAuditVerify(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseAuditVerifySeam_ServesWhenWired(t *testing.T) {
	stub := &stubAuditVerifyHandler{}
	s := &Server{}
	s.SetEnterpriseAuditVerifyHandler(stub)
	r := newAuditVerifySeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit-verify/tenants/acme", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/acme", stub.sawPath, "handler must receive the path suffix after /audit-verify/tenants")
}

// TestAuditVerifyRoutes_Coexist proves gin accepts BOTH the exact OSS self
// route GET /api/v1/audit-verify AND the enterprise wildcard
// /api/v1/audit-verify/tenants/*path on the same router (they diverge at the
// segment after audit-verify), so registerRoutes does not panic at startup.
func TestAuditVerifyRoutes_Coexist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{}

	require.NotPanics(t, func() {
		r := gin.New()
		grp := r.Group("/api/v1")
		grp.GET("/audit-verify", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
		s.mountEnterpriseAuditVerify(grp)

		// self route → 200 (OSS handler)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit-verify", nil))
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "ok")

		// cross-tenant wildcard → 404 (OSS handler nil)
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/v1/audit-verify/tenants/acme", nil))
		assert.Equal(t, http.StatusNotFound, w2.Code)
		assert.Contains(t, w2.Body.String(), "enterprise feature")
	})
}
