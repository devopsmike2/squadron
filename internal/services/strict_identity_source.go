// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import "sync"

// strict_identity_source.go — the ADR 0014 Arc C (slice 4a) inert toggle for
// strict identity-source enforcement. In the OSS build this is INERT: nothing
// calls SetStrictIdentitySource, so strictIdentitySource stays false and the
// bearer/scope auth path is byte-identical to pre-toggle behavior (a raw
// operator token authenticates exactly as before). The enterprise wire flips it
// on in enableEnterpriseStrictTenanting() alongside the three existing strict
// toggles (sqlite.SetStrictTenantScoping, opamp.SetRejectUntenantedConnections,
// services.SetReservedTokenLabels).
//
// When enabled (enterprise, slice 4d), RequireBearer / RequireScope will reject
// a Principal whose credential lacks a validated identity source — i.e. a raw
// bearer token that was never linked to an OIDC-minted or SCIM-provisioned
// identity (label prefix oidc:/scim: per ADR 0014 D2/D3). Slice 4a defines ONLY
// the inert seam (this var + setter + getter); the enforcement read in the
// middleware is wired in slice 4d. OSS default inert.

var (
	strictIdentitySourceMu sync.RWMutex
	strictIdentitySource   bool
)

// SetStrictIdentitySource toggles strict identity-source enforcement. The
// enterprise wire sets it true at startup (mirroring sqlite.SetStrictTenantScoping
// and opamp.SetRejectUntenantedConnections); OSS never calls it, leaving it
// false so the auth path is unchanged. Not safe for concurrent use with
// in-flight requests — call it during startup, before serving traffic.
//
// Slice 4a note: this is the inert seam only. The middleware enforcement that
// reads StrictIdentitySource (reject a Principal whose credential lacks a
// validated OIDC/SCIM identity source) is wired in slice 4d.
func SetStrictIdentitySource(v bool) {
	strictIdentitySourceMu.Lock()
	defer strictIdentitySourceMu.Unlock()
	strictIdentitySource = v
}

// StrictIdentitySource reports whether strict identity-source enforcement is
// enabled. Returns false in the OSS build (the toggle is never flipped), so any
// eventual identity-source check (slice 4d) is inert unless the enterprise wire
// enabled it.
func StrictIdentitySource() bool {
	strictIdentitySourceMu.RLock()
	defer strictIdentitySourceMu.RUnlock()
	return strictIdentitySource
}

// IdentitySourceValidated reports whether the given token label comes from a
// validated identity source (slice 4d). A token is a validated identity source
// iff its label is a reserved identity/service label — i.e. it is in the
// bootstrap exact-set OR carries a reserved `oidc:` / `scim:` prefix registered
// by the enterprise wire (SetReservedTokenLabels / SetReservedTokenLabelPrefixes).
// A raw operator label (e.g. `ci-bot`) is NOT validated.
//
// This is a deliberately thin wrapper over IsReservedTokenLabel so the strict
// predicate and the reserved-label substrate can never drift apart: the exact
// set of labels the enterprise reserves (and therefore mints only internally)
// is precisely the set that counts as a validated identity source. INERT in
// OSS — the reserved sets are empty, so this returns false for everything, but
// the RequireBearer enforcement is gated on StrictIdentitySource() (also false
// in OSS), so a raw token still authenticates.
func IdentitySourceValidated(label string) bool {
	return IsReservedTokenLabel(label)
}
