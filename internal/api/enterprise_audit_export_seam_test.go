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

// enterprise_audit_export_seam_test.go — ADR 0020 slice 6a. Pins the
// /api/v1/audit-export/* route seam: OSS (no handler wired) returns 404; a wired
// handler serves and receives the path suffix. Export is enterprise-reserved
// (ADR 0001: "compliance: SOC 2 evidence, access reviews").

func newAuditExportSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountEnterpriseAuditExport(grp)
	return r
}

func TestEnterpriseAuditExportSeam_OSS404(t *testing.T) {
	// Zero-value Server = OSS edition: no audit-export handler injected.
	s := &Server{}
	r := newAuditExportSeamRouter(s)

	// The bare prefix (no trailing slash) 301-redirects to the catch-all form
	// (gin RedirectTrailingSlash); test the served forms.
	for _, path := range []string{"/api/v1/audit-export/", "/api/v1/audit-export/anything"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		assert.Contains(t, w.Body.String(), "enterprise feature", path)
	}
}

// stubAuditExportHandler stands in for the enterprise handler, recording the
// path suffix it received.
type stubAuditExportHandler struct{ sawPath string }

func (h *stubAuditExportHandler) HandleAuditExport(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseAuditExportSeam_ServesWhenWired(t *testing.T) {
	stub := &stubAuditExportHandler{}
	s := &Server{}
	s.SetEnterpriseAuditExportHandler(stub)
	r := newAuditExportSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit-export/", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/", stub.sawPath, "handler must receive the path suffix after /audit-export")
}
