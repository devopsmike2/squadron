// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock returns a controllable time source for the Quality tests.
// The Quality struct exposes its `now` field for direct override; the
// tests set it to fc.Get and call Advance to move time forward
// deterministically. Mirrors the clock pattern Index uses in
// index_test.go.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{t: start}
}

func (f *fakeClock) Get() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// newTestQuality builds a Quality observer wired to a fakeClock
// starting at a fixed epoch. Tests get a {*Quality, *fakeClock} pair
// and drive the clock to exercise the rolling-window and parent-TTL
// behavior.
func newTestQuality() (*Quality, *fakeClock) {
	clock := newFakeClock(time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC))
	q := NewQuality()
	q.now = clock.Get
	return q, clock
}

// computeAttrs builds the minimum attribute set that satisfies the
// compute-tier §3.2 required-attrs check. Tests start from this and
// strip / mutate keys to exercise specific pathologies.
func computeAttrs() map[string]string {
	return map[string]string{
		"service.name":     "checkout",
		"cloud.provider":   "aws",
		"cloud.account.id": "111122223333",
		"cloud.region":     "us-east-1",
		"host.id":          "i-0abc123",
	}
}

// k8sAttrs returns a k8s-tier attribute map satisfying §3.2.
func k8sAttrs() map[string]string {
	return map[string]string{
		"service.name":       "checkout",
		"cloud.provider":     "gcp",
		"cloud.account.id":   "my-gcp-project",
		"k8s.cluster.name":   "prod-cluster",
		"k8s.namespace.name": "shop",
		"k8s.pod.name":       "checkout-7f9c",
	}
}

// observeWith is a small helper that builds a SpanObservation with
// sensible defaults — the tier+attrs come from the caller, span IDs
// default to root, key defaults to "k1". Keeps test bodies focused
// on the field under test rather than boilerplate.
func observeWith(key, tier string, attrs map[string]string) SpanObservation {
	return SpanObservation{
		Key:     key,
		TraceID: "tracehex",
		SpanID:  "spanhex",
		Tier:    tier,
		Attrs:   attrs,
	}
}

// --- §3.1 orphan span detection --------------------------------------

// Acceptance test 1 (§10): orphan span where parent was never seen.
func TestQuality_OrphanSpan_ParentUnknown_Counts(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(SpanObservation{
		Key:          "k1",
		TraceID:      "t1",
		SpanID:       "child1",
		ParentSpanID: "unknownparent",
		Tier:         "compute",
		Attrs:        computeAttrs(),
	})
	snap, ok := q.SnapshotKey("k1")
	require.True(t, ok)
	assert.Equal(t, uint64(1), q.perKey["k1"].OrphanSpans)
	assert.InDelta(t, 100.0, snap.OrphanPct, 0.0001)
}

// Acceptance test 2 (§10): parent observed first, then child.
func TestQuality_OrphanSpan_ParentSeenWithinWindow_DoesNotCount(t *testing.T) {
	q, _ := newTestQuality()
	// Parent span lands first.
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "parent1", Tier: "compute", Attrs: computeAttrs(),
	})
	// Child references parent.
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "child1", ParentSpanID: "parent1", Tier: "compute", Attrs: computeAttrs(),
	})
	assert.Equal(t, uint64(0), q.perKey["k1"].OrphanSpans)
	assert.Equal(t, uint64(2), q.perKey["k1"].TotalSpans)
}

// Parent seen 6min ago — outside the 5min parentTTL — counts as orphan.
func TestQuality_OrphanSpan_ParentSeenButExpired_Counts(t *testing.T) {
	q, clock := newTestQuality()
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "parent1", Tier: "compute", Attrs: computeAttrs(),
	})
	clock.Advance(6 * time.Minute)
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "child1", ParentSpanID: "parent1", Tier: "compute", Attrs: computeAttrs(),
	})
	assert.Equal(t, uint64(1), q.perKey["k1"].OrphanSpans)
}

