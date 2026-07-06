// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package tracebudget

import "testing"

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
