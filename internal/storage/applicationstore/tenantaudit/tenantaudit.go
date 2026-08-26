// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package tenantaudit is the shared, read-only ADR 0043 commingling scan used by
// both SQL application-store backends (sqlite, postgres). It runs three cheap
// aggregate reads per tenant-bearing table — total rows, unstamped (NULL/empty
// tenant_id) rows, and DISTINCT tenant count — plus a small sample of unstamped
// ids. It MUTATES NOTHING and issues no tenant predicate (it must see across all
// tenants), so callers invoke it under a system context.
//
// The SQL is dialect-neutral (COUNT / COUNT(DISTINCT) / IS NULL / LIMIT all work
// identically on SQLite and Postgres), so both backends share this one
// implementation and pass their own table list.
package tenantaudit

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Table names one tenant-bearing table and the natural-key column to sample when
// reporting unstamped rows.
type Table struct {
	Name  string
	IDCol string
}

// Querier is the minimal read surface both backends' *sql.DB satisfies.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Run scans every table and assembles the report. A missing table (e.g. a
// backend that never created it) is skipped rather than failing the whole audit,
// so the same table list can be reused as the schema evolves.
func Run(ctx context.Context, db Querier, backend string, tables []Table) (*types.TenantComminglingReport, error) {
	rep := &types.TenantComminglingReport{
		Backend:     backend,
		GeneratedAt: time.Now().UTC(),
	}
	for _, tbl := range tables {
		tr, ok, err := scanTable(ctx, db, tbl)
		if err != nil {
			return nil, fmt.Errorf("audit table %s: %w", tbl.Name, err)
		}
		if !ok {
			continue // table absent on this backend
		}
		if tr.Commingled {
			rep.AnyCommingled = true
		}
		if tr.UnstampedRows > 0 {
			rep.AnyUnstamped = true
		}
		rep.Tables = append(rep.Tables, tr)
	}
	return rep, nil
}

func scanTable(ctx context.Context, db Querier, tbl Table) (types.TenantComminglingTableReport, bool, error) {
	tr := types.TenantComminglingTableReport{Table: tbl.Name}

	// Total rows — also our table-exists probe: a missing table errors here and
	// we report "not ok" so Run skips it.
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+tbl.Name).Scan(&tr.TotalRows); err != nil {
		// Table absent (or otherwise unreadable) — skip, don't fail the audit.
		return tr, false, nil //nolint:nilerr // a missing table is a skip, not an error
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+tbl.Name+` WHERE tenant_id IS NULL OR tenant_id = ''`).Scan(&tr.UnstampedRows); err != nil {
		return tr, false, err
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT tenant_id) FROM `+tbl.Name+` WHERE tenant_id IS NOT NULL AND tenant_id <> ''`).Scan(&tr.DistinctTenants); err != nil {
		return tr, false, err
	}
	tr.Commingled = tr.DistinctTenants > 1

	if tr.UnstampedRows > 0 {
		rows, err := db.QueryContext(ctx,
			`SELECT `+tbl.IDCol+` FROM `+tbl.Name+` WHERE tenant_id IS NULL OR tenant_id = '' LIMIT 5`)
		if err != nil {
			return tr, false, err
		}
		defer rows.Close()
		for rows.Next() {
			var id sql.NullString
			if err := rows.Scan(&id); err != nil {
				return tr, false, err
			}
			if id.Valid {
				tr.SampleUnstampedIDs = append(tr.SampleUnstampedIDs, id.String)
			}
		}
		if err := rows.Err(); err != nil {
			return tr, false, err
		}
	}
	return tr, true, nil
}
