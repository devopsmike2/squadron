// Copyright (c) 2026 Squadron Contributors
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

// enterprise_license_seam_test.go — ADR 0032 S1. The /api/v1/license/status
// endpoint ALWAYS serves: OSS (no provider) reports edition "oss"; a wired
// enterprise provider reports its view.

func newLicenseSeamRouter(s *Server) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	grp := r.Group("/api/v1")
	s.mountLicenseStatus(grp)
	return r
}

func TestLicenseStatusSeam_OSSDefault(t *testing.T) {
	s := &Server{} // zero value = OSS
	r := newLicenseSeamRouter(s)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/license/status", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var v LicenseStatusView
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	assert.Equal(t, "oss", v.Edition)
}

type stubLicenseStatus struct{ v LicenseStatusView }

func (s stubLicenseStatus) LicenseStatus() LicenseStatusView { return s.v }

func TestLicenseStatusSeam_EnterpriseView(t *testing.T) {
	s := &Server{}
	s.SetLicenseStatusProvider(stubLicenseStatus{v: LicenseStatusView{
		Edition: "enterprise", State: "valid", Customer: "Acme", Plan: "business",
	}})
	r := newLicenseSeamRouter(s)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/license/status", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var v LicenseStatusView
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &v))
	assert.Equal(t, "enterprise", v.Edition)
	assert.Equal(t, "Acme", v.Customer)
}
