// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// defaultMaxRows caps the in-process pending-rows map and is also
// the LRU ceiling the backing store enforces on flush. Design doc
// §12 names 100K as the slice-1 default with an operator override
// via SQUADRON_TRACEINDEX_MAX_ROWS — that env var is read by the
// wiring layer (chunk 2) and passed to the Index + Store
// constructors, not by this package directly.
const defaultMaxRows = 100_000

// Store is the backend the Index flushes to. The methods deliberately
// mirror the four entry points the application store exposes on the
// trace_resource_seen table so the SQLite + memory implementations
// can satisfy both this interface and the broader ApplicationStore
// without separate adapter shims.
//
// UpsertTraceResources is the flush sink. It accepts the batched
// rows, applies them with INSERT ... ON CONFLICT(resource_key) DO
// UPDATE semantics (span counts accumulate, last_seen_at and
// attributes_json refresh, first_seen_at preserved), and after the
// upsert applies the LRU eviction cap. The evicted return value is
// the count of rows DELETEd to stay under the cap — non-zero
// triggers the slice-2 flush audit payload's eviction_count field.
//
// LastSeenAt is served from the store when the Index's pending map
// has no entry; the Index merges its pending observations with the
// store-side value before answering.
type Store interface {
	UpsertTraceResources(ctx context.Context, rows []ResourceRow) (evicted int, err error)
	GetTraceResource(ctx context.Context, key string) (*ResourceRow, error)
	ListTraceResourcesByScope(ctx context.Context, provider, scopeID string, since time.Time, limit int) ([]ResourceRow, error)
	CountTraceResourcesByScope(ctx context.Context, provider, scopeID string) (int, error)
}

// Index is the in-process, thread-safe traceindex. Observe is the
// receiver's hot path — O(1) under the lock, NO IO. Flush runs in a
// background goroutine the chunk-2 wiring will spawn; tests call it
// directly.
//
// The pending map accumulates one row per resource_key across the
// flush cadence. When two Observe calls land on the same key within
// one window, their span counts add and the LastSeenAt advances to
// the later timestamp. On Flush the entire map is drained to the
// store in one call so the store's transaction overhead amortizes
// across the batch.
type Index struct {
	store   Store
	maxRows int

	mu          sync.Mutex
	pendingRows map[string]*ResourceRow

	clock func() time.Time
}

// NewIndex constructs an Index. maxRows=0 selects the defaultMaxRows
// ceiling (100K — design doc §12). Pass a custom clock function for
// deterministic tests; nil falls through to time.Now.
//
// store must be non-nil — the Index does not provide a no-store
// "ephemeral" mode in slice 1 because the chunk-2 wiring always
// supplies a backing store (either SQLite or the memory store, both
// of which satisfy the interface).
func NewIndex(store Store, maxRows int, clock func() time.Time) *Index {
	if maxRows <= 0 {
		maxRows = defaultMaxRows
	}
	if clock == nil {
		clock = time.Now
	}
	return &Index{
		store:       store,
		maxRows:     maxRows,
		pendingRows: make(map[string]*ResourceRow),
		clock:       clock,
	}
}