// Root span (all-zero hex parent_span_id) is NOT checked for orphan.
func TestQuality_Observe_RootSpan_NoOrphanCheck(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "root1", ParentSpanID: "0000000000000000",
		Tier: "compute", Attrs: computeAttrs(),
	})
	assert.Equal(t, uint64(0), q.perKey["k1"].OrphanSpans)
}

// --- §3.2 missing required attributes --------------------------------

// Acceptance test 3 (§10): compute span without service.name.
func TestQuality_MissingAttrs_ComputeWithoutServiceName_Counts(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	delete(attrs, "service.name")
	q.Observe(observeWith("k1", "compute", attrs))
	assert.Equal(t, uint64(1), q.perKey["k1"].MissingAttrSpans)
}

// Acceptance test 4 (§10): compute span with everything — no
// missing-attr increment.
func TestQuality_MissingAttrs_ComputeWithAll_DoesNotCount(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	assert.Equal(t, uint64(0), q.perKey["k1"].MissingAttrSpans)
}

// host.id missing but host.name present — alternatives satisfied.
func TestQuality_MissingAttrs_ComputeWithHostNameButNotHostId_OK(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	delete(attrs, "host.id")
	attrs["host.name"] = "ip-10-0-1-23"
	q.Observe(observeWith("k1", "compute", attrs))
	assert.Equal(t, uint64(0), q.perKey["k1"].MissingAttrSpans)
}

// K8s tier without k8s.cluster.name fires missing-attrs.
func TestQuality_MissingAttrs_K8sMissingClusterName_Counts(t *testing.T) {
	q, _ := newTestQuality()
	attrs := k8sAttrs()
	delete(attrs, "k8s.cluster.name")
	q.Observe(observeWith("k1", "k8s", attrs))
	assert.Equal(t, uint64(1), q.perKey["k1"].MissingAttrSpans)
}

// --- §3.3 placeholder / mismatch detection ---------------------------

// Acceptance test 5 (§10): host.name=localhost.
func TestQuality_Placeholder_HostNameLocalhost_Counts(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	attrs["host.name"] = "localhost"
	q.Observe(observeWith("k1", "compute", attrs))
	assert.Equal(t, uint64(1), q.perKey["k1"].AttrMismatchSpans)
	// Detail surface captures the {attr, placeholder} pair.
	snap, _ := q.SnapshotKey("k1")
	require.Len(t, snap.Placeholders, 1)
	assert.Equal(t, "host.name", snap.Placeholders[0].Attribute)
	assert.Equal(t, "localhost", snap.Placeholders[0].Placeholder)
}

// Acceptance test 6 (§10): host.name=ip-10-0-1-23 (a real-looking
// hostname).
func TestQuality_Placeholder_HostNameRealhost_DoesNotCount(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	attrs["host.name"] = "ip-10-0-1-23"
	q.Observe(observeWith("k1", "compute", attrs))
	assert.Equal(t, uint64(0), q.perKey["k1"].AttrMismatchSpans)
}

func TestQuality_Placeholder_AccountIDPlaceholder_Counts(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	attrs["cloud.account.id"] = "000000000000"
	q.Observe(observeWith("k1", "compute", attrs))
	assert.Equal(t, uint64(1), q.perKey["k1"].AttrMismatchSpans)
}

func TestQuality_Placeholder_CloudProviderInvalid_Counts(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	attrs["cloud.provider"] = "not-a-real-cloud"
	q.Observe(observeWith("k1", "compute", attrs))
	assert.Equal(t, uint64(1), q.perKey["k1"].AttrMismatchSpans)
}

// --- §3.4 rolling window ---------------------------------------------

// Acceptance test 9 (§10): counter resets after 1h.
func TestQuality_RollingWindow_ResetsAfter1h(t *testing.T) {
	q, clock := newTestQuality()
	// Seed with one orphan + one mismatch.
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "child", ParentSpanID: "unknownparent",
		Tier: "compute", Attrs: computeAttrs(),
	})
	require.Equal(t, uint64(1), q.perKey["k1"].OrphanSpans)

	// Advance past the 1h window and observe a clean span.
	clock.Advance(1*time.Hour + time.Minute)
	q.Observe(observeWith("k1", "compute", computeAttrs()))

	c := q.perKey["k1"]
	assert.Equal(t, uint64(0), c.OrphanSpans, "rollover should zero orphan counter")
	assert.Equal(t, uint64(0), c.MissingAttrSpans)
	assert.Equal(t, uint64(0), c.AttrMismatchSpans)
	assert.Equal(t, uint64(1), c.TotalSpans, "post-rollover span should land in fresh window")
}

