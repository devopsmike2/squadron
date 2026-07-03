// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// persistErrSvc satisfies services.RolloutService by embedding the interface;
// only Persist is given a body so we can inject the error the engine's persist
// helper must classify.
type persistErrSvc struct {
	services.RolloutService
	err error
}

func (f *persistErrSvc) Persist(context.Context, *services.Rollout) error { return f.err }

// TestEngine_persist_ClassifiesVersionConflict pins the engine-side half of the
// optimistic-concurrency guard: a version conflict from the store (a concurrent
// operator Pause/Abort landing after this tick's snapshot) is returned unchanged
// so the caller's `if err != nil { return }` skips this tick's work — the
// operator's write stands and the next tick re-reconciles. A generic persist
// error propagates the same way; a success returns nil. (The conflict-vs-generic
// distinction only changes the log level, which the helper handles internally;
// here we pin the returned-error contract the 8 call sites depend on.)
func TestEngine_persist_ClassifiesVersionConflict(t *testing.T) {
	ctx := context.Background()
	r := &services.Rollout{ID: "ro-1"}

	// Version conflict → returned unchanged, no panic.
	e := &Engine{logger: zap.NewNop(), rolloutService: &persistErrSvc{err: applicationstore.ErrRolloutVersionConflict}}
	require.ErrorIs(t, e.persist(ctx, r, "advance"), applicationstore.ErrRolloutVersionConflict)

	// Any other persist error propagates unchanged.
	boom := errors.New("boom")
	e2 := &Engine{logger: zap.NewNop(), rolloutService: &persistErrSvc{err: boom}}
	require.ErrorIs(t, e2.persist(ctx, r, "advance"), boom)

	// Success returns nil.
	e3 := &Engine{logger: zap.NewNop(), rolloutService: &persistErrSvc{err: nil}}
	require.NoError(t, e3.persist(ctx, r, "advance"))
}

// reconcileSvc drives persistWithReconcile: Persist consumes persistErrs in
// order (nil once exhausted) and records every successfully-persisted rollout;
// Get returns a caller-controlled fresh row (or error) simulating the concurrent
// writer's committed state. Embeds the interface so the other methods compile.
type reconcileSvc struct {
	services.RolloutService
	persistErrs  []error
	persistCalls int
	persisted    []services.Rollout
	getRollout   *services.Rollout
	getErr       error
}

func (f *reconcileSvc) Persist(_ context.Context, r *services.Rollout) error {
	f.persistCalls++
	var err error
	if len(f.persistErrs) > 0 {
		err, f.persistErrs = f.persistErrs[0], f.persistErrs[1:]
	}
	if err == nil {
		f.persisted = append(f.persisted, *r)
	}
	return err
}

func (f *reconcileSvc) Get(_ context.Context, _ string) (*services.Rollout, error) {
	return f.getRollout, f.getErr
}

// countingFactory is a metrics.Factory whose counters accumulate by metric name,
// so a test can assert the exact reconcile-path telemetry (conflicts, retried,
// yielded) the engine emits. Only Counter is exercised here; the other kinds
// fall back to no-ops.
type countingFactory struct {
	counts map[string]int64
}

func newCountingFactory() *countingFactory { return &countingFactory{counts: map[string]int64{}} }

type countingCounter struct {
	f    *countingFactory
	name string
}

func (c *countingCounter) Inc(v int64) { c.f.counts[c.name] += v }

func (f *countingFactory) Counter(o metrics.Options) metrics.Counter {
	return &countingCounter{f: f, name: o.Name}
}
func (f *countingFactory) Gauge(metrics.Options) metrics.Gauge {
	return metrics.NullFactory.Gauge(metrics.Options{})
}
func (f *countingFactory) Timer(metrics.TimerOptions) metrics.Timer {
	return metrics.NullFactory.Timer(metrics.TimerOptions{})
}
func (f *countingFactory) Histogram(metrics.HistogramOptions) metrics.Histogram {
	return metrics.NullFactory.Histogram(metrics.HistogramOptions{})
}

