// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// wire_tracebudget_oss.go is the default open-core wiring for the per-tenant
// trace-index budget provider (ADR 0024). Built when the `enterprise` build tag
// is NOT set. It returns nil, so no per-tenant override is installed and every
// tenant keeps the global SQUADRON_TRACEINDEX_MAX_ROWS cap — the per-tenant LRU
// eviction still isolates tenants (a tenant can't evict another's rows), it just
// uses one uniform budget. The enterprise edition ships a parallel
// wire_tracebudget_enterprise.go returning a config-backed per-tenant provider.
// Both expose the same traceBudgetProvider symbol so main.go has one call site.

package main

import (
	"github.com/devopsmike2/squadron/extension/tracebudget"
	"github.com/devopsmike2/squadron/internal/config"
)

// traceBudgetProvider returns the edition's per-tenant trace-index budget
// provider. OSS returns nil (uniform global cap for every tenant).
func traceBudgetProvider(*config.Config) tracebudget.Provider {
	return nil
}
