// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"sync"
	"time"
)

// QualityCounters tracks span quality observations per resource over
// a rolling 1-hour window. See docs/proposals/span-quality-slice1.md
// §3.4. The counters are uint64 so the §11 threat model's million-
// spans-per-second-for-one-hour worst case (~3.6e9 spans) fits without
// overflow.
//
// Memory: 4 uint64 + a time.Time per resource = ~40 bytes per key. At
// the traceindex 100K key cap (design doc §12), worst case ~4MB for
// quality counters — acceptable.
type QualityCounters struct {
	OrphanSpans       uint64
	MissingAttrSpans  uint64
	AttrMismatchSpans uint64
	TotalSpans        uint64
	WindowStart       time.Time
}

// PlaceholderObservation records a specific placeholder value seen on
// a resource. The per-resource detail endpoint (chunk-2 §6.2) reads a
// capped LRU of these so the operator can see which placeholders the
// SDK is emitting. The value is the offending sentinel only — never
// real attribute content (slice-1 threat model §11: no PII via
// attribute observation).
type PlaceholderObservation struct {
	Attribute   string
	Placeholder string
	SeenAt      time.Time
}

// maxPlaceholdersPerKey caps how many recent placeholder observations
// each resource holds. The chunk-2 detail endpoint only needs a few
// recent examples; bounding the slice keeps quality memory predictable
// even on resources that thrash placeholders.
const maxPlaceholdersPerKey = 8

// Quality is the sibling of Index — same per-resource keying, same
// hot-path Observe contract — that tracks span quality pathologies
// rather than coverage. See span-quality-slice1.md §3 for the
// detection rules.
//
// Concurrency: a single sync.Mutex guards every map. The hot-path
// budget (~200ns/span) is dominated by the lock acquire + the
// placeholder/missing-attr scans; profile/bench is not required for
// chunk 1 but the code avoids allocation on the happy path (no
// placeholder, parent in window) so the lock window stays short.
type Quality struct {
	mu           sync.Mutex
	perKey       map[string]*QualityCounters
	placeholders map[string][]PlaceholderObservation
	// parentSeen maps trace_id -> span_id -> seen_at. Used by §3.1
	// orphan detection: when a span arrives with a non-zero
	// parent_span_id we look up whether we've seen that span_id on
	// the same trace within parentTTL.
	parentSeen map[string]map[string]time.Time
	window     time.Duration
	parentTTL  time.Duration
	now        func() time.Time
}

// NewQuality constructs a Quality observer with the §3.4 defaults: 1h
// rolling window per resource, 5min parent TTL. The clock is wired to
// time.Now; tests can override it after construction to drive the
// rolling-window + parent-TTL behaviors deterministically.
func NewQuality() *Quality {
	return &Quality{
		perKey:       make(map[string]*QualityCounters),
		placeholders: make(map[string][]PlaceholderObservation),
		parentSeen:   make(map[string]map[string]time.Time),
		window:       1 * time.Hour,
		parentTTL:    5 * time.Minute,
		now:          time.Now,
	}
}

// SpanObservation is the per-span input to Observe. The OTLP receiver
// builds one of these per incoming span on the hot path.
//
// Key is the traceindex key (from ComputeResourceKey) so Quality and
// Index agree on what "this resource" means. An empty Key is a no-op —
// the same drop-silently semantics ComputeResourceKey returns for
// unidentifiable resources (slice-1 §13 Q4).
//
// TraceID + SpanID + ParentSpanID are hex-encoded — receiver hot path
// already pays the encoding cost once per span, and the resulting
// strings are map-key friendly without further allocation.
//
// Tier picks the §3.2 required-attribute set ("compute" / "db" /
// "k8s"). A tier not in the required-attrs table is treated as
// "no required attrs" — that defensive choice keeps a future tier
// addition from accidentally crashing the hot path.
//
// Attrs is the merged resource + span attribute map. Quality reads
// presence + sentinel values only; values never escape the in-memory
// counter (threat-model §11).
type SpanObservation struct {
	Key          string
	TraceID      string
	SpanID       string
	ParentSpanID string
	Tier         string
	Attrs        map[string]string
}

