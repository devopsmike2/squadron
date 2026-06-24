// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"testing"
)

// Benchmarks pinning the slice 2 hot-path budget per the §5 design
// doc target: per-span overhead from W3C trace context parsing stays
// under 100ns on the canonical path (traceparent present + child
// span). The benchmarks are short-circuit safe to run in -short mode
// because they don't allocate beyond steady state — the inner
// SpanObservation is built once per Observe call but the maps it
// references are reused across iterations.
//
// Reading the numbers: the BenchmarkQuality_Observe_WithTraceparentChild
// per-op time INCLUDES the slice 1 hot path (mutex acquire + orphan
// lookup + missing-attr scan + placeholder scan). Slice 2's marginal
// overhead is the delta between WithTraceparentChild and
// NoTraceparentRoot; §18 acceptance test requires that delta stays
// under 100ns.

// benchAttrsWithTraceparent returns a compute-tier attribute map
// pre-populated with a well-formed traceparent. Allocated once per
// benchmark; the map is read-only during the b.N loop so no per-op
// allocation.
func benchAttrsWithTraceparent() map[string]string {
	attrs := map[string]string{
		"service.name":      "checkout",
		"cloud.provider":    "aws",
		"cloud.account.id":  "111122223333",
		"cloud.region":      "us-east-1",
		"host.id":           "i-0abc123",
		TraceparentAttrName: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
	}
	return attrs
}

// benchAttrsRoot returns a compute-tier attribute map with no
// traceparent — the slice 2 "fast path" where the lookupTraceparent
// + isWellFormedTraceparent branches both short-circuit.
func benchAttrsRoot() map[string]string {
	return map[string]string{
		"service.name":     "checkout",
		"cloud.provider":   "aws",
		"cloud.account.id": "111122223333",
		"cloud.region":     "us-east-1",
		"host.id":          "i-0abc123",
	}
}

// BenchmarkQuality_Observe_WithTraceparentChild measures the §18
// canonical hot path: a child span carrying a well-formed
// traceparent. Per-op time MUST stay under the §5 100ns slice 2
// budget on top of the slice 1 baseline (~200ns).
func BenchmarkQuality_Observe_WithTraceparentChild(b *testing.B) {
	q := NewQuality()
	attrs := benchAttrsWithTraceparent()
	obs := SpanObservation{
		Key:          "k1",
		TraceID:      "t1",
		SpanID:       "child1",
		ParentSpanID: "p1",
		Tier:         "compute",
		Attrs:        attrs,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Observe(obs)
	}
}

// BenchmarkQuality_Observe_NoTraceparentRoot measures the minimal-
// overhead path: a root span with no traceparent. Slice 2 cost
// reduces to:
//   - isNonRootSpan returns false (no ChildSpans++).
//   - lookupTraceparent returns "" after 2 map lookups.
//   - else-if isChild is false → no MissingTraceparentOnChildSpans++.
// Roughly: ~30ns of additional overhead above slice 1.
func BenchmarkQuality_Observe_NoTraceparentRoot(b *testing.B) {
	q := NewQuality()
	attrs := benchAttrsRoot()
	obs := SpanObservation{
		Key:     "k1",
		TraceID: "t1",
		SpanID:  "root1",
		Tier:    "compute",
		Attrs:   attrs,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q.Observe(obs)
	}
}

// BenchmarkIsWellFormedTraceparent_Canonical pins the standalone
// helper at ~50ns/op per the §5 design budget. Used as a regression
// guard — if a future change to the helper (e.g. switching from
// indexed access to substring slicing) adds an allocation or pushes
// the per-op time past the design budget, this benchmark catches it
// before the integration benchmark above masks the effect under the
// mutex acquire.
func BenchmarkIsWellFormedTraceparent_Canonical(b *testing.B) {
	const tp = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = isWellFormedTraceparent(tp)
	}
}

// BenchmarkIsWellFormedTraceparent_Rejected measures the early-exit
// path: a wrong-length value short-circuits on the very first check.
// Should be ~1-2ns/op.
func BenchmarkIsWellFormedTraceparent_Rejected(b *testing.B) {
	const tp = "too-short"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = isWellFormedTraceparent(tp)
	}
}
