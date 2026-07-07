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

// enterprise_budgets_seam_test.go pins the /api/v1/budgets/* route seam (ADR
// 0026): OSS (no handler wired) returns 404; a wired handler serves and receives
// the path suffix. Runtime per-tenant trace budgets are enterprise-reserved.

func newBudgetsSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountEnterpriseBudgets(grp)
	return r
}

func TestEnterpriseBudgetsSeam_OSS404(t *testing.T) {
	s := &Server{}
	r := newBudgetsSeamRouter(s)
	for _, path := range []string{"/api/v1/budgets/", "/api/v1/budgets/anything", "/api/v1/budgets/tenants/acme"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		assert.Contains(t, w.Body.String(), "enterprise feature", path)
	}
}

type stubBudgetsHandler struct{ sawPath string }

func (h *stubBudgetsHandler) HandleBudgets(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseBudgetsSeam_ServesWhenWired(t *testing.T) {
	stub := &stubBudgetsHandler{}
	s := &Server{}
	s.SetEnterpriseBudgetsHandler(stub)
	r := newBudgetsSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/budgets/tenants/acme", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/tenants/acme", stub.sawPath)
}
