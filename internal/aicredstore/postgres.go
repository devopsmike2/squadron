// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aicredstore

import (
	"context"
	"database/sql"

	"go.uber.org/zap"
)

// dialectPostgres is the dialect name discriminator.
const dialectPostgres = "postgres"

// postgresDialect is the Postgres statement set. Placeholders are $N and
// the blob columns are BYTEA; updated_at is TIMESTAMPTZ (a time.Time is
// passed through). The upsert keys on the fixed id=1 primary key so the
// table stays a single-row table.
var postgresDialect = dialect{
	name: dialectPostgres,
	createTable: `
		CREATE TABLE IF NOT EXISTS app_ai_credential (
			id         INTEGER PRIMARY KEY,
			provider   TEXT NOT NULL DEFAULT '',
			ciphertext BYTEA NOT NULL,
			nonce      BYTEA NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		);
	`,
	upsert: `
		INSERT INTO app_ai_credential (id, provider, ciphertext, nonce, updated_at)
		VALUES (1, $1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			provider   = EXCLUDED.provider,
			ciphertext = EXCLUDED.ciphertext,
			nonce      = EXCLUDED.nonce,
			updated_at = EXCLUDED.updated_at
	`,
	selectRow: `SELECT provider, ciphertext, nonce FROM app_ai_credential WHERE id = 1`,
	deleteRow: `DELETE FROM app_ai_credential WHERE id = 1`,
}

// NewPostgresStore runs the idempotent migration on the supplied
// Postgres *sql.DB (opened with the pgx stdlib driver, same as the
// application store) and returns a ready Store. The caller owns opening
// the DB so the substrate shares the app's Postgres deployment while
// keeping its own tiny table.
func NewPostgresStore(ctx context.Context, db *sql.DB, logger *zap.Logger) (Store, error) {
	return newGenericStore(ctx, db, logger, postgresDialect)
}
