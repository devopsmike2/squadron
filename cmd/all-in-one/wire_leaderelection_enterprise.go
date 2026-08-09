// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build enterprise

// wire_leaderelection_enterprise.go is the OPEN-CORE STUB for the leader-
// election seam (HA architecture, ADR ~0035, slice S1). Built when the
// `enterprise` build tag IS set. It exists so `go build -tags enterprise
// ./cmd/all-in-one` type-checks the seam on the pure open-core tree
// (matching wire_detectors_enterprise.go / wire_tracebudget_enterprise.go /
// wire_identity_enterprise.go), but it PANICS at startup: a build assembled
// with the edition tag but WITHOUT the private squadron-enterprise wire
// files must fail loudly, not silently fall back to always-leader behavior —
// running the coordinated singletons on every instance would double-advance
// rollouts and race the deploy poller across the fleet.
//
// The real enterprise build drops squadron-enterprise's
// wire_leaderelection_enterprise.go over this stub, replacing the panic with
// a Postgres advisory-lock elector (slice S2) that campaigns per singleton
// name so exactly one instance runs each loop, with failover. See
// docs/build.md for the edition build model.

package main

import (
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/leaderelection"
	"github.com/devopsmike2/squadron/internal/config"
)

// leaderElector is the open-core stub. Same symbol as the OSS wire and the
// private enterprise wire so main.go has a single call site. The real
// Postgres advisory-lock elector reads the backend DSN from cfg and uses
// logger for its campaign; this stub panics so an enterprise build assembled
// without the private wire file fails loudly instead of running every
// singleton on every instance.
func leaderElector(*config.Config, *zap.Logger) leaderelection.Elector {
	panic("enterprise HA leader election requires the private squadron-enterprise wire files; see docs/build.md")
}
