package worker

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// byteItem builds a WorkItem of exactly n raw bytes. The bytes are garbage —
// the byte-budget accounting keys on len(RawData) at submit and at dequeue,
// independent of whether the payload later parses (a parse failure just drops
// the item after its bytes are already released), so these tests need no valid
// OTLP payload, writer, or agent service.
func byteItem(n int) WorkItem {
	return WorkItem{Type: WorkItemTypeTraces, RawData: make([]byte, n), Timestamp: time.Now()}
}

// Over-budget submit is shed (workers=0 so nothing ever drains): the first two
// fit, the third waits ~submit_timeout then errors, and its reservation is
// rolled back.
func TestSubmit_ByteBudget_ShedsWhenExceeded(t *testing.T) {
	p := NewPool(100 /*generous count cap*/, 0 /*no workers*/, 80*time.Millisecond, nil, nil, nil, zap.NewNop())
	p.SetMaxQueueBytes(1000)

	if err := p.Submit(byteItem(400)); err != nil {
		t.Fatalf("submit 1 (400B): %v", err)
	}
	if err := p.Submit(byteItem(400)); err != nil {
		t.Fatalf("submit 2 (400B): %v", err)
	}
	if got := p.QueuedBytes(); got != 800 {
		t.Fatalf("QueuedBytes=%d, want 800", got)
	}

	start := time.Now()
	if err := p.Submit(byteItem(400)); err == nil { // would push to 1200 > 1000
		t.Fatal("expected shed error on over-budget submit")
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Fatalf("expected shed to wait ~submit_timeout before failing, took %v", elapsed)
	}
	if got := p.QueuedBytes(); got != 800 {
		t.Fatalf("after shed QueuedBytes=%d, want 800 (reservation must roll back)", got)
	}
}

// A single payload larger than the whole budget can never fit — reject
// immediately rather than waiting out submit_timeout.
func TestSubmit_ByteBudget_OversizePayloadRejectedFast(t *testing.T) {
	p := NewPool(100, 0, 5*time.Second, nil, nil, nil, zap.NewNop())
	p.SetMaxQueueBytes(500)

	start := time.Now()
	if err := p.Submit(byteItem(600)); err == nil {
		t.Fatal("expected immediate reject for payload larger than the byte budget")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("oversize reject should be immediate, took %v", elapsed)
	}
	if got := p.QueuedBytes(); got != 0 {
		t.Fatalf("QueuedBytes=%d, want 0 (nothing reserved on immediate reject)", got)
	}
}

// Workers dequeuing free the budget, so a workload whose total far exceeds the
// budget still fully succeeds (bytes are released on dequeue, waking submitters).
func TestSubmit_ByteBudget_DequeueFreesBudget(t *testing.T) {
	p := NewPool(100, 2 /*workers drain*/, 2*time.Second, nil, nil, nil, zap.NewNop())
	p.SetMaxQueueBytes(1000) // only ~2 in-flight 400B items fit at once
	p.Start()
	defer func() { _ = p.Stop(2 * time.Second) }()

	for i := 0; i < 50; i++ { // 50×400B = 20,000B >> 1000B budget
		if err := p.Submit(byteItem(400)); err != nil {
			t.Fatalf("submit %d should succeed as workers drain the budget: %v", i, err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for p.QueuedBytes() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.QueuedBytes(); got != 0 {
		t.Fatalf("after drain QueuedBytes=%d, want 0", got)
	}
}

// With the byte bound disabled (default / negative), only the request-count cap
// applies — a huge single payload still enqueues, and the gauge still tracks
// bytes for observability.
func TestSubmit_ByteBudget_DisabledByDefault(t *testing.T) {
	p := NewPool(10, 0, 2*time.Second, nil, nil, nil, zap.NewNop())
	// no SetMaxQueueBytes call => byte bound disabled

	if err := p.Submit(byteItem(10_000_000)); err != nil {
		t.Fatalf("with the byte bound disabled a huge payload should enqueue under the count cap: %v", err)
	}
	if got := p.QueuedBytes(); got != 10_000_000 {
		t.Fatalf("QueuedBytes=%d, want 10000000 (gauge tracks bytes even when the bound is off)", got)
	}
}
