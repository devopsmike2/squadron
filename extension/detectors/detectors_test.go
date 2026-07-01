// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package detectors

import "testing"

// TestNoOpProvider_NeverActivates pins the OSS entitlement posture: the
// no-op provider ignores the operator's runtime switch and always
// reports the commercial-tier detectors as inactive. This is what makes
// config.CommercialDetectors.Enabled inert in the OSS build — the
// entitlement is the compiled-in edition, not the flag.
func TestNoOpProvider_NeverActivates(t *testing.T) {
	var p Provider = NoOpProvider{}
	if p.CommercialDetectorsActive(true) {
		t.Error("NoOpProvider.CommercialDetectorsActive(true) = true; want false (OSS never activates)")
	}
	if p.CommercialDetectorsActive(false) {
		t.Error("NoOpProvider.CommercialDetectorsActive(false) = true; want false")
	}
}

// TestActive_NilProviderIsDormant pins the nil-safe helper: a nil
// Provider (no enterprise provider wired) resolves to dormant, matching
// the OSS posture, regardless of the requested switch.
func TestActive_NilProviderIsDormant(t *testing.T) {
	if Active(nil, true) {
		t.Error("Active(nil, true) = true; want false (nil provider is dormant)")
	}
	if Active(NoOpProvider{}, true) {
		t.Error("Active(NoOpProvider{}, true) = true; want false")
	}
}

// enterpriseLikeProvider is a test double standing in for the private
// enterprise provider: it honours the requested runtime switch. It lets
// the test document the contract the enterprise edition must satisfy
// (requested passes through) without shipping the real impl in OSS.
type enterpriseLikeProvider struct{}

func (enterpriseLikeProvider) CommercialDetectorsActive(requested bool) bool { return requested }

func TestActive_EnterpriseLikeHonoursRequested(t *testing.T) {
	if Active(enterpriseLikeProvider{}, false) {
		t.Error("enterprise-like provider with requested=false should be dormant")
	}
	if !Active(enterpriseLikeProvider{}, true) {
		t.Error("enterprise-like provider with requested=true should activate")
	}
}