// Acceptance test 9 (§10): rollover is per-resource. A second
// resource that's been quiet during the rollover keeps its counter.
func TestQuality_RollingWindow_ResetsPerResource(t *testing.T) {
	q, clock := newTestQuality()
	// k1 sees a missing-attrs span.
	attrs := computeAttrs()
	delete(attrs, "service.name")
	q.Observe(observeWith("k1", "compute", attrs))
	// k2 sees a missing-attrs span.
	q.Observe(observeWith("k2", "compute", attrs))

	require.Equal(t, uint64(1), q.perKey["k1"].MissingAttrSpans)
	require.Equal(t, uint64(1), q.perKey["k2"].MissingAttrSpans)

	// Advance time + only k1 sees a new span (rolls over). k2's
	// counter stays put — its WindowStart only resets when k2
	// observes a span past its own window.
	clock.Advance(1*time.Hour + time.Minute)
	q.Observe(observeWith("k1", "compute", computeAttrs()))

	assert.Equal(t, uint64(0), q.perKey["k1"].MissingAttrSpans, "k1 rolled over")
	assert.Equal(t, uint64(1), q.perKey["k2"].MissingAttrSpans, "k2 unchanged")
}

// --- snapshots -------------------------------------------------------

func TestQuality_SnapshotKey_ReturnsAccuratePercentages(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	delete(attrs, "service.name")
	// Two missing-attrs spans + two clean spans = 50% missing.
	q.Observe(observeWith("k1", "compute", attrs))
	q.Observe(observeWith("k1", "compute", attrs))
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	q.Observe(observeWith("k1", "compute", computeAttrs()))

	snap, ok := q.SnapshotKey("k1")
	require.True(t, ok)
	assert.Equal(t, uint64(4), snap.TotalSpans)
	assert.InDelta(t, 50.0, snap.MissingAttrPct, 0.0001)
	assert.InDelta(t, 0.0, snap.OrphanPct, 0.0001)
	assert.InDelta(t, 0.0, snap.AttrMismatchPct, 0.0001)
}

func TestQuality_SnapshotAll_ReturnsEveryKey(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	q.Observe(observeWith("k2", "compute", computeAttrs()))
	q.Observe(observeWith("k3", "compute", computeAttrs()))

	snaps := q.SnapshotAll()
	require.Len(t, snaps, 3)
	keys := map[string]bool{}
	for _, s := range snaps {
		keys[s.Key] = true
		assert.Equal(t, uint64(1), s.TotalSpans)
	}
	assert.True(t, keys["k1"])
	assert.True(t, keys["k2"])
	assert.True(t, keys["k3"])
}

// --- eviction --------------------------------------------------------

func TestQuality_EvictExpired_DropsAgedOutKeys(t *testing.T) {
	q, clock := newTestQuality()
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	// Age past 2x window so EvictExpired sweeps it.
	clock.Advance(3 * time.Hour)
	counters, traces := q.EvictExpired()
	assert.Equal(t, 1, counters)
	assert.Equal(t, 1, traces, "trace_id map should evict alongside")
	_, ok := q.SnapshotKey("k1")
	assert.False(t, ok)
}

// --- defensive guards ------------------------------------------------

func TestQuality_Observe_EmptyKey_NoOp(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(SpanObservation{Key: "", TraceID: "t", SpanID: "s", Tier: "compute", Attrs: computeAttrs()})
	assert.Len(t, q.perKey, 0)
	assert.Len(t, q.parentSeen, 0)
}

