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

// enterprise_rbac_seam_test.go — ADR 0010 slice 2a. Pins the /api/v1/rbac/*
// route seam: OSS (no handler wired) returns 404; a wired handler serves.

func newRBACSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountEnterpriseRBAC(grp)
	return r
}

func TestEnterpriseRBACSeam_OSS404(t *testing.T) {
	// Zero-value Server = OSS edition: no RBAC handler injected.
	s := &Server{}
	r := newRBACSeamRouter(s)

	for _, path := range []string{"/api/v1/rbac/roles", "/api/v1/rbac/bindings", "/api/v1/rbac/permissions/x"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		assert.Contains(t, w.Body.String(), "enterprise feature", path)
	}
}

// stubRBACHandler stands in for the enterprise handler, recording the path
// suffix it received.
type stubRBACHandler struct{ sawPath string }

func (h *stubRBACHandler) HandleRBAC(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseRBACSeam_ServesWhenWired(t *testing.T) {
	stub := &stubRBACHandler{}
	s := &Server{}
	s.SetEnterpriseRBACHandler(stub)
	r := newRBACSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/rbac/roles", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/roles", stub.sawPath, "handler must receive the path suffix after /rbac")
}
