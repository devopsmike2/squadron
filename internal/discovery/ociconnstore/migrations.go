// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package ociconnstore

// SchemaVersion is the current schema version for the ociconnstore
// SQLite database. The substrate owns its own database file
// (ociconnstore.db) so its migrations are independent of the chains
// in internal/discovery/credstore/migrations.go,
// internal/discovery/iacconnstore/migrations.go,
// internal/discovery/gcpconnstore/migrations.go,
// internal/discovery/azureconnstore/migrations.go, and
// internal/storage/applicationstore/sqlite/migrations.go.
//
// The numbering convention mirrors credstore + iacconnstore +
// gcpconnstore + azureconnstore: integer version, one migration per
// bump, applied in order, idempotent SQL inside each step. Existing
// migrations are NEVER edited after merge — they ran against
// historical databases and edits desynchronize the schema.
const SchemaVersion = 2

// migration0001OCIConnections is the initial schema for the OCI
// discovery slice 1 arc (#681 chunk 1). One table for OCI tenancy
// connections, parallel to azureconnstore's azure_connections,
// gcpconnstore's gcp_connections, and iacconnstore's
// iac_connections.
//
// The design doc (§5) calls for an index on tenancy_ocid so chunk
// 3's "find the connection for this tenancy" lookup is O(log n).
// The substrate does NOT enforce uniqueness on tenancy_ocid —
// operators may legitimately connect the same tenancy twice with
// different users (different role scopes).
//
// Region is NOT NULL because OCI's API endpoints are regional
// (unlike AWS/GCP/Azure where empty Region means "scan all"). The
// substrate-level NOT NULL is defense in depth around the chunk 3
// wizard validation; both must hold for a valid row to land.
//
// learn_from_accepted_recommendations defaults to 1 (true) at the
// SQL layer, mirroring the Azure/GCP/iac shapes; the Go-side
// Create stamps the same default when the caller leaves the zero
// value.
const migration0001OCIConnections = `
-- Schema version tracker, mirrors the convention used by credstore,
-- iacconnstore, gcpconnstore, and azureconnstore.
CREATE TABLE IF NOT EXISTS schema_version (
	version INTEGER PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS oci_connections (
	id                                  TEXT PRIMARY KEY,
	display_name                        TEXT NOT NULL,
	tenancy_ocid                        TEXT NOT NULL,
	user_ocid                           TEXT NOT NULL,
	fingerprint                         TEXT NOT NULL,
	sealed_private_key                  BLOB NOT NULL,
	region                              TEXT NOT NULL,
	learn_from_accepted_recommendations INTEGER NOT NULL DEFAULT 1,
	created_at                          TEXT NOT NULL,
	updated_at                          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_oci_connections_tenancy_ocid
	ON oci_connections (tenancy_ocid);

INSERT OR IGNORE INTO schema_version (version) VALUES (1);
`

// migration0002SquadronTenantID — ADR 0013 §D6-b (multi-tenancy slice
// D6-b). Adds the Squadron owner-tenant column that keys each OCI
// connection to the tenant that owns it.
//
// ⚠️ This is DISTINCT from the existing tenancy_ocid column, which is
// the OCI tenancy OCID (a cloud-side identifier, ocid1.tenancy...).
// The Squadron owner tenant is a separate namespace — hence the
// squadron_tenant_id column name / OwnerTenantID struct field.
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
ALTER TABLE oci_connections ADD COLUMN squadron_tenant_id TEXT NOT NULL DEFAULT 'default';
INSERT OR IGNORE INTO schema_version (version) VALUES (2);
`

// migrations is the ordered list of schema migrations. Index N is the
// SQL applied at version N+1. New entries are appended; existing
// entries are never edited.
var migrations = []string{
	migration0001OCIConnections,
	migration0002SquadronTenantID,
}
