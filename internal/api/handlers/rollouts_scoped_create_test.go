// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

// ADR 0053 slice 4b-2b — create-time cluster/env-scoped authorization for
// rollouts. POST /rollouts and /rollouts/plans have no :id at authz time, so the
// rollouts:write check runs in-handler, resource-aware, against the target
// env/cluster derived from the request body. These tests exercise the real
// Bearer chain plus a wired resource-aware Authorizer, and pin the OSS
// (ScopeAuthorizer) behavior as byte-identical to the old route-level check.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// envScopedAuthorizer simulates an enterprise cluster/env-scoped role: it grants
// the required scope only when the Resource's Env matches allowEnv (an empty
// allowEnv means "unscoped, allow any"). A token lacking the flat scope is denied
// regardless, mirroring deny-by-default.
type envScopedAuthorizer struct{ allowEnv string }

func (a envScopedAuthorizer) Authorize(_ context.Context, p identity.Principal, required string, res identity.Resource) identity.Decision {
	hasScope := false
	for _, s := range p.Scopes {
		if s == required || s == "*" {
			hasScope = true
			break
		}
	}
	if !hasScope {
		return identity.Decision{Allow: false, Reason: "missing scope"}
	}
	if a.allowEnv != "" && res.Env != a.allowEnv {
		return identity.Decision{Allow: false, Reason: "env out of scope"}
	}
	return identity.Decision{Allow: true}
}

func newCreateRolloutTestRouter(t *testing.T) (*gin.Engine, services.AuthService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := memory.NewStore()
	logger := zap.NewNop()
	authSvc := services.NewAuthService(store, logger)
	agentSvc := services.NewAgentService(store, nil, nil, nil, logger)
	rolloutSvc := services.NewRolloutService(store, agentSvc, nil, logger)

	require.NoError(t, store.CreateGroup(t.Context(), &types.Group{ID: "g", Name: "G"}))
	gid := "g"
	require.NoError(t, store.CreateConfig(t.Context(), &types.Config{
		ID: "cfg", Name: "C", Content: "x", GroupID: &gid, CreatedAt: time.Now(),
	}))

	h := NewRolloutHandlers(rolloutSvc, logger)
	r := gin.New()
	r.Use(middleware.RequireBearer(authSvc, logger))
	// Mirrors production wiring after 4b-2b: no route-level RequireScope.
	r.POST("/api/v1/rollouts", h.HandleCreateRollout)
	return r, authSvc
}

func createRolloutBody(env string) []byte {
	stage := map[string]any{"mode": "label", "dwell_seconds": 0,
		"label_selector": map[string]string{"deployment.environment": env}}
	out, _ := json.Marshal(map[string]any{
		"name": "r", "group_id": "g", "target_config_id": "cfg",
		"stages": []map[string]any{stage},
	})
	return out
}

func doCreate(t *testing.T, r *gin.Engine, token string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rollouts", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestCreateRollout_OSSAuthorizer_ByteIdentical: under the OSS ScopeAuthorizer the
// in-handler check reproduces the old route-level RequireScope exactly — a
// rollouts:write token creates, a token without it gets 403.
func TestCreateRollout_OSSAuthorizer_ByteIdentical(t *testing.T) {
	middleware.SetAuthEnabled(true)
	t.Cleanup(func() { middleware.SetAuthEnabled(false) })
	middleware.SetAuthorizer(identity.ScopeAuthorizer{})
	t.Cleanup(func() { middleware.SetAuthorizer(identity.ScopeAuthorizer{}) })

	r, authSvc := newCreateRolloutTestRouter(t)
	writer := issueToken(t, authSvc, "writer", []string{services.ScopeRolloutsWrite})
	reader := issueToken(t, authSvc, "reader", []string{services.ScopeRolloutsRead})

	assert.Equal(t, http.StatusCreated, doCreate(t, r, writer, createRolloutBody("prod")),
		"rollouts:write token creates under OSS authorizer (unchanged)")
	assert.Equal(t, http.StatusForbidden, doCreate(t, r, reader, createRolloutBody("prod")),
		"token without rollouts:write is denied (unchanged)")
}

// TestCreateRollout_EnvScoped: a prod-scoped role may create a prod-targeted
// rollout but is denied a staging-targeted one — the create-time enforcement 4b-2b
// adds over the route-level check that could not see the body.
func TestCreateRollout_EnvScoped(t *testing.T) {
	middleware.SetAuthEnabled(true)
	t.Cleanup(func() { middleware.SetAuthEnabled(false) })
	middleware.SetAuthorizer(envScopedAuthorizer{allowEnv: "prod"})
	t.Cleanup(func() { middleware.SetAuthorizer(identity.ScopeAuthorizer{}) })

	r, authSvc := newCreateRolloutTestRouter(t)
	tok := issueToken(t, authSvc, "prod-oncall", []string{services.ScopeRolloutsWrite})

	assert.Equal(t, http.StatusCreated, doCreate(t, r, tok, createRolloutBody("prod")),
		"prod-scoped role creates a prod-targeted rollout")
	assert.Equal(t, http.StatusForbidden, doCreate(t, r, tok, createRolloutBody("staging")),
		"prod-scoped role is denied a staging-targeted rollout at create time")
}
