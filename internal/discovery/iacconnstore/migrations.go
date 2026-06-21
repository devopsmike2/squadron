// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package iacconnstore

// SchemaVersion is the current schema version for the iacconnstore
// SQLite database. The substrate owns its own database file (separate
// from the credstore database and the application store) so its
// migrations are independent of the chains in
// internal/discovery/credstore/migrations.go and
// internal/storage/applicationstore/sqlite/migrations.go.
//
// The numbering convention mirrors credstore: integer version, one
// migration per bump, applied in order, idempotent SQL inside each
// step. Existing migrations are NEVER edited after merge — they ran
// against historical databases and edits desynchronize the schema.
const SchemaVersion = 1

// migration0001IaCConnections is the initial schema. One table for
// IaC repository connections, parallel to credstore's
// cloud_connections.
//
// A unique index on (provider, repo_full_name) enforces the
// "one connection per deployment per repo" rule (design doc §10) at
// the database layer rather than at the handler layer. The Store's
// Create method translates the SQLite uniqueness constraint failure
// into ErrConnectionConflict.
const migration0001IaCConnections = `
-- Schema version tracker, mirrors the convention used by credstore.
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS iac_connections (
	connection_id        TEXT PRIMARY KEY,
	provider             TEXT NOT NULL,
	auth_kind            TEXT NOT NULL,
	repo_full_name       TEXT NOT NULL,
	default_branch       TEXT NOT NULL,
	repo_layout          TEXT NOT NULL,
	branch_prefix        TEXT,
	reviewer_team_handle TEXT,
	placement_map_json   TEXT NOT NULL,
	cred_ciphertext      BLOB NOT NULL,
	created_at           TEXT NOT NULL,
	updated_at           TEXT NOT NULL
);

-- One connection per (provider, repo_full_name). Slice 1 enforces
-- this at the deployment scope; the unique index is the substrate
-- guard against a wizard re-submission silently overwriting an
-- existing row.
CREATE UNIQUE INDEX IF NOT EXISTS iac_connections_provider_repo_idx
	ON iac_connections (provider, repo_full_name);

INSERT OR IGNORE INTO schema_version (version) VALUES (1);
`

// migrations is the ordered list of schema migrations. Index N is the
// SQL applied at version N+1. New entries are appended; existing
// entries are never edited.
var migrations = []string{
	migration0001IaCConnections,
}
