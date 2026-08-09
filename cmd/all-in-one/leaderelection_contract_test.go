// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// leaderelection_contract_test.go pins the open-core edition's leader-
// election wiring contract (HA architecture, ADR ~0035, slice S1). It
// asserts that the default (OSS) build wires the always-leader elector, so
// every background singleton runs on this single instance — identical to
// pre-HA behavior. The enterprise edition overrides this via its own
// build-tagged wire file (a Postgres advisory-lock elector); keeping the OSS
// default pinned here means a regression fails a test instead of silently
// changing single-instance behavior. Mirrors editions_contract_test.go.

package main

import (
	"testing"

	"github.com/devopsmike2/squadron/extension/leaderelection"
)

// TestOSSEdition_LeaderElectorIsAlwaysLeader confirms the OSS build wires the
// always-leader elector. That is what keeps single-instance/SQLite behavior
// unchanged: AlwaysLeader runs every registered singleton unconditionally,
// with no coordination.
func TestOSSEdition_LeaderElectorIsAlwaysLeader(t *testing.T) {
	el := leaderElector(nil, nil)
	if el == nil {
		t.Fatal("leaderElector returned nil; the OSS build must wire an elector")
	}
	if _, ok := el.(*leaderelection.AlwaysLeader); !ok {
		t.Errorf("leaderElector() = %T, want *leaderelection.AlwaysLeader", el)
	}
}
