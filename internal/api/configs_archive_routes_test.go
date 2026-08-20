// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/events"
)

// configs_archive_routes_test.go — dogfood guard for the config soft-archive
// slice. The route-mount gap has bitten this codebase twice (automations #44,
// adopt): a handler can be fully implemented and its unit tests green while the
// route is simply absent from the router the release binary serves. These tests
// build the server the SAME way cmd/all-in-one/main.go does — through NewServer
// — and assert the archive/unarchive routes are actually present on the real
// router's route table.

// TestConfigArchiveRoutesMountedOnRealServer asserts POST
// /api/v1/configs/:id/archive and /api/v1/configs/:id/unarchive are mounted on
// the real router the entrypoint builds, alongside the pre-existing config CRUD.
// Using router.Routes() (rather than a live request) isolates "is the route
// mounted" from handler-level status codes, and needs no wired services.
func TestConfigArchiveRoutesMountedOnRealServer(t *testing.T) {
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

	got := map[string]bool{}
	for _, r := range srv.router.Routes() {
		got[r.Method+" "+r.Path] = true
	}

	want := []string{
		http.MethodPost + " /api/v1/configs/:id/archive",
		http.MethodPost + " /api/v1/configs/:id/unarchive",
	}
	for _, route := range want {
		assert.Truef(t, got[route],
			"route %q must be mounted on the real router the release binary builds", route)
	}
}
