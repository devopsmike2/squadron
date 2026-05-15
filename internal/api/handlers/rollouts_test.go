// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

func TestHandleListAbortCriteriaRecipes_ReturnsAll(t *testing.T) {
	// Handler doesn't need a rolloutService for this endpoint — the
	// cookbook is static. Pass nil; the test would catch a regression
	// if the handler ever started reaching for the service.
	h := NewRolloutHandlers(nil, zap.NewNop())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/rollout-recipes/abort-criteria", nil)
	h.HandleListAbortCriteriaRecipes(c)

	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Recipes []services.AbortCriteriaRecipe `json:"recipes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// Endpoint should return the full cookbook in source order so the
	// UI's "most-conservative-first" rendering is preserved.
	expected := services.AbortCriteriaRecipes()
	require.Len(t, resp.Recipes, len(expected))
	for i, got := range resp.Recipes {
		assert.Equal(t, expected[i].ID, got.ID, "recipe %d id should match source order", i)
		assert.Equal(t, expected[i].Name, got.Name)
		assert.Equal(t, expected[i].Criteria, got.Criteria)
	}
}
