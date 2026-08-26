// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package strict is the ADR 0043 registry of application-store "strict tenant
// scoping" togglers. Each store backend (sqlite, postgres) registers its
// SetStrictTenantScoping function here in its init(); the enterprise strict
// wire flips them all on with a single EnableAll() call instead of naming each
// backend by hand.
//
// This closes the exact hole ADR 0043 was written for: the pre-0043 enterprise
// wire armed strict scoping on SQLite ONLY (`sqlite.SetStrictTenantScoping(true)`),
// leaving the Postgres backend — the one enterprise HA actually runs on — a dead,
// un-armed seam. With the registry, adding a new backend that forgets to register
// its toggler fails the contract test (strict_contract_test.go), so a backend can
// never again ship silently un-armable.
//
// The dependency direction is one-way: backends import THIS package and call
// Register in init(); this package imports NO backend, so there is no cycle. The
// contract test lives in the external test package (strict_test) and blank-imports
// the backends to trigger their registration, then asserts every expected backend
// is present.
package strict

import (
	"sort"
	"sync"
)

// toggler is a backend's SetStrictTenantScoping function.
type toggler func(bool)

var (
	mu       sync.Mutex
	registry = map[string]toggler{}
)

// Register records a backend's strict-scoping toggler under a stable name
// (e.g. "sqlite", "postgres"). Called from each backend package's init(). A
// duplicate name overwrites — backends register exactly once, so this is only
// hit if two init()s collide, which the contract test would surface.
func Register(name string, fn toggler) {
	if fn == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	registry[name] = fn
}

// EnableAll flips strict tenant scoping ON for every registered backend. The
// enterprise strict wire calls this exactly once at startup (replacing the
// pre-0043 sqlite-only call), so whichever backend the deployment is configured
// for is armed — fail-closed on an unstamped context. Idempotent.
func EnableAll() {
	setAll(true)
}

// DisableAll flips strict scoping OFF for every registered backend. Intended for
// test cleanup so a strict-mode test can't leak into siblings.
func DisableAll() {
	setAll(false)
}

func setAll(v bool) {
	mu.Lock()
	fns := make([]toggler, 0, len(registry))
	for _, fn := range registry {
		fns = append(fns, fn)
	}
	mu.Unlock()
	for _, fn := range fns {
		fn(v)
	}
}

// Registered returns the sorted names of every registered backend. The contract
// test asserts this contains every store backend so a new, un-armed backend
// fails CI by default.
func Registered() []string {
	mu.Lock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	mu.Unlock()
	sort.Strings(names)
	return names
}
