// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// createToken issues a POST /api/v1/auth/tokens with the given label through
// the real handler and returns the recorder.
func createToken(t *testing.T, h *AuthHandlers, label string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(CreateTokenRequest{Label: label, Scopes: []string{"*"}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleCreateToken(c)
	return w
}

// TestHandleCreateToken_ReservedLabel locks ADR 0013 D1: with the reserved set
// populated (as the enterprise wire does at startup), a create request for a
// reserved label is rejected; a normal label is allowed; and with the set empty
// (the OSS default), the reserved label is allowed — the seam is inert in OSS.
func TestHandleCreateToken_ReservedLabel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	h := NewAuthHandlers(svc, zap.NewNop())

	// Ensure a clean, inert default and restore it after the test so the
	// process-wide seam doesn't leak into other tests in the package.
	services.SetReservedTokenLabels(nil)
	t.Cleanup(func() { services.SetReservedTokenLabels(nil) })

	// OSS default (empty reserved set): `bootstrap` is allowed — inert seam.
	if w := createToken(t, h, "bootstrap"); w.Code != http.StatusCreated {
		t.Fatalf("inert seam: expected 201 for bootstrap label, got %d: %s", w.Code, w.Body.String())
	}

	// Enterprise activation: reserve `bootstrap`.
	services.SetReservedTokenLabels([]string{"bootstrap"})

	// Reserved label (exact) -> rejected.
	w := createToken(t, h, "bootstrap")
	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "reserved")

	// Reserved label (different case, with surrounding whitespace) -> rejected.
	w = createToken(t, h, "  Bootstrap ")
	require.Equal(t, http.StatusForbidden, w.Code, "case-insensitive+trim bypass: body: %s", w.Body.String())

	// Normal label -> allowed.
	w = createToken(t, h, "ci-runner")
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
}
