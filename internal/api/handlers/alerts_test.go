// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func setupAlertHandlersTest(t *testing.T) (*AlertHandlers, services.AlertService) {
	t.Helper()
	svc := services.NewAlertService(memory.NewStore(), zap.NewNop())
	return NewAlertHandlers(svc, zap.NewNop()), svc
}

func validAlertInput() services.AlertRuleInput {
	return services.AlertRuleInput{
		Name:              "errors over threshold",
		Description:       "fires when error logs exceed 100",
		Query:             `logs{severity="ERROR"}`,
		ThresholdOperator: services.ThresholdGreater,
		ThresholdValue:    100,
		IntervalSeconds:   60,
		Severity:          services.AlertSeverityWarning,
		Enabled:           true,
		WebhookURL:        "https://example.com/hook",
	}
}

func TestAlertHandlers_CreateAndList(t *testing.T) {
	h, _ := setupAlertHandlersTest(t)

	// Create
	body, _ := json.Marshal(validAlertInput())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleCreateAlertRule(c)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	var created services.AlertRule
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)

	// List
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/alerts/rules", nil)
	h.HandleListAlertRules(c)
	require.Equal(t, http.StatusOK, w.Code)

	var listResp struct {
		Rules []services.AlertRule `json:"rules"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listResp))
	assert.Len(t, listResp.Rules, 1)
	assert.Equal(t, created.ID, listResp.Rules[0].ID)
}

func TestAlertHandlers_CreateValidationError(t *testing.T) {
	h, _ := setupAlertHandlersTest(t)

	bad := validAlertInput()
	bad.Name = "" // forces validation error
	body, _ := json.Marshal(bad)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/alerts/rules", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleCreateAlertRule(c)

	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var errResp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
	assert.Contains(t, errResp["error"], "name is required")
}

func TestAlertHandlers_GetMissing(t *testing.T) {
	h, _ := setupAlertHandlersTest(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/alerts/rules/nope", nil)
	c.Params = gin.Params{{Key: "id", Value: "nope"}}
	h.HandleGetAlertRule(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAlertHandlers_UpdateAndDelete(t *testing.T) {
	h, svc := setupAlertHandlersTest(t)

	created, err := svc.CreateAlertRule(t.Context(), validAlertInput())
	require.NoError(t, err)

	// Update
	input := validAlertInput()
	input.Name = "renamed"
	body, _ := json.Marshal(input)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/alerts/rules/"+created.ID, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: created.ID}}
	h.HandleUpdateAlertRule(c)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var updated services.AlertRule
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.Equal(t, "renamed", updated.Name)

	// Delete. Verify the rule is actually gone from the store rather than
	// asserting on w.Code — Gin's response writer is lazy in test contexts,
	// so c.Status(204) doesn't flush to the recorder unless we also call
	// WriteHeaderNow. The behavior that matters in production is the store
	// state, which we check directly.
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/alerts/rules/"+created.ID, nil)
	c.Params = gin.Params{{Key: "id", Value: created.ID}}
	h.HandleDeleteAlertRule(c)

	got, err := svc.GetAlertRule(t.Context(), created.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "rule should be deleted from store")
}
