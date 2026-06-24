// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/proposer"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// discovery_serverless_error_rate.go — Error rate correlation
// slice 1 chunk 2 (v0.89.128, #768 Stream 166). Per-resource
// error-rate endpoint mirroring discovery_serverless_cold_start.go.
// The chunk-1 storage layer (v0.89.127) persists per-(resource,
// window_hours) error_rate_observation rows; this handler composes
// the 24h current window and 168h baseline window into the §6.1
// JSON shape. Same nil-store-tolerant + 404-on-missing posture as
// the cold-start sibling.
//
// See docs/proposals/error-rate-correlation-slice1.md §6.1 + §11
// acceptance test 12.

// --- response shape per docs/proposals/error-rate-correlation-slice1.md §6.1 -----

// ErrorRateWindow is the JSON sub-shape carrying one observation
// window's roll-up. window_hours discriminates the 24h current
// window from the 168h baseline window; error_count /
// invocation_count / error_rate / observed_at come straight off
// the most recent persisted error_rate_observation row for the
// (resource_arn, window_hours) tuple.
type ErrorRateWindow struct {
	WindowHours     int       `json:"window_hours"`
	ErrorCount      uint64    `json:"error_count"`
	InvocationCount uint64    `json:"invocation_count"`
	ErrorRate       float64   `json:"error_rate"`
	ObservedAt      time.Time `json:"observed_at"`
}

// ErrorRateResponse is the JSON wire shape returned by
// GET /api/v1/discovery/{provider}/inventory/serverless/{id}/error_rate.
//
// Per design doc §6.1 the response carries the current window, the
// baseline window, the rate ratio between them, and the three
// boolean gate predicates. The boolean baseline_adjusted surfaces
// the §12 near-zero baseline guard — when the raw baseline rate is
// below the 0.01% floor, the response computes rate_ratio against
// the floor as the comparison denominator and signals the
// substitution via baseline_adjusted = true.
//
// would_fire_recommendation is the AND of the three Exceeds* fields,
// pre-computed on the server side so the chunk-3 UI amber-color
// logic doesn't have to re-apply the thresholds client-side.
type ErrorRateResponse struct {
	ResourceARN               string          `json:"resource_arn"`
	CurrentWindow             ErrorRateWindow `json:"current_window"`
	BaselineWindow            ErrorRateWindow `json:"baseline_window"`
	RateRatio                 float64         `json:"rate_ratio"`
	BaselineAdjusted          bool            `json:"baseline_adjusted"`
	ExceedsRateRatioFloor     bool            `json:"exceeds_rate_ratio_floor"`
	ExceedsMinimumInvocations bool            `json:"exceeds_minimum_invocations"`
	ExceedsMinimumErrors      bool            `json:"exceeds_minimum_errors"`
	WouldFireRecommendation   bool            `json:"would_fire_recommendation"`
}

// ErrorRateObservationReader is the slim interface the handler uses
// to look up the latest observations for a resource. Production
// wires the concrete *sqlite.Storage which implements
// LatestErrorRateObservation from v0.89.127 (chunk 1). Tests
// substitute an in-memory fake.
type ErrorRateObservationReader interface {
	LatestErrorRateObservation(
		ctx context.Context,
		resourceARN string,
		windowHours int,
	) (sqlite.ErrorRateObservationRow, bool, error)
}

// DiscoveryServerlessErrorRateHandlers serves the new per-resource
// error-rate endpoint. v0.89.128 (slice 1 chunk 2). The store is
// OPTIONAL — a nil reader returns 404 on every call, matching the
// "no observations recorded for resource" surface so the UI sees a
// stable shape across deployments that have / haven't wired
// chunk 1.
type DiscoveryServerlessErrorRateHandlers struct {
	store  ErrorRateObservationReader
	logger *zap.Logger
}

// NewDiscoveryServerlessErrorRateHandlers builds the handler. Nil
// store is acceptable — see the type godoc above. Nil logger
// defaults to a no-op logger so the handler stays construction-safe
// in unit tests.
func NewDiscoveryServerlessErrorRateHandlers(
	store ErrorRateObservationReader,
	logger *zap.Logger,
) *DiscoveryServerlessErrorRateHandlers {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DiscoveryServerlessErrorRateHandlers{
		store:  store,
		logger: logger,
	}
}

