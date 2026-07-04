// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// tenant_rebuild.go — ADR 0011 slice 3b′ (composite-key table rebuilds).
//
// Slice 3b added a `tenant_id` COLUMN + `idx_<t>_tenant` index to every
// per-tenant table and tenant-scoped every read/write. But six tables still
// carried a SINGLE-column PRIMARY KEY / UNIQUE on their natural key
// (expected_agents.hostname, recommendation_dismissals.recommendation_id,
// iac_recommendation_verdicts.recommendation_id, trace_resource_seen.resource_key,
// action_runner_registrations.runner_id, alert_rules.name). Two tenants that
// shared a natural key would COLLIDE on write — tenant B's upsert would
// overwrite tenant A's row. This file closes that gap by rebuilding each of
// the six tables so the key becomes composite with tenant_id.
//
// SQLite cannot ALTER a PRIMARY KEY or UNIQUE constraint in place, so each
// rebuild follows the canonical CREATE-new / copy / drop / rename recipe.
//
// Why this lives OUTSIDE the idempotent `migrations []string` swallow loop in
// migrate(): that loop swallows every non-duplicate-column error
// (isColumnExistsError), which is correct for `ADD COLUMN` idempotency but
// catastrophic for a table rebuild — a half-failed rebuild (new table created,
// copy failed) would be silently eaten and leave the DB corrupt. Instead each
// rebuild runs in its own transaction and PROPAGATES its error up through
// runTenantKeyRebuilds → migrate(), so a failed rebuild fails boot loudly
// rather than corrupting data. Each rebuild is also GUARDED by an
// "already migrated?" probe (the tenant_id column is part of the key), so
// re-running migrate() against an already-rebuilt DB is a no-op.
//
// INERT IN OSS: OSS runs a single `default` tenant, so a composite
// `(tenant_id, key)` with tenant_id constant behaves identically to the old
// single-column `key`. The isolation only becomes load-bearing under the
// enterprise multi-tenant resolver.

// tenantRebuild describes one composite-key table rebuild. Rather than
// hard-code column lists (drift-prone against createTables + the migrations
// ADD COLUMNs), the rebuild reads the live column list from
// PRAGMA table_info and the live index set from sqlite_master, so the new
// table is a faithful copy of the current shape with only the key changed.
type tenantRebuild struct {
	// table is the table being rebuilt.
	table string
	// keyCols is the natural key that becomes composite with tenant_id.
	// For a PK rebuild this is the old PRIMARY KEY column(s); for
	// alert_rules it is the old UNIQUE column (name).
	keyCols []string
	// unique, when true, emits `UNIQUE(tenant_id, keyCols...)` and keeps the
	// table's own PRIMARY KEY (already present in the copied column list)
	// intact (alert_rules — PK stays `id`). When false, emits
	// `PRIMARY KEY(tenant_id, keyCols...)` (the five natural-key-PK tables).
	unique bool
}

// tenantRebuilds is the deterministic, ordered list of the six composite-key
// rebuilds. Each is independent; order is fixed only for reproducibility.
var tenantRebuilds = []tenantRebuild{
	{table: "expected_agents", keyCols: []string{"hostname"}},
	{table: "recommendation_dismissals", keyCols: []string{"recommendation_id"}},
	{table: "iac_recommendation_verdicts", keyCols: []string{"recommendation_id"}},
	{table: "trace_resource_seen", keyCols: []string{"resource_key"}},
	{table: "action_runner_registrations", keyCols: []string{"runner_id"}},
	{table: "alert_rules", keyCols: []string{"name"}, unique: true},
}

// runTenantKeyRebuilds performs the six ADR 0011 slice 3b′ composite-key
// table rebuilds. It runs AFTER createTables + the swallow-`migrations` loop
// in migrate(), so the tenant_id column already exists on every table and
// existing rows are already backfilled to 'default'. Any error is propagated
// (boot fails loudly) rather than swallowed. Each rebuild is guarded so this
// is safe to call repeatedly.
func runTenantKeyRebuilds(db *sql.DB, logger *zap.Logger) error {
	for _, rb := range tenantRebuilds {
		if err := rebuildTenantKeyTable(db, logger, rb); err != nil {
			return fmt.Errorf("tenant-key rebuild of %q: %w", rb.table, err)
		}
	}
	return nil
}

