// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"context"
	"time"
)

// tenant_audit.go — ADR 0043 read-only commingling audit. Before an operator
// ARMS strict tenant scoping (which fail-closes cross-tenant access on Postgres),
// they run this audit as a preflight: it reports, per tenant-bearing table, how
// many rows carry no tenant (NULL/empty tenant_id — legacy rows written before
// the ADR 0043 columns landed) and how many DISTINCT tenants a table already
// holds. More than one distinct tenant in a table that was assumed single-tenant
// means genuinely COMMINGLED data — arming strict then would (correctly) start
// denying cross-tenant reads, so the operator must reconcile first. The audit
// MUTATES NOTHING.

// TenantComminglingTableReport is the per-table finding.
type TenantComminglingTableReport struct {
	Table string `json:"table"`
	// TotalRows is the row count of the table.
	TotalRows int64 `json:"total_rows"`
	// UnstampedRows counts rows with a NULL or empty tenant_id (legacy rows the
	// no-blind-backfill migration left for exactly this audit to surface).
	UnstampedRows int64 `json:"unstamped_rows"`
	// DistinctTenants counts DISTINCT non-null tenant_id values.
	DistinctTenants int64 `json:"distinct_tenants"`
	// Commingled is true when DistinctTenants > 1 — the table already holds more
	// than one tenant's rows.
	Commingled bool `json:"commingled"`
	// SampleUnstampedIDs is up to a handful of natural-key ids of unstamped rows,
	// so an operator can locate and reconcile them.
	SampleUnstampedIDs []string `json:"sample_unstamped_ids,omitempty"`
}

// TenantComminglingReport is the whole preflight result.
type TenantComminglingReport struct {
	Backend     string                         `json:"backend"`
	GeneratedAt time.Time                      `json:"generated_at"`
	Tables      []TenantComminglingTableReport `json:"tables"`
	// AnyCommingled is true if ANY table has more than one distinct tenant.
	AnyCommingled bool `json:"any_commingled"`
	// AnyUnstamped is true if ANY table has NULL/empty tenant rows.
	AnyUnstamped bool `json:"any_unstamped"`
}

// SafeToArmStrict reports whether arming strict tenant scoping is safe: no table
// holds more than one tenant's rows. Unstamped (legacy) rows alone do not block
// arming — under strict they simply become invisible to tenant-scoped reads and
// visible only to system-context jobs — but genuine commingling does, because a
// tenant would abruptly lose rows that legitimately look like a sibling's.
func (r *TenantComminglingReport) SafeToArmStrict() bool { return !r.AnyCommingled }

// TenantComminglingAuditor is the OPTIONAL store capability the audit CLI
// type-asserts. Only the real SQL backends (sqlite, postgres) implement it; the
// in-memory store does not need to (it is never an operator's production data).
type TenantComminglingAuditor interface {
	AuditTenantCommingling(ctx context.Context) (*TenantComminglingReport, error)
}
