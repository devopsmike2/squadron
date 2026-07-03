// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build enterprise

// wire_identity_enterprise.go is the enterprise-edition wiring for the
// identity seam. Built when the `enterprise` build tag IS set.
//
// This stub exists in the open core only as a placeholder so the build tag
// is documented and so a developer who checks out the open repo and tries
// `go build -tags enterprise` without the private repo present gets a clear
// error. The actual providers — SSO/SAML/OIDC + SCIM authenticator, the
// role-based (deny-by-default, resource-aware) authorizer, and the
// multi-tenant resolver — live in the private enterprise repo and are
// dropped into this directory at build time.
//
// Build the full enterprise binary with:
//
//	make build-enterprise   # copies the real wire files, then builds -tags "enterprise compliance"
//
// See extension/identity, ADR 0006, and docs/build.md for the edition
// build model.

package main

import "github.com/devopsmike2/squadron/extension/identity"

// identityProviders is the enterprise-edition version. Symbol identical to
// the OSS file so main.go has a single call site. The real providers are in
// the private repo; this open-core stub panics so an enterprise build
// assembled without the private wire file fails loudly instead of silently
// falling back to OSS behavior.
func identityProviders() identity.Providers {
	panic("squadron: built with -tags enterprise but the enterprise identity wire file was not installed; see docs/build.md")
}
