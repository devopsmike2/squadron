// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is a minimal in-memory Store implementation used by the
// index tests. It supports an injectable maxRows cap so the LRU
// eviction test can pin behavior without setting up the SQLite path.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]ResourceRow
	maxRows  int
	upserts  int
	evicts   int
	listSeen []ResourceRow
}

func newFakeStore(maxRows int) *fakeStore {
	return &fakeStore{rows: make(map[string]ResourceRow), maxRows: maxRows}
}

func (f *fakeStore) UpsertTraceResources(_ context.Context, rows []ResourceRow) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	for _, r := range rows {
		if existing, ok := f.rows[r.ResourceKey]; ok {
			existing.SpanCount24h += r.SpanCount24h
			existing.RootSpanCount24h += r.RootSpanCount24h
			if r.LastSeenAt.After(existing.LastSeenAt) {
				existing.LastSeenAt = r.LastSeenAt
				existing.AttributesJSON = r.AttributesJSON
				existing.ServiceName = r.ServiceName
			}
			existing.UpdatedAt = r.UpdatedAt
			f.rows[r.ResourceKey] = existing
		} else {
			f.rows[r.ResourceKey] = r
		}
	}
	evicted := 0
	if f.maxRows > 0 && len(f.rows) > f.maxRows {
		all := make([]ResourceRow, 0, len(f.rows))
		for _, r := range f.rows {
			all = append(all, r)
		}
		sort.Slice(all, func(i, j int) bool {
			return all[i].LastSeenAt.Before(all[j].LastSeenAt)
		})
		over := len(f.rows) - f.maxRows
		for i := 0; i < over; i++ {
			delete(f.rows, all[i].ResourceKey)
			evicted++
		}
		f.evicts += evicted
	}
	return evicted, nil
}

func (f *fakeStore) GetTraceResource(_ context.Context, key string) (*ResourceRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[key]; ok {
		return &r, nil
	}
	return nil, nil
}

func (f *fakeStore) ListTraceResourcesByScope(_ context.Context, provider, scopeID string, since time.Time, limit int) ([]ResourceRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ResourceRow, 0)
	for _, r := range f.rows {
		if r.Provider != provider || r.ScopeID != scopeID {
			continue
		}
		if !since.IsZero() && r.LastSeenAt.Before(since) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastSeenAt.After(out[j].LastSeenAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	f.listSeen = out
	return out, nil
}

func (f *fakeStore) CountTraceResourcesByScope(_ context.Context, provider, scopeID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.Provider == provider && r.ScopeID == scopeID {
			n++
		}
	}
	return n, nil
}

// fixedClock returns a function that always returns the supplied
// instant. Used to keep flush + coverage tests deterministic.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestIndex_Observe_Strong_AddsToCache pins the strong-tier observe
// path: a single Observe call lands a row in the pending map with
// match_confidence=strong and the expected counts.
func TestIndex_Observe_Strong_AddsToCache(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, fixedClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)))

	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{
			"cloud.provider":    "aws",
			"cloud.account.id":  "12345",
			"cloud.resource_id": "arn:aws:ec2:us-east-1:12345:instance/i-1",
		},
		SpanCount:     7,
		RootSpanCount: 2,
	})

	idx.mu.Lock()
	defer idx.mu.Unlock()
	require.Len(t, idx.pendingRows, 1)
	row := idx.pendingRows["arn:aws:ec2:us-east-1:12345:instance/i-1"]
	require.NotNil(t, row)
	assert.Equal(t, MatchConfidenceStrong, row.MatchConfidence)
	assert.Equal(t, int64(7), row.SpanCount24h)
	assert.Equal(t, int64(2), row.RootSpanCount24h)
}

// TestIndex_Observe_NoKey_DropsSilently — an attribute map with no
// usable identifier is dropped without crashing the hot path and
// without populating the pending map.
func TestIndex_Observe_NoKey_DropsSilently(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, nil)

	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{"cloud.provider": "aws"}, // nothing else
		SpanCount:  5,
	})
	idx.Observe(context.Background(), ResourceObservation{}) // empty obs

	idx.mu.Lock()
	defer idx.mu.Unlock()
	assert.Len(t, idx.pendingRows, 0)
}

// TestIndex_Flush_WritesToStore_ReturnsWrittenCount pins the flush
// path: pending rows drain to the store and the written count
// equals the pending count. Two distinct keys produce two rows.
func TestIndex_Flush_WritesToStore_ReturnsWrittenCount(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, fixedClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)))

	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{
			"cloud.provider":    "aws",
			"cloud.resource_id": "arn:1",
		},
		SpanCount: 3,
	})
	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{
			"cloud.provider":    "aws",
			"cloud.resource_id": "arn:2",
		},
		SpanCount: 5,
	})

	written, evicted, err := idx.Flush(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, written)
	assert.Equal(t, 0, evicted)
	assert.Len(t, store.rows, 2)

	// Pending map is drained.
	idx.mu.Lock()
	assert.Len(t, idx.pendingRows, 0)
	idx.mu.Unlock()
}

