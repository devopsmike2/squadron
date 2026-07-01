// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// retentionGCContract mirrors the two interfaces cmd/all-in-one asserts the
// application store against to schedule retention GC (operatorTableRetentionGC
// + discoveryTableRetentionGC). Those assertions are done at RUNTIME via a type
// switch, so a signature drift on any one prune method would silently fail the
// assertion and disable the whole sweep — a table would leak in production with
// no compile error and no test failure. Pinning *Storage against the combined
// contract here turns that latent failure into a build error the moment a
// signature changes. Keep this list in lockstep with the two main.go interfaces.
type retentionGCContract interface {
	// operator-activity tables
	DeleteClosedCostSpikeEventsBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteRecommendationOutcomesBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteDismissedIncidentDraftsBefore(ctx context.Context, before time.Time) (int64, error)
	// discovery/serverless scan tables
	DeleteServerlessBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteEventSourceInstancesBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteOrchestrationInstancesBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteColdStartObservationsBefore(ctx context.Context, before time.Time) error
	DeleteErrorRateObservationsBefore(ctx context.Context, before time.Time) error
}

// Compile-time guard: the sqlite store must satisfy every retention predicate
// main.go schedules. If this stops compiling, a Delete*Before signature drifted
// and the corresponding GC sweep would silently no-op in production.
var _ retentionGCContract = (*Storage)(nil)

// TestRetention_DeleteClosedCostSpikeEventsBefore: closed spikes older
// than the cutoff are pruned; recent closed spikes AND open spikes (any
// age) survive — an unresolved anomaly must never be GC'd.
func TestRetention_DeleteClosedCostSpikeEventsBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-100 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-1 * 24 * time.Hour)

		oldEnded := old
		require.NoError(t, store.CreateCostSpikeEvent(ctx, &types.CostSpikeEvent{
			ID: "closed-old", StartedAt: old.Add(-time.Hour), EndedAt: &oldEnded, Severity: "warn"}))
		recentEnded := recent
		require.NoError(t, store.CreateCostSpikeEvent(ctx, &types.CostSpikeEvent{
			ID: "closed-recent", StartedAt: recent.Add(-time.Hour), EndedAt: &recentEnded, Severity: "warn"}))
		// Open (ended_at NULL) + very old — must NOT be pruned.
		require.NoError(t, store.CreateCostSpikeEvent(ctx, &types.CostSpikeEvent{
			ID: "open-old", StartedAt: old, Severity: "critical"}))

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		n, err := store.DeleteClosedCostSpikeEventsBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the closed+old spike should be deleted")

		gone, err := store.GetCostSpikeEvent(ctx, "closed-old")
		require.NoError(t, err)
		require.Nil(t, gone, "closed-old should be pruned")
		for _, id := range []string{"closed-recent", "open-old"} {
			kept, err := store.GetCostSpikeEvent(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, kept, "%s should survive the sweep", id)
		}
	})
}

// TestRetention_DeleteRecommendationOutcomesBefore: outcomes applied
// before the cutoff are pruned; newer ones survive.
func TestRetention_DeleteRecommendationOutcomesBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-100 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-1 * 24 * time.Hour)

		require.NoError(t, store.CreateRecommendationOutcome(ctx, &types.RecommendationOutcome{
			ID: "o-old", RecommendationID: "r1", AppliedAt: old, Title: "t", Category: "c"}))
		require.NoError(t, store.CreateRecommendationOutcome(ctx, &types.RecommendationOutcome{
			ID: "o-recent", RecommendationID: "r2", AppliedAt: recent, Title: "t", Category: "c"}))

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		n, err := store.DeleteRecommendationOutcomesBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the old outcome should be deleted")

		remaining, err := store.ListRecommendationOutcomes(ctx)
		require.NoError(t, err)
		require.Len(t, remaining, 1, "only the recent outcome should survive")
		require.Equal(t, "o-recent", remaining[0].ID)
	})
}

// TestRetention_DeleteDismissedIncidentDraftsBefore: dismissed drafts
// older than the cutoff are pruned; recent dismissed drafts AND non-
// dismissed drafts (draft / published, any age) survive — a
// still-actionable or published-for-dedup draft must never be GC'd.
func TestRetention_DeleteDismissedIncidentDraftsBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-100 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-1 * 24 * time.Hour)

		// Dismissed + old → pruned.
		require.NoError(t, store.CreateIncidentDraft(ctx, &types.IncidentDraft{
			ID: "dismissed-old", Status: "dismissed", Title: "t", BodyMarkdown: "b",
			CreatedAt: old.Add(-time.Hour), UpdatedAt: old}))
		// Dismissed + recent → survives.
		require.NoError(t, store.CreateIncidentDraft(ctx, &types.IncidentDraft{
			ID: "dismissed-recent", Status: "dismissed", Title: "t", BodyMarkdown: "b",
			CreatedAt: recent.Add(-time.Hour), UpdatedAt: recent}))
		// Draft + old → survives (not dismissed).
		require.NoError(t, store.CreateIncidentDraft(ctx, &types.IncidentDraft{
			ID: "draft-old", Status: "draft", Title: "t", BodyMarkdown: "b",
			CreatedAt: old.Add(-time.Hour), UpdatedAt: old}))
		// Published + old → survives (load-bearing dedup/link record).
		require.NoError(t, store.CreateIncidentDraft(ctx, &types.IncidentDraft{
			ID: "published-old", Status: "published", Title: "t", BodyMarkdown: "b",
			ActionRequestID: "ar-1", CreatedAt: old.Add(-time.Hour), UpdatedAt: old}))

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		n, err := store.DeleteDismissedIncidentDraftsBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the dismissed+old draft should be deleted")

		gone, err := store.GetIncidentDraft(ctx, "dismissed-old")
		require.NoError(t, err)
		require.Nil(t, gone, "dismissed-old should be pruned")
		for _, id := range []string{"dismissed-recent", "draft-old", "published-old"} {
			kept, err := store.GetIncidentDraft(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, kept, "%s should survive the sweep", id)
		}
	})
}