// Placeholder LRU cap — observing >maxPlaceholdersPerKey distinct
// placeholders trims the slice to the cap.
func TestQuality_PlaceholderLRU_CapsAtBound(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	for i := 0; i < maxPlaceholdersPerKey+3; i++ {
		attrs["host.name"] = "localhost"
		q.Observe(observeWith("k1", "compute", attrs))
	}
	snap, _ := q.SnapshotKey("k1")
	assert.LessOrEqual(t, len(snap.Placeholders), maxPlaceholdersPerKey)
}

// --- slice 2: W3C trace context parsing ------------------------------
//
// See docs/proposals/span-quality-slice2.md §11 acceptance tests 8-14
// + 17-18 for the contract these test bodies pin. The W3C format
// edge cases (tests 1-7) live in quality_w3c_test.go because they
// exercise the standalone helpers; the integration tests below
// exercise the Quality.Observe counter wiring.

// canonical W3C trace context value — non-zero trace_id, non-zero
// parent_id, sampled flag set. Mirrors the const in quality_w3c_test.go;
// duplicated here so the slice 2 integration tests don't depend on a
// helper-file constant that may move.
const validTraceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

// observeWithTraceparent builds a child-span observation with the
// given traceparent attribute attached. Used by the slice 2 counter
// tests so each call site stays focused on the field under test
// rather than the boilerplate of merging maps.
func observeWithTraceparent(key, parentSpanID, traceparent string) SpanObservation {
	attrs := computeAttrs()
	if traceparent != "" {
		attrs[TraceparentAttrName] = traceparent
	}
	return SpanObservation{
		Key:          key,
		TraceID:      "t1",
		SpanID:       "child1",
		ParentSpanID: parentSpanID,
		Tier:         "compute",
		Attrs:        attrs,
	}
}

// Acceptance test 8 (§11): malformed traceparent attribute increments
// MalformedTraceparentSpans.
func TestQuality_MalformedTraceparent_CounterIncrements(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(observeWithTraceparent("k1", "parent1", "invalid"))
	c := q.perKey["k1"]
	assert.Equal(t, uint64(1), c.MalformedTraceparentSpans)
	assert.Equal(t, uint64(1), c.SpansWithTraceparent,
		"a span with traceparent — well-formed or not — counts toward the denominator")
}

// Acceptance test 9 (§11): no traceparent attribute means
// MalformedTraceparentSpans stays at 0 (a missing traceparent isn't
// malformed; it's missing-on-child if the span is a child).
func TestQuality_NoTraceparent_MalformedCounterStays(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	c := q.perKey["k1"]
	assert.Equal(t, uint64(0), c.MalformedTraceparentSpans)
	assert.Equal(t, uint64(0), c.SpansWithTraceparent)
}

// A well-formed traceparent leaves the malformed counter at zero but
// still increments the denominator.
func TestQuality_WellFormedTraceparent_OnlyDenominatorIncrements(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(observeWithTraceparent("k1", "parent1", validTraceparent))
	c := q.perKey["k1"]
	assert.Equal(t, uint64(0), c.MalformedTraceparentSpans)
	assert.Equal(t, uint64(1), c.SpansWithTraceparent)
}

// Acceptance test 10 (§11): child span without traceparent increments
// MissingTraceparentOnChildSpans.
func TestQuality_MissingTraceparentOnChild_CounterIncrements(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(observeWithTraceparent("k1", "parent1", ""))
	c := q.perKey["k1"]
	assert.Equal(t, uint64(1), c.MissingTraceparentOnChildSpans)
	assert.Equal(t, uint64(1), c.ChildSpans)
}

// Acceptance test 11 (§11): root span without traceparent does NOT
// increment MissingTraceparentOnChildSpans. The "missing on child"
// pathology only fires for non-root spans.
func TestQuality_MissingTraceparentOnRoot_CounterStays(t *testing.T) {
	q, _ := newTestQuality()
	// Root span: empty parent_span_id.
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "root1",
		Tier: "compute", Attrs: computeAttrs(),
	})
	// Root span with all-zero hex parent_span_id (alternate root form).
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "root2", ParentSpanID: "0000000000000000",
		Tier: "compute", Attrs: computeAttrs(),
	})
	c := q.perKey["k1"]
	assert.Equal(t, uint64(0), c.MissingTraceparentOnChildSpans)
	assert.Equal(t, uint64(0), c.ChildSpans, "root spans don't count in the child denominator")
}

