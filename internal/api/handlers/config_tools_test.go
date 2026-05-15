// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/devopsmike2/squadron/internal/testutils"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func setupConfigHandlers(t *testing.T) *ConfigHandlers {
	t.Helper()
	return NewConfigHandlers(testutils.NewMockAgentService(), &mockConfigSender{}, zap.NewNop())
}

func TestHandleLintConfig_ReturnsFindings(t *testing.T) {
	h := setupConfigHandlers(t)

	// Bad config: pipeline references a typo'd receiver name and no batch.
	body := []byte(`{"content":"receivers:\n  otlp:\nexporters:\n  otlp:\n    endpoint: backend:4317\nservice:\n  pipelines:\n    traces:\n      receivers: [otpl]\n      exporters: [otlp]\n"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/configs/lint", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleLintConfig(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp LintConfigResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Findings, "expected lint to surface findings on a typo'd config")

	rules := map[string]bool{}
	for _, f := range resp.Findings {
		rules[f.Rule] = true
	}
	assert.True(t, rules["undefined-component"], "should catch the receiver typo")
	assert.True(t, rules["missing-batch-processor"], "should warn about missing batch")
}

func TestHandleLintConfig_CleanConfigReturnsEmptyArray(t *testing.T) {
	h := setupConfigHandlers(t)

	clean := []byte(`{"content":"receivers:\n  otlp:\nprocessors:\n  memory_limiter:\n    limit_mib: 512\n  batch:\nexporters:\n  otlp:\n    endpoint: backend.svc:4317\nservice:\n  pipelines:\n    traces:\n      receivers: [otlp]\n      processors: [memory_limiter, batch]\n      exporters: [otlp]\n"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/configs/lint", bytes.NewReader(clean))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleLintConfig(c)

	require.Equal(t, http.StatusOK, w.Code)
	var resp LintConfigResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	// Findings must be the empty array, not null — the UI depends on
	// .length, and JSON null would break it.
	assert.NotNil(t, resp.Findings, "findings field should always be a non-nil array")
	assert.Empty(t, resp.Findings)
}

func TestHandleLintConfig_RejectsMissingBody(t *testing.T) {
	h := setupConfigHandlers(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/configs/lint", bytes.NewReader([]byte(`{}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.HandleLintConfig(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleGetConfigTemplates(t *testing.T) {
	h := setupConfigHandlers(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/configs/templates", nil)
	h.HandleGetConfigTemplates(c)

	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Templates []map[string]any `json:"templates"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Templates, "expected at least one curated template")

	// The first template should be the basic OTLP relay — it's the
	// natural starting point that the UI should default to.
	assert.Equal(t, "basic-otlp-relay", resp.Templates[0]["id"])
	assert.NotEmpty(t, resp.Templates[0]["yaml"], "templates must carry YAML body")
}

func TestHandleGetConfigTemplate_Found(t *testing.T) {
	h := setupConfigHandlers(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/configs/templates/basic-otlp-relay", nil)
	c.Params = gin.Params{{Key: "id", Value: "basic-otlp-relay"}}
	h.HandleGetConfigTemplate(c)

	require.Equal(t, http.StatusOK, w.Code)
	var tmpl map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tmpl))
	assert.Equal(t, "basic-otlp-relay", tmpl["id"])
}

func TestHandleGetConfigTemplate_NotFound(t *testing.T) {
	h := setupConfigHandlers(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/configs/templates/does-not-exist", nil)
	c.Params = gin.Params{{Key: "id", Value: "does-not-exist"}}
	h.HandleGetConfigTemplate(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}
