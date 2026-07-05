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
//
// v2: AWS-specific aws_connections table replaced with the multi-cloud
//
//	cloud_connections table per docs/universal-discovery-design.md
//	decisions 2-4. Migration is destructive (DROP TABLE then CREATE
//	TABLE) because no deployment has real rows in v1 — the
//	credential substrate was shipped in Stream 2A but not wired into
//	any user-facing connector flow.
//
// v3: ADR 0013 §D6-b adds the Squadron owner-tenant column
//
//	(tenant_id) to cloud_connections so the discovery rescan
//	scheduler can scope its discovery_scans store writes to the
//	tenant that owns the connection. NOT NULL DEFAULT 'default' —
//	inert in OSS. This is the first ALTER-based migration in this
//	chain; the migrate runner gained an isDuplicateColumnErr guard
//	alongside it (see sqlite.go migrate).
const SchemaVersion = 3

// migration0001AWSConnections is the initial (v1) schema. Retained in
// the migration chain so a hypothetical v1 database upgrades correctly,
// but the v2 migration immediately drops the table — see the comment
// on migration0002CloudConnections for the rationale.
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

// migration0002CloudConnections replaces the AWS-specific
// aws_connections table with the multi-cloud cloud_connections table.
// This is the schema implementation of decisions 2-4 in
// docs/universal-discovery-design.md "Decisions locked in this
// revision":
//
//   - Provider column lets the same row hold AWS / GCP / Azure /
//     on-prem connections.
//   - ConnectionType column lets the same row hold API-discovered
//     cloud accounts and agent-polled on-prem sites.
//   - Regions stored as a JSON-encoded TEXT column so the schema does
//     not change when slice 3 extends single-region scans to
//     multi-region.
//   - credentials_ciphertext + credentials_nonce hold the provider-
//     specific authentication material; the substrate stores opaque
//     bytes and lets each provider's scanner unmarshal its own shape.
//
// The migration is destructive: aws_connections is dropped before
// cloud_connections is created. This is safe because the credential
// substrate was shipped in Stream 2A as architecture-only — no
// deployment has real rows in any aws_connections table. If a future
// migration needs to preserve data, it gets its own non-destructive
// pattern.
const migration0002CloudConnections = `
-- v1's aws_connections table is replaced wholesale. No real data
-- exists in any deployment so dropping is safe; this is the cleanest
-- way to land the multi-cloud schema without dragging a column-rename
-- migration through the chain.
DROP TABLE IF EXISTS aws_connections;

CREATE TABLE IF NOT EXISTS cloud_connections (
	account_id             TEXT PRIMARY KEY,
	provider               TEXT NOT NULL,
	connection_type        TEXT NOT NULL,
	display_name           TEXT NOT NULL,
	regions                TEXT NOT NULL, -- JSON array
	credentials_ciphertext BLOB NOT NULL,
	credentials_nonce      BLOB NOT NULL,
	created_at             TEXT NOT NULL,
	updated_at             TEXT NOT NULL
);

-- Provider is the most common filter — the audit timeline and the
-- per-provider scan dispatch both narrow by provider before doing
-- anything else. Indexed so List(filter.Provider) doesn't table-scan.
CREATE INDEX IF NOT EXISTS idx_cloud_connections_provider
	ON cloud_connections(provider);

INSERT OR IGNORE INTO schema_version (version) VALUES (2);
`

// migration0003TenantID — ADR 0013 §D6-b (multi-tenancy slice D6-b).
// Adds the Squadron owner-tenant column that keys each cloud
// connection (AWS at runtime) to the tenant that owns it. NOT NULL
// DEFAULT 'default' so pre-D6-b rows backfill to the OSS single-tenant
// sentinel — inert in OSS, where every connection is created under
// identity.DefaultTenant. The discovery rescan scheduler reads this
// column to scope its discovery_scans store writes to the connection's
// owning tenant (a scheduled rescan runs under WithSystemContext and
// carries no operator identity).
//
// This is the FIRST ALTER-based migration in the credstore chain.
// SQLite doesn't support ALTER TABLE ... ADD COLUMN IF NOT EXISTS; the
// migrate runner gained an isDuplicateColumnErr guard (see sqlite.go
// migrate) alongside this migration so re-running on an up-to-date
// database is a no-op — mirroring iacconnstore's migration0004TenantID.
const migration0003TenantID = `
ALTER TABLE cloud_connections ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default';
INSERT OR IGNORE INTO schema_version (version) VALUES (3);
`

// migrations is the ordered list of schema migrations. Index N is the
// SQL applied at version N+1. New entries are appended; existing
// entries are never edited (they ran against historical databases and
// editing them after the fact desynchronizes the schema across
// deployments).
var migrations = []string{
	migration0001AWSConnections,
	migration0002CloudConnections,
	migration0003TenantID,
}
