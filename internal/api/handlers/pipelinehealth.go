// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/pipelinehealth"
)

// PipelineHealthHandlers wires the pipeline-health endpoints onto
// Gin. Construct one with NewPipelineHealthHandlers and call its
// Register* methods from the server's route setup.
type PipelineHealthHandlers struct {
	service *pipelinehealth.Service
	logger  *zap.Logger
}

// NewPipelineHealthHandlers creates a handler bound to the given
// service. Pass the same service instance to any background tasks
// (alert evaluator) that consume snapshots.
func NewPipelineHealthHandlers(service *pipelinehealth.Service, logger *zap.Logger) *PipelineHealthHandlers {
	return &PipelineHealthHandlers{service: service, logger: logger}
}

// HandleFleetSummary is GET /api/v1/pipeline-health/fleet. Returns
// the bucketed verdict counts + per-agent verdict map. UI consumes
// this on the Dashboard + Fleet Map.
func (h *PipelineHealthHandlers) HandleFleetSummary(c *gin.Context) {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "pipeline health service not configured"})
		return
	}
	summary, err := h.service.FleetSummary(c.Request.Context())
	if err != nil {
		h.logger.Warn("pipeline-health fleet summary failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, summary)
}

// HandleAgentSnapshot is GET /api/v1/pipeline-health/agents/:agentID.
// Returns the latest sample of every captured metric plus the
// verdict + signals. UI consumes this on the agent detail drawer.
func (h *PipelineHealthHandlers) HandleAgentSnapshot(c *gin.Context) {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "pipeline health service not configured"})
		return
	}
	agentID := c.Param("agentID")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent ID required"})
		return
	}
	snap, err := h.service.AgentSnapshot(c.Request.Context(), agentID)
	if err != nil {
		h.logger.Warn("pipeline-health agent snapshot failed",
			zap.String("agent_id", agentID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, snap)
}

// HandleAgentTimeseries is GET /api/v1/pipeline-health/agents/:agentID/timeseries.
// Query params:
//
//	metric — the otelcol_* metric name (required)
//	labels — optional "key=value;key=value" filter selecting a single
//	         (exporter, receiver) time series within the agent
//	window — go duration string (default 1h)
//
// Returns a list of 1-minute bucketed points. UI consumes this for
// sparklines on the agent detail panel.
func (h *PipelineHealthHandlers) HandleAgentTimeseries(c *gin.Context) {
	if h == nil || h.service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "pipeline health service not configured"})
		return
	}
	agentID := c.Param("agentID")
	metric := c.Query("metric")
	if agentID == "" || metric == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agentID and metric required"})
		return
	}
	window := time.Hour
	if w := c.Query("window"); w != "" {
		if d, err := time.ParseDuration(w); err == nil && d > 0 && d <= 24*time.Hour {
			window = d
		}
	}
	labelsHash := pipelinehealth.LabelHashFromQuery(c.Query("labels"))
	points, err := h.service.Timeseries(c.Request.Context(), agentID, metric, labelsHash, window)
	if err != nil {
		h.logger.Warn("pipeline-health timeseries failed",
			zap.String("agent_id", agentID),
			zap.String("metric", metric),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"points": points,
		"window": window.String(),
	})
}
