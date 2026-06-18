// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package actions

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- restart-docker-container ----------------------------------------------

func TestRestartDocker_Validate_Required(t *testing.T) {
	err := validateRestartDockerParameters(json.RawMessage(`{}`))
	require.Error(t, err)
}

func TestRestartDocker_Validate_RejectsPathSeparator(t *testing.T) {
	err := validateRestartDockerParameters(json.RawMessage(`{"container":"../escape"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path separators")
}

func TestRestartDocker_Validate_TimeoutBounds(t *testing.T) {
	require.Error(t, validateRestartDockerParameters(json.RawMessage(`{"container":"web","timeout_seconds":-1}`)))
	require.Error(t, validateRestartDockerParameters(json.RawMessage(`{"container":"web","timeout_seconds":601}`)))
	require.NoError(t, validateRestartDockerParameters(json.RawMessage(`{"container":"web","timeout_seconds":30}`)))
}

func TestRestartDocker_CapabilityGlob(t *testing.T) {
	cap := Capability{
		Type: RestartDockerContainerType,
		Constraints: map[string]any{
			"container_glob": []any{"web-*", "squadron-*"},
		},
	}
	ok, _ := matchesRestartDockerCapability(json.RawMessage(`{"container":"web-frontend"}`), cap)
	assert.True(t, ok)
	ok, reason := matchesRestartDockerCapability(json.RawMessage(`{"container":"postgres"}`), cap)
	assert.False(t, ok)
	assert.Contains(t, reason, "does not match")
}

func TestRestartDocker_NoGlobMeansAny(t *testing.T) {
	cap := Capability{Type: RestartDockerContainerType}
	ok, _ := matchesRestartDockerCapability(json.RawMessage(`{"container":"anything"}`), cap)
	assert.True(t, ok)
}

// --- run-shell-allowlist ---------------------------------------------------

func TestRunShell_Validate_Required(t *testing.T) {
	err := validateRunShellAllowlistParameters(json.RawMessage(`{}`))
	require.Error(t, err)
}

func TestRunShell_Validate_RejectsMetacharacters(t *testing.T) {
	for _, bad := range []string{
		`{"command":"ls; rm -rf /"}`,
		`{"command":"echo hi | tee secrets"}`,
		`{"command":"echo $(whoami)"}`,
		`{"command":"echo hi && cat /etc/passwd"}`,
	} {
		require.Error(t, validateRunShellAllowlistParameters(json.RawMessage(bad)), bad)
	}
}

func TestRunShell_CapabilityRequiresExactMatch(t *testing.T) {
	cap := Capability{
		Type: RunShellAllowlistType,
		Constraints: map[string]any{
			"commands": []any{
				"systemctl reload nginx.service",
				"docker logs --tail 100 squadron-app",
			},
		},
	}
	ok, _ := matchesRunShellAllowlistCapability(
		json.RawMessage(`{"command":"systemctl reload nginx.service"}`), cap,
	)
	assert.True(t, ok)
	// Substring of an allowed command must still be rejected.
	ok, reason := matchesRunShellAllowlistCapability(
		json.RawMessage(`{"command":"systemctl reload nginx"}`), cap,
	)
	assert.False(t, ok)
	assert.Contains(t, reason, "not on the runner's allowlist")
}

func TestRunShell_EmptyAllowlistRefuses(t *testing.T) {
	cap := Capability{Type: RunShellAllowlistType}
	ok, reason := matchesRunShellAllowlistCapability(
		json.RawMessage(`{"command":"ls"}`), cap,
	)
	assert.False(t, ok)
	assert.Contains(t, reason, "no commands on the allowlist")
}
