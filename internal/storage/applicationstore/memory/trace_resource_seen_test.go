// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/traceindex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeMemoryTraceRow(key string, lastSeen time.Time, spanCount int64) traceindex.ResourceRow {
	return traceindex.ResourceRow{
		ResourceKey:      key,
		Provider:         "aws",
		ScopeID:          "12345",
		ServiceName:      "checkout",
		FirstSeenAt:      lastSeen,
		LastSeenAt:       lastSeen,
		SpanCount24h:     spanCount,
		RootSpanCount24h: spanCount / 2,
		AttributesJSON:   `{"cloud.provider":"aws","host.id":"i-0abc"}`,
		MatchConfidence:  traceindex.MatchConfidenceStrong,
		UpdatedAt:        lastSeen,
	}
}

func TestTraceResource_Memory_UpsertAndGetRoundTrip(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	evicted, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{makeMemoryTraceRow("k", now, 10)})
	require.NoError(t, err)
	assert.Equal(t, 0, evicted)

	got, err := s.GetTraceResource(ctx, "k")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "k", got.ResourceKey)
	assert.Equal(t, int64(10), got.SpanCount24h)
	assert.Equal(t, traceindex.MatchConfidenceStrong, got.MatchConfidence)
}

func TestTraceResource_Memory_UpsertAccumulatesSpanCount(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	t0 := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{makeMemoryTraceRow("k", t0, 5)})
	require.NoError(t, err)
	_, err = s.UpsertTraceResources(ctx, []traceindex.ResourceRow{makeMemoryTraceRow("k", t0.Add(time.Minute), 7)})
	require.NoError(t, err)

	got, err := s.GetTraceResource(ctx, "k")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(12), got.SpanCount24h)
}

func TestTraceResource_Memory_ListByScope_FiltersByProvider(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	awsRow := makeMemoryTraceRow("a", now, 1)
	gcpRow := makeMemoryTraceRow("b", now, 1)
	gcpRow.Provider = "gcp"
	gcpRow.ScopeID = "proj"
	_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{awsRow, gcpRow})
	require.NoError(t, err)

	rows, err := s.ListTraceResourcesByScope(ctx, "aws", "12345", time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "aws", rows[0].Provider)
}

func TestTraceResource_Memory_ListByScope_FiltersBySince(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{
		makeMemoryTraceRow("old", now.Add(-2*time.Hour), 1),
		makeMemoryTraceRow("fresh", now, 1),
	})
	require.NoError(t, err)

	rows, err := s.ListTraceResourcesByScope(ctx, "aws", "12345", now.Add(-1*time.Hour), 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "fresh", rows[0].ResourceKey)
}

func TestTraceResource_Memory_CountByScope(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{
		makeMemoryTraceRow("a", now, 1),
		makeMemoryTraceRow("b", now, 1),
	})
	require.NoError(t, err)

	n, err := s.CountTraceResourcesByScope(ctx, "aws", "12345")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestTraceResource_Memory_LRUEviction_RemovesOldestLastSeen(t *testing.T) {
	s := NewStore()
	s.SetTraceIndexMaxRowsForTest(2)
	ctx := context.Background()
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{
		makeMemoryTraceRow("oldest", now.Add(-2*time.Hour), 1),
		makeMemoryTraceRow("middle", now.Add(-1*time.Hour), 1),
	})
	require.NoError(t, err)
	evicted, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{
		makeMemoryTraceRow("newest", now, 1),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, evicted)

	got, err := s.GetTraceResource(ctx, "oldest")
	require.NoError(t, err)
	assert.Nil(t, got, "oldest row evicted")
	got, err = s.GetTraceResource(ctx, "newest")
	require.NoError(t, err)
	assert.NotNil(t, got)
}