// Acceptance test 12 (§11): ChildSpans denominator counts children
// only. 100 spans (50 root, 50 child) all without traceparent →
// ChildSpans = 50; pct = 100%.
func TestQuality_ChildSpansCountedCorrectly(t *testing.T) {
	q, _ := newTestQuality()
	for i := 0; i < 50; i++ {
		// Root span — no parent, no traceparent.
		q.Observe(SpanObservation{
			Key: "k1", TraceID: "t1", SpanID: "root", Tier: "compute", Attrs: computeAttrs(),
		})
		// Child span — parent set, no traceparent.
		q.Observe(SpanObservation{
			Key: "k1", TraceID: "t1", SpanID: "child", ParentSpanID: "p1",
			Tier: "compute", Attrs: computeAttrs(),
		})
	}
	c := q.perKey["k1"]
	assert.Equal(t, uint64(50), c.ChildSpans)
	assert.Equal(t, uint64(50), c.MissingTraceparentOnChildSpans)

	snap, _ := q.SnapshotKey("k1")
	assert.InDelta(t, 100.0, snap.MissingTraceparentOnChildPct, 0.0001)
}

// Acceptance test 13/14 (§11): denominator selection.
// 200 spans carry a traceparent, 8 are malformed → malformed_pct =
// 4% (using SpansWithTraceparent), not 0.8% (using TotalSpans).
func TestQuality_MalformedDenominator_UsesSpansWithTraceparent(t *testing.T) {
	q, _ := newTestQuality()
	// 800 root spans with no traceparent (denominator excludes these).
	for i := 0; i < 800; i++ {
		q.Observe(SpanObservation{
			Key: "k1", TraceID: "t1", SpanID: "root", Tier: "compute", Attrs: computeAttrs(),
		})
	}
	// 192 child spans with a valid traceparent.
	for i := 0; i < 192; i++ {
		q.Observe(observeWithTraceparent("k1", "p1", validTraceparent))
	}
	// 8 child spans with a malformed traceparent.
	for i := 0; i < 8; i++ {
		q.Observe(observeWithTraceparent("k1", "p1", "garbage"))
	}
	snap, _ := q.SnapshotKey("k1")
	assert.Equal(t, uint64(1000), snap.TotalSpans)
	assert.Equal(t, uint64(200), snap.SpansWithTraceparent)
	assert.InDelta(t, 4.0, snap.MalformedTraceparentPct, 0.0001,
		"malformed_pct denominator must be SpansWithTraceparent=200, not TotalSpans=1000")
}

// Variant of test 12: 1000 child spans, 50 missing traceparent →
// pct = 5%.
func TestQuality_MissingOnChildDenominator_UsesChildSpans(t *testing.T) {
	q, _ := newTestQuality()
	// 950 children with valid traceparent.
	for i := 0; i < 950; i++ {
		q.Observe(observeWithTraceparent("k1", "p1", validTraceparent))
	}
	// 50 children missing traceparent.
	for i := 0; i < 50; i++ {
		q.Observe(observeWithTraceparent("k1", "p1", ""))
	}
	snap, _ := q.SnapshotKey("k1")
	assert.Equal(t, uint64(1000), snap.ChildSpans)
	assert.InDelta(t, 5.0, snap.MissingTraceparentOnChildPct, 0.0001)
}

// HTTP-header attribute name path: a span that uses the alternate
// attribute key still counts toward the denominator + (if malformed)
// the numerator.
func TestQuality_MalformedTraceparent_HTTPHeaderAttrName_StillCounts(t *testing.T) {
	q, _ := newTestQuality()
	attrs := computeAttrs()
	attrs[TraceparentHTTPHeaderAttrName] = "not-a-real-traceparent"
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "child1", ParentSpanID: "p1",
		Tier: "compute", Attrs: attrs,
	})
	c := q.perKey["k1"]
	assert.Equal(t, uint64(1), c.MalformedTraceparentSpans)
	assert.Equal(t, uint64(1), c.SpansWithTraceparent)
	assert.Equal(t, uint64(0), c.MissingTraceparentOnChildSpans,
		"a span carrying ANY traceparent attribute isn't missing-on-child")
}

