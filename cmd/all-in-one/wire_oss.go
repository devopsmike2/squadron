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
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/extension/policy"
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
