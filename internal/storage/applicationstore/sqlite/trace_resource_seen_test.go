// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/devopsmike2/squadron/internal/traceindex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTraceRow is the test fixture for trace_resource_seen rows. The
// scope tuple, provider, and confidence are what the keying chain
// produces for a strong-match AWS host.id observation.
func makeTraceRow(key string, lastSeen time.Time, spanCount int64) traceindex.ResourceRow {
	return traceindex.ResourceRow{
		ResourceKey:      key,
		Provider:         "aws",
		ScopeID:          "12345",
		ResourceIDHint:   "",
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

// TestTraceResource_UpsertAndGetRoundTrip — round-trip a single row
// through Upsert + Get, verify every field is preserved.
func TestTraceResource_UpsertAndGetRoundTrip(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()
		now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
		row := makeTraceRow("aws:12345:i-0abc", now, 10)

		evicted, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{row})
		require.NoError(t, err)
		assert.Equal(t, 0, evicted)

		got, err := s.GetTraceResource(ctx, "aws:12345:i-0abc")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, row.ResourceKey, got.ResourceKey)
		assert.Equal(t, row.Provider, got.Provider)
		assert.Equal(t, row.ScopeID, got.ScopeID)
		assert.Equal(t, row.ServiceName, got.ServiceName)
		assert.Equal(t, row.SpanCount24h, got.SpanCount24h)
		assert.Equal(t, row.RootSpanCount24h, got.RootSpanCount24h)
		assert.Equal(t, row.AttributesJSON, got.AttributesJSON)
		assert.Equal(t, traceindex.MatchConfidenceStrong, got.MatchConfidence)
	})
}

// TestTraceResource_UpsertAccumulatesSpanCount — second upsert of
// the same key adds its span count to the existing row's count
// rather than overwriting.
func TestTraceResource_UpsertAccumulatesSpanCount(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()
		t0 := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

		_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{makeTraceRow("k", t0, 5)})
		require.NoError(t, err)
		_, err = s.UpsertTraceResources(ctx, []traceindex.ResourceRow{makeTraceRow("k", t0.Add(time.Minute), 7)})
		require.NoError(t, err)

		got, err := s.GetTraceResource(ctx, "k")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, int64(12), got.SpanCount24h, "span count accumulates across re-upserts")
		assert.Equal(t, t0.Add(time.Minute).UTC(), got.LastSeenAt.UTC(),
			"last_seen_at advances to the later observation")
	})
}

// TestTraceResource_ListByScope_FiltersByProvider — only rows with
// matching provider come back. Two providers seeded; query for one.
func TestTraceResource_ListByScope_FiltersByProvider(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()
		now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

		awsRow := makeTraceRow("aws:12345:a", now, 1)
		gcpRow := makeTraceRow("gcp:proj:b", now, 1)
		gcpRow.Provider = "gcp"
		gcpRow.ScopeID = "proj"
		_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{awsRow, gcpRow})
		require.NoError(t, err)

		rows, err := s.ListTraceResourcesByScope(ctx, "aws", "12345", time.Time{}, 0)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "aws", rows[0].Provider)
	})
}

// TestTraceResource_ListByScope_FiltersBySince — rows older than the
// since cutoff are excluded.
func TestTraceResource_ListByScope_FiltersBySince(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()
		now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

		old := makeTraceRow("aws:12345:old", now.Add(-2*time.Hour), 1)
		fresh := makeTraceRow("aws:12345:fresh", now, 1)
		_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{old, fresh})
		require.NoError(t, err)

		rows, err := s.ListTraceResourcesByScope(ctx, "aws", "12345", now.Add(-1*time.Hour), 0)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "aws:12345:fresh", rows[0].ResourceKey)
	})
}

// TestTraceResource_CountByScope — Count returns the row count for
// the matching (provider, scope_id) tuple.
func TestTraceResource_CountByScope(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		ctx := context.Background()
		now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
		_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{
			makeTraceRow("aws:12345:a", now, 1),
			makeTraceRow("aws:12345:b", now, 1),
			makeTraceRow("aws:12345:c", now, 1),
		})
		require.NoError(t, err)
		n, err := s.CountTraceResourcesByScope(ctx, "aws", "12345")
		require.NoError(t, err)
		assert.Equal(t, 3, n)

		n, err = s.CountTraceResourcesByScope(ctx, "aws", "different-account")
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
}

// TestTraceResource_LRUEviction_RemovesOldestLastSeen — push past
// the cap and verify the oldest last_seen_at row got evicted.
func TestTraceResource_LRUEviction_RemovesOldestLastSeen(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		// Drop the cap to 2 via the concrete Storage handle.
		st := s.(*Storage)
		st.traceIndexMaxRow = 2

		ctx := context.Background()
		now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

		_, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{
			makeTraceRow("oldest", now.Add(-2*time.Hour), 1),
			makeTraceRow("middle", now.Add(-1*time.Hour), 1),
		})
		require.NoError(t, err)
		evicted, err := s.UpsertTraceResources(ctx, []traceindex.ResourceRow{
			makeTraceRow("newest", now, 1),
		})
		require.NoError(t, err)
		assert.Equal(t, 1, evicted, "LRU should evict one row to drop count to cap")

		got, err := s.GetTraceResource(ctx, "oldest")
		require.NoError(t, err)
		assert.Nil(t, got, "oldest row evicted")
		got, err = s.GetTraceResource(ctx, "newest")
		require.NoError(t, err)
		assert.NotNil(t, got)
	})
}
