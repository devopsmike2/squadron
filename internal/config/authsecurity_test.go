// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// TestAuthConfig_IsEnabled_SecureByDefault pins ADR 0045: an omitted
// `auth.enabled` key (nil) resolves to enabled=true, while an explicit value is
// honored as-is (so the pilot's explicit `enabled: false` still opts out).
func TestAuthConfig_IsEnabled_SecureByDefault(t *testing.T) {
	tr := true
	fa := false
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"omitted (nil) => secure default ON", nil, true},
		{"explicit true => ON", &tr, true},
		{"explicit false => OFF (opt-out preserved)", &fa, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AuthConfig{Enabled: tc.in}.IsEnabled()
			if got != tc.want {
				t.Fatalf("IsEnabled(Enabled=%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAuthConfig_YAMLDefaultsOn confirms an operator YAML that never mentions
// auth loads as auth-ENABLED (the shipped default), while an explicit
// `auth.enabled: false` loads as disabled.
func TestAuthConfig_YAMLDefaultsOn(t *testing.T) {
	if !(AuthConfig{}).IsEnabled() {
		t.Fatal("a zero-value AuthConfig (auth block omitted) must be enabled by default")
	}
}

// TestAuthStartupWarning covers the ADR 0045 warn-window decision.
func TestAuthStartupWarning(t *testing.T) {
	cases := []struct {
		name         string
		authEnabled  bool
		acknowledged bool
		bindHost     string
		wantWarn     bool
	}{
		{"auth on, all interfaces => quiet", true, false, "", false},
		{"auth on, loopback => quiet", true, false, "127.0.0.1", false},
		{"auth off, loopback IPv4 => quiet", false, false, "127.0.0.1", false},
		{"auth off, loopback IPv6 => quiet", false, false, "::1", false},
		{"auth off, localhost => quiet", false, false, "localhost", false},
		{"auth off, empty (all interfaces) => WARN", false, false, "", true},
		{"auth off, 0.0.0.0 => WARN", false, false, "0.0.0.0", true},
		{"auth off, routable IP => WARN", false, false, "10.1.2.3", true},
		{"auth off, exposed, acknowledged => WARN (softer)", false, true, "0.0.0.0", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, msg := AuthStartupWarning(tc.authEnabled, tc.acknowledged, tc.bindHost)
			if warn != tc.wantWarn {
				t.Fatalf("AuthStartupWarning(%v,%v,%q) warn = %v, want %v",
					tc.authEnabled, tc.acknowledged, tc.bindHost, warn, tc.wantWarn)
			}
			if !warn {
				if msg != "" {
					t.Errorf("no-warn case must return an empty message, got %q", msg)
				}
				return
			}
			// A warning must name the risk and the fix.
			for _, want := range []string{"UNAUTHENTICATED", "auth.enabled=true"} {
				if !strings.Contains(msg, want) {
					t.Errorf("warning message must mention %q; got: %s", want, msg)
				}
			}
			// The message tone must reflect whether the operator acknowledged.
			if tc.acknowledged && !strings.Contains(msg, "insecure_allow_unauthenticated") {
				t.Errorf("acknowledged case should reference the ack flag; got: %s", msg)
			}
			if !tc.acknowledged && !strings.Contains(msg, "FUTURE RELEASE") {
				t.Errorf("unacknowledged case should state the future-fatal timeline; got: %s", msg)
			}
		})
	}
}
