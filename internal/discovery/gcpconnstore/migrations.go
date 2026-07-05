// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcpconnstore

// SchemaVersion is the current schema version for the gcpconnstore
// SQLite database. The substrate owns its own database file
// (gcpconnstore.db) so its migrations are independent of the chains
// in internal/discovery/credstore/migrations.go,
// internal/discovery/iacconnstore/migrations.go, and
// internal/storage/applicationstore/sqlite/migrations.go.
//
// The numbering convention mirrors credstore + iacconnstore: integer
// version, one migration per bump, applied in order, idempotent SQL
// inside each step. Existing migrations are NEVER edited after merge
// — they ran against historical databases and edits desynchronize
// the schema.
const SchemaVersion = 2

// migration0001GCPConnections is the initial schema for the GCP
// discovery slice 1 arc (#667 chunk 1). One table for GCP project
// connections, parallel to iacconnstore's iac_connections.
//
// The design doc (§5) calls for an index on project_id so chunk 3's
// "find the connection for this project" lookup is O(log n). The
// substrate does NOT enforce uniqueness on project_id — operators
// may legitimately connect the same project twice with different
// SAs (different role scopes).
const migration0001GCPConnections = `
-- Schema version tracker, mirrors the convention used by credstore
-- and iacconnstore.
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS gcp_connections (
	id                                  TEXT PRIMARY KEY,
	display_name                        TEXT NOT NULL,
	project_id                          TEXT NOT NULL,
	sealed_sa                           BLOB NOT NULL,
	region                              TEXT,
	learn_from_accepted_recommendations INTEGER NOT NULL DEFAULT 1,
	created_at                          TEXT NOT NULL,
	updated_at                          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_gcp_connections_project_id
	ON gcp_connections (project_id);

INSERT OR IGNORE INTO schema_version (version) VALUES (1);
`

// migration0002TenantID — ADR 0013 §D6-b (multi-tenancy slice D6-b).
// Adds the Squadron owner-tenant column that keys each GCP connection
// to the tenant that owns it. NOT NULL DEFAULT 'default' so pre-D6-b
// rows backfill to the OSS single-tenant sentinel — inert in OSS,
// where every connection is created under identity.DefaultTenant. The
// discovery rescan scheduler reads this column to scope its
// discovery_scans store writes to the connection's tenant (a
// scheduled rescan runs under WithSystemContext and carries no
// operator identity).
//
// SQLite doesn't support ALTER TABLE ... ADD COLUMN IF NOT EXISTS; the
// migrate runner already tolerates the "duplicate column name" error
// (isDuplicateColumnErr) so re-running on an up-to-date database is a
// no-op — mirroring iacconnstore's migration0004TenantID.
const migration0002TenantID = `
ALTER TABLE gcp_connections ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
INSERT OR IGNORE INTO schema_version (version) VALUES (2);
`

// migrations is the ordered list of schema migrations. Index N is the
// SQL applied at version N+1. New entries are appended; existing
// entries are never edited.
var migrations = []string{
	migration0001GCPConnections,
	migration0002TenantID,
}
