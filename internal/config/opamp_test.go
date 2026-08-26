// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// TestOpAMPConfig_AuthRequiredDefaultsToGrace verifies ADR 0042's backward-
// compatible default: an omitted opamp.require_auth key means GRACE (false), so
// the pilot's currently token-less agents keep connecting. An explicit true
// enforces; an explicit false stays grace.
func TestOpAMPConfig_AuthRequiredDefaultsToGrace(t *testing.T) {
	if (OpAMPConfig{}).IsAuthRequired() {
		t.Fatal("omitted require_auth must default to grace (false)")
	}
	tr := true
	if !(OpAMPConfig{RequireAuth: &tr}).IsAuthRequired() {
		t.Fatal("require_auth=true must enforce")
	}
	fa := false
	if (OpAMPConfig{RequireAuth: &fa}).IsAuthRequired() {
		t.Fatal("require_auth=false must stay grace")
	}
}

// TestOpAMPConfig_ResolvedLimits covers the default/opt-out semantics of the DoS
// caps: 0 => default, negative => disabled, positive => honored.
func TestOpAMPConfig_ResolvedLimits(t *testing.T) {
	// Message bytes.
	if got := (OpAMPConfig{}).ResolvedMaxMessageBytes(); got != defaultOpAMPMaxMessageBytes {
		t.Fatalf("default message bytes = %d, want %d", got, defaultOpAMPMaxMessageBytes)
	}
	if got := (OpAMPConfig{MaxMessageBytes: -1}).ResolvedMaxMessageBytes(); got != 0 {
		t.Fatalf("negative message bytes should disable (0), got %d", got)
	}
	if got := (OpAMPConfig{MaxMessageBytes: 1024}).ResolvedMaxMessageBytes(); got != 1024 {
		t.Fatalf("explicit message bytes = %d, want 1024", got)
	}

	// Messages per second.
	if got := (OpAMPConfig{}).ResolvedMaxMessagesPerSecond(); got != defaultOpAMPMaxMessagesPerSecond {
		t.Fatalf("default rate = %v, want %v", got, defaultOpAMPMaxMessagesPerSecond)
	}
	if got := (OpAMPConfig{MaxMessagesPerSecond: -1}).ResolvedMaxMessagesPerSecond(); got != 0 {
		t.Fatalf("negative rate should disable (0), got %v", got)
	}
	if got := (OpAMPConfig{MaxMessagesPerSecond: 5}).ResolvedMaxMessagesPerSecond(); got != 5 {
		t.Fatalf("explicit rate = %v, want 5", got)
	}
}
