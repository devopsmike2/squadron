// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build enterprise

// wire_scopedstore_enterprise.go is the enterprise-edition wiring for the
// tenant-scoping store seam (ADR 0006 slice 4). Built when the `enterprise`
// build tag IS set.
//
// This stub exists in the open core only as a placeholder so the build tag is
// documented and so a developer who checks out the open repo and tries
// `go build -tags enterprise` without the private repo present gets a clear
// error. The actual ScopedStore — which scopes reads/writes by the tenant on
// the request context (identity.TenantFromContext, stamped by
// middleware.ResolveTenant) and forwards the optional store interfaces main.go
// type-asserts — lives in the private enterprise repo and is dropped into this
// directory at build time.
//
// Build the full enterprise binary with:
//
//	make build-enterprise   # copies the real wire files, then builds -tags "enterprise compliance"
//
// See ADR 0006 and docs/build.md for the edition build model.

package main

import "github.com/devopsmike2/squadron/internal/storage/applicationstore"

// scopedApplicationStore is the enterprise-edition version. Symbol identical to
// the OSS file so main.go has a single call site. The real ScopedStore is in
// the private repo; this open-core stub panics so an enterprise build assembled
// without the private wire file fails loudly instead of silently running
// unscoped.
func scopedApplicationStore(applicationstore.ApplicationStore) applicationstore.ApplicationStore {
	panic("squadron: built with -tags enterprise but the enterprise scoped-store wire file was not installed; see docs/build.md")
}
