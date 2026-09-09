// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/aicredstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// newAICredentialTestServer builds the real server with auth ENABLED and the
// AI-credential substrate wired the same way cmd/all-in-one does. Returns the
// server plus the auth service so the test can mint scoped tokens.
func newAICredentialTestServer(t *testing.T) (*Server, services.AuthService) {
	t.Helper()
	logger := zap.NewNop()
	authSvc := services.NewAuthService(memory.NewStore(), logger)
	srv := NewServer(
		nil, // agentService
		nil, // telemetryService (nil keeps this test duckdb/cgo-free)
		nil, // savedQueryService
		nil, // alertService
		nil, // auditService
		nil, // rolloutService
		authSvc,
		AuthConfig{Enabled: true},
		nil, // commander
		events.NewBroker(),
		nil, // configsTracer
		prometheus.NewRegistry(),
		logger,
	)

	// Mirror main.go: AI service, then credstore key + AI credential store.
	srv.SetAIService(ai.NewService(ai.Config{Enabled: true}, logger))

	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	key, err := credstore.NewKey(raw)
	require.NoError(t, err)
	srv.SetDiscoveryCredKey(key)

	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "aicred.db"))
	require.NoError(t, err)
	store, err := aicredstore.NewSQLiteStore(context.Background(), db, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	srv.SetAICredentialStore(store)

	return srv, authSvc
}

func aiToken(t *testing.T, authSvc services.AuthService, scopes ...string) string {
	t.Helper()
	_, plaintext, err := authSvc.Issue(context.Background(), "ai-cred-test", scopes, nil)
	require.NoError(t, err)
	return plaintext
}

// TestAICredential_RequiresAIWriteScope pins the authorization contract: a
// token WITHOUT ai:write is forbidden on PUT and DELETE, and a token WITH
// ai:write is allowed.
func TestAICredential_RequiresAIWriteScope(t *testing.T) {
	srv, authSvc := newAICredentialTestServer(t)

	readOnly := aiToken(t, authSvc, services.ScopeAgentsRead)
	writer := aiToken(t, authSvc, services.ScopeAIWrite)

	cases := []struct {
		name   string
		method string
		body   string
	}{
		{"put", http.MethodPut, `{"api_key":"tk-test-abcd1234","provider":"anthropic"}`},
		{"delete", http.MethodDelete, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name+"_forbidden_without_scope", func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/api/v1/ai/credential", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+readOnly)
			req.Header.Set("Content-Type", "application/json")
			srv.router.ServeHTTP(w, req)
			require.Equal(t, http.StatusForbidden, w.Code,
				"a token without ai:write must be 403 on %s /ai/credential", tc.method)
		})

		t.Run(tc.name+"_allowed_with_scope", func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/api/v1/ai/credential", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+writer)
			req.Header.Set("Content-Type", "application/json")
			srv.router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code,
				"a token with ai:write must be allowed on %s /ai/credential (got %d: %s)",
				tc.method, w.Code, w.Body.String())
			// The full key is write-only — it must never come back in the body.
			require.NotContains(t, w.Body.String(), "tk-test-abcd1234")
		})
	}
}

// TestAICredential_SetThenStatusReportsStored walks the operator flow: PUT a
// key, then GET /ai/status reports key_source=stored + the last-4 hint and
// never the full key.
func TestAICredential_SetThenStatusReportsStored(t *testing.T) {
	srv, authSvc := newAICredentialTestServer(t)
	writer := aiToken(t, authSvc, services.ScopeWildcard)

	put := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/ai/credential",
		strings.NewReader(`{"api_key":"fixture-secret-key-7788","provider":"anthropic"}`))
	putReq.Header.Set("Authorization", "Bearer "+writer)
	putReq.Header.Set("Content-Type", "application/json")
	srv.router.ServeHTTP(put, putReq)
	require.Equal(t, http.StatusOK, put.Code, put.Body.String())

	status := httptest.NewRecorder()
	statusReq := httptest.NewRequest(http.MethodGet, "/api/v1/ai/status", nil)
	statusReq.Header.Set("Authorization", "Bearer "+writer)
	srv.router.ServeHTTP(status, statusReq)
	require.Equal(t, http.StatusOK, status.Code)

	body := status.Body.String()
	require.Contains(t, body, `"key_source":"`+ai.KeySourceStored+`"`)
	require.Contains(t, body, `"key_last4":"7788"`)
	require.NotContains(t, body, "fixture-secret-key-7788")
}

// TestAICredential_Unwired503 asserts the route stays MOUNTED and 503s (never
// 404) when the store isn't wired — matching the trampoline contract.
func TestAICredential_Unwired503(t *testing.T) {
	logger := zap.NewNop()
	authSvc := services.NewAuthService(memory.NewStore(), logger)
	srv := NewServer(
		nil, nil, nil, nil, nil, nil, authSvc,
		AuthConfig{Enabled: true}, nil, events.NewBroker(), nil,
		prometheus.NewRegistry(), logger,
	)
	srv.SetAIService(ai.NewService(ai.Config{Enabled: true}, logger))
	// Deliberately NOT calling SetAICredentialStore.
	writer := aiToken(t, authSvc, services.ScopeAIWrite)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/ai/credential", nil)
	req.Header.Set("Authorization", "Bearer "+writer)
	srv.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotEqual(t, http.StatusNotFound, w.Code)
}
