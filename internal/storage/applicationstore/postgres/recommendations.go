// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// recommendations.go — the Postgres port of recommendation dismissals (v0.25) and
// recommendation outcomes (v0.28), ADR 0033 slice 6. Semantics mirror the sqlite
// backend EXACTLY:
//   - DismissRecommendation upserts on recommendation_id (repeat dismissals
//     refresh dismissed_at + reason); defaults DismissedAt to now and DismissedBy
//     to "system";
//   - RestoreRecommendation is a plain DELETE, idempotent (no missing-row error);
//   - IsRecommendationDismissed is a COUNT(*) existence probe;
//   - ListRecommendationDismissals returns all rows, newest-first;
//   - CreateRecommendationOutcome defaults AppliedAt to now, Status to "pending",
//     AppliedBy to "system", and stores last_observed_at as NULL when zero;
//   - UpdateRecommendationOutcome mutates ONLY the running-observation columns
//     (the frozen snapshot stays immutable); no missing-row error;
//   - ListRecommendationOutcomes returns all rows, newest-apply-first.
//
// As with the earlier ports, OSS tenant scoping is intentionally omitted here
// (neither type carries a tenant field; OSS is single-tenant).

// ============================================================================
// Recommendation dismissals.
// ============================================================================

func (s *Storage) DismissRecommendation(ctx context.Context, d *types.RecommendationDismissal) error {
	if d == nil || d.RecommendationID == "" {
		return fmt.Errorf("recommendation_id required")
	}
	when := d.DismissedAt
	if when.IsZero() {
		when = time.Now().UTC()
	}
	by := d.DismissedBy
	if by == "" {
		by = "system"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO recommendation_dismissals (recommendation_id, dismissed_at, dismissed_by, reason)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (recommendation_id) DO UPDATE SET
			dismissed_at = excluded.dismissed_at,
			dismissed_by = excluded.dismissed_by,
			reason       = excluded.reason`,
		d.RecommendationID, when, by, d.Reason)
	if err != nil {
		return fmt.Errorf("dismiss recommendation: %w", err)
	}
	return nil
}

func (s *Storage) RestoreRecommendation(ctx context.Context, recommendationID string) error {
	if recommendationID == "" {
		return fmt.Errorf("recommendation_id required")
	}
	// Idempotent: restoring an already-restored / never-dismissed id is a no-op.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM recommendation_dismissals WHERE recommendation_id = $1`, recommendationID); err != nil {
		return fmt.Errorf("restore recommendation: %w", err)
	}
	return nil
}

func (s *Storage) IsRecommendationDismissed(ctx context.Context, recommendationID string) (bool, error) {
	if recommendationID == "" {
		return false, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recommendation_dismissals WHERE recommendation_id = $1`, recommendationID).Scan(&n); err != nil {
		return false, fmt.Errorf("check dismissal: %w", err)
	}
	return n > 0, nil
}

func (s *Storage) ListRecommendationDismissals(ctx context.Context) ([]*types.RecommendationDismissal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT recommendation_id, dismissed_at, dismissed_by, COALESCE(reason, '')
		FROM recommendation_dismissals ORDER BY dismissed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list dismissals: %w", err)
	}
	defer rows.Close()
	out := []*types.RecommendationDismissal{}
	for rows.Next() {
		d := &types.RecommendationDismissal{}
		if err := rows.Scan(&d.RecommendationID, &d.DismissedAt, &d.DismissedBy, &d.Reason); err != nil {
			return nil, fmt.Errorf("scan dismissal row: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ============================================================================
// Recommendation outcomes.
// ============================================================================

func (s *Storage) CreateRecommendationOutcome(ctx context.Context, o *types.RecommendationOutcome) error {
	if o == nil || o.ID == "" || o.RecommendationID == "" {
		return fmt.Errorf("id + recommendation_id required")
	}
	if o.AppliedAt.IsZero() {
		o.AppliedAt = time.Now().UTC()
	}
	if o.Status == "" {
		o.Status = "pending"
	}
	if o.AppliedBy == "" {
		o.AppliedBy = "system"
	}
	var lastObs any
	if !o.LastObservedAt.IsZero() {
		lastObs = o.LastObservedAt
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO recommendation_outcomes
			(id, recommendation_id, applied_at, applied_by, title, category,
			 signal, attribute_key, baseline_bytes_per_hour,
			 est_savings_per_month_usd_at_apply, last_observed_bytes_per_hour,
			 last_observed_at, realized_savings_per_month_usd, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		o.ID, o.RecommendationID, o.AppliedAt, o.AppliedBy, o.Title, o.Category,
		o.Signal, o.AttributeKey, o.BaselineBytesPerHour,
		o.EstSavingsPerMonthUSDAtApply, o.LastObservedBytesPerHour,
		lastObs, o.RealizedSavingsPerMonthUSD, o.Status)
	if err != nil {
		return fmt.Errorf("create recommendation outcome: %w", err)
	}
	return nil
}

// UpdateRecommendationOutcome refreshes only the running-observation columns; the
// frozen snapshot fields stay immutable. No missing-row error (mirrors sqlite).
func (s *Storage) UpdateRecommendationOutcome(ctx context.Context, o *types.RecommendationOutcome) error {
	if o == nil || o.ID == "" {
		return fmt.Errorf("id required")
	}
	var lastObs any
	if !o.LastObservedAt.IsZero() {
		lastObs = o.LastObservedAt
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE recommendation_outcomes SET
			last_observed_bytes_per_hour = $2,
			last_observed_at = $3,
			realized_savings_per_month_usd = $4,
			status = $5
		WHERE id = $1`,
		o.ID, o.LastObservedBytesPerHour, lastObs, o.RealizedSavingsPerMonthUSD, o.Status)
	if err != nil {
		return fmt.Errorf("update recommendation outcome: %w", err)
	}
	return nil
}

func (s *Storage) ListRecommendationOutcomes(ctx context.Context) ([]*types.RecommendationOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, recommendation_id, applied_at, applied_by, title, category,
		       signal, attribute_key, baseline_bytes_per_hour,
		       est_savings_per_month_usd_at_apply, last_observed_bytes_per_hour,
		       last_observed_at, realized_savings_per_month_usd, status
		FROM recommendation_outcomes ORDER BY applied_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list outcomes: %w", err)
	}
	defer rows.Close()
	out := []*types.RecommendationOutcome{}
	for rows.Next() {
		o := &types.RecommendationOutcome{}
		var lastObs sql.NullTime
		if err := rows.Scan(&o.ID, &o.RecommendationID, &o.AppliedAt, &o.AppliedBy,
			&o.Title, &o.Category, &o.Signal, &o.AttributeKey,
			&o.BaselineBytesPerHour, &o.EstSavingsPerMonthUSDAtApply,
			&o.LastObservedBytesPerHour, &lastObs,
			&o.RealizedSavingsPerMonthUSD, &o.Status); err != nil {
			return nil, fmt.Errorf("scan outcome row: %w", err)
		}
		if lastObs.Valid {
			o.LastObservedAt = lastObs.Time
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
