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

// ProposerPreviewRequest is the JSON wire shape for the playground.
// It mirrors ai.CostSpikeContext (which is an internal Go type
// without JSON tags) so the UI can POST a clean request without
// coupling to internal field layout. v0.84 (Arc C slice 3).
type ProposerPreviewRequest struct {
	SpikeID               string   `json:"spike_id"`
	Signal                string   `json:"signal"`
	Severity              string   `json:"severity"`
	BaselineMonthlyUSD    float64  `json:"baseline_monthly_usd"`
	PeakMonthlyUSD        float64  `json:"peak_monthly_usd"`
	PeakPctAboveBaseline  float64  `json:"peak_pct_above_baseline"`
	TopAgents             []string `json:"top_agents"`
	TopAttributes         []string `json:"top_attributes"`
	GroupID               string   `json:"group_id"`
	GroupName             string   `json:"group_name"`
	RecentLintFindings    []string `json:"recent_lint_findings"`
	RecentRecommendations []string `json:"recent_recommendations"`
}

// ProposerPreviewResponse wraps ai.ProposalResult with derived
// fields the playground UI surfaces directly (estimated USD cost,
// derived "would create" summary). Keeps the playground UI thin —
// the cost math lives in one place server-side.
type ProposerPreviewResponse struct {
	*ai.ProposalResult
	EstimatedUSD float64 `json:"estimated_usd"`
}

// HandleProposerPreview — POST /api/v1/ai/proposer/preview
//
// Non-persisting preview of the proposer's response for the v0.84
// playground UI. Takes a hand-crafted CostSpikeContext, calls
// ProposeFromCostSpike, returns the result + token metering +
// estimated cost. Does NOT create rollouts, plans, audit events,
// or anything else with side effects. Operators use this to:
//   - validate a prompt change before pushing it through CI
//   - demo the proposer's reasoning without seeding a fake spike
//   - dogfood new CostSpikeContext shapes
//
// Sonnet 4.6 pricing matches docs/proposer-bench.md so the cost
// line stays consistent between the bench and the playground.
func (h *AIHandlers) HandleProposerPreview(c *gin.Context) {
	var req ProposerPreviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := ai.CostSpikeContext{
		SpikeID:               req.SpikeID,
		Signal:                req.Signal,
		Severity:              req.Severity,
		BaselineMonthlyUSD:    req.BaselineMonthlyUSD,
		PeakMonthlyUSD:        req.PeakMonthlyUSD,
		PeakPctAboveBaseline:  req.PeakPctAboveBaseline,
		TopAgents:             req.TopAgents,
		TopAttributes:         req.TopAttributes,
		GroupID:               req.GroupID,
		GroupName:             req.GroupName,
		RecentLintFindings:    req.RecentLintFindings,
		RecentRecommendations: req.RecentRecommendations,
	}
	result, err := h.svc.ProposeFromCostSpike(c.Request.Context(), ctx)
	if err != nil {
		h.writeAIError(c, err, "ProposerPreview")
		return
	}
	c.JSON(http.StatusOK, ProposerPreviewResponse{
		ProposalResult: result,
		EstimatedUSD:   estimateProposerCostUSD(result.TokensIn, result.TokensOut),
	})
}

// estimateProposerCostUSD mirrors the bench's cost math at v0.84.
// Sonnet 4.6 pricing: $3/MTok input, $15/MTok output. If pricing
// shifts, update both this helper and the constants in
// cmd/squadron-proposer-bench/main.go so the playground and the
// bench stay synchronized.
func estimateProposerCostUSD(tokensIn, tokensOut int) float64 {
	return float64(tokensIn)/1e6*3.0 + float64(tokensOut)/1e6*15.0
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
