// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aicredstore

import (
	"context"
	"database/sql"

	"go.uber.org/zap"
)

// dialectSQLite is the dialect name discriminator (used by Set to pick
// the updated_at encoding).
const dialectSQLite = "sqlite"

// sqliteDialect is the SQLite statement set. Placeholders are "?" and
// the blob columns are BLOB; updated_at is TEXT (RFC3339Nano). The
// upsert keys on the fixed id=1 primary key so the table stays a
// single-row table.
var sqliteDialect = dialect{
	name: dialectSQLite,
	createTable: `
		CREATE TABLE IF NOT EXISTS app_ai_credential (
			id         INTEGER PRIMARY KEY,
			provider   TEXT NOT NULL DEFAULT '',
			ciphertext BLOB NOT NULL,
			nonce      BLOB NOT NULL,
			updated_at TEXT NOT NULL
		);
	`,
	upsert: `
		INSERT INTO app_ai_credential (id, provider, ciphertext, nonce, updated_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			provider   = excluded.provider,
			ciphertext = excluded.ciphertext,
			nonce      = excluded.nonce,
			updated_at = excluded.updated_at
	`,
	selectRow: `SELECT provider, ciphertext, nonce FROM app_ai_credential WHERE id = 1`,
	deleteRow: `DELETE FROM app_ai_credential WHERE id = 1`,
}

// NewSQLiteStore runs the idempotent migration on the supplied SQLite
// *sql.DB and returns a ready Store. The caller owns opening the DB
// (mirrors credstore / iacconnstore) so the substrate can live in its
// own database file, independent of the application store's lifecycle.
func NewSQLiteStore(ctx context.Context, db *sql.DB, logger *zap.Logger) (Store, error) {
	return newGenericStore(ctx, db, logger, sqliteDialect)
}