// Observe is called from the OTLP receiver hot path once per span.
// Increments per-key counters, tracks parent span IDs in the 5min
// orphan-detection window, and records placeholder observations for
// the chunk-2 detail endpoint.
//
// Hot-path budget: ~200ns/span (§11 threat model). The happy path —
// no placeholders, parent present in the window, key already seen,
// window not yet elapsed — does NOT allocate.
//
// Disabled-mode: an empty Key short-circuits before the lock. The
// receiver also guards `if qual == nil { return }` at the call site,
// so SQUADRON_SPANQUALITY_DISABLED leaves the hot path untouched.
func (q *Quality) Observe(obs SpanObservation) {
	if obs.Key == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.now()
	counters := q.perKey[obs.Key]
	if counters == nil {
		counters = &QualityCounters{WindowStart: now}
		q.perKey[obs.Key] = counters
	}
	// Rolling 1h window per resource (§3.4 step 5). Reset zeros the
	// four counters in place rather than reallocating — keeps the hot
	// path allocation-free on the window-rollover tick.
	if now.Sub(counters.WindowStart) > q.window {
		counters.OrphanSpans = 0
		counters.MissingAttrSpans = 0
		counters.AttrMismatchSpans = 0
		counters.TotalSpans = 0
		counters.WindowStart = now
	}
	counters.TotalSpans++

	// §3.1 orphan detection: a span with a non-root parent_span_id is
	// orphan when we have NOT seen a span with that span_id on the
	// same trace within parentTTL. The lookup also evicts the entry
	// from the trace's map if it's aged out, so a "parent seen but
	// expired" case is counted as orphan and won't double-count on
	// subsequent children.
	if isNonRootSpan(obs.ParentSpanID) {
		if !q.parentSpanSeen(obs.TraceID, obs.ParentSpanID, now) {
			counters.OrphanSpans++
		}
	}
	// Record this span as a potential parent for future spans on the
	// same trace. The recording is unconditional — root spans count
	// as potential parents too because a child span may carry the
	// root's span_id as its parent_span_id.
	q.recordSpan(obs.TraceID, obs.SpanID, now)

	// §3.2 missing required attributes. firstMissingRequired returns
	// the first attribute name that's absent (or the magic
	// host.id|host.name|cloud.resource_id token when none of the
	// compute alternatives are present). Counted once per span — a
	// single missing attribute increments the counter once even if
	// the span happens to miss several.
	if missingAttr := firstMissingRequired(obs.Tier, obs.Attrs); missingAttr != "" {
		counters.MissingAttrSpans++
	}

	// §3.3 placeholder values. firstPlaceholder returns the first
	// {attr, placeholder} pair the span matches (deterministic
	// iteration order is NOT guaranteed across attrs, but Observe
	// counts mismatch per-span not per-attr, so the choice of "which
	// placeholder we recorded" only affects the §6.2 detail surface).
	if attr, placeholder := firstPlaceholder(obs.Attrs); attr != "" {
		counters.AttrMismatchSpans++
		q.recordPlaceholder(obs.Key, attr, placeholder, now)
	}
}

// isNonRootSpan returns true when parentSpanID is non-empty and not
// the all-zero hex placeholder (16 chars of '0'). OTLP wire format
// uses an empty bytes field for root spans, but the receiver hex-
// encodes before calling Observe, so we have to handle both empty
// AND the all-zero-hex form.
func isNonRootSpan(parentSpanID string) bool {
	if parentSpanID == "" {
		return false
	}
	for i := 0; i < len(parentSpanID); i++ {
		if parentSpanID[i] != '0' {
			return true
		}
	}
	return false
}

// parentSpanSeen looks up whether we've seen the given span_id on the
// given trace within parentTTL. Side effect: if the entry has aged
// out it gets removed, so a subsequent span that asks for the same
// parent within the same Observe call sees a clean "not present"
// answer.
//
// The caller holds q.mu.
func (q *Quality) parentSpanSeen(traceID, parentSpanID string, now time.Time) bool {
	spans := q.parentSeen[traceID]
	if spans == nil {
		return false
	}
	seenAt, ok := spans[parentSpanID]
	if !ok {
		return false
	}
	if now.Sub(seenAt) > q.parentTTL {
		delete(spans, parentSpanID)
		if len(spans) == 0 {
			delete(q.parentSeen, traceID)
		}
		return false
	}
	return true
}

// recordSpan stores the (trace_id, span_id, seen_at) tuple so future
// children on the same trace can satisfy the parent lookup. Allocates
// a fresh inner map ONLY the first time we see a trace_id; the happy
// path on a previously-seen trace is a single map write.
//
// The caller holds q.mu.
func (q *Quality) recordSpan(traceID, spanID string, now time.Time) {
	if traceID == "" || spanID == "" {
		return
	}
	spans := q.parentSeen[traceID]
	if spans == nil {
		spans = make(map[string]time.Time)
		q.parentSeen[traceID] = spans
	}
	spans[spanID] = now
}

