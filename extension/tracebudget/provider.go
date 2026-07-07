// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package tracebudget is the open-core seam for per-tenant trace-index LRU
// budgets (ADR 0024). The trace-resource index (trace_resource_seen) evicts
// oldest rows per tenant; a Provider resolves each tenant's row budget. OSS
// wires no provider (nil), so every tenant gets the global
// SQUADRON_TRACEINDEX_MAX_ROWS cap — behavior identical to the pre-per-tenant
// global LRU, just no longer able to evict across tenants. The enterprise
// edition wires a Provider that returns differentiated per-tenant budgets
// (plan-tier quotas). Mirrors the extension/detectors + extension/changewindow
// seam pattern.
package tracebudget

import "context"

// Provider resolves a per-tenant row budget for the trace-resource index. A
// return <= 0 means "no per-tenant override — use the global cap".
type Provider interface {
	CapFor(tenant string) int
}

// MapProvider is a config-backed Provider: an explicit per-tenant budget map.
// Tenants absent from the map (or with a non-positive budget) return 0 → the
// global default cap. Set once at construction and never mutated, so concurrent
// CapFor reads are safe.
type MapProvider struct {
	budgets map[string]int
}

// NewMapProvider builds a MapProvider from a per-tenant budget map, dropping
// non-positive entries. A nil/empty map yields a provider whose CapFor always
// returns 0 (global default for every tenant).
func NewMapProvider(budgets map[string]int) *MapProvider {
	m := make(map[string]int, len(budgets))
	for k, v := range budgets {
		if v > 0 {
			m[k] = v
		}
	}
	return &MapProvider{budgets: m}
}

// CapFor returns the tenant's configured budget, or 0 when unset (→ global cap).
func (p *MapProvider) CapFor(tenant string) int {
	if p == nil {
		return 0
	}
	return p.budgets[tenant]
}

// BudgetStore is the minimal store surface the DB-backed StoreProvider reads
// (ADR 0026). *sqlite.Storage satisfies it. SeedTraceBudgets is here for the
// enterprise wire (which seeds config budgets into the runtime table at boot);
// the StoreProvider itself only reads via GetTraceBudget.
type BudgetStore interface {
	GetTraceBudget(ctx context.Context, tenant string) (int, bool, error)
	SeedTraceBudgets(ctx context.Context, budgets map[string]int) error
}

// StoreProvider is a DB-backed Provider (ADR 0026): CapFor reads the persisted
// per-tenant budget on each call, so an admin edit takes effect on the next
// flush (~30s). No cache — the flush cadence makes a straight read cheap and
// avoids staleness. A read error or missing/non-positive row yields 0 → the
// caller falls back to the global cap (same contract as MapProvider).
type StoreProvider struct {
	store BudgetStore
}

// NewStoreProvider builds a StoreProvider over the given BudgetStore.
func NewStoreProvider(store BudgetStore) *StoreProvider {
	return &StoreProvider{store: store}
}

// CapFor returns the tenant's persisted budget, or 0 when unset/on error.
func (p *StoreProvider) CapFor(tenant string) int {
	if p == nil || p.store == nil {
		return 0
	}
	if maxRows, ok, err := p.store.GetTraceBudget(context.Background(), tenant); err == nil && ok && maxRows > 0 {
		return maxRows
	}
	return 0
}