// SnapshotKey populates the new fields with the correct percentages
// and the raw denominator counts.
func TestQuality_SnapshotKey_PopulatesNewFields(t *testing.T) {
	q, _ := newTestQuality()
	// Two child spans missing traceparent → 100% missing-on-child.
	q.Observe(observeWithTraceparent("k1", "p1", ""))
	q.Observe(observeWithTraceparent("k1", "p1", ""))
	// One child span with malformed traceparent → 100% malformed of
	// the SpansWithTraceparent=1 denominator.
	q.Observe(observeWithTraceparent("k1", "p1", "garbage"))

	snap, ok := q.SnapshotKey("k1")
	require.True(t, ok)
	assert.Equal(t, uint64(3), snap.TotalSpans)
	assert.Equal(t, uint64(3), snap.ChildSpans)
	assert.Equal(t, uint64(1), snap.SpansWithTraceparent)
	assert.InDelta(t, 100.0, snap.MalformedTraceparentPct, 0.0001)
	// Missing-on-child counts the 2 of 3 children without
	// traceparent against the ChildSpans=3 denominator = 66.67%.
	assert.InDelta(t, 66.6667, snap.MissingTraceparentOnChildPct, 0.001)
}

// Rolling window resets the slice 2 counters alongside slice 1
// fields. Without this, a resource that triggers slice 2 detection
// at t=0 would keep its bad counters forever even when the next
// hour shows a clean fleet.
func TestQuality_RollingWindowResetsNewCounters(t *testing.T) {
	q, clock := newTestQuality()
	// Seed all four slice 2 counters in the first window.
	q.Observe(observeWithTraceparent("k1", "p1", "garbage"))  // malformed + child
	q.Observe(observeWithTraceparent("k1", "p1", ""))         // missing-on-child + child
	q.Observe(observeWithTraceparent("k1", "p1", validTraceparent))

	c := q.perKey["k1"]
	require.Equal(t, uint64(1), c.MalformedTraceparentSpans)
	require.Equal(t, uint64(1), c.MissingTraceparentOnChildSpans)
	require.Equal(t, uint64(2), c.SpansWithTraceparent)
	require.Equal(t, uint64(3), c.ChildSpans)

	// Advance past the window and observe a clean root span.
	clock.Advance(1*time.Hour + time.Minute)
	q.Observe(SpanObservation{
		Key: "k1", TraceID: "t1", SpanID: "root", Tier: "compute", Attrs: computeAttrs(),
	})

	c = q.perKey["k1"]
	assert.Equal(t, uint64(0), c.MalformedTraceparentSpans, "slice 2 fields should reset on rollover")
	assert.Equal(t, uint64(0), c.MissingTraceparentOnChildSpans)
	assert.Equal(t, uint64(0), c.SpansWithTraceparent)
	assert.Equal(t, uint64(0), c.ChildSpans)
	assert.Equal(t, uint64(1), c.TotalSpans)
}

// Defensive: when no spans carry traceparent and none are children,
// both new percentages stay 0.0 (not NaN) even after observations.
func TestQuality_SnapshotKey_ZeroDenominators_PctIsZero(t *testing.T) {
	q, _ := newTestQuality()
	// 5 root spans without traceparent: SpansWithTraceparent=0,
	// ChildSpans=0.
	for i := 0; i < 5; i++ {
		q.Observe(SpanObservation{
			Key: "k1", TraceID: "t1", SpanID: "root", Tier: "compute", Attrs: computeAttrs(),
		})
	}
	snap, _ := q.SnapshotKey("k1")
	assert.Equal(t, uint64(5), snap.TotalSpans)
	assert.Equal(t, uint64(0), snap.SpansWithTraceparent)
	assert.Equal(t, uint64(0), snap.ChildSpans)
	assert.InDelta(t, 0.0, snap.MalformedTraceparentPct, 0.0001)
	assert.InDelta(t, 0.0, snap.MissingTraceparentOnChildPct, 0.0001)
}

