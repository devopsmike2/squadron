// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Factory implements applicationstore.ApplicationStoreFactory and creates
// storage components backed by an external Postgres / Aurora database
// (ADR 0033). It mirrors the sqlite Factory shape: NewFactory captures the
// connection string, Initialize opens the pool (the expensive, fail-able step),
// and CreateApplicationStore hands back the ready store.
//
// Unlike sqlite (which takes a filesystem Path), the Postgres backend is
// selected by storage.app.type: postgres + storage.app.dsn. There is no
// package-level init/register hook to mirror — the meta-factory wires backends
// explicitly via its switch, so the only thing the meta-factory must supply
// here is the DSN.
type Factory struct {
	dsn    string
	logger *zap.Logger
	store  *Storage
}

// NewFactory creates a new Factory with the given Postgres DSN, e.g.
// "postgres://user:pass@host:5432/squadron?sslmode=require".
func NewFactory(dsn string) *Factory {
	return &Factory{dsn: dsn}
}

// Initialize opens the Postgres connection pool, verifies it with a Ping, and
// applies the schema. A missing or unreachable DSN surfaces here as a specific
// postgres error — the backend never silently falls back to another store.
func (f *Factory) Initialize(logger *zap.Logger) error {
	f.logger = logger
	store, err := Open(context.Background(), f.dsn)
	if err != nil {
		return err
	}
	f.store = store
	return nil
}

// CreateApplicationStore returns the initialized Postgres store.
func (f *Factory) CreateApplicationStore() (types.ApplicationStore, error) {
	return f.store, nil
}

// Close releases the connection pool.
func (f *Factory) Close() error {
	if f.store != nil {
		return f.store.Close()
	}
	return nil
}
