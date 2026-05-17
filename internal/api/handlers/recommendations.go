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
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/recommendations"
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// DismissalStore is the narrow slice of ApplicationStore the
// recommendations handlers need. Extracted as an interface to
// keep these handlers testable without standing up SQLite.
type DismissalStore interface {
	DismissRecommendation(ctx context.Context, d *storetypes.RecommendationDismissal) error
	RestoreRecommendation(ctx context.Context, recommendationID string) error
	ListRecommendationDismissals(ctx context.Context) ([]*storetypes.RecommendationDismissal, error)
}

// RecommendationsHandlers wires the v0.25 cost-recommendation
// engine into HTTP. Thin shells — the engine owns the heuristics,
// the handlers own request parsing + dismissal bookkeeping.
type RecommendationsHandlers struct {
	engine *recommendations.Engine
	store  DismissalStore
	logger *zap.Logger
}

func NewRecommendationsHandlers(engine *recommendations.Engine, store DismissalStore, logger *zap.Logger) *RecommendationsHandlers {
	return &RecommendationsHandlers{engine: engine, store: store, logger: logger}
}

// HandleList — GET /api/v1/recommendations
//
// Query params:
//
//	window = 5m | 1h | 24h   (default 1h)
//	limit  = 1..200          (default 50)
func (h *RecommendationsHandlers) HandleList(c *gin.Context) {
	win, ok := parseWindow(c)
	if !ok {
		return
	}
	recs, err := h.engine.Evaluate(c.Request.Context(), win)
	if err != nil {
		h.logger.Warn("Recommendations Evaluate failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	limit := 50
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 200 {
				n = 200
			}
			limit = n
		}
	}
	if len(recs) > limit {
		recs = recs[:limit]
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  recs,
		"window": win,
		"count":  len(recs),
		// Estimates downstream: surface the caveat at the envelope
		// level so any client showing this can render the disclaimer
		// without inspecting each item.
		"estimated": true,
	})
}

// HandleListForAgent — GET /api/v1/recommendations/agents/:id
func (h *RecommendationsHandlers) HandleListForAgent(c *gin.Context) {
	agentID := c.Param("id")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent id required"})
		return
	}
	win, ok := parseWindow(c)
	if !ok {
		return
	}
	recs, err := h.engine.EvaluateForAgent(c.Request.Context(), win, agentID)
	if err != nil {
		h.logger.Warn("Recommendations EvaluateForAgent failed",
			zap.String("agent_id", agentID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items":     recs,
		"window":    win,
		"agent_id":  agentID,
		"count":     len(recs),
		"estimated": true,
	})
}

// dismissRequest is the JSON body for HandleDismiss. Reason is
// optional — operators in a hurry can dismiss without explaining.
type dismissRequest struct {
	Reason string `json:"reason,omitempty"`
}

// HandleDismiss — POST /api/v1/recommendations/:id/dismiss
//
// The id parameter is the engine's deterministic recommendation ID,
// not a UUID. Dismissals are upsert — re-dismissing just refreshes
// the row, which is what operators expect.
func (h *RecommendationsHandlers) HandleDismiss(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recommendation id required"})
		return
	}
	var req dismissRequest
	// Body is optional; ignore decode errors on empty bodies.
	_ = c.ShouldBindJSON(&req)

	actor := middleware.ActorFromGin(c).String()
	if actor == "" {
		actor = "system"
	}

	dismissal := &storetypes.RecommendationDismissal{
		RecommendationID: id,
		DismissedAt:      time.Now().UTC(),
		DismissedBy:      actor,
		Reason:           req.Reason,
	}
	if err := h.store.DismissRecommendation(c.Request.Context(), dismissal); err != nil {
		h.logger.Warn("DismissRecommendation failed",
			zap.String("rec_id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Bust the engine cache so the dismissed recommendation
	// disappears on the next list without waiting for TTL.
	h.engine.InvalidateCache()
	c.JSON(http.StatusOK, dismissal)
}

// HandleRestore — POST /api/v1/recommendations/:id/restore
// Removes a dismissal so the recommendation re-appears on the next
// list. Idempotent: restoring something that wasn't dismissed is a
// no-op (returns 200 with a small JSON acknowledgement).
func (h *RecommendationsHandlers) HandleRestore(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recommendation id required"})
		return
	}
	if err := h.store.RestoreRecommendation(c.Request.Context(), id); err != nil {
		h.logger.Warn("RestoreRecommendation failed",
			zap.String("rec_id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.engine.InvalidateCache()
	c.JSON(http.StatusOK, gin.H{"ok": true, "recommendation_id": id})
}

// HandleListDismissals — GET /api/v1/recommendations/dismissals
// Returns the current dismissal set. Mostly useful for an admin
// "show me what I've hidden" view in the UI.
func (h *RecommendationsHandlers) HandleListDismissals(c *gin.Context) {
	out, err := h.store.ListRecommendationDismissals(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// (context is used implicitly via the gin Request.Context calls
// above; this var keeps the import live without pulling in unused
// helpers.)
var _ context.Context = context.Background()
