// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/costspikes"
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// CostSpikeStore is the narrow storage slice the handlers need.
// Same shape as costspikes.SpikeStore plus the read paths.
type CostSpikeStore interface {
	GetCostSpikeEvent(ctx context.Context, id string) (*storetypes.CostSpikeEvent, error)
	UpdateCostSpikeEvent(ctx context.Context, e *storetypes.CostSpikeEvent) error
	ListCostSpikeEvents(ctx context.Context, filter storetypes.CostSpikeFilter) ([]*storetypes.CostSpikeEvent, error)
}

// CostSpikesHandlers serves the v0.29 cost-spike alerting API.
// Two responsibilities:
//   - List spikes (open / closed / all) for the dashboard banner
//     and Savings page panel.
//   - Acknowledge spikes (operator clicks "got it"). Ack doesn't
//     close the spike — the detector closes when the projection
//     drops back below threshold. Ack just hides the banner.
//
// The detector itself runs in main.go via Detector.Tick; the
// handlers are read-only against its writes.
type CostSpikesHandlers struct {
	store    CostSpikeStore
	detector *costspikes.Detector // optional — when nil, /tick is disabled
}

func NewCostSpikesHandlers(store CostSpikeStore, det *costspikes.Detector) *CostSpikesHandlers {
	return &CostSpikesHandlers{store: store, detector: det}
}

// HandleList — GET /api/v1/alerts/cost-spikes
//
// Query params:
//
//	status — "open" | "closed" | "all" (default "open")
//	limit  — int, defaults to 50
func (h *CostSpikesHandlers) HandleList(c *gin.Context) {
	status := strings.ToLower(strings.TrimSpace(c.Query("status")))
	if status == "" {
		status = "open"
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	out, err := h.store.ListCostSpikeEvents(c.Request.Context(), storetypes.CostSpikeFilter{
		Status: status,
		Limit:  limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if out == nil {
		out = []*storetypes.CostSpikeEvent{}
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  out,
		"count":  len(out),
		"status": status,
	})
}

// HandleAcknowledge — POST /api/v1/alerts/cost-spikes/:id/acknowledge
//
// Records the operator's "I've seen this" without closing the
// spike (the detector closes it auto when projection recovers).
// The Dashboard banner suppresses acknowledged spikes; the
// Savings page still shows them in the panel so the operator
// can re-open the detail if they want.
func (h *CostSpikesHandlers) HandleAcknowledge(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "spike id required"})
		return
	}
	ev, err := h.store.GetCostSpikeEvent(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if ev == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "cost spike not found"})
		return
	}
	if ev.AcknowledgedAt != nil {
		c.JSON(http.StatusOK, ev) // idempotent
		return
	}
	now := time.Now().UTC()
	ev.AcknowledgedAt = &now
	actor := middleware.ActorFromGin(c).String()
	if actor == "" {
		actor = "system"
	}
	ev.AcknowledgedBy = actor
	if err := h.store.UpdateCostSpikeEvent(c.Request.Context(), ev); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ev)
}

// HandleTick — POST /api/v1/alerts/cost-spikes/tick
//
// Forces an immediate detector pass. Useful for tests, for
// operators who want to refresh after applying a fix, and for
// the demo path that needs to provoke an evaluation without
// waiting the full minute. No-op when the detector isn't wired.
func (h *CostSpikesHandlers) HandleTick(c *gin.Context) {
	if h.detector == nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "reason": "detector disabled"})
		return
	}
	if err := h.detector.Tick(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