// recordPlaceholder appends a placeholder observation to the per-key
// slice, capping the slice length at maxPlaceholdersPerKey by
// evicting the oldest entry (FIFO). A more sophisticated LRU is not
// worth it for chunk 1 — the chunk-2 detail endpoint only needs a
// handful of recent examples.
//
// The caller holds q.mu.
func (q *Quality) recordPlaceholder(key, attr, placeholder string, now time.Time) {
	obs := q.placeholders[key]
	obs = append(obs, PlaceholderObservation{
		Attribute:   attr,
		Placeholder: placeholder,
		SeenAt:      now,
	})
	if len(obs) > maxPlaceholdersPerKey {
		// Drop the oldest. Slice copy keeps the slice header bounded
		// — appending forever and never trimming would grow without
		// bound on a resource that keeps thrashing placeholders.
		obs = obs[len(obs)-maxPlaceholdersPerKey:]
	}
	q.placeholders[key] = obs
}

// EvictExpired removes counters whose window elapsed at least 2x ago
// (so a resource that just rolled over still has a fresh window) and
// trace_id maps whose every span has aged out of parentTTL. Returns
// how many of each were dropped — the chunk-2 background flusher can
// log the counts so an operator can see the eviction cadence.
//
// Memory bound: with no eviction, the Quality structure would grow
// indefinitely on a fleet that adds new resources over time. Calling
// EvictExpired periodically (the chunk-2 flusher hook does this on
// each tick) keeps total memory proportional to ACTIVE resources, not
// historical.
func (q *Quality) EvictExpired() (countersEvicted, tracesEvicted int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	for key, counters := range q.perKey {
		if now.Sub(counters.WindowStart) > 2*q.window {
			delete(q.perKey, key)
			delete(q.placeholders, key)
			countersEvicted++
		}
	}
	for traceID, spans := range q.parentSeen {
		anyFresh := false
		for _, seenAt := range spans {
			if now.Sub(seenAt) <= q.parentTTL {
				anyFresh = true
				break
			}
		}
		if !anyFresh {
			delete(q.parentSeen, traceID)
			tracesEvicted++
		}
	}
	return
}

// QualityCountersSnapshot is the read-only projection of a single
// resource's counters + recorded placeholders. The chunk-2 API
// handlers consume this directly; the rolling-percentage math runs
// here so the handlers don't have to re-derive it.
//
// OrphanPct / MissingAttrPct / AttrMismatchPct are 0.0 when
// TotalSpans is 0 (not NaN). The same zero-safety pattern Index.Coverage
// follows for the cold-start case.
type QualityCountersSnapshot struct {
	Key             string
	OrphanPct       float64
	MissingAttrPct  float64
	AttrMismatchPct float64
	TotalSpans      uint64
	WindowStart     time.Time
	Placeholders    []PlaceholderObservation
}

// SnapshotKey returns the snapshot for a single key. ok=false when no
// observations exist for that key. Holds the lock once and deep-
// copies the placeholders slice so the caller can read it without
// further synchronization.
func (q *Quality) SnapshotKey(key string) (QualityCountersSnapshot, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	c, ok := q.perKey[key]
	if !ok {
		return QualityCountersSnapshot{}, false
	}
	snap := snapshotFromCounters(key, c)
	if obs := q.placeholders[key]; len(obs) > 0 {
		snap.Placeholders = make([]PlaceholderObservation, len(obs))
		copy(snap.Placeholders, obs)
	}
	return snap, true
}

// SnapshotAll returns one snapshot per observed key. Caller-friendly
// for the chunk-2 §6.1 dashboard rollup that walks every key,
// projects per-provider aggregates, and serializes the response.
func (q *Quality) SnapshotAll() []QualityCountersSnapshot {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]QualityCountersSnapshot, 0, len(q.perKey))
	for key, c := range q.perKey {
		snap := snapshotFromCounters(key, c)
		if obs := q.placeholders[key]; len(obs) > 0 {
			snap.Placeholders = make([]PlaceholderObservation, len(obs))
			copy(snap.Placeholders, obs)
		}
		out = append(out, snap)
	}
	return out
}

// snapshotFromCounters projects raw counters into the percentage-
// based snapshot shape. Zero-safe on TotalSpans=0 (returns 0.0% for
// all three rates rather than NaN).
//
// The caller holds q.mu.
func snapshotFromCounters(key string, c *QualityCounters) QualityCountersSnapshot {
	snap := QualityCountersSnapshot{
		Key:         key,
		TotalSpans:  c.TotalSpans,
		WindowStart: c.WindowStart,
	}
	if c.TotalSpans > 0 {
		total := float64(c.TotalSpans)
		snap.OrphanPct = float64(c.OrphanSpans) / total * 100
		snap.MissingAttrPct = float64(c.MissingAttrSpans) / total * 100
		snap.AttrMismatchPct = float64(c.AttrMismatchSpans) / total * 100
	}
	return snap
}
