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
)

// discovery_serverless_sampling.go — Sampling rate analysis slice 1
// chunk 2 (v0.89.123, #763 Stream 161). Sibling of
// discovery_serverless_cold_start.go: per-resource sampling endpoint
// returning the §6.1 wire shape.
//
// See docs/proposals/sampling-rate-analysis-slice1.md §6.1.

// SamplingResponse is the JSON wire shape returned by
// GET /api/v1/discovery/{provider}/inventory/serverless/{id}/sampling.
//
// Per design doc §6.1 the response carries the observed +
// expected counts, the computed ratio, the two boolean predicates
// (exceeds_floor + exceeds_minimum_invocations), and the combined
// would_fire_recommendation flag.
//
// The two boolean predicates surface SEPARATELY from
// would_fire_recommendation so the UI / operator can distinguish:
//
//   - exceeds_floor=true + exceeds_minimum_invocations=true
//     → would_fire_recommendation=true (the trigger fires; ratio is
//       below 5% AND the resource has enough invocations to be
//       statistically meaningful)
//
//   - exceeds_floor=true + exceeds_minimum_invocations=false
//     → would_fire_recommendation=false (the ratio is below 5% but
//       the invocation count is below the 1000 statistical noise
//       floor; the percentage isn't trustworthy)
//
//   - exceeds_floor=false + exceeds_minimum_invocations=true
//     → would_fire_recommendation=false (enough invocations, but
//       sampling ratio is at or above 5% — within the acceptable
//       band)
//
//   - exceeds_floor=false + exceeds_minimum_invocations=false
//     → would_fire_recommendation=false (insufficient data AND
//       ratio above floor; double-negative no-fire)
//
// The UI's amber-color logic reads would_fire_recommendation; the
// per-resource drill-down tooltip surfaces the two underlying
// booleans so an operator can see exactly which gate held.
type SamplingResponse struct {
	ResourceARN               string    `json:"resource_arn"`
	WindowHours               int       `json:"window_hours"`
	ObservedSpanCount         uint64    `json:"observed_span_count"`
	ExpectedInvocationCount   uint64    `json:"expected_invocation_count"`
	SamplingRatio             float64   `json:"sampling_ratio"`
	ExceedsFloor              bool      `json:"exceeds_floor"`
	ExceedsMinimumInvocations bool      `json:"exceeds_minimum_invocations"`
	WouldFireRecommendation   bool      `json:"would_fire_recommendation"`
	ObservedAt                time.Time `json:"observed_at"`
}

// SamplingResourceLookup is the slim interface the handler uses to
// resolve a path-param resource id to its surface + traceindex key.
// Production wires a per-cloud serverless inventory query; tests
// substitute an in-memory fake.
//
// ok=false means the resource isn't in the inventory for the named
// provider — handler returns 404 to the caller.
type SamplingResourceLookup interface {
	LookupSamplingResource(provider, resourceID string) (surface string, traceindexKey string, ok bool)
}

// SamplingDetector is the seam the handler calls to compute the
// detection result. Production wires a closure that holds the
// per-cloud MetricQuerier + the traceindex Quality observer and
// dispatches to proposer.DetectSamplingRate. Tests substitute a
// pre-canned result.
//
// Returning a populated result + nil error is the success path;
// the handler builds the response from the result. Returning a
// non-nil error surfaces as 500.
type SamplingDetector interface {
	DetectSampling(
		ctx context.Context,
		resourceARN string,
		surface string,
		traceindexKey string,
	) (proposer.SamplingRateDetectionResult, error)
}

// DiscoveryServerlessSamplingHandlers serves the per-resource
// sampling endpoint. v0.89.123. Both lookup AND detector are
// OPTIONAL — nil either returns 404 on every call, matching the
// "no observation yet" surface so the UI sees a stable shape
// across deployments that haven't wired the per-cloud
// MetricQuerier substrate yet.
type DiscoveryServerlessSamplingHandlers struct {
	lookup   SamplingResourceLookup
	detector SamplingDetector
	logger   *zap.Logger
}

// NewDiscoveryServerlessSamplingHandlers builds the handler. Nil
// lookup OR nil detector is acceptable — the handler degrades to
// 404 in either case (matching the "no data yet" surface so the
// UI keeps a single rendering path).
func NewDiscoveryServerlessSamplingHandlers(
	lookup SamplingResourceLookup,
	detector SamplingDetector,
	logger *zap.Logger,
) *DiscoveryServerlessSamplingHandlers {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DiscoveryServerlessSamplingHandlers{
		lookup:   lookup,
		detector: detector,
		logger:   logger,
	}
}

// HandleSampling serves
// GET /api/v1/discovery/:provider/inventory/serverless/:id/sampling.
//
// 200 case: the lookup resolves the resource to a surface +
// traceindex key, the detector returns a populated result, the
// handler composes the §6.1 response.
//
// 404 cases:
//   - lookup wire not present (deployment hasn't wired the per-
//     cloud serverless inventory),
//   - detector wire not present (deployment hasn't wired the
//     per-cloud MetricQuerier substrate),
//   - lookup returns ok=false (resource not in inventory).
//
// 400 case: the :id path param is empty.
//
// 5xx case: the detector returns an error other than NotFound.
//
// Per design doc §6.1.
func (h *DiscoveryServerlessSamplingHandlers) HandleSampling(c *gin.Context) {
	provider := strings.TrimSpace(c.Param("provider"))
	resourceID := strings.TrimSpace(c.Param("id"))
	if resourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id (resource id) is required"})
		return
	}

	if h.lookup == nil || h.detector == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no sampling observations recorded for resource"})
		return
	}

	surface, key, ok := h.lookup.LookupSamplingResource(provider, resourceID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no sampling observations recorded for resource"})
		return
	}

	result, err := h.detector.DetectSampling(c.Request.Context(), resourceID, surface, key)
	if err != nil {
		h.logger.Warn("sampling rate: detection failed",
			zap.String("provider", provider),
			zap.String("resource_id", resourceID),
			zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "sampling rate lookup failed"})
		return
	}

	resp := SamplingResponse{
		ResourceARN:               result.ResourceARN,
		WindowHours:               24,
		ObservedSpanCount:         result.ObservedSpanCount,
		ExpectedInvocationCount:   result.ExpectedInvocationCount,
		SamplingRatio:             result.Ratio,
		ExceedsFloor:              result.ExceedsFloor,
		ExceedsMinimumInvocations: result.ExceedsMinimumInvocations,
		WouldFireRecommendation:   result.ShouldFireRecommendation(),
		ObservedAt:                result.ObservedAt.UTC(),
	}
	if resp.ResourceARN == "" {
		resp.ResourceARN = resourceID
	}

	c.JSON(http.StatusOK, resp)
}
