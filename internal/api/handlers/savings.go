// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/api/middleware"
	"github.com/devopsmike2/squadron/internal/insights"
	"github.com/devopsmike2/squadron/internal/pricing"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// OutcomeStore is the narrow slice of ApplicationStore the savings
// handlers need. Extracted so tests can fake it.
type OutcomeStore interface {
	CreateRecommendationOutcome(ctx context.Context, o *storetypes.RecommendationOutcome) error
	UpdateRecommendationOutcome(ctx context.Context, o *storetypes.RecommendationOutcome) error
	ListRecommendationOutcomes(ctx context.Context) ([]*storetypes.RecommendationOutcome, error)
}

// SavingsHandlers owns the v0.28 retrospective-tracker endpoints.
// Two responsibilities:
//   - Record Apply clicks (POST /recommendations/:id/applied)
//   - Aggregate + refresh realized savings (GET /savings/realized)
//
// The handler does NOT run a background poller. Observations are
// refreshed lazily on each GET /savings/realized hit, which is
// sufficient at v0.28 scale and avoids extra goroutine plumbing.
// If outcome counts climb into the hundreds we'd revisit.
type SavingsHandlers struct {
	store    OutcomeStore
	engine   *recommendations.Engine
	insights *insights.Service
	pricer   *pricing.Projector
	logger   *zap.Logger

	// auditService, when non-nil, receives a savings.recommendation_applied
	// event each time an operator clicks Apply. Optional — a nil recorder
	// means "no audit emission" so existing tests that don't wire it stay
	// compiling. Mirrors the DiscoveryHandlers.WithAuditService idiom.
	auditService services.AuditService

	// Window used for byte-rate measurements. 1h matches insights'
	// default cache key, so reads here piggyback on the same cache.
	measureWindow insights.Window
}

// WithAuditService wires the audit recorder used by HandleApplied. Optional —
// a nil recorder is treated as "no audit emission". Fluent so the server can
// chain it onto NewSavingsHandlers. Mirrors DiscoveryHandlers.WithAuditService.
func (h *SavingsHandlers) WithAuditService(a services.AuditService) *SavingsHandlers {
	h.auditService = a
	return h
}

func NewSavingsHandlers(
	store OutcomeStore,
	engine *recommendations.Engine,
	insightsSvc *insights.Service,
	pricer *pricing.Projector,
	logger *zap.Logger,
) *SavingsHandlers {
	return &SavingsHandlers{
		store:         store,
		engine:        engine,
		insights:      insightsSvc,
		pricer:        pricer,
		logger:        logger,
		measureWindow: insights.Window1h,
	}
}

// applyRequest is the optional body the UI POSTs alongside the
// click. The id in the URL is authoritative; the body lets the UI
// pass a frozen-at-click view of the recommendation in case the
// engine has stopped producing it by the time the request lands
// (race condition between Evaluate cache flips).
type applyRequest struct {
	Title                 string  `json:"title,omitempty"`
	Category              string  `json:"category,omitempty"`
	Signal                string  `json:"signal,omitempty"`
	EstSavingsPerMonthUSD float64 `json:"est_savings_per_month_usd,omitempty"`
	EstSavingsBytes       int64   `json:"est_savings_bytes,omitempty"`
	AttributeKey          string  `json:"attribute_key,omitempty"`
}

