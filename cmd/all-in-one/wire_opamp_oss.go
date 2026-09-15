// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// wire_opamp_oss.go is the default open-core wiring for the OpAMP control-channel
// enterprise seams (ADR 0052). Built when the `enterprise` build tag is NOT set.
//
// It is a no-op: OSS installs no identity-pin resolver, so an authenticated
// OpAMP connection keeps deriving its fleet id from the reported (spoofable)
// AgentDescription — the pre-0052 behavior, unchanged. The seam exists only so
// the enterprise edition can call opampServer.SetIdentityPinResolver (and
// register the scope-gated `pin:` mint guard) against a single main.go call
// site without any change to OSS behavior.
//
// The enterprise edition ships a parallel wire_opamp_enterprise.go (build tag:
// enterprise) that installs the real pin resolver + mint guard. Both files
// expose the same enterpriseOpAMPWiring symbol so main.go has one call site.
// Mirrors the no-op posture of enterpriseServerWiring() / identityProviders().

package main

import "github.com/devopsmike2/squadron/internal/opamp"

// enterpriseOpAMPWiring installs the edition's OpAMP control-channel seams
// (identity pinning + the pinned-token mint guard, ADR 0052). The OSS build does
// nothing — pinning stays inert and fleet identity is derived from the reported
// AgentDescription as before.
func enterpriseOpAMPWiring(server *opamp.Server) {}
