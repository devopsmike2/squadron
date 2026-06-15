// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"
	"time"

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

// HandleForecast — GET /api/v1/pricing/forecast
//
// Returns the projected month-end spend by extrapolating the current
// daily ingest rate over the remaining days in the current calendar
// month and adding the spend already incurred so far in the month.
//
// The intuition: if I'm currently shipping at $X/day and there are
// N days left in the month plus M days already elapsed, my month-end
// spend lands around (X * M) + (X * N) = X * (M + N) — assuming
// constant volume. We use the 24h trailing rate as the "current
// daily rate" because it smooths out diurnal traffic patterns and
// is what an operator would intuit as "the rate right now."
//
// Pure projection — no new schema, no stateful tracker. The number
// updates every time the page polls because it's recomputed from
// the latest 24h fleet summary.
//
// Added in v0.39.0 (insights expansion).
func (h *PricingHandlers) HandleForecast(c *gin.Context) {
	if !h.pricer.Enabled() {
		c.JSON(http.StatusOK, gin.H{
			"enabled":  false,
			"currency": h.pricer.Currency(),
		})
		return
	}
	// 24h is the only window we accept — anything shorter is too
	// noisy to project a month with, anything longer is wasted work
	// because the DuckDB scan grows linearly.
	win := insights.Window("24h")
	fleet, err := h.insights.FleetVolume(c.Request.Context(), win, nil)
	if err != nil {
		h.logger.Warn("forecast: fleet volume failed", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	windowSeconds := int64(0)
	if dur, err := win.AsDuration(); err == nil {
		windowSeconds = int64(dur.Seconds())
	}
	if windowSeconds <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window"})
		return
	}

	// Roll the 24h numbers into a bytes-per-hour map and project.
	// Same math as HandleProjection — reused so the steady-state
	// number on Savings stays consistent between "estimated"
	// and "forecast" tiles.
	in := pricing.FleetInput{SignalBytesPerHour: map[pricing.Signal]int64{}}
	for _, sv := range fleet.BySignal {
		if sv.Bytes <= 0 {
			continue
		}
		bytesPerHour := int64(float64(sv.Bytes) * 3600.0 / float64(windowSeconds))
		in.SignalBytesPerHour[pricing.Signal(sv.Signal)] = bytesPerHour
	}
	out := h.pricer.ProjectFleet(in)

	// Pro-rate the steady-state monthly figure across the calendar
	// month to derive a forecast. monthlyUSD is "this rate, run for
	// 30 days"; we re-stretch it to the actual day count of the
	// current month and split into elapsed + remaining.
	now := time.Now().UTC()
	year, month, _ := now.Date()
	firstOfMonth := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	firstOfNextMonth := firstOfMonth.AddDate(0, 1, 0)
	daysInMonth := int(firstOfNextMonth.Sub(firstOfMonth).Hours() / 24.0)
	if daysInMonth <= 0 {
		daysInMonth = 30
	}
	// Use seconds for accurate partial-day handling — "1.7 days
	// elapsed" gives a smoother delta than rounding to whole days.
	secondsElapsed := now.Sub(firstOfMonth).Seconds()
	secondsInMonth := firstOfNextMonth.Sub(firstOfMonth).Seconds()
	if secondsInMonth <= 0 {
		secondsInMonth = float64(daysInMonth) * 86400
	}
	fractionElapsed := secondsElapsed / secondsInMonth
	if fractionElapsed < 0 {
		fractionElapsed = 0
	}
	if fractionElapsed > 1 {
		fractionElapsed = 1
	}

	// Convert pricing.ProjectFleet's 30-day baseline to "actual
	// days in this calendar month" — June has 30, July has 31, etc.
	calendarMonthly := out.MonthlyUSD * float64(daysInMonth) / 30.0
	spentSoFar := calendarMonthly * fractionElapsed
	remaining := calendarMonthly - spentSoFar

	c.JSON(http.StatusOK, gin.H{
		"enabled":           true,
		"currency":          out.Currency,
		"steady_state_usd":  out.MonthlyUSD,
		"forecast_usd":      calendarMonthly,
		"spent_so_far_usd":  spentSoFar,
		"remaining_usd":     remaining,
		"fraction_elapsed":  fractionElapsed,
		"days_in_month":     daysInMonth,
		"days_elapsed":      secondsElapsed / 86400,
		"calendar_month":    now.Format("January 2006"),
		"agent_count":       fleet.AgentCount,
	})
}
