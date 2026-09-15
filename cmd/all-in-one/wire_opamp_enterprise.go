// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build enterprise

// wire_opamp_enterprise.go is the enterprise-edition wiring for the OpAMP
// control-channel seams (ADR 0052 identity pinning + the pinned-token mint
// guard). Built when the `enterprise` build tag IS set.
//
// This stub exists in the open core only as a placeholder so the build tag is
// documented and so a developer who checks out the open repo and tries
// `go build -tags enterprise` without the private repo present gets a clear
// error rather than an "undefined: enterpriseOpAMPWiring" link failure. The
// actual wiring — which installs the identity-pin resolver via
// server.SetIdentityPinResolver and registers the `pin:` -> agents:write mint
// guard via services.SetScopeGatedTokenLabelPrefixes — lives in the private
// enterprise repo and is dropped into this directory at build time.
//
// Build the full enterprise binary with:
//
//	make build-enterprise   # copies the real wire files, then builds -tags "enterprise compliance"
//
// See internal/opamp (SetIdentityPinResolver), ADR 0052, and docs/build.md for
// the edition build model.

package main

import "github.com/devopsmike2/squadron/internal/opamp"

// enterpriseOpAMPWiring is the enterprise-edition version. Symbol identical to
// the OSS file (wire_opamp_oss.go) so main.go has a single call site. The real
// wiring is in the private repo; this open-core stub panics so an enterprise
// build assembled WITHOUT the private squadron-enterprise wire files fails
// loudly instead of silently shipping without identity pinning.
func enterpriseOpAMPWiring(server *opamp.Server) {
	panic("enterprise OpAMP identity pinning requires the private squadron-enterprise wire files; see docs/build.md")
}
