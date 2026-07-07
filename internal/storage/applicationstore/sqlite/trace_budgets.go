// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetTraceBudget returns the persisted per-tenant trace-index row budget
// (ADR 0026). ok=false means no override → callers fall back to the global cap.
func (s *Storage) GetTraceBudget(ctx context.Context, tenant string) (int, bool, error) {
	var maxRows int
	err := s.db.QueryRowContext(ctx,
		`SELECT max_rows FROM trace_budgets WHERE tenant_id = ?`, tenant).Scan(&maxRows)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get trace budget for %q: %w", tenant, err)
	}
	return maxRows, true, nil
}

// ListTraceBudgets returns all persisted per-tenant budgets (ADR 0026).
func (s *Storage) ListTraceBudgets(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, max_rows FROM trace_budgets ORDER BY tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("list trace budgets: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var tenant string
		var maxRows int
		if err := rows.Scan(&tenant, &maxRows); err != nil {
			return nil, fmt.Errorf("scan trace budget: %w", err)
		}
		out[tenant] = maxRows
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trace budgets: %w", err)
	}
	return out, nil
}

// SetTraceBudget upserts a per-tenant budget (ADR 0026). max_rows must be
// positive; delete the override to clear it.
func (s *Storage) SetTraceBudget(ctx context.Context, tenant string, maxRows int) error {
	if tenant == "" {
		return fmt.Errorf("trace budget: tenant must not be empty")
	}
	if maxRows <= 0 {
		return fmt.Errorf("trace budget: max_rows must be positive (got %d); delete the override to clear it", maxRows)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO trace_budgets (tenant_id, max_rows, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(tenant_id) DO UPDATE SET max_rows = excluded.max_rows, updated_at = excluded.updated_at`,
		tenant, maxRows, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("set trace budget for %q: %w", tenant, err)
	}
	return nil
}

// DeleteTraceBudget removes a per-tenant override (ADR 0026); the tenant then
// falls back to the global cap.
func (s *Storage) DeleteTraceBudget(ctx context.Context, tenant string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM trace_budgets WHERE tenant_id = ?`, tenant); err != nil {
		return fmt.Errorf("delete trace budget for %q: %w", tenant, err)
	}
	return nil
}

// SeedTraceBudgets inserts config-provided budgets WITHOUT overwriting existing
// runtime edits (INSERT OR IGNORE). The enterprise wire calls this at boot so
// config.TraceIndex.PerTenantMaxRows still seeds the runtime table (ADR 0026 D4).
func (s *Storage) SeedTraceBudgets(ctx context.Context, budgets map[string]int) error {
	for tenant, maxRows := range budgets {
		if tenant == "" || maxRows <= 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO trace_budgets (tenant_id, max_rows, updated_at) VALUES (?, ?, ?)`,
			tenant, maxRows, time.Now().UTC()); err != nil {
			return fmt.Errorf("seed trace budget for %q: %w", tenant, err)
		}
	}
	return nil
}
