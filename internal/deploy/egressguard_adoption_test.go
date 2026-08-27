// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/egressguard"
)

// TestProviders_UseGuardedClient asserts every deploy provider (and the
// completion-webhook client) routes through the shared SSRF egress guard
// (ADR 0046) — the base URL / target host and its PAT are operator-supplied.
func TestProviders_UseGuardedClient(t *testing.T) {
	if !egressguard.IsGuarded(NewGitHubProvider("").HTTP) {
		t.Error("GitHub provider must use a guarded client")
	}
	if !egressguard.IsGuarded(NewAzureDevOpsProvider("").HTTP) {
		t.Error("Azure DevOps provider must use a guarded client")
	}
	if !egressguard.IsGuarded(NewAnsibleTowerProvider().HTTP) {
		t.Error("Ansible Tower provider must use a guarded client")
	}
	svc := NewService(nil, nil, nil, nil)
	if !egressguard.IsGuarded(svc.httpClient) {
		t.Error("deploy service completion-webhook client must be guarded")
	}
}

// TestSanitizeProbeErr_NoRawLeak asserts the /validate probe-error sanitizer
// returns a category and never echoes the raw upstream body.
func TestSanitizeProbeErr_NoRawLeak(t *testing.T) {
	got := sanitizeProbeErr(&egressguard.BlockedError{Reason: "loopback address blocked by egress policy"})
	if got != "blocked by egress policy" {
		t.Fatalf("blocked error should map to policy category, got %q", got)
	}
}
