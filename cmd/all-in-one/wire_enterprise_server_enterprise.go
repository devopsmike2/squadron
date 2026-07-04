// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build enterprise

// wire_enterprise_server_enterprise.go is the enterprise-edition wiring for the
// RBAC management handler seam (ADR 0010 slice 2b). Built when the `enterprise`
// build tag IS set.
//
// This stub exists in the open core only as a placeholder so the build tag is
// documented and so a developer who checks out the open repo and tries
// `go build -tags enterprise` without the private repo present gets a clear
// error. The actual wiring — which installs the role/binding/permission
// management handler via server.SetEnterpriseRBACHandler — lives in the private
// enterprise repo and is dropped into this directory at build time.
//
// Build the full enterprise binary with:
//
//	make build-enterprise   # copies the real wire files, then builds -tags "enterprise compliance"
//
// See internal/api (SetEnterpriseRBACHandler), ADR 0010, and docs/build.md for
// the edition build model.

package main

import "github.com/devopsmike2/squadron/internal/api"

// enterpriseServerWiring is the enterprise-edition version. Symbol identical to
// the OSS file so main.go has a single call site. The real wiring is in the
// private repo; this open-core stub panics so an enterprise build assembled
// without the private wire file fails loudly instead of silently leaving the
// RBAC management routes unmounted.
func enterpriseServerWiring(*api.Server) {
	panic("squadron: built with -tags enterprise but the enterprise server wire file was not installed; see docs/build.md")
}
