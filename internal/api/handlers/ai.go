// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
)

// AIHandlers wraps the v0.26 AI service in HTTP shells. Thin —
// the service owns the prompts, the handlers own request
// validation and JSON wiring.
type AIHandlers struct {
	svc    *ai.Service
	logger *zap.Logger
}

func NewAIHandlers(svc *ai.Service, logger *zap.Logger) *AIHandlers {
	return &AIHandlers{svc: svc, logger: logger}
}

// HandleStatus — GET /api/v1/ai/status
//
// Returns whether AI is enabled + which models are wired. The UI
// hits this on app load to decide which AI buttons to render.
// Always 200 — even when AI is disabled, the body is the
// authoritative answer for the UI's capability gating.
func (h *AIHandlers) HandleStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.svc.Capabilities())
}

// HandleExplainSnippet — POST /api/v1/ai/explain
func (h *AIHandlers) HandleExplainSnippet(c *gin.Context) {
	var req ai.ExplainSnippetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := h.svc.ExplainSnippet(c.Request.Context(), req)
	if err != nil {
		h.writeAIError(c, err, "ExplainSnippet")
		return
	}
	c.JSON(http.StatusOK, resp)
}

// HandleMergeIntoConfig — POST /api/v1/ai/merge
func (h *AIHandlers) HandleMergeIntoConfig(c *gin.Context) {
	var req ai.MergeIntoConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := h.svc.MergeIntoConfig(c.Request.Context(), req)
	if err != nil {
		h.writeAIError(c, err, "MergeIntoConfig")
		return
	}
	c.JSON(http.StatusOK, resp)
}

// HandleExplainConfig — POST /api/v1/ai/explain-config
func (h *AIHandlers) HandleExplainConfig(c *gin.Context) {
	var req ai.ExplainConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := h.svc.ExplainConfig(c.Request.Context(), req)
	if err != nil {
		h.writeAIError(c, err, "ExplainConfig")
		return
	}
	c.JSON(http.StatusOK, resp)
}

// HandleFleetQuery — POST /api/v1/ai/fleet-query
//
// Translates a plain-English fleet query into structured filter
// params the UI can apply. v0.44 addition.
func (h *AIHandlers) HandleFleetQuery(c *gin.Context) {
	var req ai.FleetQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := h.svc.TranslateFleetQuery(c.Request.Context(), req)
	if err != nil {
		h.writeAIError(c, err, "FleetQuery")
		return
	}
	c.JSON(http.StatusOK, resp)
}

// HandleRemediateLint — POST /api/v1/ai/remediate-lint
//
// Takes a YAML config + lint findings and returns a remediated YAML
// with a one-line summary and a list of any findings the model
// declined to fix. v0.44 addition.
func (h *AIHandlers) HandleRemediateLint(c *gin.Context) {
	var req ai.RemediateLintRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := h.svc.RemediateLintWarnings(c.Request.Context(), req)
	if err != nil {
		h.writeAIError(c, err, "RemediateLint")
		return
	}
	c.JSON(http.StatusOK, resp)
}

// writeAIError maps service-layer errors to HTTP status codes.
// ErrDisabled becomes a 503 (not enabled / no key); everything
// else becomes 500 with the message surfaced verbatim so the UI
// can render the actual Anthropic error to the operator (rate
// limit, bad key, model unavailable, etc.).
func (h *AIHandlers) writeAIError(c *gin.Context, err error, op string) {
	if errors.Is(err, ai.ErrDisabled) {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "AI assist is not configured (set ANTHROPIC_API_KEY and enable in config)",
			"enabled": false,
		})
		return
	}
	h.logger.Warn(op+" failed", zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}
