// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// wire_scopedstore_oss.go is the default open-core wiring for the
// tenant-scoping store seam (ADR 0006 slice 4). Built when the `enterprise`
// build tag is NOT set.
//
// It returns the application store UNCHANGED — an identity pass-through. OSS
// runs under the single implicit identity.DefaultTenant, so there is nothing
// to scope: no query rewrite, no tenant_id column, no migration, and zero
// overhead. The seam exists only so the enterprise edition can wrap the store
// with a real per-tenant ScopedStore (which reads
// identity.TenantFromContext(ctx), stamped by middleware.ResolveTenant in
// slice 3) without any change to main.go.
//
// IMPORTANT for the enterprise wrapper: main.go type-asserts several OPTIONAL
// store interfaces on the value returned here (handlers.CostSpikeStore, the
// retention-GC interfaces, the cold-start / error-rate observation readers,
// handlers.CommercialObservationStore, etc.). A decorator that embeds only
// types.ApplicationStore would fail those assertions and silently disable
// those features. The enterprise ScopedStore must therefore forward those
// optional interfaces (or perform tenant scoping at the query layer instead of
// wrapping the store). See ADR 0006 and docs/build.md.
//
// The enterprise edition ships a parallel wire_scopedstore_enterprise.go
// (build tag: enterprise). Both files expose the same scopedApplicationStore
// symbol so main.go has a single call site.

package main

import "github.com/devopsmike2/squadron/internal/storage/applicationstore"

// scopedApplicationStore returns the edition's tenant-scoped application store.
// The OSS build returns it unchanged (single implicit tenant). Mirrors the
// no-op posture of commercialDetectorProvider() and the identity seam.
func scopedApplicationStore(s applicationstore.ApplicationStore) applicationstore.ApplicationStore {
	return s
}
