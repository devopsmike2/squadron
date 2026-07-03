// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

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