// TestIndex_Flush_RespectsMaxRows_EvictsLRU — pin the slice-1
// threat-model §12 LRU cap. We pre-seed the store at capacity, then
// flush ONE more row and verify the oldest LastSeenAt got evicted.
func TestIndex_Flush_RespectsMaxRows_EvictsLRU(t *testing.T) {
	store := newFakeStore(2)
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)

	// Pre-seed two rows directly to the store via an out-of-band
	// upsert. The "old" row's LastSeenAt is 2h before the others.
	_, err := store.UpsertTraceResources(context.Background(), []ResourceRow{
		{ResourceKey: "old", Provider: "aws", ScopeID: "12345", LastSeenAt: now.Add(-2 * time.Hour)},
		{ResourceKey: "mid", Provider: "aws", ScopeID: "12345", LastSeenAt: now.Add(-1 * time.Hour)},
	})
	require.NoError(t, err)
	assert.Len(t, store.rows, 2)

	// Now flush a third row through the index, tripping the cap.
	idx := NewIndex(store, 0, fixedClock(now))
	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{"cloud.provider": "aws", "cloud.account.id": "12345", "host.id": "new"},
		SpanCount:  1,
	})
	written, evicted, err := idx.Flush(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, written)
	assert.Equal(t, 1, evicted)

	// Oldest row should be gone.
	_, hasOld := store.rows["old"]
	assert.False(t, hasOld, "LRU should have evicted the oldest LastSeenAt row")
	assert.Len(t, store.rows, 2)
}

// TestIndex_Coverage_ZeroSafeOnEmpty — acceptance test 11. Cold
// start: no inventory, no spans. CoveragePct MUST be 0.0, NOT NaN.
func TestIndex_Coverage_ZeroSafeOnEmpty(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, fixedClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)))

	got, err := idx.Coverage(context.Background(), "aws", "12345", 0)
	require.NoError(t, err)
	assert.Equal(t, 0.0, got.CoveragePct)
	assert.Equal(t, 0, got.EmittingCount)
	assert.Equal(t, 0, got.InventoryCount)
}

// TestIndex_Coverage_StrongAndWeakBreakdown — verify the per-
// confidence percentages are computed honestly when both tiers
// contribute rows.
func TestIndex_Coverage_StrongAndWeakBreakdown(t *testing.T) {
	store := newFakeStore(0)
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	_, err := store.UpsertTraceResources(context.Background(), []ResourceRow{
		{ResourceKey: "k1", Provider: "aws", ScopeID: "12345",
			MatchConfidence: MatchConfidenceStrong, LastSeenAt: now, UpdatedAt: now},
		{ResourceKey: "k2", Provider: "aws", ScopeID: "12345",
			MatchConfidence: MatchConfidenceStrong, LastSeenAt: now, UpdatedAt: now},
		{ResourceKey: "k3", Provider: "aws", ScopeID: "12345",
			MatchConfidence: MatchConfidenceWeak, LastSeenAt: now, UpdatedAt: now},
		{ResourceKey: "k4", Provider: "aws", ScopeID: "12345",
			MatchConfidence: MatchConfidenceWeak, LastSeenAt: now, UpdatedAt: now},
	})
	require.NoError(t, err)

	idx := NewIndex(store, 0, fixedClock(now.Add(time.Minute)))
	got, err := idx.Coverage(context.Background(), "aws", "12345", 10)
	require.NoError(t, err)
	assert.Equal(t, 4, got.EmittingCount)
	assert.Equal(t, 10, got.InventoryCount)
	assert.InDelta(t, 40.0, got.CoveragePct, 0.001)
	assert.InDelta(t, 50.0, got.StrongMatchPct, 0.001)
	assert.InDelta(t, 50.0, got.WeakMatchPct, 0.001)
}

// TestIndex_LastSeenAt_ReturnsCachedAndStored — exercises both the
// pending-map fast path and the store-backed slow path.
func TestIndex_LastSeenAt_ReturnsCachedAndStored(t *testing.T) {
	store := newFakeStore(0)
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	_, err := store.UpsertTraceResources(context.Background(), []ResourceRow{
		{ResourceKey: "stored-only", LastSeenAt: now},
	})
	require.NoError(t, err)

	idx := NewIndex(store, 0, fixedClock(now.Add(time.Minute)))
	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{
			"cloud.provider":    "aws",
			"cloud.resource_id": "pending-key",
		},
		SpanCount: 1,
		Timestamp: now.Add(time.Hour),
	})

	// Pending fast path.
	ts, ok, err := idx.LastSeenAt(context.Background(), "pending-key")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, now.Add(time.Hour).UTC(), ts.UTC())

	// Store slow path.
	ts, ok, err = idx.LastSeenAt(context.Background(), "stored-only")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, now.UTC(), ts.UTC())
}

// TestIndex_LastSeenAt_NotFound_ReturnsOkFalse — neither cache nor
// store has a row; returns ok=false (and no error).
func TestIndex_LastSeenAt_NotFound_ReturnsOkFalse(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, nil)
	ts, ok, err := idx.LastSeenAt(context.Background(), "never-observed")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.True(t, ts.IsZero())
}

// TestIndex_Observe_Accumulates — two Observe calls on the same key
// add their span counts and refresh LastSeenAt to the later
// timestamp.
func TestIndex_Observe_Accumulates(t *testing.T) {
	store := newFakeStore(0)
	t0 := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	idx := NewIndex(store, 0, fixedClock(t0))

	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{
			"cloud.provider":    "aws",
			"cloud.resource_id": "k",
		},
		SpanCount:     3,
		RootSpanCount: 1,
		Timestamp:     t0,
	})
	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{
			"cloud.provider":    "aws",
			"cloud.resource_id": "k",
		},
		SpanCount:     5,
		RootSpanCount: 2,
		Timestamp:     t0.Add(time.Minute),
	})

	idx.mu.Lock()
	defer idx.mu.Unlock()
	require.Len(t, idx.pendingRows, 1)
	row := idx.pendingRows["k"]
	assert.Equal(t, int64(8), row.SpanCount24h)
	assert.Equal(t, int64(3), row.RootSpanCount24h)
	assert.Equal(t, t0.Add(time.Minute), row.LastSeenAt)
	assert.Equal(t, t0, row.FirstSeenAt, "FirstSeenAt sticks at the first observation")
}
