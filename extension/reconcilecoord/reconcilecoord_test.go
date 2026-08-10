// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package reconcilecoord

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestNoOp_PokeReconcileDoesNothing pins the OSS default: PokeReconcile is inert
// — it neither blocks, panics, nor has any observable effect. That is what keeps
// single-instance behavior identical to running without the seam (S3d's retained
// direct push already delivers immediately on the one instance that owns every
// socket). The only assertion available is that the call returns cleanly.
func TestNoOp_PokeReconcileDoesNothing(t *testing.T) {
	c := NewNoOp()
	// Multiple calls, a nil context, and the zero id must all be safe no-ops.
	c.PokeReconcile(context.Background(), uuid.New())
	c.PokeReconcile(context.Background(), uuid.Nil)
	//nolint:staticcheck // SA1012: exercising the nil-context path is intentional here.
	c.PokeReconcile(nil, uuid.New())
}

// TestNoOp_SatisfiesCoordinator is a belt-and-suspenders check that NoOp is a
// valid Coordinator (the package already asserts this at compile time).
func TestNoOp_SatisfiesCoordinator(t *testing.T) {
	var _ Coordinator = NewNoOp()
}
