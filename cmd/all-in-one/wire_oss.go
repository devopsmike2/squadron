// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !compliance

// wire_oss.go is the default open-core wiring. Built when the
// `compliance` build tag is NOT set. It plugs no-op providers into
// every Compliance Pack extension point so the OSS binary is fully
// functional but does not enforce regulated-industry policies.
//
// The Compliance Pack build (squadron-compliance private repo) ships
// a parallel wire_compliance.go file that replaces these no-ops with
// real implementations. Build with `go build -tags compliance` to
// pick that file up instead.
//
// Both files implement the same `wireExtensions` function so the rest
// of main.go calls it without caring which build is active. That's
// what makes the open/closed split clean.

package main

import (
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/changewindow"
	"github.com/devopsmike2/squadron/extension/policy"
	"github.com/devopsmike2/squadron/extension/siem"
	"github.com/devopsmike2/squadron/internal/rollouts"
	"github.com/devopsmike2/squadron/internal/services"
)

// wireExtensions installs Compliance Pack extension points on a
// freshly constructed rollout service. The OSS build wires no-op
// providers: groups can still carry require_approval=true as
// metadata, but the engine does not enforce it. Operators who need
// enforcement run the Compliance Pack build.
//
// Returns a short build-identifier string that main.go can log /
// expose on /metrics so operators always know which build they're
// running.
func wireExtensions(rolloutService services.RolloutService) string {
	if impl, ok := rolloutService.(*services.RolloutServiceImpl); ok {
		impl.SetGroupPolicyProvider(policy.NoOpProvider{})
	}
	return "squadron-oss"
}

// wireEngineExtensions installs engine-level Compliance Pack
// extension points after the rollout engine is constructed. The
// OSS build wires NoOpProvider for change-window enforcement so
// groups can carry windows as metadata but the engine never
// blocks. Compliance Pack build replaces this with a real
// store-backed provider.
//
// Separated from wireExtensions because the rollout engine is
// constructed late in main.go (after most other services), so the
// two extension wiring points fire at different moments.
func wireEngineExtensions(engine *rollouts.Engine) {
	engine.SetChangeWindowProvider(changewindow.NoOpProvider{})
}

// wireSiemDispatcher installs the SIEM fan-out dispatcher on the
// audit service. The OSS build wires NoOpDispatcher; events still
// persist locally in the audit_events table but never leave the
// box. Configured SIEM destinations are stored and visible in the
// UI for inventory purposes but are not actually delivered to.
//
// Operators who need centralized retention or signed external
// export run the Compliance Pack build, which wires a real
// dispatcher backed by Splunk HEC + HMAC-signed webhook posters.
//
// The siemSvc and logger arguments are accepted but unused by the
// OSS wire; they exist to keep the signature stable across both
// build editions so main.go has a single call site. The unused
// variables are silenced explicitly.
func wireSiemDispatcher(auditService services.AuditService, siemSvc services.SiemService, logger *zap.Logger) {
	_ = siemSvc
	if logger != nil {
		logger.Info("SIEM dispatcher: OSS build (destinations stored only, no external fan-out)")
	}
	if impl, ok := auditService.(*services.AuditServiceImpl); ok {
		impl.SetSiemDispatcher(siem.NoOpDispatcher{})
	}
}
