// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTestExcludedRec is the shared projection the check-run tests
// use to seed the scope tuple. The scope fields are what the
// chunk-2 bridge will fill in at PR-open time; the tests here pin
// down storage round-trip behavior independently.
func makeTestExcludedRec(recID string) types.ExcludedRecommendation {
	return types.ExcludedRecommendation{
		RecommendationID:   recID,
		ConnectionID:       "conn-1",
		AccountID:          "123456789012",
		Region:             "us-east-1",
		RecommendationKind: "rds-pi-em",
		ResourceID:         "",
	}
}

// TestCheckRunStorage_SQLite_RoundTrip pins the happy-path round-
// trip on the SQLite path: SetCheckRunForRecommendation followed by
// GetCheckRunForRecommendation returns the same ref, status, and
// conclusion. The CheckRunRef carries Owner/Repo/CheckID/HeadSHA;
// all four MUST round-trip.
func TestCheckRunStorage_SQLite_RoundTrip(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		ref := types.CheckRunRef{
			Owner: "octo", Repo: "widgets", CheckID: 12345, HeadSHA: "abc123",
		}
		require.NoError(t, store.SetCheckRunForRecommendation(
			ctx, makeTestExcludedRec("rec-1"), ref,
			"in_progress", "",
		))

		gotRef, status, conclusion, exists, err := store.GetCheckRunForRecommendation(ctx, "rec-1")
		require.NoError(t, err)
		assert.True(t, exists)
		assert.Equal(t, ref, gotRef)
		assert.Equal(t, "in_progress", status)
		assert.Equal(t, "", conclusion)
	})
}

// TestCheckRunStorage_SQLite_UpdateExisting pins the upsert path:
// two consecutive Set calls on the same recID produce a row that
// reflects the SECOND write — the status moves from in_progress to
// completed/success.
func TestCheckRunStorage_SQLite_UpdateExisting(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		ref := types.CheckRunRef{
			Owner: "octo", Repo: "widgets", CheckID: 12345, HeadSHA: "abc123",
		}
		require.NoError(t, store.SetCheckRunForRecommendation(
			ctx, makeTestExcludedRec("rec-2"), ref,
			"in_progress", "",
		))
		// Touch the clock so the updated_at column would change if
		// we were inspecting it directly.
		time.Sleep(1 * time.Millisecond)
		require.NoError(t, store.SetCheckRunForRecommendation(
			ctx, makeTestExcludedRec("rec-2"), ref,
			"completed", "success",
		))

		gotRef, status, conclusion, exists, err := store.GetCheckRunForRecommendation(ctx, "rec-2")
		require.NoError(t, err)
		assert.True(t, exists)
		assert.Equal(t, ref, gotRef)
		assert.Equal(t, "completed", status)
		assert.Equal(t, "success", conclusion)
	})
}

// TestCheckRunStorage_SQLite_GetMissingReturnsExistsFalse pins the
// not-found path: Get on a recID that has no underlying row
// returns exists=false (NOT an error). Chunks 2/3/4 read this to
// skip the check-run side entirely on the inbound event.
func TestCheckRunStorage_SQLite_GetMissingReturnsExistsFalse(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		ref, status, conclusion, exists, err := store.GetCheckRunForRecommendation(ctx, "rec-does-not-exist")
		require.NoError(t, err)
		assert.False(t, exists)
		assert.Equal(t, types.CheckRunRef{}, ref)
		assert.Equal(t, "", status)
		assert.Equal(t, "", conclusion)
	})
}

// TestCheckRunStorage_SQLite_PreservesExclusionRow pins the §11 Q3
// guarantee: writing a check run on a recID does NOT set
// exclude_from_learning. The follow-up SetRecommendationExclusion
// call's prevExcluded MUST read false (the chunk-1 row never had
// the bit set), AND the subsequent ListExcludedRecommendations
// returns the row (chunk-4 logic stays intact).
func TestCheckRunStorage_SQLite_PreservesExclusionRow(t *testing.T) {
	withSQLiteStore(t, func(store types.ApplicationStore) {
		ctx := context.Background()
		ref := types.CheckRunRef{
			Owner: "octo", Repo: "widgets", CheckID: 22222, HeadSHA: "def456",
		}
		rec := makeTestExcludedRec("rec-3")
		require.NoError(t, store.SetCheckRunForRecommendation(
			ctx, rec, ref, "in_progress", "",
		))

		// The chunk-4 exclusion handler now lands on the same
		// recommendation_id. prevExcluded MUST be false — the
		// chunk-1 row never set the bit.
		rec.ExcludedBy = "alice"
		rec.ExcludedAt = time.Now().UTC()
		prevExcluded, err := store.SetRecommendationExclusion(ctx, rec, true)
		require.NoError(t, err)
		assert.False(t, prevExcluded,
			"check-run row MUST start with exclude_from_learning=0")

		// The check run still resolves with the original check_run_id.
		gotRef, _, _, exists, err := store.GetCheckRunForRecommendation(ctx, "rec-3")
		require.NoError(t, err)
		assert.True(t, exists)
		assert.Equal(t, int64(22222), gotRef.CheckID)

		// And ListExcludedRecommendations returns it (chunk-4
		// listing stays intact).
		rows, err := store.ListExcludedRecommendations(
			ctx, "conn-1", "123456789012", "us-east-1", 100,
		)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "rec-3", rows[0].RecommendationID)
	})
}
