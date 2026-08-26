// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// cost_spike.go — the Postgres port of cost-spike events (v0.29), ADR 0033 slice
// 6. Semantics mirror the sqlite backend EXACTLY:
//   - CreateCostSpikeEvent requires a non-empty ID, defaults StartedAt to now and
//     Severity to "warn";
//   - UpdateCostSpikeEvent overwrites the mutable fields (bump peak / close via
//     ended_at / acknowledge); no missing-row error;
//   - GetCostSpikeEvent / LatestOpenCostSpike return (nil, nil) on a miss;
//   - ListCostSpikeEvents filters open (ended_at IS NULL) / closed (NOT NULL) /
//     all, orders started_at DESC, and applies Limit only when > 0;
//   - LatestOpenCostSpike returns the newest open spike (ended_at IS NULL).
//
// ended_at + acknowledged_at are nullable TIMESTAMPTZ via nullTime; USD/pct
// columns are DOUBLE PRECISION. OSS tenant scoping is omitted (the type carries
// no tenant field).

const costSpikeColumns = `id, started_at, ended_at, severity, signal, ` +
	`baseline_monthly_usd, peak_monthly_usd, peak_pct_above_baseline, ` +
	`attribution_json, acknowledged_at, acknowledged_by`

func (s *Storage) CreateCostSpikeEvent(ctx context.Context, e *types.CostSpikeEvent) error {
	if e == nil || e.ID == "" {
		return fmt.Errorf("id required")
	}
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now().UTC()
	}
	if e.Severity == "" {
		e.Severity = "warn"
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return err
	}
	if !apply {
		tenant = identity.DefaultTenant
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO cost_spike_events (`+costSpikeColumns+`, tenant_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		e.ID, e.StartedAt.UTC(), nullTime(e.EndedAt), e.Severity, e.Signal,
		e.BaselineMonthlyUSD, e.PeakMonthlyUSD, e.PeakPctAboveBaseline,
		e.AttributionJSON, nullTime(e.AcknowledgedAt), e.AcknowledgedBy, tenant)
	if err != nil {
		return fmt.Errorf("create cost spike event: %w", err)
	}
	return nil
}

func (s *Storage) UpdateCostSpikeEvent(ctx context.Context, e *types.CostSpikeEvent) error {
	if e == nil || e.ID == "" {
		return fmt.Errorf("id required")
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return err
	}
	query := `
		UPDATE cost_spike_events SET
			ended_at = $2, severity = $3, signal = $4,
			baseline_monthly_usd = $5, peak_monthly_usd = $6,
			peak_pct_above_baseline = $7, attribution_json = $8,
			acknowledged_at = $9, acknowledged_by = $10
		WHERE id = $1`
	args := []any{
		e.ID, nullTime(e.EndedAt), e.Severity, e.Signal,
		e.BaselineMonthlyUSD, e.PeakMonthlyUSD, e.PeakPctAboveBaseline,
		e.AttributionJSON, nullTime(e.AcknowledgedAt), e.AcknowledgedBy,
	}
	if apply {
		query += ` AND tenant_id = $11`
		args = append(args, tenant)
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("update cost spike event: %w", err)
	}
	return nil
}

func (s *Storage) GetCostSpikeEvent(ctx context.Context, id string) (*types.CostSpikeEvent, error) {
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + costSpikeColumns + ` FROM cost_spike_events WHERE id = $1`
	args := []any{id}
	if apply {
		query += ` AND tenant_id = $2`
		args = append(args, tenant)
	}
	row := s.db.QueryRowContext(ctx, query, args...)
	e, err := scanCostSpikeEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

func (s *Storage) ListCostSpikeEvents(ctx context.Context, filter types.CostSpikeFilter) ([]*types.CostSpikeEvent, error) {
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + costSpikeColumns + ` FROM cost_spike_events WHERE 1=1`
	var args []any
	if apply {
		query += ` AND tenant_id = $1`
		args = append(args, tenant)
	}
	switch filter.Status {
	case "open":
		query += " AND ended_at IS NULL"
	case "closed":
		query += " AND ended_at IS NOT NULL"
	}
	query += " ORDER BY started_at DESC"
	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list cost spike events: %w", err)
	}
	defer rows.Close()
	var out []*types.CostSpikeEvent
	for rows.Next() {
		e, err := scanCostSpikeEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Storage) LatestOpenCostSpike(ctx context.Context) (*types.CostSpikeEvent, error) {
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + costSpikeColumns + `
		FROM cost_spike_events WHERE ended_at IS NULL`
	var args []any
	if apply {
		query += ` AND tenant_id = $1`
		args = append(args, tenant)
	}
	query += ` ORDER BY started_at DESC LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, args...)
	e, err := scanCostSpikeEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

func scanCostSpikeEvent(sc scanner) (*types.CostSpikeEvent, error) {
	e := &types.CostSpikeEvent{}
	var endedAt, ackAt sql.NullTime
	if err := sc.Scan(&e.ID, &e.StartedAt, &endedAt, &e.Severity, &e.Signal,
		&e.BaselineMonthlyUSD, &e.PeakMonthlyUSD, &e.PeakPctAboveBaseline,
		&e.AttributionJSON, &ackAt, &e.AcknowledgedBy); err != nil {
		return nil, err
	}
	if endedAt.Valid {
		t := endedAt.Time
		e.EndedAt = &t
	}
	if ackAt.Valid {
		t := ackAt.Time
		e.AcknowledgedAt = &t
	}
	return e, nil
}
