// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azureconnstore

// SchemaVersion is the current schema version for the azureconnstore
// SQLite database. The substrate owns its own database file
// (azureconnstore.db) so its migrations are independent of the
// chains in internal/discovery/credstore/migrations.go,
// internal/discovery/iacconnstore/migrations.go,
// internal/discovery/gcpconnstore/migrations.go, and
// internal/storage/applicationstore/sqlite/migrations.go.
//
// The numbering convention mirrors credstore + iacconnstore +
// gcpconnstore: integer version, one migration per bump, applied in
// order, idempotent SQL inside each step. Existing migrations are
// NEVER edited after merge — they ran against historical databases
// and edits desynchronize the schema.
const SchemaVersion = 1

// migration0001AzureConnections is the initial schema for the Azure
// discovery slice 1 arc (#674 chunk 1). One table for Azure
// subscription connections, parallel to gcpconnstore's
// gcp_connections and iacconnstore's iac_connections.
//
// The design doc (§5) calls for an index on subscription_id so chunk
// 3's "find the connection for this subscription" lookup is O(log n).
// The substrate does NOT enforce uniqueness on subscription_id —
// operators may legitimately connect the same subscription twice with
// different SPs (different role scopes).
const migration0001AzureConnections = `
-- Schema version tracker, mirrors the convention used by credstore,
-- iacconnstore, and gcpconnstore.
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS azure_connections (
	id                                  TEXT PRIMARY KEY,
	display_name                        TEXT NOT NULL,
	tenant_id                           TEXT NOT NULL,
	subscription_id                     TEXT NOT NULL,
	client_id                           TEXT NOT NULL,
	sealed_secret                       BLOB NOT NULL,
	location                            TEXT,
	learn_from_accepted_recommendations INTEGER NOT NULL DEFAULT 1,
	created_at                          TEXT NOT NULL,
	updated_at                          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_azure_connections_subscription_id
	ON azure_connections (subscription_id);

INSERT OR IGNORE INTO schema_version (version) VALUES (1);
`

// migrations is the ordered list of schema migrations. Index N is the
// SQL applied at version N+1. New entries are appended; existing
// entries are never edited.
var migrations = []string{
	migration0001AzureConnections,
}
