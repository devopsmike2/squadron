// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// smokePerfBudget is the per-endpoint latency ceiling for the headline-surface
// smoke gate. The harness store is empty/in-memory, so every headline endpoint
// should answer near-instantly; a multi-second response means a handler is
// hanging (e.g. an unbounded query — the class of regression that took
// /pipeline-health/fleet to ~18s before it was time-bounded). The budget is
// deliberately generous so it flags gross hangs without flaking on slow CI.
const smokePerfBudget = 5 * time.Second

// TestSmokeHeadlineEndpoints is the GA smoke gate: every headline GET surface
// that a new user / demo hits on first load must return 200 and answer within
// smokePerfBudget. It runs in the existing `go test ./integration/...` CI job,
// so a change that makes one of these surfaces 5xx, panic, hang, or disappear
// fails the PR before it can ship.
//
// Scope note: these are the surfaces the integration harness wires
// (agents/configs/groups/alerts/audit/rollouts/telemetry + pipeline-health +
// the static quickstart endpoints). Discovery/savings/recommendations require
// cloud credentials + their own stores and are exercised by the live e2e
// passes, not this offline gate.
func TestSmokeHeadlineEndpoints(t *testing.T) {
	ts := NewTestServer(t, true)
	defer ts.Stop()
	ts.Start()

	endpoints := []string{
		"/health",
		"/api/v1/agents",
		"/api/v1/agents/stats",
		"/api/v1/configs",
		"/api/v1/groups",
		"/api/v1/alerts/rules",
		"/api/v1/audit/events",
		"/api/v1/rollouts",
		"/api/v1/telemetry/overview",
		"/api/v1/telemetry/services",
		"/api/v1/topology",
		"/api/v1/pipeline-health/fleet",
		"/api/v1/quickstart/backends",
		"/api/v1/quickstart/opamp-snippet",
	}

	for _, path := range endpoints {
		path := path
		t.Run(path, func(t *testing.T) {
			start := time.Now()
			resp, err := ts.GET(path)
			elapsed := time.Since(start)
			require.NoError(t, err, "GET %s should not error", path)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode,
				"GET %s should return 200 (a 5xx/503 means a headline surface is broken or unwired)", path)
			assert.Less(t, elapsed, smokePerfBudget,
				"GET %s took %s (> %s budget) — a headline surface is hanging", path, elapsed, smokePerfBudget)
		})
	}
}
