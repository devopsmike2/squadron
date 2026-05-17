// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/insights"
)

// InsightsHandlers wraps the insights.Service in HTTP shells. Thin
// by design — the service owns the query logic, the handler owns
// the request validation and JSON wiring.
type InsightsHandlers struct {
	svc    *insights.Service
	logger *zap.Logger
}

func NewInsightsHandlers(svc *insights.Service, logger *zap.Logger) *InsightsHandlers {
	return &InsightsHandlers{svc: svc, logger: logger}
}

// HandleFleetVolume — GET /api/v1/insights/volume
//
// Query params:
//
//	window = 5m | 1h | 24h   (required-ish; default 1h)
//	signal = traces | metrics | logs  (repeatable, default all)
func (h *InsightsHandlers) HandleFleetVolume(c *gin.Context) {
	win, ok := parseWindow(c)
	if !ok {
		return
	}
	sigs, ok := parseSignals(c)
	if !ok {
		return
	}
	resp, err := h.svc.FleetVolume(c.Request.Context(), win, sigs)
	if err != nil {
		h.logger.Warn("FleetVolume failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// HandleAgentVolume — GET /api/v1/insights/volume/agents/:id
func (h *InsightsHandlers) HandleAgentVolume(c *gin.Context) {
	agentID := c.Param("id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent id required"})
		return
	}
	win, ok := parseWindow(c)
	if !ok {
		return
	}
	resp, err := h.svc.AgentVolume(c.Request.Context(), agentID, win)
	if err != nil {
		h.logger.Warn("AgentVolume failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// HandleTopAgents — GET /api/v1/insights/volume/agents
//
// Query params:
//
//	window = 5m | 1h | 24h
//	limit  = 1..500 (default 20)
func (h *InsightsHandlers) HandleTopAgents(c *gin.Context) {
	win, ok := parseWindow(c)
	if !ok {
		return
	}
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		limit = n
	}
	resp, err := h.svc.TopAgents(c.Request.Context(), win, limit)
	if err != nil {
		h.logger.Warn("TopAgents failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Wrap in an envelope so v0.25 can add total counts without
	// breaking the response shape.
	c.JSON(http.StatusOK, gin.H{"items": resp, "limit": limit})
}

// HandleTopAttributes — GET /api/v1/insights/volume/attributes
//
// Query params:
//
//	window = 5m | 1h | 24h
//	signal = traces | metrics | logs  (required)
//	limit  = 1..100 (default 20)
//
// Returned bytes are estimated (sampled); the response rows carry
// estimated:true so the UI can label them.
func (h *InsightsHandlers) HandleTopAttributes(c *gin.Context) {
	win, ok := parseWindow(c)
	if !ok {
		return
	}
	sigRaw := strings.ToLower(strings.TrimSpace(c.Query("signal")))
	if sigRaw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "signal query param required (traces|metrics|logs)"})
		return
	}
	sig, ok := validSignal(sigRaw)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid signal",
			"allowed": []string{"traces", "metrics", "logs"},
		})
		return
	}
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
			return
		}
		limit = n
	}
	resp, err := h.svc.TopAttributes(c.Request.Context(), win, sig, limit)
	if err != nil {
		h.logger.Warn("TopAttributes failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  resp,
		"signal": sig,
		"limit":  limit,
		// estimated=true at the envelope level so clients that only
		// glance at the response shape see the caveat without
		// inspecting each row.
		"estimated": true,
	})
}

// HandleDrops — GET /api/v1/insights/volume/drops
func (h *InsightsHandlers) HandleDrops(c *gin.Context) {
	win, ok := parseWindow(c)
	if !ok {
		return
	}
	resp, err := h.svc.Drops(c.Request.Context(), win)
	if err != nil {
		h.logger.Warn("Drops failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": resp})
}

// parseWindow extracts the ?window= param with a 1h default. On
// invalid values it 400s and returns ok=false; the caller bails
// without further work.
func parseWindow(c *gin.Context) (insights.Window, bool) {
	raw := c.Query("window")
	if raw == "" {
		return insights.Window1h, true
	}
	switch insights.Window(raw) {
	case insights.Window5m, insights.Window1h, insights.Window24h:
		return insights.Window(raw), true
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "invalid window",
			"allowed": []string{"5m", "1h", "24h"},
		})
		return "", false
	}
}

// parseSignals extracts ?signal= values (repeatable). Empty/missing
// means "all signals" — return nil.
func parseSignals(c *gin.Context) ([]insights.Signal, bool) {
	raws := c.QueryArray("signal")
	if len(raws) == 0 {
		return nil, true
	}
	out := make([]insights.Signal, 0, len(raws))
	for _, r := range raws {
		sig, ok := validSignal(strings.ToLower(strings.TrimSpace(r)))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "invalid signal",
				"allowed": []string{"traces", "metrics", "logs"},
			})
			return nil, false
		}
		out = append(out, sig)
	}
	return out, true
}

func validSignal(s string) (insights.Signal, bool) {
	switch s {
	case "traces":
		return insights.SignalTraces, true
	case "metrics":
		return insights.SignalMetrics, true
	case "logs":
		return insights.SignalLogs, true
	}
	return "", false
}
