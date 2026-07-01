// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestStatusAtBaseline pins the transition table for the "observed byte rate
// back at baseline" case. The critical row is realized→reverted: an outcome
// whose credited savings evaporated must NOT be folded into "not_observed"
// (which reads as "the fix never worked"). The never-realized rows keep the
// pending(<1h)/not_observed(>1h) settling behavior.
func TestStatusAtBaseline(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		prior     string
		appliedAt time.Time
		want      string
	}{
		{"realized regresses -> reverted (fresh)", "realized", now.Add(-10 * time.Minute), "reverted"},
		{"realized regresses -> reverted (aged)", "realized", now.Add(-3 * time.Hour), "reverted"},
		{"never realized, still settling -> pending", "pending", now.Add(-10 * time.Minute), "pending"},
		{"never realized, past window -> not_observed", "pending", now.Add(-2 * time.Hour), "not_observed"},
		{"fresh apply, still settling -> pending", "", now.Add(-1 * time.Minute), "pending"},
		{"fresh apply, past window -> not_observed", "", now.Add(-90 * time.Minute), "not_observed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusAtBaseline(tc.prior, tc.appliedAt, now); got != tc.want {
				t.Errorf("statusAtBaseline(%q, %s) = %q, want %q", tc.prior, now.Sub(tc.appliedAt), got, tc.want)
			}
		})
	}
}

// fakeOutcomeStore is a minimal OutcomeStore for the savings handler tests.
type fakeOutcomeStore struct {
	outcomes []*storetypes.RecommendationOutcome
	updated  int
}

func (f *fakeOutcomeStore) CreateRecommendationOutcome(_ context.Context, o *storetypes.RecommendationOutcome) error {
	f.outcomes = append(f.outcomes, o)
	return nil
}
func (f *fakeOutcomeStore) UpdateRecommendationOutcome(_ context.Context, _ *storetypes.RecommendationOutcome) error {
	f.updated++
	return nil
}
func (f *fakeOutcomeStore) ListRecommendationOutcomes(_ context.Context) ([]*storetypes.RecommendationOutcome, error) {
	return f.outcomes, nil
}

// TestHandleRealized_RevertedExcludedFromTallyAndCounted proves the reverted
// status is (a) excluded from monthly_realized_usd — the savings no longer
// exist — and (b) counted in its own bucket so the sub-counts sum to total.
// Before the fix, reverted fell through every switch case: its stale realized
// USD would be missed from the tally AND the counts silently under-summed.
// Uses non-refreshable outcomes (category outlier_agent) so the insights
// refresh path is skipped — nil engine/insights/pricer are all fine here.
func TestHandleRealized_RevertedExcludedFromTallyAndCounted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	twoHoursAgo := time.Now().Add(-2 * time.Hour).UTC()

	store := &fakeOutcomeStore{outcomes: []*storetypes.RecommendationOutcome{
		{ID: "a", Category: "outlier_agent", Status: "realized", AppliedAt: twoHoursAgo, RealizedSavingsPerMonthUSD: 10},
		// Reverted row still carries a stale realized figure; the tally must
		// ignore it (savings evaporated).
		{ID: "b", Category: "outlier_agent", Status: "reverted", AppliedAt: twoHoursAgo, RealizedSavingsPerMonthUSD: 99},
		{ID: "c", Category: "outlier_agent", Status: "not_observed", AppliedAt: twoHoursAgo},
	}}

	h := &SavingsHandlers{store: store, logger: zap.NewNop()}
	r := gin.New()
	r.GET("/savings/realized", h.HandleRealized)

	req := httptest.NewRequest(http.MethodGet, "/savings/realized", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		MonthlyRealizedUSD float64 `json:"monthly_realized_usd"`
		Counts             struct {
			Realized    int `json:"realized"`
			Pending     int `json:"pending"`
			NotObserved int `json:"not_observed"`
			Reverted    int `json:"reverted"`
			Total       int `json:"total"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.MonthlyRealizedUSD != 10 {
		t.Errorf("monthly_realized_usd = %v, want 10 (reverted's stale $99 must be excluded)", resp.MonthlyRealizedUSD)
	}
	if resp.Counts.Reverted != 1 {
		t.Errorf("counts.reverted = %d, want 1", resp.Counts.Reverted)
	}
	if resp.Counts.Realized != 1 || resp.Counts.NotObserved != 1 {
		t.Errorf("counts realized/not_observed = %d/%d, want 1/1", resp.Counts.Realized, resp.Counts.NotObserved)
	}
	// The sub-counts must sum to total — the regression the missing switch
	// case caused.
	sum := resp.Counts.Realized + resp.Counts.Pending + resp.Counts.NotObserved + resp.Counts.Reverted
	if sum != resp.Counts.Total {
		t.Errorf("sub-counts (%d) != total (%d)", sum, resp.Counts.Total)
	}
}
