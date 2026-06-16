// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package siem

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeSource emits a fixed list of destinations. Implements
// SourceProvider so dispatcher tests don't touch storage.
type fakeSource struct {
	dests   []*Destination
	secrets [][]byte
}

func (f *fakeSource) LoadEnabled(ctx context.Context) ([]*Destination, [][]byte, error) {
	return f.dests, f.secrets, nil
}

// fakeExporter records what got sent. Optionally returns an error
// the first N times to exercise retry.
type fakeExporter struct {
	mu      sync.Mutex
	sent    []Event
	failFor int // first N calls return error
}

func (e *fakeExporter) Send(_ context.Context, ev Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failFor > 0 {
		e.failFor--
		return errors.New("transient")
	}
	e.sent = append(e.sent, ev)
	return nil
}

func (e *fakeExporter) seen() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, len(e.sent))
	copy(out, e.sent)
	return out
}

// TestDispatcher_FanOut verifies an event lands on every enabled
// destination that matches its filter.
func TestDispatcher_FanOut(t *testing.T) {
	exA := &fakeExporter{}
	exB := &fakeExporter{}

	// Override BuildExporter by injecting workers directly.
	d := NewDispatcher(&fakeSource{}, 0, zap.NewNop())
	d.workers["a"] = newWorker(&Destination{ID: "a", Name: "A"}, exA, zap.NewNop())
	d.workers["b"] = newWorker(&Destination{ID: "b", Name: "B"}, exB, zap.NewNop())
	go d.workers["a"].run()
	go d.workers["b"].run()
	defer d.shutdownAll()

	ev := sampleEvent()
	d.Dispatch(ev)

	// Allow workers a moment to drain the channel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(exA.seen()) > 0 && len(exB.seen()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(exA.seen()) != 1 || len(exB.seen()) != 1 {
		t.Errorf("expected 1 event on each exporter, got A=%d B=%d", len(exA.seen()), len(exB.seen()))
	}
}

// TestDispatcher_FilterRespected: a destination with a prefix only
// gets matching events; the other still gets everything.
func TestDispatcher_FilterRespected(t *testing.T) {
	exA := &fakeExporter{}
	exB := &fakeExporter{}
	d := NewDispatcher(&fakeSource{}, 0, zap.NewNop())
	d.workers["a"] = newWorker(&Destination{ID: "a", EventTypePrefix: []string{"rollout."}}, exA, zap.NewNop())
	d.workers["b"] = newWorker(&Destination{ID: "b"}, exB, zap.NewNop())
	go d.workers["a"].run()
	go d.workers["b"].run()
	defer d.shutdownAll()

	d.Dispatch(Event{EventType: "rollout.approved"})
	d.Dispatch(Event{EventType: "config.created"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(exB.seen()) == 2 && len(exA.seen()) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(exA.seen()) != 1 {
		t.Errorf("expected A to see 1 event, got %d", len(exA.seen()))
	}
	if len(exB.seen()) != 2 {
		t.Errorf("expected B to see 2 events, got %d", len(exB.seen()))
	}
}

// TestDispatcher_DropOnFullQueue verifies the dropped counter
// increments when a destination's queue is saturated. This is the
// critical safety property: audit writes must never block on a slow
// SIEM, so we trade event delivery for forward progress.
func TestDispatcher_DropOnFullQueue(t *testing.T) {
	// Block the exporter forever so the queue fills up.
	blocker := &blockingExporter{}
	d := NewDispatcher(&fakeSource{}, 0, zap.NewNop())
	w := newWorker(&Destination{ID: "a"}, blocker, zap.NewNop())
	d.workers["a"] = w
	go w.run()
	defer d.shutdownAll()

	// Fill the queue past capacity. First event goes to the
	// (blocked) worker; the rest fill the bounded channel.
	for i := 0; i < queueCapacity+50; i++ {
		d.Dispatch(Event{ID: "evt"})
	}
	if got := d.Dropped(); got < 49 {
		// At least 49 should be dropped (the worker holds one,
		// the channel holds queueCapacity, so anything beyond
		// queueCapacity+1 drops). Looser bound to avoid timing
		// flake.
		t.Errorf("expected ≥49 drops, got %d", got)
	}
}

// blockingExporter never returns from Send so its worker stays busy.
type blockingExporter struct{}

func (blockingExporter) Send(ctx context.Context, _ Event) error {
	<-ctx.Done()
	return ctx.Err()
}
