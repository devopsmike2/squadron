// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package loki

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/connectors"
	"github.com/devopsmike2/squadron/internal/egressguard"
	"github.com/stretchr/testify/require"
)

// TestNew_UsesGuardedClient asserts the Loki connector routes queries through
// the shared SSRF egress guard (ADR 0046) — the endpoint is operator-supplied.
func TestNew_UsesGuardedClient(t *testing.T) {
	c, err := New(connectors.Config{ID: "l1", Type: TypeName, Endpoint: "https://loki.example:3100"}, connectors.ConnectorCredentials{})
	require.NoError(t, err)
	lc, ok := c.(*lokiConnector)
	require.True(t, ok)
	require.True(t, egressguard.IsGuarded(lc.client), "loki connector must use a guarded client")
}
