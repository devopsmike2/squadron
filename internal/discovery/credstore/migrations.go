// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package credstore

// SchemaVersion is the current schema version for the credstore
// SQLite database. Bumped any time a migration is appended to
// migrations. The substrate owns its own database file (separate from
// the application store) so its migrations are independent of the
// application-store migration chain in
// internal/storage/applicationstore/sqlite/.
//
// The numbering convention mirrors the application store: integer
// version, one migration per bump, applied in order, idempotent SQL
// inside each step.
const SchemaVersion = 1

// migration0001AWSConnections is the initial schema for the credential
// substrate. It contains a single table — aws_connections — keyed by
// AccountID, with the ExternalID stored as ciphertext + nonce. There
// is no plaintext column for the ExternalID; the application has no
// path that writes one even by accident.
//
// The schema matches the design doc's "Credential substrate" section
// exactly. All timestamp columns use ISO-8601 strings (TEXT) so the
// row contents are human-inspectable during incident response without
// pulling the SQLite driver into the read path.
const migration0001AWSConnections = `
-- Schema version tracker, mirrors the convention used by the
-- application store. Single row carrying the highest applied version.
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- AWS account trust-policy metadata. One row per connected account.
-- ExternalID is stored as ciphertext + nonce; there is no plaintext
-- column anywhere in this schema.
CREATE TABLE IF NOT EXISTS aws_connections (
	account_id TEXT PRIMARY KEY,
	role_arn TEXT NOT NULL,
	display_name TEXT NOT NULL,
	region TEXT NOT NULL,
	external_id_ciphertext BLOB NOT NULL,
	external_id_nonce BLOB NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

INSERT OR IGNORE INTO schema_version (version) VALUES (1);
`

// migrations is the ordered list of schema migrations. Index N is the
// SQL applied at version N+1. New entries are appended; existing
// entries are never edited (they ran against historical databases and
// editing them after the fact desynchronizes the schema across
// deployments).
var migrations = []string{
	migration0001AWSConnections,
}
