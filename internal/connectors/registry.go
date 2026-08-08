// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Config is the stored, non-secret configuration for one connector
// instance. Secrets (endpoint tokens, passwords) never live here — they
// live in the CredentialStore, sealed at rest, and are resolved and
// passed to the Factory separately. This split is what lets a Config be
// logged, listed in an API response, or persisted in a plain row without
// leaking credentials.
type Config struct {
	// ID is the stable identifier for this connector instance. It is
	// also the key the CredentialStore seals this connector's secrets
	// under.
	ID string

	// Type is the registered type name (e.g. "loki", "splunk") the
	// Registry looks up to find the Factory. Matches Descriptor.Type.
	Type string

	// DisplayName is the operator-facing label for this instance.
	DisplayName string

	// Endpoint is the backend base URL / host. Not a secret (the token
	// that authenticates against it is), so it lives in the plain
	// Config.
	Endpoint string

	// Settings carries type-specific, non-secret knobs (an index name,
	// a tenant header value, a TLS-skip flag, ...). Interpreted by the
	// connector's Factory. Nil is equivalent to empty.
	Settings map[string]string
}

// Factory builds a Connector from a stored Config and the resolved
// (already-decrypted) credentials for that connector. The registry
// calls this on New. Credentials are passed as a value, not fetched by
// the factory, so the factory has no dependency on the credential store
// and stays trivially testable.
type Factory func(cfg Config, creds ConnectorCredentials) (Connector, error)

// Registry errors. Callers errors.Is against these sentinels.
var (
	// ErrUnknownType is returned by New when no Factory is registered
	// for the Config's Type.
	ErrUnknownType = errors.New("connectors: unknown connector type")

	// ErrDuplicateType is returned by Register when a type name is
	// already registered. Registration is deliberately fail-loud rather
	// than last-write-wins so two packages cannot silently clobber each
	// other's connector.
	ErrDuplicateType = errors.New("connectors: connector type already registered")

	// ErrNilFactory is returned by Register when the supplied Factory is
	// nil.
	ErrNilFactory = errors.New("connectors: factory is nil")

	// ErrEmptyType is returned by Register when the type name is empty.
	ErrEmptyType = errors.New("connectors: connector type name is empty")
)

// Registry maps connector type names to their Factory. It is the seam
// through which vendor connectors register themselves; in ADR 0034
// slice 1 it starts effectively empty — no real backend registers in
// production code, so the framework is not selectable in a running
// deployment. Real connectors (Loki, Splunk, ...) register in later
// slices; tests register a mock.
//
// A Registry is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty Registry ready to accept Register calls.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register associates typeName with factory. It returns ErrDuplicateType
// if typeName is already registered, ErrNilFactory if factory is nil, or
// ErrEmptyType if typeName is empty. Register is fail-loud on conflict
// by design (see ErrDuplicateType).
func (r *Registry) Register(typeName string, factory Factory) error {
	if typeName == "" {
		return ErrEmptyType
	}
	if factory == nil {
		return ErrNilFactory
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[typeName]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateType, typeName)
	}
	r.factories[typeName] = factory
	return nil
}

// New instantiates a Connector from a stored Config and the resolved
// credentials for it. It returns ErrUnknownType (errors.Is-comparable)
// when no Factory is registered for cfg.Type, or whatever error the
// Factory returns.
func (r *Registry) New(cfg Config, creds ConnectorCredentials) (Connector, error) {
	r.mu.RLock()
	factory, ok := r.factories[cfg.Type]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, cfg.Type)
	}
	return factory(cfg, creds)
}

// Has reports whether a Factory is registered for typeName.
func (r *Registry) Has(typeName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[typeName]
	return ok
}

// Types returns the registered type names, sorted, for listing and
// diagnostics. Returns an empty (non-nil) slice when nothing is
// registered.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for name := range r.factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
