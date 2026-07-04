// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package siem

import (
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// TestWorker_ConfigHotSwapVsDeliver_NoRace pins the fix for the dispatcher
// data race: reload() hot-swaps a worker's destination+exporter while the
// worker goroutine is reading them in deliver(). reload's swap is exactly
// `worker.cfg.Store(&workerConfig{...})` (see dispatcher.go reload), and
// deliver loads the config to pick the exporter and destination name.
//
// The swapper goroutine below mimics reload's atomic Store; the worker's own
// run()->deliver() loop loads the config for each event. Run under
// `go test -race`: before the fix (raw worker.dest/worker.exporter fields
// written under d.mu but read lock-free in deliver) this reports a data race;
// with the atomic.Pointer[workerConfig] swap it is clean.
func TestWorker_ConfigHotSwapVsDeliver_NoRace(t *testing.T) {
	w := newWorker(&Destination{ID: "d", Name: "dest-0"}, &fakeExporter{}, zap.NewNop())
	go w.run()
	defer w.stop()

	const iters = 500
	var wg sync.WaitGroup

	// Swapper: what reload does when an operator edits a destination's
	// URL/secret — atomically replace the whole {dest, exporter} config.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			w.cfg.Store(&workerConfig{
				dest:     &Destination{ID: "d", Name: fmt.Sprintf("dest-%d", i)},
				exporter: &fakeExporter{},
			})
		}
	}()

	// Feeder: drives deliver() (which loads w.cfg) on the worker goroutine.
	// The buffered queue (1024) comfortably absorbs iters with run() draining.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			w.in <- Event{ID: fmt.Sprintf("evt-%d", i), EventType: "rollout.approved"}
		}
	}()

	wg.Wait()
}
