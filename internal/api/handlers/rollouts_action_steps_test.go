// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

// v0.89.14 (#630) — action runner steps in plans, slice 1.
// Acceptance test #5 from spec §10. The other four acceptance
// tests live in internal/services/rollout_service_action_steps_test.go
// because they're service-layer concerns; #5 has to exercise the
// real Bearer + RequireScope middleware chain so it lives here.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// newActionStepsTestRouter wires the canonical
// /api/v1/rollouts/plans POST chain: RequireBearer → RequireScope
// (rollouts:write) → HandleCreatePlan. Mirrors server.go's
// production wiring exactly so the test exercises the real
// middleware stack rather than calling the handler directly.
func newActionStepsTestRouter(t *testing.T) (*gin.Engine, services.AuthService, *memory.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := memory.NewStore()
	logger := zap.NewNop()
	authSvc := services.NewAuthService(store, logger)
	agentSvc := services.NewAgentService(store, nil, nil, nil, logger)
	rolloutSvc := services.NewRolloutService(store, agentSvc, nil, logger)

	// Seed a runner so action-step plans pass downstream validation.
	require.NoError(t, store.CreateActionRunnerRegistration(t.Context(), &types.ActionRunnerRegistration{
		RunnerID:         "runner-test",
		Hostname:         "host",
		PublicKeyPEM:     "fake",
		CapabilitiesJSON: `[{"type":"restart-systemd-service"}]`,
		RegisteredAt:     time.Now(),
		LastSeenAt:       time.Now(),
	}))
	// Seed a group + config so kind=rollout steps validate.
	require.NoError(t, store.CreateGroup(t.Context(), &types.Group{ID: "g", Name: "G"}))
	gid := "g"
	require.NoError(t, store.CreateConfig(t.Context(), &types.Config{
		ID:        "cfg",
		Name:      "C",
		Content:   "x",
		GroupID:   &gid,
		CreatedAt: time.Now(),
	}))

	h := NewRolloutHandlers(rolloutSvc, logger)
	r := gin.New()
	r.Use(middleware.RequireBearer(authSvc, logger))
	r.POST("/api/v1/rollouts/plans",
		middleware.RequireScope(services.ScopeRolloutsWrite),
		h.HandleCreatePlan)
	return r, authSvc, store
}

// issueToken wraps the auth service's Issue with the test's
// context so the helper reads cleanly.
func issueToken(t *testing.T, authSvc services.AuthService, label string, scopes []string) string {
	t.Helper()
	_, plaintext, err := authSvc.Issue(t.Context(), label, scopes, nil)
	require.NoError(t, err)
	return plaintext
}

// actionStepPlanBody builds the JSON body for a plan whose step 1
// is kind=action. Step 0 is a vanilla rollout — the spec wants the
// scope check to fire whenever ANY step has kind=action, so a
// mixed plan exercises the check the same way a pure-action plan
// would.
func actionStepPlanBody() []byte {
	body := map[string]any{
		"steps": []map[string]any{
			{
				"name":             "Step 0 — rollout",
				"group_id":         "g",
				"target_config_id": "cfg",
				"stages": []map[string]any{
					{"mode": "percent", "percentage": 100, "dwell_seconds": 0},
				},
			},
			{
				"name":     "Step 1 — action",
				"group_id": "g",
				"kind":     "action",
				"action": map[string]any{
					"runner_id":   "runner-test",
					"action_type": "restart-systemd-service",
					"parameters":  map[string]any{"unit_name": "otelcol.service"},
				},
			},
		},
	}
	out, _ := json.Marshal(body)
	return out
}

// rolloutOnlyPlanBody builds a plan with two kind=rollout steps so
// the regression branch (existing rollouts:write-only token keeps
// working when the plan has no action steps) is asserted on the
// real middleware chain.
func rolloutOnlyPlanBody() []byte {
	body := map[string]any{
		"steps": []map[string]any{
			{
				"name":             "Step 0 — rollout",
				"group_id":         "g",
				"target_config_id": "cfg",
				"stages": []map[string]any{
					{"mode": "percent", "percentage": 100, "dwell_seconds": 0},
				},
			},
			{
				"name":             "Step 1 — rollout",
				"group_id":         "g",
				"target_config_id": "cfg",
				"stages": []map[string]any{
					{"mode": "percent", "percentage": 100, "dwell_seconds": 0},
				},
			},
		},
	}
	out, _ := json.Marshal(body)
	return out
}

// #5: create_plan_requires_actions_write_when_kind_action_present
//
// Token with only rollouts:write POSTs a plan whose step 1 is
// kind=action. Assert: 403, error names the missing scope. Same
// plan with token carrying both scopes: 200, plan created. Plan
// with only kind=rollout steps and rollouts:write-only token:
// 200, regression guard for the existing path.
func TestRollout_create_plan_requires_actions_write_when_kind_action_present(t *testing.T) {
	r, authSvc, _ := newActionStepsTestRouter(t)

	t.Run("rollouts:write only + action step → 403", func(t *testing.T) {
		token := issueToken(t, authSvc, "writer", []string{services.ScopeRolloutsWrite})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rollouts/plans",
			bytes.NewReader(actionStepPlanBody()))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusForbidden, w.Code,
			"expected 403 when plan has action steps but token lacks actions:write")
		body := w.Body.String()
		assert.Contains(t, body, "required_scope",
			"403 body must name the missing scope")
		assert.Contains(t, body, services.ScopeActionsWrite,
			"403 body must specifically name actions:write")
	})

	t.Run("rollouts:write + actions:write + action step → 201", func(t *testing.T) {
		token := issueToken(t, authSvc, "writer-with-actions",
			[]string{services.ScopeRolloutsWrite, services.ScopeActionsWrite})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rollouts/plans",
			bytes.NewReader(actionStepPlanBody()))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusCreated, w.Code,
			"plan create with both scopes should succeed (body=%s)", w.Body.String())
		var resp CreatePlanResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.NotEmpty(t, resp.PlanID)
		assert.Equal(t, 2, resp.Count)
	})

	t.Run("rollouts:write only + rollout-only plan → 201 (regression guard)", func(t *testing.T) {
		token := issueToken(t, authSvc, "rollouts-only", []string{services.ScopeRolloutsWrite})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rollouts/plans",
			bytes.NewReader(rolloutOnlyPlanBody()))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		require.Equal(t, http.StatusCreated, w.Code,
			"rollout-only plans must still work with rollouts:write only (body=%s)", w.Body.String())
		var resp CreatePlanResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, 2, resp.Count)
	})

	t.Run("no token → 401 (auth precedes scope)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rollouts/plans",
			bytes.NewReader(actionStepPlanBody()))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code,
			"missing token → 401 from RequireBearer, not the action-step scope check")
	})
}
