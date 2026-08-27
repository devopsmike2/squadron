// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/connectors"
	"github.com/devopsmike2/squadron/internal/egressguard"
	"github.com/stretchr/testify/require"
)

// TestNew_UsesGuardedClient asserts the Datadog connector routes its host-
// inventory calls through the shared SSRF egress guard (ADR 0046) — the site/
// endpoint is operator-supplied.
func TestNew_UsesGuardedClient(t *testing.T) {
	c, err := New(connectors.Config{
		ID:   "d1",
		Type: TypeName,
	}, connectors.ConnectorCredentials{
		Headers: map[string]string{HeaderAPIKey: "api", HeaderAppKey: "app"},
	})
	require.NoError(t, err)
	require.True(t, egressguard.IsGuarded(c.client), "datadog connector must use a guarded client")
}