// HandleApplied — POST /api/v1/recommendations/:id/applied
//
// Records that the operator clicked Apply on a recommendation.
// Captures a frozen snapshot of the engine's view + the baseline
// byte rate for the affected attribute. The outcome row starts in
// `pending`; subsequent /savings/realized hits refresh observation.
func (h *SavingsHandlers) HandleApplied(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "recommendation id required"})
		return
	}

	// Body is optional; deserialize whatever the UI sent.
	var req applyRequest
	_ = c.ShouldBindJSON(&req)

	// Try to find the live recommendation from the engine. If found,
	// its fields override the request body (engine view is canonical).
	// If not (operator clicked an aged-out card from a stale render),
	// fall through with the body's frozen view.
	if recs, err := h.engine.Evaluate(c.Request.Context(), h.measureWindow); err == nil {
		for _, r := range recs {
			if r.ID == id {
				req.Title = r.Title
				req.Category = string(r.Category)
				req.Signal = string(r.Signal)
				req.EstSavingsPerMonthUSD = r.EstSavingsPerMonthUSD
				req.EstSavingsBytes = r.EstSavingsBytes
				if req.AttributeKey == "" {
					req.AttributeKey = extractAttributeKeyFromTitle(r.Title)
				}
				break
			}
		}
	}

	// Compute baseline_bytes_per_hour from the engine's window-scoped
	// est_savings_bytes. The recommendation engine projects savings
	// over its evaluation window; normalize to hourly for the
	// observation math.
	var baselineBytesPerHour int64
	if dur, err := h.measureWindow.AsDuration(); err == nil && dur.Seconds() > 0 && req.EstSavingsBytes > 0 {
		baselineBytesPerHour = int64(float64(req.EstSavingsBytes) * 3600.0 / dur.Seconds())
	}

	actor := middleware.ActorFromGin(c).String()
	if actor == "" {
		actor = "system"
	}

	outcome := &storetypes.RecommendationOutcome{
		ID:                           newOutcomeID(),
		RecommendationID:             id,
		AppliedAt:                    time.Now().UTC(),
		AppliedBy:                    actor,
		Title:                        req.Title,
		Category:                     req.Category,
		Signal:                       req.Signal,
		AttributeKey:                 req.AttributeKey,
		BaselineBytesPerHour:         baselineBytesPerHour,
		EstSavingsPerMonthUSDAtApply: req.EstSavingsPerMonthUSD,
		Status:                       "pending",
	}

	if err := h.store.CreateRecommendationOutcome(c.Request.Context(), outcome); err != nil {
		h.logger.Warn("create recommendation outcome failed",
			zap.String("rec_id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Operator-action audit: an Apply click mutates persisted state (the
	// outcome row the Savings page tracks), so it belongs on the "what
	// changed when" timeline. Best-effort — a nil recorder or a Record error
	// never fails the request the operator already succeeded at.
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      actor,
			EventType:  services.AuditEventSavingsRecommendationApplied,
			TargetType: services.AuditTargetRecommendation,
			TargetID:   id,
			Action:     "applied",
			Payload: map[string]any{
				"recommendation_id":                  id,
				"outcome_id":                         outcome.ID,
				"title":                              outcome.Title,
				"category":                           outcome.Category,
				"est_savings_per_month_usd_at_apply": outcome.EstSavingsPerMonthUSDAtApply,
			},
		})
	}
	c.JSON(http.StatusOK, outcome)
}

