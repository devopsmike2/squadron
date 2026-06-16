// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package siem

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// queueCapacity is the per-destination bounded channel size. 1024 is
// a reasonable cushion: at a sustained 10 events/s with a slow SIEM
// dropping packets for a minute, the queue absorbs the burst without
// overflow. Bursts beyond that point drop — better than blocking
// audit writes.
const queueCapacity = 1024

// retryAttempts caps backoff so a permanently-down SIEM doesn't tie
// up the worker goroutine forever. After the cap, the event is
// dropped (with a metric) and the worker moves on. We don't persist
// the in-flight queue — accepting some loss during outages is the
// trade for keeping audit writes unblocked.
const retryAttempts = 4

// SourceProvider lets the dispatcher reload destinations periodically
// without a tight coupling to the storage layer. Implementations
// return the currently-enabled destinations and the corresponding
// decrypted secret bytes (parallel slices, same indices).
type SourceProvider interface {
	LoadEnabled(ctx context.Context) ([]*Destination, [][]byte, error)
}

// Dispatcher fans audit events out to all configured SIEM
// destinations. Each destination gets its own bounded channel and
// worker goroutine so a slow SIEM only backs up its own pipe.
//
// The dispatcher reloads destinations from storage every reloadEvery
// so operator changes (add/remove/disable) apply without a restart.
// Reload is idempotent — workers for unchanged destinations keep
// running.
type Dispatcher struct {
	source      SourceProvider
	reloadEvery time.Duration
	logger      *zap.Logger

	mu      sync.Mutex
	workers map[string]*worker

	dropped atomic.Uint64
}

// NewDispatcher constructs a Dispatcher. reloadEvery=0 disables
// periodic reload (tests only — production should pass 60s or so).
func NewDispatcher(source SourceProvider, reloadEvery time.Duration, logger *zap.Logger) *Dispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Dispatcher{
		source:      source,
		reloadEvery: reloadEvery,
		logger:      logger,
		workers:     make(map[string]*worker),
	}
}

// Start launches the reload loop. Returns immediately; cancel ctx to
// stop the dispatcher and drain workers.
func (d *Dispatcher) Start(ctx context.Context) {
	// Eager first load so we don't drop the first event after
	// startup waiting for the reload tick.
	d.reload(ctx)
	if d.reloadEvery <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(d.reloadEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				d.shutdownAll()
				return
			case <-t.C:
				d.reload(ctx)
			}
		}
	}()
}

// Dispatch enqueues an event on every matching destination. Never
// blocks — drops on full queue and bumps the dropped counter.
func (d *Dispatcher) Dispatch(ev Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range d.workers {
		if !w.dest.MatchesFilter(ev.EventType) {
			continue
		}
		select {
		case w.in <- ev:
		default:
			d.dropped.Add(1)
			d.logger.Warn("siem: dropping event, queue full",
				zap.String("destination", w.dest.Name),
				zap.String("event_type", ev.EventType))
		}
	}
}

// Dropped returns the cumulative count of events dropped on full
// queue. Exported for the /metrics endpoint.
func (d *Dispatcher) Dropped() uint64 {
	return d.dropped.Load()
}

// reload refreshes the worker set against the storage layer.
// Destinations that are still enabled keep their existing worker
// (queue + retry state preserved); new destinations get a fresh
// worker; removed/disabled destinations get their workers cancelled.
func (d *Dispatcher) reload(ctx context.Context) {
	dests, secrets, err := d.source.LoadEnabled(ctx)
	if err != nil {
		d.logger.Warn("siem: reload failed", zap.Error(err))
		return
	}
	want := make(map[string]struct{}, len(dests))
	for i, dest := range dests {
		want[dest.ID] = struct{}{}
		exp, err := BuildExporter(dest, secrets[i])
		if err != nil {
			d.logger.Warn("siem: skipping destination, build failed",
				zap.String("destination", dest.Name), zap.Error(err))
			continue
		}
		d.mu.Lock()
		if existing, ok := d.workers[dest.ID]; ok {
			// Hot-swap config so URL/secret edits take effect
			// without losing the in-flight queue.
			existing.dest = dest
			existing.exporter = exp
		} else {
			w := newWorker(dest, exp, d.logger)
			d.workers[dest.ID] = w
			go w.run()
		}
		d.mu.Unlock()
	}
	// Stop workers for destinations that disappeared or were
	// disabled.
	d.mu.Lock()
	for id, w := range d.workers {
		if _, keep := want[id]; !keep {
			w.stop()
			delete(d.workers, id)
		}
	}
	d.mu.Unlock()
}

func (d *Dispatcher) shutdownAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range d.workers {
		w.stop()
	}
	d.workers = nil
}

// --- worker ----------------------------------------------------------

type worker struct {
	dest     *Destination
	exporter Exporter
	in       chan Event
	done     chan struct{}
	logger   *zap.Logger
}

func newWorker(dest *Destination, exp Exporter, logger *zap.Logger) *worker {
	return &worker{
		dest:     dest,
		exporter: exp,
		in:       make(chan Event, queueCapacity),
		done:     make(chan struct{}),
		logger:   logger,
	}
}

func (w *worker) run() {
	for {
		select {
		case <-w.done:
			return
		case ev := <-w.in:
			w.deliver(ev)
		}
	}
}

func (w *worker) stop() {
	close(w.done)
}

// deliver runs the retry loop for a single event. Backoff is
// exponential starting at 500ms (covers SIEM blips up to ~15s before
// dropping). We deliberately don't retry forever — the next event's
// freshness matters more than this event's eventual delivery once
// the SIEM has been down for minutes.
func (w *worker) deliver(ev Event) {
	backoff := 500 * time.Millisecond
	for attempt := 0; attempt < retryAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := w.exporter.Send(ctx, ev)
		cancel()
		if err == nil {
			return
		}
		w.logger.Warn("siem: export attempt failed",
			zap.String("destination", w.dest.Name),
			zap.Int("attempt", attempt+1),
			zap.Error(err))
		if attempt == retryAttempts-1 {
			return
		}
		time.Sleep(backoff)
		backoff *= 2
	}
}