// Observe records one ResourceSpan's worth of activity. The method
// computes the resource_key via ComputeResourceKey, accumulates the
// span counts onto the pending row, and refreshes LastSeenAt /
// AttributesJSON. Hot path — O(1) under the lock, no IO, no
// allocations beyond the pending-row insert on first sight.
//
// Observations with no usable identifier (ComputeResourceKey
// returns ok=false) are dropped silently per design doc §13 Q4.
// Slice 2 may add an orphan-trace counter; slice 1 keeps the index
// lean.
//
// Per the slice-1 contract the Observe call is fire-and-forget — it
// returns no error because the receiver's hot path cannot do
// anything useful with a downstream error. Flush errors surface to
// the chunk-2 background goroutine where they get logged and audited.
func (i *Index) Observe(_ context.Context, obs ResourceObservation) {
	key, provider, scopeID, hint, serviceName, confidence, ok := ComputeResourceKey(obs.Attributes)
	if !ok {
		return
	}
	ts := obs.Timestamp
	if ts.IsZero() {
		ts = i.clock()
	}
	ts = ts.UTC()

	// Serialize the attribute map deterministically so the
	// attributes_json column is stable across re-observations of the
	// same row. The marshal is best-effort — a map with un-marshalable
	// values (which can't happen for map[string]string but the guard
	// is cheap) drops to an empty string rather than crashing the hot
	// path.
	attrJSON := ""
	if len(obs.Attributes) > 0 {
		if b, err := json.Marshal(obs.Attributes); err == nil {
			attrJSON = string(b)
		}
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if existing, found := i.pendingRows[key]; found {
		existing.SpanCount24h += int64(obs.SpanCount)
		existing.RootSpanCount24h += int64(obs.RootSpanCount)
		if ts.After(existing.LastSeenAt) {
			existing.LastSeenAt = ts
			existing.AttributesJSON = attrJSON
			existing.ServiceName = serviceName
		}
		existing.UpdatedAt = ts
		return
	}

	i.pendingRows[key] = &ResourceRow{
		ResourceKey:      key,
		Provider:         provider,
		ScopeID:          scopeID,
		ResourceIDHint:   hint,
		ServiceName:      serviceName,
		FirstSeenAt:      ts,
		LastSeenAt:       ts,
		SpanCount24h:     int64(obs.SpanCount),
		RootSpanCount24h: int64(obs.RootSpanCount),
		AttributesJSON:   attrJSON,
		MatchConfidence:  confidence,
		UpdatedAt:        ts,
	}
}

// Flush writes the accumulated pending rows to the backing store and
// returns the count written + the count evicted by the store-side
// LRU cap. Called by the chunk-2 background goroutine every 30s;
// tests call it directly.
//
// Concurrency note: Flush snapshots the pending map under the lock,
// resets it, and then calls the store WITHOUT the lock held. This
// matters because the store's UpsertTraceResources can be slow
// (SQLite transaction + LRU sweep) and the receiver's Observe path
// must never wait on flush IO.
func (i *Index) Flush(ctx context.Context) (written, evicted int, err error) {
	i.mu.Lock()
	if len(i.pendingRows) == 0 {
		i.mu.Unlock()
		return 0, 0, nil
	}
	rows := make([]ResourceRow, 0, len(i.pendingRows))
	for _, r := range i.pendingRows {
		rows = append(rows, *r)
	}
	i.pendingRows = make(map[string]*ResourceRow)
	i.mu.Unlock()

	// Sort for determinism so flush-pair tests can compare snapshots
	// without flake. The sort is O(n log n) over a small batch
	// (bounded by maxRows) and runs OUTSIDE the lock so the hot path
	// is unaffected.
	sort.Slice(rows, func(a, b int) bool {
		return rows[a].ResourceKey < rows[b].ResourceKey
	})

	evicted, err = i.store.UpsertTraceResources(ctx, rows)
	if err != nil {
		return 0, 0, err
	}
	return len(rows), evicted, nil
}

// Coverage projects the per-provider, per-scope coverage summary
// the Discovery dashboard endpoint returns. The caller supplies
// inventoryCount because the inventory join lives on the discovery
// side (the scanner snapshot per design doc §13 Q1); emittingCount
// is sourced from the store via CountTraceResourcesByScope.
//
// The CoveragePct calculation is zero-safe: inventoryCount==0
// returns 0.0, not NaN. Acceptance test 11 ("cold-start parity
// preserved") pins this.
//
// StrongMatchPct + WeakMatchPct are derived from the row-level
// MatchConfidence on the store side — slice 1 reads the rows once
// (with a small cap to keep the call bounded) and computes the split
// in process. Slice 2 candidate: push the split to the SQL layer for
// a per-confidence COUNT(*) without the row read.
func (i *Index) Coverage(ctx context.Context, provider, scopeID string, inventoryCount int) (Summary, error) {
	now := i.clock().UTC()

	emitting, err := i.store.CountTraceResourcesByScope(ctx, provider, scopeID)
	if err != nil {
		return Summary{}, err
	}

	// Pull rows for the confidence split. The 24h floor keeps the
	// scan bounded; the limit of maxRows is the same ceiling the
	// store enforces on flush so a single Coverage call can't read
	// more rows than the index could possibly hold.
	since := now.Add(-24 * time.Hour)
	rows, err := i.store.ListTraceResourcesByScope(ctx, provider, scopeID, since, i.maxRows)
	if err != nil {
		return Summary{}, err
	}

	strong, weak := 0, 0
	var lastUpdate time.Time
	for _, r := range rows {
		if r.MatchConfidence == MatchConfidenceWeak {
			weak++
		} else {
			strong++
		}
		if r.UpdatedAt.After(lastUpdate) {
			lastUpdate = r.UpdatedAt
		}
	}

	// Merge pending rows into the per-confidence split so a freshly
	// observed resource shows up in the dashboard immediately rather
	// than waiting on the next flush. The dashboard's 30s cache
	// already covers most of the staleness window but the in-process
	// merge keeps the cold-start path honest for tests.
	i.mu.Lock()
	for _, r := range i.pendingRows {
		if r.Provider != provider || r.ScopeID != scopeID {
			continue
		}
		if _, alreadyInStore := findRowByKey(rows, r.ResourceKey); alreadyInStore {
			continue
		}
		if r.MatchConfidence == MatchConfidenceWeak {
			weak++
		} else {
			strong++
		}
		emitting++
		if r.UpdatedAt.After(lastUpdate) {
			lastUpdate = r.UpdatedAt
		}
	}
	i.mu.Unlock()

	coveragePct := 0.0
	if inventoryCount > 0 {
		coveragePct = float64(emitting) / float64(inventoryCount) * 100.0
	}
	strongPct, weakPct := 0.0, 0.0
	if total := strong + weak; total > 0 {
		strongPct = float64(strong) / float64(total) * 100.0
		weakPct = float64(weak) / float64(total) * 100.0
	}

	return Summary{
		Provider:          provider,
		ScopeID:           scopeID,
		InventoryCount:    inventoryCount,
		EmittingCount:     emitting,
		CoveragePct:       coveragePct,
		StrongMatchPct:    strongPct,
		WeakMatchPct:      weakPct,
		LastIndexUpdateAt: lastUpdate,
	}, nil
}

// LastSeenAt returns the most recent observation timestamp for the
// supplied resource key, merging the pending-rows cache with the
// store. Returns ok=false (no error) when neither side has an entry.
//
// The per-provider Inventory tabs (chunk 4) call this method for
// every visible row at scan-render time, so the in-process cache
// path keeps the common case off the SQLite read.
func (i *Index) LastSeenAt(ctx context.Context, key string) (time.Time, bool, error) {
	i.mu.Lock()
	if r, ok := i.pendingRows[key]; ok {
		ts := r.LastSeenAt
		i.mu.Unlock()
		return ts, true, nil
	}
	i.mu.Unlock()

	row, err := i.store.GetTraceResource(ctx, key)
	if err != nil {
		return time.Time{}, false, err
	}
	if row == nil {
		return time.Time{}, false, nil
	}
	return row.LastSeenAt, true, nil
}

// findRowByKey is a small linear-scan helper Coverage uses to skip
// pending rows that already appear in the store-side list. Linear
// is fine because the rows slice is bounded by maxRows and Coverage
// is not on the receiver hot path.
func findRowByKey(rows []ResourceRow, key string) (ResourceRow, bool) {
	for _, r := range rows {
		if r.ResourceKey == key {
			return r, true
		}
	}
	return ResourceRow{}, false
}