// TestEngine_persistWithReconcile_RetriesWhenTransitionStillHolds pins the retry
// path: the first Persist conflicts (an operator write landed after the tick's
// snapshot), the engine reloads the fresh row, reapply confirms its transition
// still applies and re-projects it, and the second Persist converges the store
// THIS tick. The reconciled state must be copied back onto r (so the caller's
// follow-up audit/publish see it), a retry counted, and no error returned.
func TestEngine_persistWithReconcile_RetriesWhenTransitionStillHolds(t *testing.T) {
	ctx := context.Background()
	// Fresh row still Pending → the engine's start() transition still holds.
	fresh := &services.Rollout{ID: "ro-1", State: services.RolloutStatePending, Version: 7}
	svc := &reconcileSvc{
		persistErrs: []error{applicationstore.ErrRolloutVersionConflict, nil},
		getRollout:  fresh,
	}
	factory := newCountingFactory()
	e := &Engine{logger: zap.NewNop(), rolloutService: svc, metrics: metrics.NewRolloutMetrics(factory)}

	r := &services.Rollout{ID: "ro-1", State: services.RolloutStateInProgress}
	reapplied := false
	err := e.persistWithReconcile(ctx, r, "start", func(f *services.Rollout) bool {
		reapplied = true
		if f.State != services.RolloutStatePending {
			return false
		}
		f.State = services.RolloutStateInProgress
		return true
	})

	require.NoError(t, err)
	require.True(t, reapplied, "reapply must be consulted on conflict")
	require.Equal(t, 2, svc.persistCalls, "original persist + one bounded retry")
	require.Equal(t, services.RolloutStateInProgress, r.State, "reconciled state copied back onto r")
	require.Equal(t, 7, r.Version, "r reflects the reloaded fresh row's version")
	require.Equal(t, int64(1), factory.counts["rollout_engine_version_conflicts_total"])
	require.Equal(t, int64(1), factory.counts["rollout_engine_version_conflicts_retried_total"])
	require.Equal(t, int64(0), factory.counts["rollout_engine_version_conflicts_yielded_total"])
}

// TestEngine_persistWithReconcile_YieldsWhenSuperseded pins the clean-yield path:
// the reloaded row shows the operator's write (Paused) no longer permits the
// engine's transition, so reapply returns false. The engine must NOT re-persist
// (the operator's intent stands), must return nil (an expected win, not an
// error), count a yield, and emit the rollout.update_superseded audit event so
// the operator can see on the timeline that their change took precedence.
func TestEngine_persistWithReconcile_YieldsWhenSuperseded(t *testing.T) {
	ctx := context.Background()
	fresh := &services.Rollout{ID: "ro-1", Name: "n", State: services.RolloutStatePaused}
	svc := &reconcileSvc{
		persistErrs: []error{applicationstore.ErrRolloutVersionConflict},
		getRollout:  fresh,
	}
	factory := newCountingFactory()
	audit := &recordingAuditService{}
	e := &Engine{logger: zap.NewNop(), rolloutService: svc, auditService: audit, metrics: metrics.NewRolloutMetrics(factory)}

	r := &services.Rollout{ID: "ro-1", Name: "n", State: services.RolloutStateInProgress}
	err := e.persistWithReconcile(ctx, r, "advance", func(f *services.Rollout) bool {
		return f.State == services.RolloutStatePending // false → operator superseded us
	})

	require.NoError(t, err, "a clean yield to the operator is not an error")
	require.Equal(t, 1, svc.persistCalls, "no retry persist when the transition is moot")
	require.Equal(t, int64(1), factory.counts["rollout_engine_version_conflicts_total"])
	require.Equal(t, int64(1), factory.counts["rollout_engine_version_conflicts_yielded_total"])
	require.Equal(t, int64(0), factory.counts["rollout_engine_version_conflicts_retried_total"])
	require.Equal(t, []string{"rollout.update_superseded"}, audit.eventTypes(),
		"yield must emit the operator-facing supersede audit event")
	require.Equal(t, "advance", audit.events[0].Payload["engine_action"])
}

// TestEngine_persistWithReconcile_ReloadFailureYields pins the bounded-failure
// path: if the post-conflict reload itself fails, the engine cannot safely
// reconcile, so it yields (audit + metric) and returns the ORIGINAL conflict
// error unchanged — the caller's `if err != nil { return }` skips the tick and
// the next active tick re-reconciles. It must never loop or re-persist.
func TestEngine_persistWithReconcile_ReloadFailureYields(t *testing.T) {
	ctx := context.Background()
	svc := &reconcileSvc{
		persistErrs: []error{applicationstore.ErrRolloutVersionConflict},
		getErr:      errors.New("db unavailable"),
	}
	factory := newCountingFactory()
	audit := &recordingAuditService{}
	e := &Engine{logger: zap.NewNop(), rolloutService: svc, auditService: audit, metrics: metrics.NewRolloutMetrics(factory)}

	r := &services.Rollout{ID: "ro-1"}
	reapplied := false
	err := e.persistWithReconcile(ctx, r, "rollback", func(*services.Rollout) bool {
		reapplied = true
		return true
	})

	require.ErrorIs(t, err, applicationstore.ErrRolloutVersionConflict, "original conflict is returned unchanged")
	require.False(t, reapplied, "reapply is never consulted when the reload fails")
	require.Equal(t, 1, svc.persistCalls, "no retry persist after a failed reload")
	require.Equal(t, int64(1), factory.counts["rollout_engine_version_conflicts_yielded_total"])
	require.Equal(t, []string{"rollout.update_superseded"}, audit.eventTypes())
}
