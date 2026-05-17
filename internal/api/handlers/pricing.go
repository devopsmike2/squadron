// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/insights"
	"github.com/devopsmike2/squadron/internal/pricing"
)

// PricingHandlers exposes the v0.27 dollar-projection layer.
// Thin shells: the projector owns the math, the handler owns the
// request validation and the FleetInput assembly from the v0.24
// insights surface.
type PricingHandlers struct {
	pricer   *pricing.Projector
	insights *insights.Service
	logger   *zap.Logger
}

func NewPricingHandlers(pricer *pricing.Projector, insightsSvc *insights.Service, logger *zap.Logger) *PricingHandlers {
	return &PricingHandlers{pricer: pricer, insights: insightsSvc, logger: logger}
}

// HandleConfig — GET /api/v1/pricing/config
//
// Returns the configured rules + currency + enabled flag. The UI
// uses this to render the assumptions footer on the Savings page
// AND to do client-side per-destination $ math (so the UI doesn't
// have to round-trip to the backend for every destination card).
func (h *PricingHandlers) HandleConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"enabled":  h.pricer.Enabled(),
		"currency": h.pricer.Currency(),
		"rules":    h.pricer.Rules(),
	})
}

// HandleProjection — GET /api/v1/pricing/projection
//
// Query params:
//
//	window = 5m | 1h | 24h    (default 1h — pricing is most stable
//	                            at the hourly aggregation)
//
// Computes the fleet's current ingest in bytes-per-hour by signal,
// applies the catch-all rule rates, and returns $/month + per-signal
// breakdown. The per-destination breakdown is computed client-side
// (the UI already has the destination attribution from the v0.24
// CostInsights page); this endpoint is intentionally simple and
// destination-agnostic.
//
// Returns enabled=false in the body when pricing is off; never
// 503 (the Savings UI uses the response to decide what to render).
func (h *PricingHandlers) HandleProjection(c *gin.Context) {
	win, ok := parseWindow(c)
	if !ok {
		return
	}
	if !h.pricer.Enabled() {
		c.JSON(http.StatusOK, gin.H{
			"enabled":     false,
			"currency":    h.pricer.Currency(),
			"monthly_usd": 0,
		})
		return
	}

	// Pull the same fleet summary the v0.24 dashboard uses. Same
	// caching layer; this call is cheap.
	fleet, err := h.insights.FleetVolume(c.Request.Context(), win, nil)
	if err != nil {
		h.logger.Warn("pricing projection: fleet volume failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	windowSeconds := int64(0)
	if dur, err := win.AsDuration(); err == nil {
		windowSeconds = int64(dur.Seconds())
	}
	if windowSeconds <= 0 {
		// Shouldn't happen — parseWindow restricts to known windows.
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window"})
		return
	}

	in := pricing.FleetInput{
		SignalBytesPerHour: map[pricing.Signal]int64{},
	}
	for _, sv := range fleet.BySignal {
		if sv.Bytes <= 0 {
			continue
		}
		bytesPerHour := int64(float64(sv.Bytes) * 3600.0 / float64(windowSeconds))
		in.SignalBytesPerHour[pricing.Signal(sv.Signal)] = bytesPerHour
	}

	out := h.pricer.ProjectFleet(in)
	c.JSON(http.StatusOK, gin.H{
		"enabled":     out.Enabled,
		"currency":    out.Currency,
		"monthly_usd": out.MonthlyUSD,
		"by_signal":   out.BySignal,
		"assumptions": out.Assumptions,
		"window":      win,
		"agent_count": fleet.AgentCount,
	})
}