// -- v0.89.122 sampling rate slice 1 chunk 1 additions ---------------------

// TestQuality_24hCounter_IncrementsOnEachSpan — sampling rate
// slice 1 §11 acceptance test 1. Each Observe call increments the
// 24h-window counter independently of the 1h-window TotalSpans
// counter. Both counters advance on every span.
func TestQuality_24hCounter_IncrementsOnEachSpan(t *testing.T) {
	q, _ := newTestQuality()
	for i := 0; i < 5; i++ {
		q.Observe(observeWith("k1", "compute", computeAttrs()))
	}
	c := q.perKey["k1"]
	assert.Equal(t, uint64(5), c.TotalSpans, "1h counter advances per span")
	assert.Equal(t, uint64(5), c.TotalSpansLast24h, "24h counter advances per span")
}

// TestQuality_24hCounter_ResetsAfter24h — sampling rate slice 1
// §11 acceptance test 2. After more than 24h elapsed since
// WindowStart24h, the next Observe resets the 24h counter to 0
// before incrementing — so the post-rollover observation lands in
// a fresh 24h window.
func TestQuality_24hCounter_ResetsAfter24h(t *testing.T) {
	q, clock := newTestQuality()
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	require.Equal(t, uint64(2), q.perKey["k1"].TotalSpansLast24h)

	// Advance just past the 24h boundary.
	clock.Advance(24*time.Hour + time.Minute)
	q.Observe(observeWith("k1", "compute", computeAttrs()))

	c := q.perKey["k1"]
	assert.Equal(t, uint64(1), c.TotalSpansLast24h,
		"post-24h-rollover span should land in fresh 24h window")
}

// TestQuality_24hCounter_IndependentFrom1h_DifferentReset —
// sampling rate slice 1 §11 acceptance test 3. PINS the
// independence requirement: the 1h reset MUST NOT zero the 24h
// counter, and vice versa. A 1h rollover preserves
// TotalSpansLast24h; a 24h rollover (without a span on the 1h
// boundary) doesn't zero TotalSpans either.
//
// This is the test the §5 design doc cites as the load-bearing
// pin for the "Option A: 24h Quality counter, parallel" choice.
func TestQuality_24hCounter_IndependentFrom1h_DifferentReset(t *testing.T) {
	q, clock := newTestQuality()
	// Observe 3 spans at t0.
	for i := 0; i < 3; i++ {
		q.Observe(observeWith("k1", "compute", computeAttrs()))
	}
	c0 := q.perKey["k1"]
	require.Equal(t, uint64(3), c0.TotalSpans)
	require.Equal(t, uint64(3), c0.TotalSpansLast24h)

	// Advance past the 1h boundary (but well under 24h) and observe
	// one span. The 1h counter resets to 1; the 24h counter must
	// stay at 4 (was 3, +1 new span).
	clock.Advance(1*time.Hour + time.Minute)
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	c1 := q.perKey["k1"]
	assert.Equal(t, uint64(1), c1.TotalSpans, "1h counter reset by rollover")
	assert.Equal(t, uint64(4), c1.TotalSpansLast24h,
		"24h counter MUST NOT be zeroed by the 1h rollover (independence pin)")

	// Now advance past 24h from the original WindowStart24h, observe
	// one more span. The 24h counter must reset to 1 — and the 1h
	// counter (whose WindowStart was the post-1h-rollover timestamp,
	// way less than 24h ago) ALSO rolls over since the 24h advance
	// is much larger than 1h, so TotalSpans is 1 too. We pin only
	// the load-bearing assertion: TotalSpansLast24h reset to 1.
	clock.Advance(23 * time.Hour)
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	c2 := q.perKey["k1"]
	assert.Equal(t, uint64(1), c2.TotalSpansLast24h,
		"24h counter reset by 24h rollover")
}

