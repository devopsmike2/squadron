// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// AuditEmitter is the minimal audit-write surface the background
// flusher uses. The full services.AuditService satisfies a richer
// interface; the chunk-2 wiring layer adapts it to this shape so
// the traceindex package does NOT import internal/services (which
// would create a cycle — services transitively depends on the
// storage layer which depends on traceindex.ResourceRow).
//
// The actor + eventType + payload triplet matches the audit row's
// minimum-viable shape — TargetType / TargetID / Action are fixed
// at the adapter layer because the flush event is always fleet-wide
// and always means the same thing.
type AuditEmitter interface {
	Record(ctx context.Context, eventType, actor string, payload map[string]any)
}

// BackgroundFlusher runs the slice-1 chunk-2 periodic flush of the
// in-memory traceindex cache to the backing store. The constructor
// pairs the Index with the flush cadence + an optional audit
// emitter; Start runs the loop until the supplied context cancels.
//
// Lifecycle: cmd/all-in-one constructs the flusher AFTER the Index
// and the audit service, spawns Start in a goroutine, and relies on
// context cancellation (the same ctx that drives the rollout engine
// + other long-running goroutines) to stop the loop on shutdown.
// Stop is not a method on the flusher because cancellation through
// the context is sufficient and adds no extra synchronization
// surface to manage.
type BackgroundFlusher struct {
	index    *Index
	interval time.Duration
	audit    AuditEmitter
	logger   *zap.Logger
	clock    func() time.Time
	// quality, when non-nil, has EvictExpired called on every flush tick
	// so the span-quality index's memory stays bounded to ACTIVE
	// resources/traces. The Quality structure is fed on the OTLP receive
	// hot path (one Observe per span) and its parentSeen map otherwise
	// grows unbounded — there is no other production eviction driver.
	quality qualityEvictor
}

// qualityEvictor is the minimal surface the flusher needs to bound the
// span-quality index's memory. *Quality satisfies it; tests substitute a
// fake. Returns (countersEvicted, tracesEvicted).
type qualityEvictor interface {
	EvictExpired() (countersEvicted, tracesEvicted int)
}

var _ qualityEvictor = (*Quality)(nil)

// defaultFlushInterval is the slice-1 design-doc §4 cadence — 30
// seconds. The chunk-2 wiring passes this explicitly so an operator-
// configurable env var can override later without a package-level
// constant change.
const defaultFlushInterval = 30 * time.Second