// HandleErrorRate serves
// GET /api/v1/discovery/:provider/inventory/serverless/:id/error_rate.
//
// 200 case: BOTH the 24h current-window and 168h baseline-window
// observations exist for the resource. Computes the rate ratio +
// three gate predicates from the persisted values and returns the
// composed response.
//
// The §12 near-zero baseline guard fires when the baseline error
// rate is below proposer.ErrorRateBaselineFloor (0.01%): the
// handler substitutes the floor as the comparison denominator and
// sets baseline_adjusted = true. This matches the detection
// branch's posture in DetectErrorRate so the per-resource endpoint
// surfaces the same comparison the would_fire_recommendation
// boolean is computed against.
//
// 404 cases:
//   - store wire not present (deployment hasn't wired chunk 1 yet),
//   - LatestErrorRateObservation returns ok=false for either
//     window (the resource has been observed in zero scans, or
//     only in the current-window scan but not the baseline-window
//     scan yet — either way the comparison isn't meaningful).
//
// 400 case: the :id path param is empty.
//
// 5xx case: the store returns an error other than NotFound.
//
// See docs/proposals/error-rate-correlation-slice1.md §6.1.
func (h *DiscoveryServerlessErrorRateHandlers) HandleErrorRate(c *gin.Context) {
	resourceARN := strings.TrimSpace(c.Param("id"))
	if resourceARN == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id (resource ARN) is required"})
		return
	}

	if h.store == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no error-rate observations recorded for resource"})
		return
	}

	ctx := c.Request.Context()
	currentHours := proposer.ErrorRateCurrentWindowHours
	baselineHours := proposer.ErrorRateBaselineWindowHours

	current, currentFound, err := h.store.LatestErrorRateObservation(ctx, resourceARN, currentHours)
	if err != nil {
		h.logger.Warn("error rate: current window lookup failed",
			zap.String("resource_arn", resourceARN),
			zap.Int("window_hours", currentHours),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "error-rate lookup failed"})
		return
	}
	baseline, baselineFound, err := h.store.LatestErrorRateObservation(ctx, resourceARN, baselineHours)
	if err != nil {
		h.logger.Warn("error rate: baseline window lookup failed",
			zap.String("resource_arn", resourceARN),
			zap.Int("window_hours", baselineHours),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "error-rate lookup failed"})
		return
	}
	if !currentFound || !baselineFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "no error-rate observations recorded for resource"})
		return
	}

	resp := ErrorRateResponse{
		ResourceARN: resourceARN,
		CurrentWindow: ErrorRateWindow{
			WindowHours:     current.WindowHours,
			ErrorCount:      uint64Clamp(current.ErrorCount),
			InvocationCount: uint64Clamp(current.InvocationCount),
			ErrorRate:       current.ErrorRate,
			ObservedAt:      current.ObservedAt.UTC(),
		},
		BaselineWindow: ErrorRateWindow{
			WindowHours:     baseline.WindowHours,
			ErrorCount:      uint64Clamp(baseline.ErrorCount),
			InvocationCount: uint64Clamp(baseline.InvocationCount),
			ErrorRate:       baseline.ErrorRate,
			ObservedAt:      baseline.ObservedAt.UTC(),
		},
	}

	// §12 near-zero baseline guard — when the persisted baseline
	// rate would otherwise produce a spurious large ratio against a
	// near-zero denominator, substitute the floor and surface the
	// substitution via baseline_adjusted = true so the operator
	// reading the response knows the ratio is computed against a
	// floor, not the raw baseline.
	effectiveBaseline := baseline.ErrorRate
	if effectiveBaseline < proposer.ErrorRateBaselineFloor {
		effectiveBaseline = proposer.ErrorRateBaselineFloor
		resp.BaselineAdjusted = true
	}
	resp.RateRatio = current.ErrorRate / effectiveBaseline

	resp.ExceedsRateRatioFloor = resp.RateRatio > proposer.ErrorRateRatioFloor
	resp.ExceedsMinimumInvocations = uint64Clamp(current.InvocationCount) >= proposer.ErrorRateMinInvocationCount
	resp.ExceedsMinimumErrors = uint64Clamp(current.ErrorCount) >= proposer.ErrorRateMinErrorCount
	resp.WouldFireRecommendation = resp.ExceedsRateRatioFloor && resp.ExceedsMinimumInvocations && resp.ExceedsMinimumErrors

	c.JSON(http.StatusOK, resp)
}

// uint64Clamp converts an int (from the storage row's int-typed
// count columns) to uint64, clamping negative values to zero.
// Defensive: the schema column is INTEGER NOT NULL and the persist
// path only writes non-negative values, but the explicit clamp keeps
// the API arithmetic well-defined under hypothetical future schema
// drift.
func uint64Clamp(v int) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}
