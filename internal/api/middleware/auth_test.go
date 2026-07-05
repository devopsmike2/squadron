// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

func newTestRouter(authSvc services.AuthService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequireBearer(authSvc, zap.NewNop()))
	r.GET("/protected", func(c *gin.Context) {
		actor := ActorFromGin(c)
		c.JSON(http.StatusOK, gin.H{"actor": actor.TokenLabel})
	})
	return r
}

func TestRequireBearer_MissingHeader_401(t *testing.T) {
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	r := newTestRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "missing Authorization")
}

func TestRequireBearer_MalformedHeader_401(t *testing.T) {
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	r := newTestRouter(svc)

	cases := []string{
		"sqd_anything-without-bearer",
		"Basic dXNlcjpwYXNz",
		"Bearer",
		"Bearer ",
	}
	for _, h := range cases {
		t.Run(h, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			req.Header.Set("Authorization", h)
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusUnauthorized, w.Code)
		})
	}
}

func TestRequireBearer_UnknownToken_401(t *testing.T) {
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	r := newTestRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer sqd_neverissued")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireBearer_ValidToken_200_AndActorAvailable(t *testing.T) {
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "ci-bot", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	r := newTestRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"actor":"ci-bot"`)
}

func TestRequireBearer_RevokedToken_401(t *testing.T) {
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	token, plaintext, err := svc.Issue(t.Context(), "rotate-me", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)
	require.NoError(t, svc.Revoke(t.Context(), token.ID))

	r := newTestRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireBearer_CaseInsensitiveScheme(t *testing.T) {
	// RFC 7235 says auth schemes are case-insensitive. We assert that
	// here so a curl client lowercasing the scheme still authenticates.
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "ci-bot", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	r := newTestRouter(svc)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		t.Run(scheme, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			req.Header.Set("Authorization", scheme+" "+plaintext)
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
		})
	}
}

func TestRequireBearer_OptionsPassThrough(t *testing.T) {
	// CORS preflight should NOT trip the auth middleware. Browsers send
	// OPTIONS unauthenticated to learn whether the real request will
	// be allowed; rejecting it here would break every browser client.
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	r := newTestRouter(svc)
	r.OPTIONS("/protected", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/protected", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

// strictIdentityCleanup resets all three process-wide toggles the slice-4d
// enforcement reads, so a strict-mode test can't leak into the others (these are
// global setters mirroring the enterprise wire).
func strictIdentityCleanup(t *testing.T) {
	t.Cleanup(func() {
		services.SetStrictIdentitySource(false)
		services.SetReservedTokenLabels(nil)
		services.SetReservedTokenLabelPrefixes(nil)
	})
}

// TestRequireBearer_StrictOff_RawTokenAuthenticates pins the OSS inertness of
// slice 4d: with StrictIdentitySource() false (the OSS default), a raw operator
// token (label `ci-bot`, not linked to any OIDC/SCIM identity) authenticates
// exactly as before — the enforcement block is skipped entirely.
func TestRequireBearer_StrictOff_RawTokenAuthenticates(t *testing.T) {
	strictIdentityCleanup(t)
	// Strict OFF (default) — do not flip the toggle.
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "ci-bot", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	r := newTestRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"actor":"ci-bot"`)
}

// TestRequireBearer_StrictOn_RawToken_401 pins the enterprise-side enforcement:
// with strict on and the reserved allow-set populated (as the enterprise wire
// does), a raw operator token whose label is not a validated identity source is
// rejected. The reject is the EXACT generic bad-token 401 — no provenance leak.
func TestRequireBearer_StrictOn_RawToken_401(t *testing.T) {
	strictIdentityCleanup(t)
	services.SetReservedTokenLabels([]string{"bootstrap"})
	services.SetReservedTokenLabelPrefixes([]string{"oidc:", "scim:"})
	services.SetStrictIdentitySource(true)

	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "ci-bot", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	r := newTestRouter(svc)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	// Indistinguishable from a bad/revoked token 401 — no "identity source" or
	// label echoed in the body.
	assert.Contains(t, w.Body.String(), "invalid or revoked token")
	assert.NotContains(t, w.Body.String(), "ci-bot")
	assert.NotContains(t, w.Body.String(), "identity source")
}

// TestRequireBearer_StrictOn_ValidatedIdentity_200 pins that under strict, a
// token from a validated identity source — bootstrap break-glass (exact) or an
// oidc:/scim: prefixed identity/service token — still authenticates.
func TestRequireBearer_StrictOn_ValidatedIdentity_200(t *testing.T) {
	strictIdentityCleanup(t)
	services.SetReservedTokenLabels([]string{"bootstrap"})
	services.SetReservedTokenLabelPrefixes([]string{"oidc:", "scim:"})
	services.SetStrictIdentitySource(true)

	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	r := newTestRouter(svc)

	// Internal mint (like the OIDC/SCIM/bootstrap paths) bypasses the public
	// reserved-label handler and calls Issue directly.
	for _, label := range []string{"bootstrap", "oidc:alice@example.com", "scim:svc-token"} {
		t.Run(label, func(t *testing.T) {
			_, plaintext, err := svc.Issue(t.Context(), label, []string{services.ScopeWildcard}, nil)
			require.NoError(t, err)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/protected", nil)
			req.Header.Set("Authorization", "Bearer "+plaintext)
			r.ServeHTTP(w, req)

			require.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), label)
		})
	}
}

func TestRequireBearer_ActorAlsoOnRequestContext(t *testing.T) {
	// The middleware must populate BOTH the Gin context (for handlers)
	// and the request's context.Context (for service-layer code that
	// doesn't see Gin). This test exercises the request context path.
	svc := services.NewAuthService(memory.NewStore(), zap.NewNop())
	_, plaintext, err := svc.Issue(t.Context(), "deep-actor", []string{services.ScopeWildcard}, nil)
	require.NoError(t, err)

	r := gin.New()
	r.Use(RequireBearer(svc, zap.NewNop()))
	r.GET("/deep", func(c *gin.Context) {
		// Read via context.Context, not c.Get — this is the path the
		// audit service uses since it lives below the Gin layer.
		actor := services.ActorFromContext(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"actor": actor.TokenLabel})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/deep", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"actor":"deep-actor"`)
}
