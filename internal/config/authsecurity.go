// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net"
	"strings"
)

// isLoopbackBindHost reports whether a configured server bind host is a
// loopback interface (local-only). An empty host, "0.0.0.0", or "::" all bind
// EVERY interface and are therefore NOT loopback — they are network-reachable.
// "localhost", "127.0.0.0/8", and "::1" are loopback. Any other routable
// address is treated as non-loopback (exposed).
func isLoopbackBindHost(host string) bool {
	h := strings.TrimSpace(host)
	// Empty / wildcard binds mean "all interfaces" — reachable, not loopback.
	switch h {
	case "", "0.0.0.0", "::", "[::]", "*":
		return false
	case "localhost":
		return true
	}
	// Strip an optional zone / brackets before parsing.
	h = strings.Trim(h, "[]")
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	// A hostname we can't classify — assume the safe-for-warning posture
	// (treat as exposed) so we never silently suppress the warning.
	return false
}

// StartupWarning is the AuthConfig-bound form of AuthStartupWarning, resolving
// the effective enabled state and the acknowledgement flag from the config. It
// exists so callers holding a *Config (where the `config` package identifier is
// shadowed by the variable) can reach the decision via config.Auth.StartupWarning.
func (a AuthConfig) StartupWarning(bindHost string) (warn bool, message string) {
	return AuthStartupWarning(a.IsEnabled(), a.InsecureAllowUnauthenticated, bindHost)
}

// AuthStartupWarning implements the ADR 0045 "warn window" decision. It reports
// whether Squadron is starting in an insecure posture — API auth DISABLED while
// the control plane binds a NON-loopback (network-reachable) interface — and
// returns a loud, operator-facing message describing the risk and the fix.
//
// This is deliberately NOT fatal in this release: a pilot currently running
// auth-off must not hard-break on upgrade. A future release will refuse to
// start in this posture unless the operator has explicitly set
// auth.insecure_allow_unauthenticated=true. The message states that timeline.
//
// Returns warn=false (no message) when auth is enabled, or when auth is
// disabled but the bind is loopback-only (the supported local-dev path).
func AuthStartupWarning(authEnabled, insecureAcknowledged bool, bindHost string) (warn bool, message string) {
	if authEnabled {
		return false, ""
	}
	if isLoopbackBindHost(bindHost) {
		// Auth off but local-only: the supported dev posture. Quiet.
		return false, ""
	}

	var b strings.Builder
	b.WriteString("SECURITY: API authentication is DISABLED and the control plane is bound to a NON-loopback interface. ")
	b.WriteString("Squadron's control plane is UNAUTHENTICATED and REACHABLE OVER THE NETWORK: any peer that can reach this port can dispatch actions, read configs and resolved secrets, mint tokens, and export the audit trail. ")
	b.WriteString("Fix now by setting auth.enabled=true (Squadron prints a one-time bootstrap token on first start; see docs/auth.md), or bind a loopback-only interface (server.host=127.0.0.1) for local development. ")
	if insecureAcknowledged {
		b.WriteString("You have set auth.insecure_allow_unauthenticated=true, so this insecure posture is explicitly acknowledged for now — but it remains dangerous and a FUTURE RELEASE WILL still require auth.enabled=true for any non-loopback bind.")
	} else {
		b.WriteString("This is a WARN-ONLY window: a FUTURE RELEASE WILL REFUSE TO START in this configuration. Set auth.enabled=true (recommended) before then.")
	}
	return true, b.String()
}
