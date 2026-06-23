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
