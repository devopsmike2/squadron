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
const SchemaVersion = 2

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

// migration0002SquadronTenantID — ADR 0013 §D6-b (multi-tenancy slice
// D6-b). Adds the Squadron owner-tenant column that keys each Azure
// connection to the tenant that owns it.
//
// ⚠️ This is DISTINCT from the existing tenant_id column, which is the
// Azure AD tenant the Service Principal lives in (required, non-default,
// a UUID). The Squadron owner tenant is a separate namespace — hence
// the squadron_tenant_id column name / SquadronTenantID struct field.
//
// NOT NULL DEFAULT 'default' so pre-D6-b rows backfill to the OSS
// single-tenant sentinel — inert in OSS, where every connection is
// created under identity.DefaultTenant. The discovery rescan scheduler
// reads this column to scope its discovery_scans store writes to the
// connection's owning tenant (a scheduled rescan runs under
// WithSystemContext and carries no operator identity).
//
// SQLite doesn't support ALTER TABLE ... ADD COLUMN IF NOT EXISTS; the
// migrate runner already tolerates the "duplicate column name" error
// (isDuplicateColumnErr) so re-running on an up-to-date database is a
// no-op — mirroring iacconnstore's migration0004TenantID.
const migration0002SquadronTenantID = `
ALTER TABLE azure_connections ADD COLUMN squadron_tenant_id TEXT NOT NULL DEFAULT 'default';
INSERT OR IGNORE INTO schema_version (version) VALUES (2);
`

// migrations is the ordered list of schema migrations. Index N is the
// SQL applied at version N+1. New entries are appended; existing
// entries are never edited.
var migrations = []string{
	migration0001AzureConnections,
	migration0002SquadronTenantID,
}
