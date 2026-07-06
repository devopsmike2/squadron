// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// enterprise_usage_seam_test.go pins the /api/v1/usage/* route seam: OSS (no
// handler wired) returns 404; a wired handler serves and receives the path
// suffix. Per-tenant usage is enterprise-reserved.

func newUsageSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountEnterpriseUsage(grp)
	return r
}

func TestEnterpriseUsageSeam_OSS404(t *testing.T) {
	s := &Server{}
	r := newUsageSeamRouter(s)
	for _, path := range []string{"/api/v1/usage/", "/api/v1/usage/tenants/acme"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		assert.Contains(t, w.Body.String(), "enterprise feature", path)
	}
}

type stubUsageHandler struct{ sawPath string }

func (h *stubUsageHandler) HandleUsage(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseUsageSeam_ServesWhenWired(t *testing.T) {
	stub := &stubUsageHandler{}
	s := &Server{}
	s.SetEnterpriseUsageHandler(stub)
	r := newUsageSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/usage/tenants/acme", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/tenants/acme", stub.sawPath)
}
