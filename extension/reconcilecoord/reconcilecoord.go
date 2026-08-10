// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package reconcilecoord defines the boundary between the open-core single-
// instance runtime and the enterprise edition's high-availability (HA)
// reconcile-coordination (HA architecture, ADR 0035, slice S3c).
//
// THE PROBLEM. Under HA (S3a–S3d) the leader "decides and writes desired
// state" while whichever instance OWNS an agent's OpAMP socket "delivers" it:
// when the rollout engine (leader-only) writes a per-agent desired-state
// config assignment (S3d), the agent's owning instance picks it up on its next
// S3a reconcile tick (~30s) and pushes it. That tick is the DURABILITY
// backstop — it always eventually delivers — but it is also up to a full
// interval of latency. In a single-instance/SQLite deployment there is no
// latency: S3d retains a best-effort direct push on the writing instance, and
// that instance owns every socket, so the agent is served immediately.
//
// THE SEAM. This package lets the leader POKE the owning instance to reconcile
// a just-written agent NOW instead of waiting for its tick — a pure LATENCY
// optimization. OSS wires the no-op Coordinator: PokeReconcile does nothing,
// because single-instance delivery already happens immediately via S3d's
// retained direct push, so observable OSS behavior is IDENTICAL to before the
// seam existed. The enterprise edition (private squadron-enterprise repo,
// slice S3c-enterprise) wires a Coordinator that publishes the poke over
// Postgres LISTEN/NOTIFY, keyed on the S3b connection registry, so the poke
// reaches the instance that owns the agent's socket; that instance's LISTEN
// handler calls the S3a reconciler's ReconcileAgentNow to deliver at once.
//
// NOTIFY IS A HINT, NOT A CONTRACT. A dropped or missed poke costs latency,
// never correctness: the S3a reconcile tick still delivers the desired state
// and the S3d convergence gate remains the rollout's advancement authority.
// PokeReconcile is therefore fire-and-forget and MUST NOT block the caller or
// return an error.
//
// The boundary lives under extension/ (not internal/) so the private
// enterprise repo can import it across module boundaries, mirroring
// extension/leaderelection (S1), extension/detectors, extension/identity, and
// extension/tracebudget. See cmd/all-in-one/wire_reconcilecoord_oss.go
// (default, no-op) and wire_reconcilecoord_enterprise.go (build tag:
// enterprise, panic stub), plus docs/build.md.
package reconcilecoord

import (
	"context"

	"github.com/google/uuid"
)

// Coordinator is the reconcile-coordination seam. The rollout engine calls
// PokeReconcile after it writes an agent's desired state (S3d) to request that
// the agent's owning instance reconcile it immediately rather than on its next
// S3a reconcile tick.
//
// main.go always wires a Coordinator via the edition seam (NoOp in OSS); a nil
// Coordinator is not a valid runtime state, though the engine guards against
// one defensively so struct-literal tests need not wire it.
type Coordinator interface {
	// PokeReconcile hints that agentID's owning instance should reconcile it
	// now. It is a fire-and-forget LATENCY optimization: it MUST NOT block and
	// has no error return — a lost poke costs latency (the S3a reconcile tick
	// still delivers), never correctness. In OSS it is a no-op.
	PokeReconcile(ctx context.Context, agentID uuid.UUID)
}

// ReconcileFunc reconciles a single agent immediately on the instance that
// owns its OpAMP socket, no-op if the agent is not connected to this instance.
// It is satisfied by the S3a reconciler's ReconcileAgentNow method. The
// edition wire threads it into the enterprise Coordinator so its Postgres
// LISTEN handler can drive a reconcile on a received poke; the OSS wire
// ignores it (the no-op Coordinator never reconciles).
type ReconcileFunc func(ctx context.Context, agentID uuid.UUID)

// NoOp is the OSS default Coordinator: PokeReconcile does nothing. That is
// exactly right for a single-instance/SQLite deployment, where S3d's retained
// direct push already delivers desired state immediately on the one instance
// that owns every socket — there is no other instance to poke, and observable
// behavior is identical to running without the seam.
type NoOp struct{}

// NewNoOp returns the no-op Coordinator.
func NewNoOp() NoOp { return NoOp{} }

// PokeReconcile does nothing — OSS delivers immediately via the S3d retained
// direct push; there is no peer instance to notify.
func (NoOp) PokeReconcile(context.Context, uuid.UUID) {}

// Compile-time assertion that NoOp satisfies the Coordinator interface.
var _ Coordinator = NoOp{}
