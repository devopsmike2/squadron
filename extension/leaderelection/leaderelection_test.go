// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package leaderelection

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestAlwaysLeader_RunsSingleton pins the OSS default: AlwaysLeader is the
// leader for every name, so a registered singleton actually runs. This is
// what makes single-instance/SQLite behavior identical to running each loop
// directly.
func TestAlwaysLeader_RunsSingleton(t *testing.T) {
	el := NewAlwaysLeader()
	ran := make(chan struct{})
	el.RunSingleton("test-singleton", func(ctx context.Context) {
		close(ran)
		<-ctx.Done()
	})

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("AlwaysLeader did not run the registered singleton")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	el.Shutdown(shutdownCtx)
}

// TestAlwaysLeader_ShutdownCancelsAndWaits confirms Shutdown cancels the
// singleton's context AND waits for the run function to return before
// returning — preserving the graceful-drain behavior main.go relied on when
// each loop stopped via its own deferred Stop().
func TestAlwaysLeader_ShutdownCancelsAndWaits(t *testing.T) {
	el := NewAlwaysLeader()
	var returned bool
	var mu sync.Mutex
	started := make(chan struct{})

	el.RunSingleton("drains-on-shutdown", func(ctx context.Context) {
		close(started)
		<-ctx.Done() // block until leadership is relinquished
		mu.Lock()
		returned = true
		mu.Unlock()
	})

	<-started
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	el.Shutdown(shutdownCtx)

	mu.Lock()
	got := returned
	mu.Unlock()
	if !got {
		t.Fatal("Shutdown returned before the singleton's run function drained")
	}
}

// TestAlwaysLeader_ShutdownIsIdempotentAndBlocksNewSingletons confirms
// Shutdown can be called more than once safely and that RunSingleton is a
// no-op after Shutdown.
func TestAlwaysLeader_ShutdownIsIdempotentAndBlocksNewSingletons(t *testing.T) {
	el := NewAlwaysLeader()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	el.Shutdown(ctx)
	el.Shutdown(ctx) // must not panic

	ran := false
	el.RunSingleton("after-shutdown", func(context.Context) { ran = true })
	// Give a stray goroutine a chance to (incorrectly) run.
	time.Sleep(20 * time.Millisecond)
	if ran {
		t.Fatal("RunSingleton ran a singleton after Shutdown; want no-op")
	}
}

// grantingElector is a test double standing in for an enterprise elector
// that has won leadership: it runs every registered singleton. It documents
// one half of the contract the enterprise Postgres advisory-lock elector
// must satisfy.
type grantingElector struct{ wg sync.WaitGroup }

func (g *grantingElector) RunSingleton(_ string, run func(ctx context.Context)) {
	g.wg.Add(1)
	go func() { defer g.wg.Done(); run(context.Background()) }()
}
func (g *grantingElector) Shutdown(context.Context) { g.wg.Wait() }

// denyingElector is a test double standing in for an enterprise elector that
// has NOT won leadership: it never runs a registered singleton. This is the
// behavior that makes HA safe — a non-leader instance must not run the
// coordinated loops.
type denyingElector struct{}

func (denyingElector) RunSingleton(string, func(ctx context.Context)) {}
func (denyingElector) Shutdown(context.Context)                       {}

var (
	_ Elector = (*grantingElector)(nil)
	_ Elector = denyingElector{}
)

// TestElectorContract_GrantVsDeny pins the coordination contract the seam
// exists to express: an elector that grants leadership runs the singleton;
// one that denies it does not. main.go dispatches every background loop
// through Elector.RunSingleton, so this is what governs which instance runs
// each loop in an HA deployment.
func TestElectorContract_GrantVsDeny(t *testing.T) {
	t.Run("granted runs the singleton", func(t *testing.T) {
		var el Elector = &grantingElector{}
		ran := make(chan struct{})
		el.RunSingleton("s", func(context.Context) { close(ran) })
		select {
		case <-ran:
		case <-time.After(2 * time.Second):
			t.Fatal("granting elector did not run the singleton")
		}
		el.Shutdown(context.Background())
	})

	t.Run("denied does not run the singleton", func(t *testing.T) {
		var el Elector = denyingElector{}
		ran := false
		el.RunSingleton("s", func(context.Context) { ran = true })
		time.Sleep(20 * time.Millisecond)
		if ran {
			t.Fatal("denying elector ran the singleton; a non-leader must not run coordinated loops")
		}
		el.Shutdown(context.Background())
	})
}
