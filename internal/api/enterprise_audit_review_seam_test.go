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

// enterprise_audit_review_seam_test.go — ADR 0020 slice 6c. Pins the
// /api/v1/audit-review/* route seam: OSS (no handler wired) returns 404; a wired
// handler serves and receives the path suffix. Access review is enterprise-
// reserved (ADR 0001: "compliance: SOC 2 evidence, access reviews").

func newAuditReviewSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountEnterpriseAuditReview(grp)
	return r
}

func TestEnterpriseAuditReviewSeam_OSS404(t *testing.T) {
	// Zero-value Server = OSS edition: no audit-review handler injected.
	s := &Server{}
	r := newAuditReviewSeamRouter(s)

	for _, path := range []string{"/api/v1/audit-review/", "/api/v1/audit-review/anything"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
		assert.Contains(t, w.Body.String(), "enterprise feature", path)
	}
}

// stubAuditReviewHandler stands in for the enterprise handler, recording the
// path suffix it received.
type stubAuditReviewHandler struct{ sawPath string }

func (h *stubAuditReviewHandler) HandleAuditReview(c *gin.Context) {
	h.sawPath = c.Param("path")
	c.JSON(http.StatusOK, gin.H{"handled": true})
}

func TestEnterpriseAuditReviewSeam_ServesWhenWired(t *testing.T) {
	stub := &stubAuditReviewHandler{}
	s := &Server{}
	s.SetEnterpriseAuditReviewHandler(stub)
	r := newAuditReviewSeamRouter(s)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/audit-review/", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "handled")
	assert.Equal(t, "/", stub.sawPath, "handler must receive the path suffix after /audit-review")
}