// rebuildTenantKeyTable rebuilds a single table so its key includes tenant_id.
// Guarded (no-op if already migrated), transactional (rollback on any error),
// error-propagating.
func rebuildTenantKeyTable(db *sql.DB, logger *zap.Logger, rb tenantRebuild) error {
	// Idempotency guard: has this table already been rebuilt with a composite
	// key that includes tenant_id?
	already, err := hasCompositeTenantKey(db, rb)
	if err != nil {
		return fmt.Errorf("guard probe: %w", err)
	}
	if already {
		logger.Debug("tenant-key rebuild skipped (already migrated)", zap.String("table", rb.table))
		return nil
	}

	// Enumerate the current columns (in ordinal order) and the current
	// index-creation DDL BEFORE we drop the table.
	cols, colDefs, err := tableColumns(db, rb.table)
	if err != nil {
		return fmt.Errorf("read columns: %w", err)
	}
	if len(cols) == 0 {
		return fmt.Errorf("table %q has no columns (missing?)", rb.table)
	}
	indexDDL, err := tableIndexDDL(db, rb.table)
	if err != nil {
		return fmt.Errorf("read indexes: %w", err)
	}

	// Pre-count for the post-condition check.
	var preCount int
	if err := db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %q`, rb.table)).Scan(&preCount); err != nil {
		return fmt.Errorf("pre-count: %w", err)
	}

	// The rebuild runs in a single transaction. foreign_keys must be toggled
	// OFF at the connection level OUTSIDE the transaction — SQLite ignores a
	// PRAGMA foreign_keys change inside an open transaction. None of the six
	// tables is an FK parent/child in practice, but we follow the canonical
	// recipe defensively so a future FK doesn't silently cascade-drop rows.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	// Best-effort re-enable on the way out (matches NewSQLiteStorage's ON).
	defer func() { _, _ = db.Exec(`PRAGMA foreign_keys=ON`) }()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	newTable := rb.table + "_new"

	// Build the key clause.
	tenantKey := append([]string{"tenant_id"}, rb.keyCols...)
	var keyClause string
	if rb.unique {
		// PK stays whatever the copied column defs declare (id for
		// alert_rules); add a composite UNIQUE on (tenant_id, name).
		keyClause = fmt.Sprintf(",\n\tUNIQUE(%s)", strings.Join(tenantKey, ", "))
	} else {
		keyClause = fmt.Sprintf(",\n\tPRIMARY KEY(%s)", strings.Join(tenantKey, ", "))
	}

	createNew := fmt.Sprintf("CREATE TABLE %q (\n\t%s%s\n)", newTable, strings.Join(colDefs, ",\n\t"), keyClause)
	if _, err := tx.Exec(createNew); err != nil {
		return fmt.Errorf("create %s: %w", newTable, err)
	}

	colList := strings.Join(quoteAll(cols), ", ")
	copyStmt := fmt.Sprintf(`INSERT INTO %q (%s) SELECT %s FROM %q`, newTable, colList, colList, rb.table)
	if _, err := tx.Exec(copyStmt); err != nil {
		return fmt.Errorf("copy rows: %w", err)
	}

	if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE %q`, rb.table)); err != nil {
		return fmt.Errorf("drop old table: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`ALTER TABLE %q RENAME TO %q`, newTable, rb.table)); err != nil {
		return fmt.Errorf("rename table: %w", err)
	}

	// Recreate every index that existed on the original table. DROP TABLE
	// removed them; the auto-index backing the old single-column UNIQUE
	// (alert_rules.name) is not in this list (it had no CREATE INDEX DDL) —
	// its replacement is the composite UNIQUE we just declared.
	for _, ddl := range indexDDL {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("recreate index (%s): %w", ddl, err)
		}
	}

	// Post-condition: row count preserved.
	var postCount int
	if err := tx.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %q`, rb.table)).Scan(&postCount); err != nil {
		return fmt.Errorf("post-count: %w", err)
	}
	if postCount != preCount {
		return fmt.Errorf("row count changed during rebuild: pre=%d post=%d", preCount, postCount)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	logger.Info("tenant-key rebuild complete",
		zap.String("table", rb.table),
		zap.Strings("composite_key", tenantKey),
		zap.Int("rows", postCount))
	return nil
}

// hasCompositeTenantKey reports whether the table's key already includes
// tenant_id — the idempotency guard. For a PK rebuild it checks whether
// tenant_id is part of the PRIMARY KEY (PRAGMA table_info pk>0). For a UNIQUE
// rebuild (alert_rules) it checks whether any UNIQUE index spans
// (tenant_id, <keyCols...>).
func hasCompositeTenantKey(db *sql.DB, rb tenantRebuild) (bool, error) {
	if rb.unique {
		return hasCompositeUniqueIndex(db, rb.table, append([]string{"tenant_id"}, rb.keyCols...))
	}
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, rb.table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == "tenant_id" && pk > 0 {
			return true, nil
		}
	}
	return false, rows.Err()
}

// hasCompositeUniqueIndex reports whether the table has a UNIQUE index whose
// exact column set (in order) matches want.
func hasCompositeUniqueIndex(db *sql.DB, table string, want []string) (bool, error) {
	idxRows, err := db.Query(fmt.Sprintf(`PRAGMA index_list(%q)`, table))
	if err != nil {
		return false, err
	}
	type idxMeta struct {
		name   string
		unique bool
	}
	var idxs []idxMeta
	for idxRows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := idxRows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			_ = idxRows.Close()
			return false, err
		}
		idxs = append(idxs, idxMeta{name: name, unique: unique == 1})
	}
	if err := idxRows.Err(); err != nil {
		_ = idxRows.Close()
		return false, err
	}
	_ = idxRows.Close()

	for _, idx := range idxs {
		if !idx.unique {
			continue
		}
		cols, err := indexColumns(db, idx.name)
		if err != nil {
			return false, err
		}
		if equalStrings(cols, want) {
			return true, nil
		}
	}
	return false, nil
}

// indexColumns returns the ordered column names of an index.
func indexColumns(db *sql.DB, indexName string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA index_info(%q)`, indexName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var (
			seqno int
			cid   int
			name  sql.NullString
		)
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return nil, err
		}
		if name.Valid {
			cols = append(cols, name.String)
		}
	}
	return cols, rows.Err()
}