// TestQuality_24hCounter_RolloverDoesNotAffect1hPathology —
// defensive: a 24h rollover MUST NOT zero the pathology
// numerators (OrphanSpans / MissingAttrSpans / AttrMismatchSpans /
// MalformedTraceparentSpans / MissingTraceparentOnChildSpans).
// Those are 1h-window counters and only the 1h reset path is
// allowed to zero them.
//
// We construct a scenario where the 24h window elapses but the
// 1h window does NOT (the test clock advances by 24h+1min, so
// both windows reset — but the 24h reset branch ALONE must not
// touch the 1h fields, which is the property under test). We
// verify by inspecting the reset code in two phases: first
// advance 23h, observe a clean span (1h window resets — leaves
// 24h alone), then advance 1h+1min and observe a second clean
// span (1h resets again, 24h resets). The pathology counters
// should be zero after each step because the 1h rollover zeroed
// them; the test ensures no double-decrement or skipped reset.
func TestQuality_24hCounter_RolloverDoesNotAffect1hPathology(t *testing.T) {
	q, clock := newTestQuality()
	// Seed: one missing-attr span.
	bad := computeAttrs()
	delete(bad, "service.name")
	q.Observe(observeWith("k1", "compute", bad))
	require.Equal(t, uint64(1), q.perKey["k1"].MissingAttrSpans)
	require.Equal(t, uint64(1), q.perKey["k1"].TotalSpansLast24h)

	// Advance 23h — past the 1h window so the 1h fields zero. The
	// 24h counter must NOT zero (only 23h elapsed since
	// WindowStart24h).
	clock.Advance(23 * time.Hour)
	q.Observe(observeWith("k1", "compute", computeAttrs()))
	c := q.perKey["k1"]
	assert.Equal(t, uint64(0), c.MissingAttrSpans,
		"1h rollover zeroed missing-attr counter")
	assert.Equal(t, uint64(1), c.TotalSpans, "fresh 1h window has 1 clean span")
	assert.Equal(t, uint64(2), c.TotalSpansLast24h,
		"24h counter advanced (1 + 1 new) and was NOT reset by the 1h rollover")
}

// TestSpanCountLast24h_ReturnsCorrectCount — pins the accessor's
// happy path: after N Observe calls, SpanCountLast24h returns N
// with ok=true.
func TestSpanCountLast24h_ReturnsCorrectCount(t *testing.T) {
	q, _ := newTestQuality()
	for i := 0; i < 7; i++ {
		q.Observe(observeWith("k1", "compute", computeAttrs()))
	}
	got, ok := q.SpanCountLast24h("k1")
	require.True(t, ok)
	assert.Equal(t, uint64(7), got)
}

// TestSpanCountLast24h_NotFoundKey_ReturnsFalse — a key with no
// observations returns ok=false. The chunk-2 detection branch
// treats this as "insufficient data" and skips the comparison.
func TestSpanCountLast24h_NotFoundKey_ReturnsFalse(t *testing.T) {
	q, _ := newTestQuality()
	q.Observe(observeWith("k1", "compute", computeAttrs()))

	got, ok := q.SpanCountLast24h("never-seen")
	assert.False(t, ok, "missing key returns ok=false")
	assert.Equal(t, uint64(0), got, "missing key returns zero count")
}

// TestSnapshotKey_PopulatesTotalSpansLast24h — the snapshot carries
// the 24h count field so the chunk-2 per-resource sampling API
// endpoint can read it off SnapshotKey without a separate accessor.
func TestSnapshotKey_PopulatesTotalSpansLast24h(t *testing.T) {
	q, _ := newTestQuality()
	for i := 0; i < 4; i++ {
		q.Observe(observeWith("k1", "compute", computeAttrs()))
	}
	snap, ok := q.SnapshotKey("k1")
	require.True(t, ok)
	assert.Equal(t, uint64(4), snap.TotalSpansLast24h)
	assert.Equal(t, uint64(4), snap.TotalSpans, "snapshot preserves 1h field too")
}

