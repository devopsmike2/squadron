// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/configlint"
	"github.com/devopsmike2/squadron/internal/configtemplates"
)

// LintConfigRequest is the body of POST /api/v1/configs/lint.
type LintConfigRequest struct {
	Content string `json:"content" binding:"required"`
}

// LintConfigResponse is the body of POST /api/v1/configs/lint. Findings is
// always non-nil so the UI can rely on `.length` without a null check.
type LintConfigResponse struct {
	Findings []configlint.Finding `json:"findings"`
}

// HandleLintConfig runs the structural lint engine over a YAML body and
// returns every finding. This is the endpoint the editor's debounced
// validation panel calls on every keystroke pause.
//
// The existing `/api/v1/configs/validate` endpoint focuses on whether the
// YAML parses and meets a handful of OTel-specific shape checks; this one
// goes further with anti-pattern detection (memory_limiter position,
// missing batch, localhost exporters, undefined component references).
func (h *ConfigHandlers) HandleLintConfig(c *gin.Context) {
	var req LintConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "detail": err.Error()})
		return
	}

	findings := configlint.Lint(req.Content)
	if findings == nil {
		findings = []configlint.Finding{}
	}
	c.JSON(http.StatusOK, LintConfigResponse{Findings: findings})
}

// HandleGetConfigTemplates returns the catalog of curated YAML snippets.
// The UI renders these in a "Templates" dropdown in the config editor
// header.
func (h *ConfigHandlers) HandleGetConfigTemplates(c *gin.Context) {
	templates := configtemplates.All()
	c.JSON(http.StatusOK, gin.H{"templates": templates})
}

// HandleGetConfigTemplate returns a single template by ID. Useful for the
// UI to load just the YAML body on insert without re-fetching the whole
// catalog.
func (h *ConfigHandlers) HandleGetConfigTemplate(c *gin.Context) {
	id := c.Param("id")
	template := configtemplates.Get(id)
	if template == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "template not found", "id": id})
		return
	}
	c.JSON(http.StatusOK, template)
}