// HandleRealized — GET /api/v1/savings/realized
//
// Refreshes observations for every outcome (cheap — engine's
// TopAttributes is cached) and returns the aggregated realized
// savings plus a per-outcome breakdown.
func (h *SavingsHandlers) HandleRealized(c *gin.Context) {
	outcomes, err := h.store.ListRecommendationOutcomes(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Refresh observations for each outcome.
	var totalRealizedUSD float64
	var realizedCount, pendingCount, notObservedCount, revertedCount int

	for _, o := range outcomes {
		// Only re-observe attribute-class outcomes today; outlier_agent
		// and drop_hotspot don't have a clean "affected scope" to
		// re-query against. Their realized savings stay frozen at
		// est_at_apply.
		if o.Category == "noisy_attribute" && o.AttributeKey != "" && o.Signal != "" {
			h.refreshAttributeOutcome(c.Request.Context(), o)
		} else {
			// Non-refreshable: assume realized after a settling window
			// (1 hour). Honest but coarse — operators see SOMETHING
			// for these; we mark them clearly in the response.
			if time.Since(o.AppliedAt) > time.Hour && o.Status == "pending" {
				o.Status = "realized"
				o.RealizedSavingsPerMonthUSD = o.EstSavingsPerMonthUSDAtApply
				o.LastObservedAt = time.Now().UTC()
				_ = h.store.UpdateRecommendationOutcome(c.Request.Context(), o)
			}
		}
		switch o.Status {
		case "realized":
			realizedCount++
			totalRealizedUSD += o.RealizedSavingsPerMonthUSD
		case "pending":
			pendingCount++
		case "not_observed":
			notObservedCount++
		case "reverted":
			// Once-realized savings that regressed back to baseline.
			// Counted separately and NOT added to totalRealizedUSD —
			// the savings no longer exist. Previously this status was
			// unhandled, so a reverted outcome fell through every case
			// and the sub-counts silently failed to sum to total.
			revertedCount++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"monthly_realized_usd": totalRealizedUSD,
		"currency":             h.pricer.Currency(),
		"counts": gin.H{
			"realized":     realizedCount,
			"pending":      pendingCount,
			"not_observed": notObservedCount,
			"reverted":     revertedCount,
			"total":        len(outcomes),
		},
		"outcomes": outcomes,
	})
}

// refreshAttributeOutcome re-queries insights for the affected
// attribute's current byte rate and updates the outcome's
// observation fields. Idempotent; safe to call from a GET handler.
func (h *SavingsHandlers) refreshAttributeOutcome(ctx context.Context, o *storetypes.RecommendationOutcome) {
	if h.insights == nil {
		return
	}
	// Find the attribute's current byte share via the same sampled
	// TopAttributes path the engine uses.
	attrs, err := h.insights.TopAttributes(ctx, h.measureWindow, insights.Signal(o.Signal), 100)
	if err != nil {
		return
	}
	var current insights.AttributeVolume
	found := false
	for _, a := range attrs {
		if a.Key == o.AttributeKey {
			current = a
			found = true
			break
		}
	}

	dur, _ := h.measureWindow.AsDuration()
	windowSeconds := int64(dur.Seconds())
	var observedBytesPerHour int64
	if found && windowSeconds > 0 {
		observedBytesPerHour = int64(float64(current.Bytes) * 3600.0 / float64(windowSeconds))
	}
	// If the attribute is no longer in the top-100, treat as zero
	// observed — the fix dropped it below the noise threshold,
	// which is the success case.

	o.LastObservedBytesPerHour = observedBytesPerHour
	o.LastObservedAt = time.Now().UTC()

	// Compute realized savings via the pricing projector. We only
	// count POSITIVE delta (current < baseline); negative means the
	// attribute got noisier post-apply, which isn't "savings" — it's
	// regression, and we don't subtract from the tally.
	if observedBytesPerHour < o.BaselineBytesPerHour {
		savedPerHour := o.BaselineBytesPerHour - observedBytesPerHour
		if h.pricer != nil && h.pricer.Enabled() {
			// Pricing has its own Signal type duplicated from insights
			// to avoid an import cycle; convert via the string literal
			// (both packages encode "metrics" / "logs" / "traces"
			// identically).
			o.RealizedSavingsPerMonthUSD = h.pricer.MonthlyForBytes(
				savedPerHour, pricing.Signal(o.Signal), "")
		}
		o.Status = "realized"
	} else {
		// Observed rate is back at/above baseline: the status transition
		// depends on the PRIOR status (was-realized → reverted vs
		// never-realized → pending/not_observed). See statusAtBaseline.
		o.Status = statusAtBaseline(o.Status, o.AppliedAt, time.Now())
		// In every branch the currently-observed savings are zero: the
		// attribute is back at baseline, so no bytes are being saved
		// right now. A reverted outcome therefore drops out of the
		// realized-USD tally, which is the correct behavior.
		o.RealizedSavingsPerMonthUSD = 0
	}

	if err := h.store.UpdateRecommendationOutcome(ctx, o); err != nil {
		h.logger.Warn("update recommendation outcome failed",
			zap.String("outcome_id", o.ID), zap.Error(err))
	}
}

// ----------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------

// statusAtBaseline decides the next outcome status when a refreshed
// observation shows the affected attribute's byte rate is back AT or ABOVE
// baseline (i.e. no savings are currently observed). The decision hinges on the
// PRIOR status so the two operator stories stay distinct — a distinction the
// RecommendationOutcome type doc explicitly pins:
//
//   - prior "realized": savings Squadron already counted have evaporated (a
//     rollback or config drift pushed the byte rate back up) → "reverted".
//     Folding this into "not_observed" (as the code did before) hid genuine
//     regressions of already-credited savings inside the "never worked" bucket.
//   - never realized: the fix simply hasn't taken effect yet → "pending" for
//     the first settling hour, then "not_observed".
func statusAtBaseline(priorStatus string, appliedAt, now time.Time) string {
	if priorStatus == "realized" {
		return "reverted"
	}
	if now.Sub(appliedAt) > time.Hour {
		return "not_observed"
	}
	return "pending"
}

// newOutcomeID returns a 16-hex-char random identifier. We don't
// need a full UUID — the IDs are private to one Squadron instance
// and the cardinality stays small.
func newOutcomeID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// extractAttributeKeyFromTitle pulls the quoted key out of a
// "Drop attribute %q from %s" title. Returns "" when the title
// doesn't match the noisy_attribute shape.
func extractAttributeKeyFromTitle(title string) string {
	start := strings.Index(title, `"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(title[start+1:], `"`)
	if end < 0 {
		return ""
	}
	return title[start+1 : start+1+end]
}
