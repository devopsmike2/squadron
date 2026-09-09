// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aicredstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// timestampLayout is the on-disk timestamp format for the SQLite
// dialect's TEXT updated_at column. RFC3339Nano round-trips cleanly
// through time.Parse and matches credstore / iacconnstore.
const timestampLayout = time.RFC3339Nano

// ErrNilDB is returned by the constructors when handed a nil *sql.DB.
var ErrNilDB = errors.New("aicredstore: db is required")

// Store is the app-global AI-credential substrate. Implementations are
// safe for concurrent use (they hold no mutable state beyond the
// *sql.DB, which is itself concurrency-safe).
//
// The stored bytes are the SEALED ciphertext + nonce — the caller seals
// the plaintext key with the credstore.Key before calling Set, and
// opens the returned bytes with the same key after Get. This package
// never sees plaintext.
type Store interface {
	// Set upserts the single credential row: the sealed ciphertext, its
	// nonce, and the provider label ("" is allowed and means "unchanged
	// / default"). sealed and nonce must both be non-empty.
	Set(ctx context.Context, sealed, nonce []byte, provider string) error

	// Get returns the sealed ciphertext + nonce + provider for the single
	// row. ok is false (with nil error) when no credential has been set;
	// err is non-nil only on a storage failure.
	Get(ctx context.Context) (sealed, nonce []byte, provider string, ok bool, err error)

	// Clear removes the credential row. Idempotent — clearing an already
	// empty store is a no-op success.
	Clear(ctx context.Context) error

	// Close releases the underlying database handle.
	Close() error
}

// dialect carries the SQL a genericStore executes. The two supported
// dialects (SQLite, Postgres) differ only in placeholder style and
// column types, so the CRUD logic is shared and only the statements
// swap. The application store already runs on BOTH backends; this
// mirrors that split.
type dialect struct {
	name        string
	createTable string
	upsert      string // upserts id=1; args: provider, ciphertext, nonce, updated_at
	selectRow   string // returns provider, ciphertext, nonce for id=1
	deleteRow   string
}

// genericStore is the shared implementation both dialects delegate to.
type genericStore struct {
	db      *sql.DB
	logger  *zap.Logger
	dialect dialect
}

// newGenericStore opens the substrate: validates the handle, runs the
// idempotent migration, and returns a ready Store.
func newGenericStore(ctx context.Context, db *sql.DB, logger *zap.Logger, d dialect) (Store, error) {
	if db == nil {
		return nil, ErrNilDB
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &genericStore{db: db, logger: logger, dialect: d}
	if err := s.migrate(ctx); err != nil {
		return nil, fmt.Errorf("aicredstore: migrate (%s): %w", d.name, err)
	}
	logger.Info("aicredstore substrate initialized", zap.String("dialect", d.name))
	return s, nil
}

// migrate applies the idempotent CREATE TABLE IF NOT EXISTS. Re-running
// on an up-to-date database is a no-op.
func (s *genericStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, s.dialect.createTable); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	return nil
}

// Set upserts the single credential row. The sealed bytes are opaque to
// this package; validation is limited to "non-empty" so a caller bug
// (sealing an empty key, or forgetting to seal) fails loud here rather
// than persisting a broken row.
func (s *genericStore) Set(ctx context.Context, sealed, nonce []byte, provider string) error {
	if len(sealed) == 0 {
		return errors.New("aicredstore: Set: sealed ciphertext is required")
	}
	if len(nonce) == 0 {
		return errors.New("aicredstore: Set: nonce is required")
	}
	now := time.Now().UTC()
	// SQLite stores updated_at as TEXT (RFC3339Nano); Postgres stores it
	// as TIMESTAMPTZ. Format for SQLite, pass time.Time for Postgres.
	var updatedAt any = now
	if s.dialect.name == dialectSQLite {
		updatedAt = now.Format(timestampLayout)
	}
	if _, err := s.db.ExecContext(ctx, s.dialect.upsert,
		provider, sealed, nonce, updatedAt,
	); err != nil {
		return fmt.Errorf("aicredstore: upsert credential: %w", err)
	}
	return nil
}

// Get returns the sealed credential, or ok=false when unset.
func (s *genericStore) Get(ctx context.Context) (sealed, nonce []byte, provider string, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, s.dialect.selectRow)
	var (
		p   string
		ct  []byte
		nce []byte
	)
	if scanErr := row.Scan(&p, &ct, &nce); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil, nil, "", false, nil
		}
		return nil, nil, "", false, fmt.Errorf("aicredstore: get credential: %w", scanErr)
	}
	return ct, nce, p, true, nil
}

// Clear removes the credential row (idempotent).
func (s *genericStore) Clear(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, s.dialect.deleteRow); err != nil {
		return fmt.Errorf("aicredstore: clear credential: %w", err)
	}
	return nil
}

// Close releases the underlying database handle.
func (s *genericStore) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}
