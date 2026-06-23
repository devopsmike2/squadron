// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeMemoryTestExcludedRec(recID string) types.ExcludedRecommendation {
	return types.ExcludedRecommendation{
		RecommendationID:   recID,
		ConnectionID:       "conn-1",
		AccountID:          "123456789012",
		Region:             "us-east-1",
		RecommendationKind: "rds-pi-em",
		ResourceID:         "",
	}
}

// TestCheckRunStorage_Memory_RoundTrip — memory-store mirror of the
// SQLite RoundTrip test.
func TestCheckRunStorage_Memory_RoundTrip(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	ref := types.CheckRunRef{
		Owner: "octo", Repo: "widgets", CheckID: 12345, HeadSHA: "abc123",
	}
	require.NoError(t, s.SetCheckRunForRecommendation(
		ctx, makeMemoryTestExcludedRec("rec-1"), ref,
		"in_progress", "",
	))

	gotRef, status, conclusion, exists, err := s.GetCheckRunForRecommendation(ctx, "rec-1")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, ref, gotRef)
	assert.Equal(t, "in_progress", status)
	assert.Equal(t, "", conclusion)
}

// TestCheckRunStorage_Memory_UpdateExisting — memory-store mirror.
func TestCheckRunStorage_Memory_UpdateExisting(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	ref := types.CheckRunRef{
		Owner: "octo", Repo: "widgets", CheckID: 12345, HeadSHA: "abc123",
	}
	require.NoError(t, s.SetCheckRunForRecommendation(
		ctx, makeMemoryTestExcludedRec("rec-2"), ref,
		"in_progress", "",
	))
	time.Sleep(1 * time.Millisecond)
	require.NoError(t, s.SetCheckRunForRecommendation(
		ctx, makeMemoryTestExcludedRec("rec-2"), ref,
		"completed", "success",
	))
	gotRef, status, conclusion, exists, err := s.GetCheckRunForRecommendation(ctx, "rec-2")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, ref, gotRef)
	assert.Equal(t, "completed", status)
	assert.Equal(t, "success", conclusion)
}

// TestCheckRunStorage_Memory_GetMissingReturnsExistsFalse — memory-
// store mirror of the not-found path.
func TestCheckRunStorage_Memory_GetMissingReturnsExistsFalse(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	ref, status, conclusion, exists, err := s.GetCheckRunForRecommendation(ctx, "rec-missing")
	require.NoError(t, err)
	assert.False(t, exists)
	assert.Equal(t, types.CheckRunRef{}, ref)
	assert.Equal(t, "", status)
	assert.Equal(t, "", conclusion)
}

// TestCheckRunStorage_Memory_PreservesExclusionRow — memory-store
// mirror of the §11 Q3 guarantee: writing a check run on a recID
// MUST NOT set exclude_from_learning, the follow-up exclusion
// transition reads prevExcluded=false, and ListExcludedRecommendations
// returns the row after the bit is set.
func TestCheckRunStorage_Memory_PreservesExclusionRow(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	ref := types.CheckRunRef{
		Owner: "octo", Repo: "widgets", CheckID: 22222, HeadSHA: "def456",
	}
	rec := makeMemoryTestExcludedRec("rec-3")
	require.NoError(t, s.SetCheckRunForRecommendation(
		ctx, rec, ref, "in_progress", "",
	))

	rec.ExcludedBy = "alice"
	rec.ExcludedAt = time.Now().UTC()
	prevExcluded, err := s.SetRecommendationExclusion(ctx, rec, true)
	require.NoError(t, err)
	assert.False(t, prevExcluded,
		"check-run row MUST start with exclude_from_learning=0")

	gotRef, _, _, exists, err := s.GetCheckRunForRecommendation(ctx, "rec-3")
	require.NoError(t, err)
	assert.True(t, exists)
	assert.Equal(t, int64(22222), gotRef.CheckID)

	rows, err := s.ListExcludedRecommendations(
		ctx, "conn-1", "123456789012", "us-east-1", 100,
	)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "rec-3", rows[0].RecommendationID)
}
