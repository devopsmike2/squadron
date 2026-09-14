// Copyright (c) 2026 Squadron Contributors
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

// createTokenAsActor issues a POST /api/v1/auth/tokens for the given label with
// an authenticated actor carrying the given scopes stamped on the request
// context (mirroring what RequireBearer does upstream), and returns the recorder.
func createTokenAsActor(t *testing.T, h *AuthHandlers, label string, scopes []string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(CreateTokenRequest{Label: label, Scopes: []string{"*"}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := services.WithActor(req.Context(), services.AuthActor{TokenID: "caller", Scopes: scopes})
	c.Request = req.WithContext(ctx)
	h.HandleCreateToken(c)
	return w
}

// TestHandleCreateToken_ScopeGatedLabel locks the ADR 0052 mint guard: with the
// `pin:` prefix gated on agents:write (as the enterprise wire does at startup),
// a caller WITHOUT agents:write cannot mint a pin: token, a caller WITH it can,
// and a non-pin label is unaffected. With no gated prefixes (OSS default) the
// pin: label is allowed — the seam is inert in OSS.
func TestHandleCreateToken_ScopeGatedLabel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	h := NewAuthHandlers(svc, zap.NewNop())

	services.SetScopeGatedTokenLabelPrefixes(nil)
	t.Cleanup(func() { services.SetScopeGatedTokenLabelPrefixes(nil) })

	// OSS default (no gated prefixes): pin: label allowed even for a caller with
	// no scopes — inert seam.
	if w := createTokenAsActor(t, h, "pin:fleet-1", nil); w.Code != http.StatusCreated {
		t.Fatalf("inert seam: expected 201 for pin: label, got %d: %s", w.Code, w.Body.String())
	}

	// Enterprise activation: gate pin: on agents:write.
	services.SetScopeGatedTokenLabelPrefixes(map[string]string{"pin:": services.ScopeAgentsWrite})

	// Caller lacks agents:write -> 403.
	w := createTokenAsActor(t, h, "pin:fleet-1", []string{services.ScopeAgentsRead})
	require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), services.ScopeAgentsWrite)

	// Caller holds agents:write -> allowed.
	w = createTokenAsActor(t, h, "pin:fleet-1", []string{services.ScopeAgentsWrite})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	// Wildcard caller -> allowed.
	w = createTokenAsActor(t, h, "pin:fleet-2", []string{"*"})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	// Non-gated label is unaffected even for a scopeless caller.
	w = createTokenAsActor(t, h, "ci-runner", nil)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
}
