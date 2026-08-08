// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package rollouts

import (
	"context"
	"errors"
	"fmt"

	"github.com/devopsmike2/squadron/internal/connectors"
)

// ConnectorResolver resolves a configured telemetry connector by its stored
// ID so the SLO-burn auto-abort criterion (ADR 0034) can query it. It is
// the seam between the rollout engine and the connector framework
// (internal/connectors): the engine holds only this read-only lookup, not
// the registry / credential-store wiring, which keeps the engine testable
// with a fake and keeps the connector persistence story out of the rollout
// package.
//
// A nil ConnectorResolver on the Engine disables the SLO-burn criterion
// entirely — the OSS default, since no connector is registered in a running
// deployment until a later ADR 0034 slice wires connector persistence.
type ConnectorResolver interface {
	// ResolveConnector returns the configured connector instance for
	// connectorID, or an error if it is unknown or cannot be built. The
	// engine only ever calls Query on the returned connector (read-only).
	ResolveConnector(ctx context.Context, connectorID string) (connectors.Connector, error)
}

// ConnectorConfigSource returns the stored, non-secret connectors.Config
// for a connector ID. A later ADR 0034 slice backs this with a persistent
// connector-config store; the interface keeps the resolver (and thus the
// engine) decoupled from that store's concrete shape. The bool reports
// whether a config exists for the id.
type ConnectorConfigSource interface {
	ConnectorConfig(ctx context.Context, connectorID string) (connectors.Config, bool, error)
}

// RegistryConnectorResolver is the default ConnectorResolver. It builds a
// connector on demand from the three collaborators the connector framework
// already defines: a Registry (type name -> Factory), a source of stored
// connector Config by id, and a CredentialStore that unseals that
// connector's secrets. This is exactly the instantiation path
// connectors.Registry.New documents — New(cfg, creds) — so the SLO-burn
// criterion reuses the framework's own "build from stored config" seam
// rather than inventing a parallel one. It is read-only.
type RegistryConnectorResolver struct {
	registry *connectors.Registry
	configs  ConnectorConfigSource
	creds    connectors.CredentialStore
}

// NewRegistryConnectorResolver wires a RegistryConnectorResolver. registry
// and configs are required; creds may be nil for backends that need no
// credentials (a resolved connector then gets zero-value credentials).
func NewRegistryConnectorResolver(registry *connectors.Registry, configs ConnectorConfigSource, creds connectors.CredentialStore) *RegistryConnectorResolver {
	return &RegistryConnectorResolver{registry: registry, configs: configs, creds: creds}
}

// ResolveConnector loads the stored config for connectorID, unseals its
// credentials (when a credential store is wired), and instantiates the
// connector via the registry. It never mutates any backend.
func (r *RegistryConnectorResolver) ResolveConnector(ctx context.Context, connectorID string) (connectors.Connector, error) {
	if r.registry == nil || r.configs == nil {
		return nil, errors.New("rollouts: connector resolver is not fully wired")
	}
	cfg, ok, err := r.configs.ConnectorConfig(ctx, connectorID)
	if err != nil {
		return nil, fmt.Errorf("rollouts: load connector config %q: %w", connectorID, err)
	}
	if !ok {
		return nil, fmt.Errorf("rollouts: no connector configured with id %q", connectorID)
	}

	var creds connectors.ConnectorCredentials
	if r.creds != nil {
		got, err := r.creds.Get(ctx, connectorID)
		// A connector legitimately may have no stored credentials (an
		// unauthenticated backend); only a real load error is fatal.
		if err != nil && !errors.Is(err, connectors.ErrCredentialsNotFound) {
			return nil, fmt.Errorf("rollouts: load connector credentials %q: %w", connectorID, err)
		}
		if got != nil {
			creds = *got
		}
	}

	return r.registry.New(cfg, creds)
}

// compile-time assertion that the concrete resolver satisfies the seam.
var _ ConnectorResolver = (*RegistryConnectorResolver)(nil)
