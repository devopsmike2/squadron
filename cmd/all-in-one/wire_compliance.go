// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build compliance

// wire_compliance.go is the Compliance Pack wiring. Built when the
// `compliance` build tag IS set. This file is NOT shipped in the
// OSS distribution; it lives in the squadron-compliance private repo
// and is dropped into the cmd/all-in-one directory at build time
// via a Make target.
//
// This stub exists in the open core only as a placeholder so the
// build tag is documented and so a developer who checks out the
// open repo and tries `go build -tags compliance` without the
// private repo present gets a clear error. The actual implementation
// lives in the Compliance Pack and wires:
//
//   - compliance/policy.SQLiteGroupPolicy reading require_approval
//     from the group row to enforce per-group approval policy
//   - compliance/changewindow.SQLiteProvider reading blackout
//     windows from the group row to gate rollout advancement
//   - compliance/siem.DispatcherAdapter for signed SIEM export with
//     HMAC-SHA256 / Splunk HEC authorization
//
// Build the full Compliance binary with:
//
//	make build-enterprise   # in the squadron-compliance repo
//
// which copies the real wire_compliance.go into this directory and
// runs `go build -tags compliance`.

package main

import (
	"github.com/devopsmike2/squadron/internal/rollouts"
	"github.com/devopsmike2/squadron/internal/services"
)

// wireExtensions is the Compliance Pack version. Symbol identical
// to the OSS file so main.go has a single call site. The real
// implementation is in the private repo.
func wireExtensions(_ services.RolloutService) string {
	panic("squadron: built with -tags compliance but the Compliance Pack wire file was not installed; see docs/build.md")
}

// wireEngineExtensions is the Compliance Pack version. Symbol
// identical to the OSS file. The real implementation in the private
// repo wires a store-backed change-window provider here.
func wireEngineExtensions(_ *rollouts.Engine) {
	panic("squadron: built with -tags compliance but the Compliance Pack wire file was not installed; see docs/build.md")
}
