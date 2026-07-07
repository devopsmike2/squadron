// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package tracebudget

import (
	"context"
	"errors"
	"testing"
)

func TestMapProvider_CapFor(t *testing.T) {
	p := NewMapProvider(map[string]int{"acme": 500_000, "beta": 50_000, "zero": 0, "neg": -5})
	cases := map[string]int{
		"acme":    500_000,
		"beta":    50_000,
		"zero":    0, // non-positive dropped → global default
		"neg":     0, // non-positive dropped
		"unknown": 0, // absent → global default
	}
	for tenant, want := range cases {
		if got := p.CapFor(tenant); got != want {
			t.Errorf("CapFor(%q) = %d, want %d", tenant, got, want)
		}
	}
}

func TestMapProvider_NilAndEmpty(t *testing.T) {
	var nilp *MapProvider
	if got := nilp.CapFor("acme"); got != 0 {
		t.Errorf("nil provider CapFor = %d, want 0", got)
	}
	empty := NewMapProvider(nil)
	if got := empty.CapFor("acme"); got != 0 {
		t.Errorf("empty provider CapFor = %d, want 0", got)
	}
}

// fakeBudgetStore is a test double for BudgetStore (ADR 0026).
type fakeBudgetStore struct {
	budgets map[string]int
	err     error
}

func (f *fakeBudgetStore) GetTraceBudget(_ context.Context, tenant string) (int, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	v, ok := f.budgets[tenant]
	return v, ok, nil
}

func (f *fakeBudgetStore) SeedTraceBudgets(context.Context, map[string]int) error { return nil }

func TestStoreProvider_CapFor(t *testing.T) {
	p := NewStoreProvider(&fakeBudgetStore{budgets: map[string]int{"acme": 500_000, "zero": 0}})
	cases := map[string]int{
		"acme":    500_000,
		"zero":    0, // stored non-positive → 0 (global default)
		"unknown": 0, // miss → 0
	}
	for tenant, want := range cases {
		if got := p.CapFor(tenant); got != want {
			t.Errorf("CapFor(%q) = %d, want %d", tenant, got, want)
		}
	}
}

func TestStoreProvider_ErrorYieldsZero(t *testing.T) {
	p := NewStoreProvider(&fakeBudgetStore{err: errors.New("boom")})
	if got := p.CapFor("acme"); got != 0 {
		t.Errorf("CapFor on store error = %d, want 0", got)
	}
}

func TestStoreProvider_NilAndNilStore(t *testing.T) {
	var nilp *StoreProvider
	if got := nilp.CapFor("acme"); got != 0 {
		t.Errorf("nil StoreProvider CapFor = %d, want 0", got)
	}
	p := NewStoreProvider(nil)
	if got := p.CapFor("acme"); got != 0 {
		t.Errorf("nil-store StoreProvider CapFor = %d, want 0", got)
	}
}
