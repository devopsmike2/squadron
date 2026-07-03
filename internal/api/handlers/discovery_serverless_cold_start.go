// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
)

// --- response shape per docs/proposals/cold-start-latency-slice1.md §6.1 -----

// ColdStartWindow is the JSON sub-shape carrying one observation
// window's roll-up. window_hours discriminates the 24h current window
// from the 168h baseline window; p95_ms / sample_count / observed_at
// come straight off the most recent persisted cold_start_observation
// row for the (resource_arn, window_hours) tuple.
//
// observed_at is the timestamp the scanner ran the detection — NOT
// the timestamp of the underlying CloudWatch datapoints (each
// datapoint stays anonymized at the substrate level per §12 of the
// design doc). Round-tripped from the storage layer's UTC value.
type ColdStartWindow struct {
	WindowHours int       `json:"window_hours"`
	P95Ms       float64   `json:"p95_ms"`
	SampleCount int       `json:"sample_count"`
	ObservedAt  time.Time `json:"observed_at"`
}

// ColdStartResponse is the JSON wire shape returned by
// GET /api/v1/discovery/{provider}/inventory/serverless/{id}/cold_start.
//
// Per design doc §6.1 the response carries the current window, the
// baseline window, the ratio between them, and the boolean
// threshold/floor predicates. Slice 1 (and slice 2 unchanged)
// presents the predicates pre-computed on the server side so the UI
// chunk-3 amber-color logic doesn't have to re-apply the thresholds
// client-side.
//
// ratio is current / baseline; zero when the baseline is also zero
// (cold-start function or function younger than the baseline
// window). The UI renders an em dash for zero ratios — the absence
// of a meaningful comparison rather than a 0:0 result.
type ColdStartResponse struct {
	ResourceARN      string          `json:"resource_arn"`
	CurrentWindow    ColdStartWindow `json:"current_window"`
	BaselineWindow   ColdStartWindow `json:"baseline_window"`
	Ratio            float64         `json:"ratio"`
	ExceedsThreshold bool            `json:"exceeds_threshold"`
	ExceedsFloorMs   bool            `json:"exceeds_floor_ms"`
}

// ColdStartObservationReader is the slim interface the handler uses
// to look up the latest observations for a resource. Production wires
// the concrete *sqlite.Storage which already implements
// LatestColdStartObservation from v0.89.113 (chunk 1). Tests
// substitute an in-memory fake.
type ColdStartObservationReader interface {
	LatestColdStartObservation(
		ctx context.Context,
		connectionID string,
		resourceARN string,
		windowHours int,
	) (sqlite.ColdStartObservationRow, bool, error)
}

// ColdStartDetectionConstants is the slim interface the handler uses
// to read the substrate-default current + baseline window hours and
// the threshold + floor values. v0.89.114 — production wires a
// fixed-value adapter sourced from internal/discovery/aws constants
// (so the per-cloud Lambda numbers stay the single source of truth);
// tests pin the constants directly. The interface keeps the
// handlers package from importing the aws package and pulling the
// AWS SDK transitively into every API binary.
//
// Slice 2 may make these per-provider when GCP / Azure / OCI
// cold-start cover lands — the interface is provider-agnostic, the
// values are slice-1-AWS-only today.
type ColdStartDetectionConstants interface {
	CurrentWindowHours() int
	BaselineWindowHours() int
	RatioThreshold() float64
	FloorMs() float64
}

// staticColdStartDetectionConstants is the production
// ColdStartDetectionConstants adapter. v0.89.114 — pulls the four
// values from a single construction-time configuration so the
// per-cloud constants from internal/discovery/aws can be injected
// without cross-package import cycles. Same shape the chunk-3 UI
// extension will consume.
type staticColdStartDetectionConstants struct {
	current  int
	baseline int
	ratio    float64
	floor    float64
}

// NewStaticColdStartDetectionConstants — v0.89.114. Builds the
// production adapter from the four substrate constants. Callers in
// server.go pull from internal/discovery/aws so the per-cloud value
// stays canonical.
func NewStaticColdStartDetectionConstants(currentHours, baselineHours int, ratio, floor float64) ColdStartDetectionConstants {
	return &staticColdStartDetectionConstants{
		current:  currentHours,
		baseline: baselineHours,
		ratio:    ratio,
		floor:    floor,
	}
}

func (c *staticColdStartDetectionConstants) CurrentWindowHours() int  { return c.current }
func (c *staticColdStartDetectionConstants) BaselineWindowHours() int { return c.baseline }
func (c *staticColdStartDetectionConstants) RatioThreshold() float64  { return c.ratio }
func (c *staticColdStartDetectionConstants) FloorMs() float64         { return c.floor }

