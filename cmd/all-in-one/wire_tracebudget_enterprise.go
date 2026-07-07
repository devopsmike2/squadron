// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build enterprise

// wire_tracebudget_enterprise.go is the OPEN-CORE STUB for the per-tenant
// trace-index budget provider seam (ADR 0024). Built when the `enterprise` tag
// IS set. It exists so `go build -tags enterprise ./cmd/all-in-one` type-checks
// the seam on the pure open-core tree (matching wire_detectors_enterprise.go /
// wire_scopedstore_enterprise.go), but it PANICS at startup: a build assembled
// with the edition tag but WITHOUT the private squadron-enterprise wire files
// must fail loudly, not silently fall back to OSS behavior.
//
// The real enterprise build drops squadron-enterprise's
// wire_tracebudget_enterprise.go over this stub (see scripts/build-enterprise.sh
// PAIRS), replacing the panic with a config-backed per-tenant budget provider.
// The per-tenant LRU eviction correctness (a tenant can't evict another's rows)
// is OSS breadth and holds regardless; only differentiated per-tenant budgets are
// the enterprise wedge.

package main

import (
	"github.com/devopsmike2/squadron/extension/tracebudget"
	"github.com/devopsmike2/squadron/internal/config"
)

// traceBudgetProvider is the open-core stub. Same symbol as the OSS wire and the
// private enterprise wire so main.go has a single call site.
func traceBudgetProvider(*config.Config) tracebudget.Provider {
	panic("enterprise per-tenant trace budgets require the private squadron-enterprise wire files; see docs/build.md")
}
