// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// wire_leaderelection_oss.go is the default open-core wiring for the leader-
// election seam (HA architecture, ADR ~0035, slice S1). Built when the
// `enterprise` build tag is NOT set.
//
// It returns the always-leader elector: this single instance is the leader
// for every named singleton, so every background loop (rollout engine,
// cost-spike detector, AI proposer bridge, alert evaluator, silent-agent
// watcher, discovery scan scheduler, deploy poller, usage reporter) runs
// unconditionally — observable single-instance/SQLite behavior is IDENTICAL
// to before the seam existed. There is no coordination in OSS.
//
// The enterprise edition ships a parallel wire_leaderelection_enterprise.go
// (build tag: enterprise) that returns a Postgres advisory-lock elector, so
// exactly one instance of a multi-instance deployment runs each singleton
// with failover. Both files expose the same leaderElector symbol so main.go
// has a single call site. See extension/leaderelection and docs/build.md.

package main

import (
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/leaderelection"
	"github.com/devopsmike2/squadron/internal/config"
)

// leaderElector returns the edition's leader elector. The OSS build returns
// the always-leader elector (every singleton runs on this instance). The
// cfg + logger params are accepted but unused by the OSS wire; they exist so
// the enterprise wire — which needs the Postgres DSN from config and a
// logger for its advisory-lock campaign — has a stable single call site in
// main.go. Mirrors traceBudgetProvider / identityProviders.
func leaderElector(*config.Config, *zap.Logger) leaderelection.Elector {
	return leaderelection.NewAlwaysLeader()
}