// tableColumns returns the ordered column names and their reconstructed
// column-definition fragments (name + type + NOT NULL + DEFAULT) for a table,
// read from PRAGMA table_info. The fragments are used to CREATE the rebuilt
// table; a table-level PRIMARY KEY / UNIQUE is appended by the caller so the
// per-column defs here deliberately omit inline PK/UNIQUE.
func tableColumns(db *sql.DB, table string) (cols []string, colDefs []string, err error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, nil, err
		}
		cols = append(cols, name)
		var b strings.Builder
		fmt.Fprintf(&b, "%q", name)
		if ctype != "" {
			b.WriteString(" ")
			b.WriteString(ctype)
		}
		if notnull == 1 {
			b.WriteString(" NOT NULL")
		}
		if dflt.Valid {
			b.WriteString(" DEFAULT ")
			b.WriteString(dflt.String)
		}
		colDefs = append(colDefs, b.String())
	}
	return cols, colDefs, rows.Err()
}

// tableIndexDDL returns the CREATE INDEX statements for every explicitly
// created index on the table (origin 'c'), read from sqlite_master. Auto
// indexes (the implicit index backing an inline UNIQUE, origin 'u'/'pk') have
// a NULL sql and are excluded — their replacement is declared as a table-level
// constraint in the rebuilt CREATE TABLE.
func tableIndexDDL(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(
		`SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL`,
		table,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ddl sql.NullString
		if err := rows.Scan(&ddl); err != nil {
			return nil, err
		}
		if ddl.Valid && strings.TrimSpace(ddl.String) != "" {
			out = append(out, ddl.String)
		}
	}
	return out, rows.Err()
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
