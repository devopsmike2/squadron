// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package leaderelection defines the boundary between the open-core
// single-instance runtime and the enterprise edition's high-availability
// (HA) leader election (HA architecture, ADR ~0035, slice S1).
//
// Squadron OSS runs as a single instance: every background singleton loop
// (the rollout engine, cost-spike detector, AI proposer bridge, alert
// evaluator, silent-agent watcher, discovery scan scheduler, deploy poller,
// usage reporter) runs unconditionally on the one process. That is correct
// for OSS/SQLite and is the behavior this package must preserve exactly.
//
// HA changes the picture: when two or more Squadron instances share a
// Postgres backend, each of those loops must run on EXACTLY ONE instance at
// a time, with automatic failover if the leader dies. Running the rollout
// engine on two instances at once would double-advance rollouts; running two
// deploy pollers would race on run status. Coordination is required.
//
// This package is that boundary, mirroring extension/detectors,
// extension/identity, and extension/tracebudget. An Elector guarantees a
// named singleton runs on exactly one instance. The OSS binary wires
// AlwaysLeader — this instance is always the leader for every name, so every
// singleton runs, identical to pre-HA behavior. The enterprise edition
// (private squadron-enterprise repo) wires a Postgres advisory-lock elector
// against this same interface, so exactly one instance runs each singleton
// with failover. See cmd/all-in-one/wire_leaderelection_oss.go (default) and
// wire_leaderelection_enterprise.go (build tag: enterprise), plus
// docs/build.md.
//
// The boundary lives under extension/ (not internal/) so the private
// enterprise repo can import it across module boundaries.
package leaderelection

import (
	"context"
	"sync"
)

// Elector guarantees that a named singleton runs on exactly one instance.
// main.go wires one Elector at startup and dispatches every background
// singleton loop through it instead of starting the loops unconditionally.
//
// A nil Elector is NOT a valid runtime state — main.go always wires one via
// the edition seam (AlwaysLeader in OSS). This differs from the detector /
// tracebudget seams, whose nil provider means "dormant"; here "dormant"
// would mean no singleton ever runs, which is never correct.
type Elector interface {
	// RunSingleton ensures run executes on exactly one instance for the
	// given name. run is invoked when this instance holds (or acquires)
	// leadership for name. The context passed to run is cancelled when this
	// instance loses leadership OR the elector is shut down; run MUST block
	// until that context is done and then return promptly (releasing any
	// resources it holds). A real elector MAY invoke run again if this
	// instance re-acquires leadership after a loss.
	//
	// RunSingleton itself does not block: it returns after registering the
	// singleton, launching run in the background under leadership.
	//
	// name must be stable across instances and restarts — it is the key the
	// coordination substrate locks on. Registering the same name twice on
	// one instance is a programming error.
	RunSingleton(name string, run func(ctx context.Context))

	// Shutdown relinquishes leadership for every registered singleton,
	// cancels each run's context, and waits for every run function to
	// return — or until ctx is done, whichever comes first. After Shutdown
	// returns, RunSingleton is a no-op. Shutdown is idempotent.
	Shutdown(ctx context.Context)
}

// AlwaysLeader is the OSS default Elector: this instance is the leader for
// every name, so every registered singleton runs immediately and
// unconditionally. There is no coordination — that is exactly right for a
// single-instance OSS/SQLite deployment, where observable behavior is
// identical to running each loop directly.
//
// It is safe for concurrent use. Shutdown cancels every running singleton's
// context and waits for them to drain, preserving the graceful-shutdown
// behavior main.go had when each loop stopped via its own deferred Stop().
type AlwaysLeader struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

// NewAlwaysLeader returns an AlwaysLeader ready to run singletons. Its
// singletons run under a context cancelled by Shutdown.
func NewAlwaysLeader() *AlwaysLeader {
	ctx, cancel := context.WithCancel(context.Background())
	return &AlwaysLeader{ctx: ctx, cancel: cancel}
}

// RunSingleton launches run immediately in its own goroutine — this instance
// is always the leader — passing the AlwaysLeader's context so Shutdown can
// stop it. After Shutdown, it is a no-op.
func (a *AlwaysLeader) RunSingleton(name string, run func(ctx context.Context)) {
	_ = name // AlwaysLeader never coordinates, so the name is unused here.
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.wg.Add(1)
	ctx := a.ctx
	a.mu.Unlock()

	go func() {
		defer a.wg.Done()
		run(ctx)
	}()
}

// Shutdown cancels every running singleton's context and waits for them to
// return, or until ctx is done. Idempotent.
func (a *AlwaysLeader) Shutdown(ctx context.Context) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.cancel()
	a.mu.Unlock()

	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Compile-time assertion that AlwaysLeader satisfies the Elector interface.
var _ Elector = (*AlwaysLeader)(nil)
