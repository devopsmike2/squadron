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
//
// Slice 2 (v0.89.109) adds four uint64 fields for W3C trace context
// parsing — see docs/proposals/span-quality-slice2.md §3.3.
// MalformedTraceparentSpans and MissingTraceparentOnChildSpans are the
// numerator counters; SpansWithTraceparent and ChildSpans are the
// separate denominators per §3.3 "honest framing" — a root span can't
// be missing-on-child, and a span without traceparent can't be
// malformed, so neither belongs in the shared TotalSpans denominator.
// Memory cost: +32 bytes per key, ~3.2MB at full cap.
type QualityCounters struct {
	OrphanSpans       uint64
	MissingAttrSpans  uint64
	AttrMismatchSpans uint64
	TotalSpans        uint64

	// Slice 2 (v0.89.109): W3C trace context parsing.
	// See docs/proposals/span-quality-slice2.md §3.3 for denominator
	// semantics — note the separate SpansWithTraceparent + ChildSpans
	// denominators (rather than reusing TotalSpans).
	MalformedTraceparentSpans      uint64
	MissingTraceparentOnChildSpans uint64
	SpansWithTraceparent           uint64 // denominator for malformed_pct
	ChildSpans                     uint64 // denominator for missing_on_child_pct

	WindowStart time.Time

	// Slice 1 of sampling rate (v0.89.122): parallel 24h-window
	// counter for sampling-rate-analysis. Resets when the 24h
	// window elapses, independently from the 1h-window counters
	// above — see docs/proposals/sampling-rate-analysis-slice1.md
	// §5 "Option A: 24h Quality counter".
	//
	// The 24h-window count is used by the sampling-rate detection
	// branch as the numerator of observed_span_count /
	// expected_invocation_count. The 1h TotalSpans counter is
	// preserved as-is so the existing quality percentages keep
	// their semantics.
	//
	// Memory cost: +16 bytes per key (uint64 + time.Time), ~1.6MB
	// at the 100K-key cap per §12 threat model.
	TotalSpansLast24h uint64
	WindowStart24h    time.Time
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
	// providers maps a quality key -> the normalized provider token
	// the first observation for that key reported. Stable across the
	// rolling window: once a key is bucketed under a provider we keep
	// that mapping (a key flipping providers mid-window would be a
	// keying-rule bug; the chunk-2 aggregate endpoint reads this map
	// rather than re-parsing the key string).
	providers map[string]string
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
		providers:    make(map[string]string),
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
//
// Provider — v0.89.86 chunk 2 additive — normalized provider token
// ("aws"/"gcp"/"azure"/"oci"/"unknown") sourced from the same
// ComputeResourceKey call site that computed Key. The chunk-2 §6.1
// per-provider aggregate endpoint reads it off SnapshotAll() to bucket
// keys without re-parsing the key string (tier-5 and tier-6 keys carry
// no provider prefix). Empty Provider is preserved as "" in the
// snapshot — the API handler buckets the unknown case explicitly.
type SpanObservation struct {
	Key          string
	Provider     string
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
		// WindowStart + WindowStart24h are seeded to the same `now`
		// on first observation. They diverge afterward — the 1h
		// window resets every hour, the 24h window every 24 hours.
		// See sampling-rate-analysis-slice1.md §5 for the
		// independence requirement (test 3 in §11 pins it).
		counters = &QualityCounters{
			WindowStart:    now,
			WindowStart24h: now,
		}
		q.perKey[obs.Key] = counters
	}
	// Provider bucketing — chunk-2 §6.1 aggregate endpoint reads this
	// off the snapshot. Recorded once per key on first observation;
	// subsequent observations don't rewrite the value (a key flipping
	// providers mid-window would indicate a keying bug, not a
	// legitimate signal worth surfacing).
	if obs.Provider != "" {
		if _, ok := q.providers[obs.Key]; !ok {
			q.providers[obs.Key] = obs.Provider
		}
	}
	// Rolling 1h window per resource (§3.4 step 5). Reset zeros the
	// counters in place rather than reallocating — keeps the hot
	// path allocation-free on the window-rollover tick. The slice 2
	// counters (MalformedTraceparentSpans + MissingTraceparentOnChildSpans
	// + their denominators) reset alongside the slice 1 fields.
	if now.Sub(counters.WindowStart) > q.window {
		counters.OrphanSpans = 0
		counters.MissingAttrSpans = 0
		counters.AttrMismatchSpans = 0
		counters.TotalSpans = 0
		counters.MalformedTraceparentSpans = 0
		counters.MissingTraceparentOnChildSpans = 0
		counters.SpansWithTraceparent = 0
		counters.ChildSpans = 0
		counters.WindowStart = now
	}
	// Slice 1 of sampling rate (v0.89.122): independent 24h reset.
	// Kept as a separate branch (not folded into the 1h reset
	// above) so the two windows roll over on different cadences —
	// the 1h counter resets every hour for the quality
	// percentages, the 24h counter resets every 24 hours for
	// sampling-rate analysis. Hot-path cost: one extra branch-
	// predictable Sub() + comparison, ~5ns per span (§12 threat
	// model budget for slice 1 of sampling rate is 10ns).
	if now.Sub(counters.WindowStart24h) > 24*time.Hour {
		counters.TotalSpansLast24h = 0
		counters.WindowStart24h = now
	}
	counters.TotalSpans++
	counters.TotalSpansLast24h++

	// §3.1 orphan detection: a span with a non-root parent_span_id is
	// orphan when we have NOT seen a span with that span_id on the
	// same trace within parentTTL. The lookup also evicts the entry
	// from the trace's map if it's aged out, so a "parent seen but
	// expired" case is counted as orphan and won't double-count on
	// subsequent children.
	//
	// isChild is computed once here and reused by the slice 2 W3C
	// detection below — keeps the per-span isNonRootSpan call count
	// at one even when both slice 1 and slice 2 care about the answer.
	isChild := isNonRootSpan(obs.ParentSpanID)
	if isChild {
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

	// Slice 2 §3.1/§3.2: W3C trace context parsing. Three counter
	// updates fan out from one lookup + one (optional) parse:
	//   - ChildSpans (denominator for missing-on-child) increments
	//     when the span is a child, regardless of traceparent state.
	//   - SpansWithTraceparent (denominator for malformed) increments
	//     when the span carries a traceparent attribute.
	//   - MalformedTraceparentSpans (numerator) increments when the
	//     traceparent value fails the W3C format check.
	//   - MissingTraceparentOnChildSpans (numerator) increments when
	//     the span is a child but carries no traceparent attribute.
	// The two numerators are mutually exclusive (a single span has
	// either a traceparent or no traceparent — never both states).
	if isChild {
		counters.ChildSpans++
	}
	traceparent := lookupTraceparent(obs.Attrs)
	if traceparent != "" {
		counters.SpansWithTraceparent++
		if !isWellFormedTraceparent(traceparent) {
			counters.MalformedTraceparentSpans++
		}
	} else if isChild {
		counters.MissingTraceparentOnChildSpans++
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
// indefinitely — perKey/placeholders/providers with each new resource,
// and parentSeen with cumulative unique span count on the receive hot
// path. The production driver is the background flusher, which calls
// EvictExpired on every tick when wired via
// BackgroundFlusher.WithQualityEvictor (cmd/all-in-one passes the
// span-quality index). That keeps total memory proportional to ACTIVE
// resources/traces, not historical.
func (q *Quality) EvictExpired() (countersEvicted, tracesEvicted int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	for key, counters := range q.perKey {
		if now.Sub(counters.WindowStart) > 2*q.window {
			delete(q.perKey, key)
			delete(q.placeholders, key)
			delete(q.providers, key)
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
//
// Slice 2 additions (v0.89.109): MalformedTraceparentPct and
// MissingTraceparentOnChildPct each compute against their own
// denominator per §3.3 — a root span can't be missing-on-child, and
// a span without traceparent can't be malformed, so the shared
// TotalSpans denominator would underweight both rates. The
// SpansWithTraceparent + ChildSpans fields are exposed too so the
// chunk-2 API handler + dashboard can render honest "N of M" framing
// alongside the percentage.
type QualityCountersSnapshot struct {
	Key             string
	Provider        string
	OrphanPct       float64
	MissingAttrPct  float64
	AttrMismatchPct float64
	TotalSpans      uint64
	WindowStart     time.Time
	Placeholders    []PlaceholderObservation

	// Slice 2 additions. Both percentages use their own denominators
	// per §3.3 of the design doc.
	MalformedTraceparentPct      float64
	MissingTraceparentOnChildPct float64
	SpansWithTraceparent         uint64 // exposed for honest framing
	ChildSpans                   uint64 // same

	// Slice 1 of sampling rate (v0.89.122): 24h-window span count.
	// Surfaced on the snapshot so the chunk-2 detection branch
	// (and the chunk-2 per-resource sampling API endpoint) can
	// reason against the same denominator the SpanCountLast24h
	// accessor returns. Decoupled from TotalSpans (1h) so the
	// existing snapshot shape stays backward-compatible.
	TotalSpansLast24h uint64
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
	snap.Provider = q.providers[key]
	if obs := q.placeholders[key]; len(obs) > 0 {
		snap.Placeholders = make([]PlaceholderObservation, len(obs))
		copy(snap.Placeholders, obs)
	}
	return snap, true
}

// SpanCountLast24h returns the count of spans observed for the
// given resource key over the last 24h. Returns ok=false when the
// key has no observations — the sampling-rate detection branch
// treats that as "insufficient data" and skips the comparison per
// docs/proposals/sampling-rate-analysis-slice1.md §3 step 1.
//
// The counter rolls over independently from the 1h-window
// counters: a key that observed spans 25h ago and nothing since
// will see TotalSpansLast24h reset to zero on its next Observe
// call (Observe is the only place the window reset runs). Until
// that next observation, SpanCountLast24h returns the stale value
// — the detection branch additionally gates on the
// expected_invocation_count denominator's MIN_INVOCATION_COUNT
// threshold (1000) which a stale key won't satisfy in practice.
//
// Hot-path posture: same single-mutex contract as SnapshotKey —
// holds q.mu only long enough to read the counter, never
// allocates.
func (q *Quality) SpanCountLast24h(key string) (uint64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	counters, ok := q.perKey[key]
	if !ok {
		return 0, false
	}
	return counters.TotalSpansLast24h, true
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
		snap.Provider = q.providers[key]
		if obs := q.placeholders[key]; len(obs) > 0 {
			snap.Placeholders = make([]PlaceholderObservation, len(obs))
			copy(snap.Placeholders, obs)
		}
		out = append(out, snap)
	}
	return out
}

// snapshotFromCounters projects raw counters into the percentage-
// based snapshot shape. Zero-safe on every denominator (returns 0.0%
// rather than NaN when the relevant denominator is zero) — the slice
// 2 fields follow the same zero-safety pattern as the slice 1 fields.
//
// The caller holds q.mu.
func snapshotFromCounters(key string, c *QualityCounters) QualityCountersSnapshot {
	snap := QualityCountersSnapshot{
		Key:                  key,
		TotalSpans:           c.TotalSpans,
		WindowStart:          c.WindowStart,
		SpansWithTraceparent: c.SpansWithTraceparent,
		ChildSpans:           c.ChildSpans,
		TotalSpansLast24h:    c.TotalSpansLast24h,
	}
	if c.TotalSpans > 0 {
		total := float64(c.TotalSpans)
		snap.OrphanPct = float64(c.OrphanSpans) / total * 100
		snap.MissingAttrPct = float64(c.MissingAttrSpans) / total * 100
		snap.AttrMismatchPct = float64(c.AttrMismatchSpans) / total * 100
	}
	// Slice 2 §3.3: malformed_pct uses SpansWithTraceparent as
	// denominator, not TotalSpans. A resource with 1000 spans,
	// 200 carrying a traceparent, 8 malformed → pct = 4% (8/200),
	// not 0.8% (8/1000). The latter would understate the rate of
	// malformed-among-spans-that-claim-to-have-context.
	if c.SpansWithTraceparent > 0 {
		snap.MalformedTraceparentPct = float64(c.MalformedTraceparentSpans) / float64(c.SpansWithTraceparent) * 100
	}
	// Slice 2 §3.3: missing_on_child_pct uses ChildSpans as
	// denominator, not TotalSpans. A root span without traceparent
	// is correctly-rooted, not missing-on-child, so it shouldn't
	// land in this rate's denominator.
	if c.ChildSpans > 0 {
		snap.MissingTraceparentOnChildPct = float64(c.MissingTraceparentOnChildSpans) / float64(c.ChildSpans) * 100
	}
	return snap
}
