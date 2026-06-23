// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"sort"
	"time"

	"github.com/devopsmike2/squadron/internal/traceindex"
)

// SetTraceIndexMaxRowsForTest overrides the LRU cap for tests that
// need to exercise the eviction path without seeding 100K rows. The
// SQLite store reads the cap from SQUADRON_TRACEINDEX_MAX_ROWS at
// construction; this exposes a parallel hook for the memory store.
// Production code never calls this.
func (s *Store) SetTraceIndexMaxRowsForTest(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > 0 {
		s.traceMaxRows = n
	}
}

// UpsertTraceResources — v0.89.74 (#705 Stream 103, slice 1 chunk 1).
// Memory-store mirror of the SQLite same-named method. Accumulates
// span counts on re-observations, refreshes last_seen_at and
// attributes_json, preserves first_seen_at, and applies the LRU cap
// to keep the map size bounded.
func (s *Store) UpsertTraceResources(
	_ context.Context,
	rows []traceindex.ResourceRow,
) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range rows {
		if existing, ok := s.traceResourceSeen[r.ResourceKey]; ok {
			existing.Provider = r.Provider
			existing.ScopeID = r.ScopeID
			if r.ResourceIDHint != "" {
				existing.ResourceIDHint = r.ResourceIDHint
			}
			existing.ServiceName = r.ServiceName
			existing.SpanCount24h += r.SpanCount24h
			existing.RootSpanCount24h += r.RootSpanCount24h
			if r.LastSeenAt.After(existing.LastSeenAt) {
				existing.LastSeenAt = r.LastSeenAt.UTC()
			}
			existing.AttributesJSON = r.AttributesJSON
			existing.MatchConfidence = r.MatchConfidence
			existing.UpdatedAt = r.UpdatedAt.UTC()
			s.traceResourceSeen[r.ResourceKey] = existing
			continue
		}
		row := r
		row.FirstSeenAt = r.FirstSeenAt.UTC()
		row.LastSeenAt = r.LastSeenAt.UTC()
		row.UpdatedAt = r.UpdatedAt.UTC()
		s.traceResourceSeen[r.ResourceKey] = row
	}

	cap := s.traceMaxRows
	if cap <= 0 {
		cap = memoryTraceIndexMaxRows
	}
	evicted := 0
	if len(s.traceResourceSeen) > cap {
		over := len(s.traceResourceSeen) - cap
		all := make([]traceindex.ResourceRow, 0, len(s.traceResourceSeen))
		for _, r := range s.traceResourceSeen {
			all = append(all, r)
		}
		sort.Slice(all, func(i, j int) bool {
			return all[i].LastSeenAt.Before(all[j].LastSeenAt)
		})
		for i := 0; i < over && i < len(all); i++ {
			delete(s.traceResourceSeen, all[i].ResourceKey)
			evicted++
		}
	}
	return evicted, nil
}

// GetTraceResource — v0.89.74. Memory-store mirror.
func (s *Store) GetTraceResource(
	_ context.Context,
	key string,
) (*traceindex.ResourceRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.traceResourceSeen[key]; ok {
		out := r
		return &out, nil
	}
	return nil, nil
}

// ListTraceResourcesByScope — v0.89.74. Memory-store mirror.
func (s *Store) ListTraceResourcesByScope(
	_ context.Context,
	provider, scopeID string,
	since time.Time,
	limit int,
) ([]traceindex.ResourceRow, error) {
	if provider == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100_000 {
		limit = 1000
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]traceindex.ResourceRow, 0)
	for _, r := range s.traceResourceSeen {
		if r.Provider != provider {
			continue
		}
		if r.ScopeID != scopeID {
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
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CountTraceResourcesByScope — v0.89.74. Memory-store mirror.
func (s *Store) CountTraceResourcesByScope(
	_ context.Context,
	provider, scopeID string,
) (int, error) {
	if provider == "" {
		return 0, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, r := range s.traceResourceSeen {
		if r.Provider == provider && r.ScopeID == scopeID {
			n++
		}
	}
	return n, nil
}
