// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// failingFakeStore wraps fakeStore with an injectable upsert failure
// counter so the "continues after error" test can force the first
// flush to fail and the second to succeed.
type failingFakeStore struct {
	*fakeStore
	mu          sync.Mutex
	failNext    int
	failedCount int
}

func newFailingFakeStore() *failingFakeStore {
	return &failingFakeStore{fakeStore: newFakeStore(0)}
}

func (f *failingFakeStore) UpsertTraceResources(ctx context.Context, rows []ResourceRow) (int, error) {
	f.mu.Lock()
	shouldFail := f.failNext > 0
	if shouldFail {
		f.failNext--
		f.failedCount++
	}
	f.mu.Unlock()
	if shouldFail {
		return 0, errors.New("simulated store failure")
	}
	return f.fakeStore.UpsertTraceResources(ctx, rows)
}

func (f *failingFakeStore) setFailNext(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = n
}

func (f *failingFakeStore) failures() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failedCount
}

// spyAudit records every Record call so flush_test can assert on the
// per-cycle audit emission shape without touching the real
// services.AuditService.
type spyAudit struct {
	mu       sync.Mutex
	calls    []spyAuditCall
	callsCnt int32
}

type spyAuditCall struct {
	EventType string
	Actor     string
	Payload   map[string]any
}

func (s *spyAudit) Record(_ context.Context, eventType, actor string, payload map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, spyAuditCall{EventType: eventType, Actor: actor, Payload: payload})
	atomic.AddInt32(&s.callsCnt, 1)
}

func (s *spyAudit) count() int {
	return int(atomic.LoadInt32(&s.callsCnt))
}

func (s *spyAudit) snapshot() []spyAuditCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]spyAuditCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// observeStrongKey is a small helper that lands one strong-confidence
// row in the index's pending map so a subsequent Flush has something
// to write.
func observeStrongKey(idx *Index, key string) {
	idx.Observe(context.Background(), ResourceObservation{
		Attributes: map[string]string{
			"cloud.provider":    "aws",
			"cloud.resource_id": key,
		},
		SpanCount:     1,
		RootSpanCount: 1,
	})
}

// TestBackgroundFlusher_FlushesEveryInterval — a 25ms cadence for
// 150ms should yield at least two upserts on the fake store (one
// per tick that found pending rows). The test re-observes between
// flushes so each tick has something to write.
func TestBackgroundFlusher_FlushesEveryInterval(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, nil)
	flusher := NewBackgroundFlusher(idx, 25*time.Millisecond, nil, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		flusher.Start(ctx)
		close(done)
	}()

	// Re-observe each tick so the pending map is non-empty on flush.
	stopFeed := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-stopFeed:
				return
			case <-t.C:
				i++
				observeStrongKey(idx, "arn:test:i-"+itoa(i))
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stopFeed)
	cancel()
	<-done

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.GreaterOrEqual(t, store.upserts, 2, "at least two flush cycles within 150ms at 25ms cadence")
}

// TestBackgroundFlusher_EmitsAuditOnEachFlush asserts that the spy
// audit emitter sees one Record call per successful non-empty flush,
// with the payload carrying the documented meta-shape — counts +
// duration + interval, no span content.
func TestBackgroundFlusher_EmitsAuditOnEachFlush(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, nil)
	audit := &spyAudit{}
	flusher := NewBackgroundFlusher(idx, 25*time.Millisecond, audit, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		flusher.Start(ctx)
		close(done)
	}()

	stopFeed := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-stopFeed:
				return
			case <-t.C:
				i++
				observeStrongKey(idx, "arn:audit:i-"+itoa(i))
			}
		}
	}()

	time.Sleep(120 * time.Millisecond)
	close(stopFeed)
	cancel()
	<-done

	require.GreaterOrEqual(t, audit.count(), 2)

	calls := audit.snapshot()
	for _, c := range calls {
		assert.Equal(t, "trace_index.background_flushed", c.EventType)
		assert.Equal(t, "system", c.Actor)
		require.Contains(t, c.Payload, "rows_written")
		require.Contains(t, c.Payload, "rows_evicted")
		require.Contains(t, c.Payload, "duration_ms")
		require.Contains(t, c.Payload, "interval_s")
		assert.Equal(t, 1, c.Payload["interval_s"], "sub-second interval reports min 1s")
		// Acceptance test 12 — span content must NOT be in payload.
		for k := range c.Payload {
			assert.NotContains(t, k, "span")
			assert.NotContains(t, k, "trace_id")
			assert.NotContains(t, k, "attributes")
		}
	}
}

// TestBackgroundFlusher_ContinuesAfterFlushError verifies the loop
// survives a transient store failure. The fake store fails the
// first upsert, then succeeds — after enough ticks, at least one
// successful upsert lands.
func TestBackgroundFlusher_ContinuesAfterFlushError(t *testing.T) {
	store := newFailingFakeStore()
	store.setFailNext(1)
	idx := NewIndex(store, 0, nil)
	flusher := NewBackgroundFlusher(idx, 20*time.Millisecond, nil, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		flusher.Start(ctx)
		close(done)
	}()

	stopFeed := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-stopFeed:
				return
			case <-t.C:
				i++
				observeStrongKey(idx, "arn:retry:i-"+itoa(i))
			}
		}
	}()

	time.Sleep(120 * time.Millisecond)
	close(stopFeed)
	cancel()
	<-done

	assert.Equal(t, 1, store.failures(), "store rejected exactly one flush")
	store.fakeStore.mu.Lock()
	successful := store.fakeStore.upserts
	store.fakeStore.mu.Unlock()
	assert.GreaterOrEqual(t, successful, 1, "at least one successful flush after the failed one")
}

// TestBackgroundFlusher_StopOnContextCancel asserts that Start
// returns promptly when ctx is canceled. The harness allows up to
// 200ms — Start blocks on a Ticker.C select, so cancellation should
// land within one tick at most.
func TestBackgroundFlusher_StopOnContextCancel(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, nil)
	flusher := NewBackgroundFlusher(idx, 50*time.Millisecond, nil, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		flusher.Start(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond) // let the goroutine reach select
	cancel()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Start did not return within 200ms of context cancel")
	}
}

// TestBackgroundFlusher_NilIndexExitsCleanly — defensive guard. A
// nil Index would crash on the first Flush; the flusher logs +
// returns instead.
func TestBackgroundFlusher_NilIndexExitsCleanly(t *testing.T) {
	flusher := NewBackgroundFlusher(nil, 50*time.Millisecond, nil, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		flusher.Start(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Start with nil index did not return")
	}
}

// TestBackgroundFlusher_SuppressesAuditOnQuietTick — when the
// pending map is empty, no audit row should fire. The dashboard
// would otherwise drown in "flushed 0 rows" events.
func TestBackgroundFlusher_SuppressesAuditOnQuietTick(t *testing.T) {
	store := newFakeStore(0)
	idx := NewIndex(store, 0, nil)
	audit := &spyAudit{}
	flusher := NewBackgroundFlusher(idx, 20*time.Millisecond, audit, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		flusher.Start(ctx)
		close(done)
	}()

	// No observations — every tick finds an empty pending map.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, 0, audit.count(), "no audit rows on quiet ticks")
}

// itoa is a small allocation-light int → string the flush tests use
// to vary keys per tick without dragging in strconv at every call
// site. The test runtime doesn't care about hot-path performance
// but keeping the helper local keeps the test file self-contained.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}
