// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// automations_routes_test.go — regression guard for the release-binary route
// mount gap (ADR 0038). On the deployed all-in-one image (git 63c12910 /
// v0.89.474) every /api/v1/automations call returned 404, even though the
// evaluator, store, migrations, and handlers all shipped. Root cause:
// registerRoutes captured a nil automationService at construction because
// cmd/all-in-one calls SetAutomations AFTER NewServer, so the eager
// construction-time nil check left the whole route group unmounted. #41's tests
// exercised the handlers/store/evaluator directly and never the mounted route on
// the real server, so CI stayed green.
//
// These tests build the server the SAME way cmd/all-in-one does — NewServer(...)
// then SetAutomations(...) — and assert the routes are present on the REAL router
// the entrypoint serves, not a hand-rolled parallel test router.

// newAutomationsTestServer builds a Server through the real NewServer route-table
// construction, exactly as cmd/all-in-one/main.go does. Auth is disabled so
// RequireScope is a pass-through and a MOUNTED route returns 200 while an
// UNMOUNTED one returns a routing 404. telemetryService is nil, which keeps this
// test free of the DuckDB/cgo telemetry stack. When wire is true the automation
// service is wired via SetAutomations AFTER NewServer — reproducing the real
// wiring order that exposed the bug.
func newAutomationsTestServer(t *testing.T, wire bool) *Server {
	t.Helper()
	logger := zap.NewNop()
	srv := NewServer(
		nil, // agentService
		nil, // telemetryService (nil keeps this test cgo/duckdb-free)
		nil, // savedQueryService
		nil, // alertService
		nil, // auditService
		nil, // rolloutService
		nil, // authService
		AuthConfig{Enabled: false},
		nil, // commander
		events.NewBroker(),
		nil, // configsTracer
		prometheus.NewRegistry(),
		logger,
	)
	if wire {
		// Mirrors cmd/all-in-one/main.go: SetAutomations is called AFTER NewServer.
		srv.SetAutomations(services.NewAutomationService(memory.NewStore(), logger))
	}
	return srv
}

// TestAutomationsRouteMountedOnRealServer is the anti-regression guard: after the
// real NewServer -> SetAutomations sequence, GET /api/v1/automations must be
// reachable on the router the release binary serves. It FAILS on pre-fix main
// with 404 (the route group was never mounted) and PASSES after the mount fix
// with 200 + an empty list.
func TestAutomationsRouteMountedOnRealServer(t *testing.T) {
	srv := newAutomationsTestServer(t, true)

	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/automations", nil))

	require.NotEqual(t, http.StatusNotFound, w.Code,
		"/api/v1/automations must be MOUNTED on the real server after SetAutomations; a 404 means the route group is absent from the router the release binary builds")
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Automations []json.RawMessage `json:"automations"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(t, body.Automations, "a fresh store lists zero automations")
}

// TestAutomationsFullGroupMountedOnRealServer asserts the WHOLE slice-1 CRUD
// surface (list/create/get/update/enable/delete) is present in the real router's
// route table, not just the list endpoint. Using router.Routes() rather than
// per-request status codes avoids conflating a handler-level 404 (unknown id)
// with a routing 404 (route absent) — the property under test is purely "is the
// route mounted".
func TestAutomationsFullGroupMountedOnRealServer(t *testing.T) {
	srv := newAutomationsTestServer(t, true)

	got := map[string]bool{}
	for _, r := range srv.router.Routes() {
		got[r.Method+" "+r.Path] = true
	}

	want := []string{
		http.MethodGet + " /api/v1/automations",
		http.MethodPost + " /api/v1/automations",
		http.MethodGet + " /api/v1/automations/:id",
		http.MethodPut + " /api/v1/automations/:id",
		http.MethodPost + " /api/v1/automations/:id/enable",
		http.MethodDelete + " /api/v1/automations/:id",
	}
	for _, route := range want {
		assert.Truef(t, got[route],
			"route %q must be mounted on the real router the release binary builds", route)
	}
}

// TestAutomationsRouteMountedWhenServiceUnwired pins the trampoline contract: the
// route is present regardless of wiring order, returning a clean 503 (not a 404)
// when the service is absent — so an operator sees "unavailable", not "route
// doesn't exist", and the mount can never silently disappear again.
func TestAutomationsRouteMountedWhenServiceUnwired(t *testing.T) {
	srv := newAutomationsTestServer(t, false)

	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/automations", nil))

	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"an unwired automations service must 503 on a MOUNTED route, never 404")
	assert.NotEqual(t, http.StatusNotFound, w.Code)
}
