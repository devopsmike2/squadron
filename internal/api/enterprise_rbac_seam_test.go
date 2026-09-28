// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestRBACCatalog_ServedInOSS pins that GET /rbac/catalog returns the authoring
// vocabulary even on OSS (zero-value Server, no enterprise handler): the role
// editor's typed pickers must work without the enterprise RBAC handler wired.
func TestRBACCatalog_ServedInOSS(t *testing.T) {
	s := &Server{} // OSS: no enterprise RBAC handler
	r := newRBACSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/rbac/catalog", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Scopes        []string `json:"scopes"`
		ResourceTypes []string `json:"resource_types"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.Scopes, "*", "catalog must offer the wildcard scope")
	assert.Contains(t, body.Scopes, "agents:read", "catalog must include known scopes")
	assert.Contains(t, body.ResourceTypes, "agent", "catalog must include known resource types")
	assert.NotContains(t, body.ResourceTypes, "", "resource types must not include the empty class")
}

// TestRBACCatalog_ServedEvenWhenEnterpriseWired confirms the catalog branch wins
// over the enterprise wildcard handler (it must not be swallowed by it).
func TestRBACCatalog_ServedEvenWhenEnterpriseWired(t *testing.T) {
	stub := &stubRBACHandler{}
	s := &Server{}
	s.SetEnterpriseRBACHandler(stub)
	r := newRBACSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/rbac/catalog", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "resource_types")
	assert.Empty(t, stub.sawPath, "catalog must be served locally, not delegated to the enterprise handler")
}
