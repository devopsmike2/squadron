// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// wire_reconcilecoord_oss.go is the default open-core wiring for the reconcile-
// coordination seam (HA architecture, ADR 0035, slice S3c). Built when the
// `enterprise` build tag is NOT set.
//
// It returns the no-op Coordinator: PokeReconcile does nothing. In a single-
// instance/SQLite deployment the S3d desired-state write retains a best-effort
// direct push on the writing instance, which owns every agent socket, so the
// agent is served immediately — there is no peer instance to poke and no
// latency to shave. Observable single-instance behavior is IDENTICAL to before
// the seam existed.
//
// The enterprise edition ships a parallel wire_reconcilecoord_enterprise.go
// (build tag: enterprise) that returns a Postgres LISTEN/NOTIFY coordinator
// keyed on the S3b connection registry, so the leader's poke reaches the
// instance owning an agent's socket and that instance reconciles it at once via
// the reconcileNow handle. Both files expose the same reconcileCoordinator
// symbol so main.go has a single call site. See extension/reconcilecoord and
// docs/build.md.

package main

import (
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/reconcilecoord"
	"github.com/devopsmike2/squadron/internal/config"
)

// reconcileCoordinator returns the edition's reconcile coordinator. The OSS
// build returns the no-op coordinator (PokeReconcile does nothing). The cfg,
// logger, and reconcileNow params are accepted but unused by the OSS wire; they
// exist so the enterprise wire — which needs the Postgres DSN from config, a
// logger, and a handle to drive an immediate reconcile on a received NOTIFY —
// has a stable single call site in main.go. Mirrors leaderElector.
func reconcileCoordinator(*config.Config, *zap.Logger, reconcilecoord.ReconcileFunc) reconcilecoord.Coordinator {
	return reconcilecoord.NewNoOp()
}
