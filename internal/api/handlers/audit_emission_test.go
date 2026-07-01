// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/insights"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// --- cost-spike acknowledge audit ------------------------------------------

type fakeCostSpikeStore struct {
	ev *storetypes.CostSpikeEvent
}

func (f *fakeCostSpikeStore) GetCostSpikeEvent(_ context.Context, _ string) (*storetypes.CostSpikeEvent, error) {
	return f.ev, nil
}
func (f *fakeCostSpikeStore) UpdateCostSpikeEvent(_ context.Context, e *storetypes.CostSpikeEvent) error {
	f.ev = e
	return nil
}
func (f *fakeCostSpikeStore) ListCostSpikeEvents(_ context.Context, _ storetypes.CostSpikeFilter) ([]*storetypes.CostSpikeEvent, error) {
	return nil, nil
}

// TestHandleAcknowledge_EmitsAuditOnFirstAck proves the new operator-action
// audit row: acknowledging an un-acked spike records exactly one
// cost_spike.acknowledged event scoped to the spike id.
func TestHandleAcknowledge_EmitsAuditOnFirstAck(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeCostSpikeStore{ev: &storetypes.CostSpikeEvent{ID: "spike-1"}}
	audit := &recordingAuditService{}
	h := NewCostSpikesHandlers(store, nil).WithAuditService(audit)

	r := gin.New()
	r.POST("/alerts/cost-spikes/:id/acknowledge", h.HandleAcknowledge)
	req := httptest.NewRequest(http.MethodPost, "/alerts/cost-spikes/spike-1/acknowledge", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, audit.recorded, 1, "first ack must emit one audit event")
	assert.Equal(t, services.AuditEventCostSpikeAcknowledged, audit.recorded[0].EventType)
	assert.Equal(t, services.AuditTargetCostSpike, audit.recorded[0].TargetType)
	assert.Equal(t, "spike-1", audit.recorded[0].TargetID)
}

// TestHandleAcknowledge_IdempotentReackEmitsNoAudit confirms the idempotency
// guard: re-acking an already-acknowledged spike returns 200 but records no
// duplicate audit row.
func TestHandleAcknowledge_IdempotentReackEmitsNoAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	store := &fakeCostSpikeStore{ev: &storetypes.CostSpikeEvent{ID: "spike-1", AcknowledgedAt: &now}}
	audit := &recordingAuditService{}
	h := NewCostSpikesHandlers(store, nil).WithAuditService(audit)

	r := gin.New()
	r.POST("/alerts/cost-spikes/:id/acknowledge", h.HandleAcknowledge)
	req := httptest.NewRequest(http.MethodPost, "/alerts/cost-spikes/spike-1/acknowledge", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, audit.recorded, 0, "re-ack of an already-acknowledged spike must not emit a second audit row")
}

// --- savings applied audit -------------------------------------------------

// erroringInsights satisfies recommendations.InsightsQuerier but fails the
// first query, so Engine.Evaluate returns an error and HandleApplied skips the
// live-recommendation enrichment loop and proceeds with the request body — the
// audit emission under test happens after the outcome is persisted regardless.
type erroringInsights struct{}

func (erroringInsights) FleetVolume(context.Context, insights.Window, []insights.Signal) (*insights.FleetSummary, error) {
	return nil, errors.New("no insights in test")
}
func (erroringInsights) TopAgents(context.Context, insights.Window, int) ([]insights.AgentVolume, error) {
	return nil, nil
}
func (erroringInsights) TopAttributes(context.Context, insights.Window, insights.Signal, int) ([]insights.AttributeVolume, error) {
	return nil, nil
}
func (erroringInsights) TopMetricCardinality(context.Context, insights.Window, int, int64) ([]insights.MetricCardinality, error) {
	return nil, nil
}

// TestHandleApplied_EmitsAuditEvent proves the Apply click now lands on the
// audit timeline: after the RecommendationOutcome row is created, a
// savings.recommendation_applied event is recorded, scoped to the
// recommendation id and carrying the frozen savings estimate.
func TestHandleApplied_EmitsAuditEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeOutcomeStore{}
	audit := &recordingAuditService{}
	engine := recommendations.NewEngine(erroringInsights{}, nil, nil, zap.NewNop())
	h := NewSavingsHandlers(store, engine, nil, nil, zap.NewNop()).WithAuditService(audit)

	r := gin.New()
	r.POST("/recommendations/:id/applied", h.HandleApplied)
	body := `{"title":"Drop attribute \"k8s.pod.uid\"","category":"noisy_attribute","est_savings_per_month_usd":12.5}`
	req := httptest.NewRequest(http.MethodPost, "/recommendations/rec-1/applied", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	require.Len(t, audit.recorded, 1, "an Apply click must emit one audit event")
	got := audit.recorded[0]
	assert.Equal(t, services.AuditEventSavingsRecommendationApplied, got.EventType)
	assert.Equal(t, services.AuditTargetRecommendation, got.TargetType)
	assert.Equal(t, "rec-1", got.TargetID)
	assert.Equal(t, "rec-1", got.Payload["recommendation_id"])
	assert.Equal(t, 12.5, got.Payload["est_savings_per_month_usd_at_apply"])
}

// TestHandleApplied_NilAuditServiceSafe guards the optional-audit path: with no
// recorder wired the Apply still succeeds (the outcome is created) without
// panicking.
func TestHandleApplied_NilAuditServiceSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeOutcomeStore{}
	engine := recommendations.NewEngine(erroringInsights{}, nil, nil, zap.NewNop())
	h := NewSavingsHandlers(store, engine, nil, nil, zap.NewNop()) // no WithAuditService

	r := gin.New()
	r.POST("/recommendations/:id/applied", h.HandleApplied)
	req := httptest.NewRequest(http.MethodPost, "/recommendations/rec-1/applied",
		strings.NewReader(`{"title":"t","category":"noisy_attribute"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	assert.NotPanics(t, func() { r.ServeHTTP(w, req) })
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, store.outcomes, 1, "outcome still persisted without an audit recorder")
}
