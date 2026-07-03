// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// wire_identity_oss.go is the default open-core wiring for the identity
// seam (SSO/RBAC/multi-tenancy boundary). Built when the `enterprise`
// build tag is NOT set.
//
// It returns the OSS default providers: a bearer authenticator, the
// flat-scope authorizer whose allow/deny is byte-identical to
// middleware.RequireScope, and a single-tenant resolver. Observable OSS
// behavior is unchanged — the seam exists so the enterprise edition can
// drop in real SSO/RBAC/multi-tenant providers against the same
// interfaces.
//
// The enterprise edition ships a parallel wire_identity_enterprise.go
// (build tag: enterprise) that returns its own providers. Both files
// expose the same identityProviders symbol so main.go has a single call
// site. See extension/identity, ADR 0006, and docs/build.md.

package main

import "github.com/devopsmike2/squadron/extension/identity"

// identityProviders returns the edition's identity providers. The OSS
// build returns the open-core defaults (bearer auth, flat-scope
// authorizer, single tenant). Mirrors commercialDetectorProvider() and the
// wire_oss.go no-op posture.
func identityProviders() identity.Providers {
	return identity.OSSProviders()
}