// DiscoveryServerlessColdStartHandlers serves the new per-resource
// cold-start endpoint. v0.89.114 (slice 1 chunk 2). The store is
// OPTIONAL — a nil reader returns 404 on every call, matching the
// "no observations recorded for resource" surface so the UI sees a
// stable shape across deployments that have / haven't wired chunk 1.
type DiscoveryServerlessColdStartHandlers struct {
	store     ColdStartObservationReader
	constants ColdStartDetectionConstants
	logger    *zap.Logger
}

// NewDiscoveryServerlessColdStartHandlers builds the handler. Nil
// store is acceptable — see the type godoc above. Nil constants
// falls through to a default adapter pinned at the v0.89.114
// substrate constants (24h current / 168h baseline / 1.5x ratio /
// 500ms floor) so a partially-wired deployment still serves a
// consistent shape.
func NewDiscoveryServerlessColdStartHandlers(
	store ColdStartObservationReader,
	constants ColdStartDetectionConstants,
	logger *zap.Logger,
) *DiscoveryServerlessColdStartHandlers {
	if constants == nil {
		constants = NewStaticColdStartDetectionConstants(24, 168, 1.5, 500.0)
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DiscoveryServerlessColdStartHandlers{
		store:     store,
		constants: constants,
		logger:    logger,
	}
}

// HandleColdStart serves
// GET /api/v1/discovery/:provider/inventory/serverless/:id/cold_start.
//
// 200 case: BOTH the 24h current-window and 168h baseline-window
// observations exist for the resource. Computes the ratio +
// threshold predicates from the persisted values and returns the
// composed response.
//
// 404 cases:
//   - store wire not present (deployment hasn't wired chunk 1 yet),
//   - LatestColdStartObservation returns ok=false for either window
//     (the resource has been observed in zero scans, or only in the
//     current-window scan but not the baseline-window scan yet —
//     either way the comparison isn't meaningful).
//
// 400 case: the :id path param is empty.
//
// 5xx case: the store returns an error other than NotFound.
//
// The :id path param is treated as the resource ARN verbatim. Gin
// URL-decodes the path segment before binding it; operators
// passing arns:aws:lambda:... via the dashboard see Squadron forward
// the URL-encoded form transparently.
//
// See docs/proposals/cold-start-latency-slice1.md §6.1.
func (h *DiscoveryServerlessColdStartHandlers) HandleColdStart(c *gin.Context) {
	resourceARN := strings.TrimSpace(c.Param("id"))
	if resourceARN == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id (resource ARN) is required"})
		return
	}

	if h.store == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no cold-start observations recorded for resource"})
		return
	}

	ctx := c.Request.Context()
	currentHours := h.constants.CurrentWindowHours()
	baselineHours := h.constants.BaselineWindowHours()

	current, currentFound, err := h.store.LatestColdStartObservation(ctx, "", resourceARN, currentHours)
	if err != nil {
		h.logger.Warn("cold start: current window lookup failed",
			zap.String("resource_arn", resourceARN),
			zap.Int("window_hours", currentHours),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cold-start lookup failed"})
		return
	}
	baseline, baselineFound, err := h.store.LatestColdStartObservation(ctx, "", resourceARN, baselineHours)
	if err != nil {
		h.logger.Warn("cold start: baseline window lookup failed",
			zap.String("resource_arn", resourceARN),
			zap.Int("window_hours", baselineHours),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cold-start lookup failed"})
		return
	}
	if !currentFound || !baselineFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "no cold-start observations recorded for resource"})
		return
	}

	resp := ColdStartResponse{
		ResourceARN: resourceARN,
		CurrentWindow: ColdStartWindow{
			WindowHours: current.WindowHours,
			P95Ms:       current.P95Ms,
			SampleCount: current.SampleCount,
			ObservedAt:  current.ObservedAt.UTC(),
		},
		BaselineWindow: ColdStartWindow{
			WindowHours: baseline.WindowHours,
			P95Ms:       baseline.P95Ms,
			SampleCount: baseline.SampleCount,
			ObservedAt:  baseline.ObservedAt.UTC(),
		},
	}
	if baseline.P95Ms > 0 {
		resp.Ratio = current.P95Ms / baseline.P95Ms
		resp.ExceedsThreshold = resp.Ratio >= h.constants.RatioThreshold()
	}
	resp.ExceedsFloorMs = current.P95Ms >= h.constants.FloorMs()

	c.JSON(http.StatusOK, resp)
}

// ErrColdStartNotConfigured is returned by helper code that needs to
// distinguish "the cold-start handler isn't wired in this deployment"
// from "the resource has no observations yet". Defensive code path
// for chunk-3 callers that compose the cold-start handler with other
// per-resource projections; the chunk-2 handler itself returns 404
// in both cases per the §6.1 contract.
var ErrColdStartNotConfigured = errors.New("cold-start observation store not configured")
