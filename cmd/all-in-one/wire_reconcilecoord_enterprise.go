// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build enterprise

// wire_reconcilecoord_enterprise.go is the OPEN-CORE STUB for the reconcile-
// coordination seam (HA architecture, ADR 0035, slice S3c). Built when the
// `enterprise` build tag IS set. It exists so `go build -tags enterprise
// ./cmd/all-in-one` type-checks the seam on the pure open-core tree (matching
// wire_leaderelection_enterprise.go / wire_detectors_enterprise.go), but it
// PANICS at startup: a build assembled with the edition tag but WITHOUT the
// private squadron-enterprise wire files must fail loudly, not silently fall
// back to the no-op coordinator. Falling back would be a correctness-safe but
// silent LOSS of the NOTIFY latency optimization — an enterprise build must
// get the real coordinator, so a missing private wire fails the build's
// startup rather than degrading quietly.
//
// The real enterprise build drops squadron-enterprise's
// wire_reconcilecoord_enterprise.go over this stub, replacing the panic with a
// Postgres LISTEN/NOTIFY coordinator (slice S3c-enterprise): PokeReconcile
// NOTIFYs on a channel keyed by the S3b connection registry so the poke reaches
// the instance owning the agent's socket, whose LISTEN handler invokes
// reconcileNow (the S3a reconciler's ReconcileAgentNow) to deliver immediately.
// See docs/build.md for the edition build model.

package main

import (
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/reconcilecoord"
	"github.com/devopsmike2/squadron/internal/config"
)

// reconcileCoordinator is the open-core stub. Same symbol as the OSS wire and
// the private enterprise wire so main.go has a single call site. The real
// coordinator reads the backend DSN from cfg, uses logger for its LISTEN/NOTIFY
// pump, and calls reconcileNow when a poke arrives for an agent this instance
// owns; this stub panics so an enterprise build assembled without the private
// wire file fails loudly instead of silently dropping the optimization.
func reconcileCoordinator(*config.Config, *zap.Logger, reconcilecoord.ReconcileFunc) reconcilecoord.Coordinator {
	panic("enterprise HA reconcile coordination requires the private squadron-enterprise wire files; see docs/build.md")
}