// NewBackgroundFlusher constructs a flusher. A zero or negative
// interval falls through to the 30s default — tests pin a much
// shorter interval (50ms is typical) to keep the loop test runtime
// in the millisecond range.
//
// The audit emitter is OPTIONAL: a nil emitter disables the per-
// cycle audit row without affecting the flush itself. Useful in
// tests that don't care about the audit surface and in deployments
// where the audit service hasn't been wired (none exist today, but
// the safety belt is cheap).
//
// The logger is REQUIRED — flush errors get logged but do not stop
// the loop, and a nil logger would crash on the first error. The
// chunk-2 wiring always passes a real logger.
func NewBackgroundFlusher(index *Index, interval time.Duration, audit AuditEmitter, logger *zap.Logger) *BackgroundFlusher {
	if interval <= 0 {
		interval = defaultFlushInterval
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BackgroundFlusher{
		index:    index,
		interval: interval,
		audit:    audit,
		logger:   logger,
		clock:    time.Now,
	}
}

// WithQualityEvictor wires the span-quality index so its EvictExpired runs
// on every flush tick — the single production driver that keeps the
// Quality structure's memory bounded to ACTIVE resources/traces (its
// parentSeen map otherwise grows unbounded with cumulative unique span
// count on the receive hot path). Nil-safe and optional: span-quality may
// be disabled, in which case the flusher just flushes the Index. Returns
// the receiver for chaining off NewBackgroundFlusher.
func (b *BackgroundFlusher) WithQualityEvictor(q qualityEvictor) *BackgroundFlusher {
	// Guard against a typed-nil *Quality being wrapped in a non-nil
	// interface (the common main.go "qualityIndex may be nil" case).
	if q == nil {
		return b
	}
	if qi, ok := q.(*Quality); ok && qi == nil {
		return b
	}
	b.quality = q
	return b
}

// Start runs the flush loop until ctx is canceled. Errors during
// flush are logged but do not stop the loop — the in-memory cache
// is preserved on flush failure (the Index drains its pending map
// into the store call's []rows argument, and a store-level error
// returns from Flush WITHOUT re-staging those rows back into the
// map; this is consistent with the design-doc §4 promise that
// flush IS the persistence boundary and a transient store failure
// loses the batch). The "preserved on failure" wording in the
// chunk-2 prompt refers to the NEXT cycle picking up subsequent
// observations cleanly; the loop survives, not the failed batch.
//
// Start blocks until ctx is canceled. The caller spawns it in a
// goroutine — the chunk-2 cmd/all-in-one wiring does exactly that
// alongside the rollout engine's Start.
func (b *BackgroundFlusher) Start(ctx context.Context) {
	if b.index == nil {
		b.logger.Error("traceindex background flusher started with nil index, exiting")
		return
	}
	b.logger.Info("Starting traceindex background flusher",
		zap.Duration("interval", b.interval),
		zap.Bool("audit_enabled", b.audit != nil))

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	intervalSeconds := int(b.interval.Seconds())
	if intervalSeconds <= 0 {
		// Sub-second intervals (used in tests) report a positive
		// integer so the payload shape stays stable; the field is a
		// debugging aid not a precision counter.
		intervalSeconds = 1
	}

	for {
		select {
		case <-ctx.Done():
			b.logger.Info("traceindex background flusher stopping")
			return
		case <-ticker.C:
			b.flushOnce(ctx, intervalSeconds)
		}
	}
}

// flushOnce runs a single Flush + audit-emit cycle. Factored out of
// Start so flush_test can drive a single iteration without standing
// up the ticker.
func (b *BackgroundFlusher) flushOnce(ctx context.Context, intervalSeconds int) {
	// Span-quality eviction runs every tick, independent of the Index
	// flush (and of whether that flush errors) — it is the only production
	// bound on the Quality structure's memory. Without it parentSeen leaks
	// unboundedly on the OTLP receive hot path. Counts are debug-logged;
	// the audit payload contract (counts/duration/interval only) is
	// unchanged.
	if b.quality != nil {
		if ce, te := b.quality.EvictExpired(); ce > 0 || te > 0 {
			b.logger.Debug("traceindex quality eviction",
				zap.Int("counters_evicted", ce),
				zap.Int("traces_evicted", te))
		}
	}

	start := b.clock()
	written, evicted, err := b.index.Flush(ctx)
	duration := b.clock().Sub(start)

	if err != nil {
		b.logger.Error("traceindex flush failed",
			zap.Error(err),
			zap.Duration("duration", duration))
		return
	}

	if written == 0 && evicted == 0 {
		// Quiet path: no rows pending. Don't emit an audit row for
		// every quiet tick — the dashboard would fill with empty
		// "flushed 0 rows" events that don't carry operational
		// signal. Tests assert at-least-N flushes by reading the
		// fake store's upsert counter instead.
		return
	}

	b.logger.Debug("traceindex flush completed",
		zap.Int("rows_written", written),
		zap.Int("rows_evicted", evicted),
		zap.Duration("duration", duration))

	if b.audit != nil {
		// Payload contract — design doc §8 + chunk-2 prompt:
		// counts + duration + interval ONLY. No span content. No
		// resource attributes. Acceptance test 12 ("Span content
		// not in audit") pins this.
		b.audit.Record(ctx,
			"trace_index.background_flushed",
			"system",
			map[string]any{
				"rows_written": written,
				"rows_evicted": evicted,
				"duration_ms":  int(duration.Milliseconds()),
				"interval_s":   intervalSeconds,
			})
	}
}
