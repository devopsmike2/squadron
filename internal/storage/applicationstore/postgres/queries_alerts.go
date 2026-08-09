// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// queries_alerts.go — the Postgres port of saved queries + alert rules
// (ADR 0033 slice 4). Both are CRUD-only entities whose semantics mirror the
// sqlite backend EXACTLY:
//   - a missing Get returns (nil, nil);
//   - Update / Delete on a missing row returns "<entity> not found";
//   - ListSavedQueries orders by updated_at DESC; ListAlertRules orders by
//     name ASC.
//
// As with the agents/configs/rollouts ports, OSS tenant scoping is intentionally
// omitted here (types.SavedQuery / types.AlertRule carry no tenant field, the
// OSS path is single-tenant). In the sqlite backend the tenant predicate is a
// no-op for OSS (everything is DefaultTenant), so behavior is identical.

// ============================================================================
// Saved queries.
// ============================================================================

const savedQueryColumns = `id, name, description, query, tags, created_at, updated_at`

func (s *Storage) CreateSavedQuery(ctx context.Context, query *types.SavedQuery) error {
	tags, err := json.Marshal(query.Tags)
	if err != nil {
		return fmt.Errorf("marshal saved query tags: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO saved_queries (`+savedQueryColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		query.ID, query.Name, query.Description, query.Query, tags, query.CreatedAt, query.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create saved query: %w", err)
	}
	return nil
}

func (s *Storage) GetSavedQuery(ctx context.Context, id string) (*types.SavedQuery, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+savedQueryColumns+` FROM saved_queries WHERE id = $1`, id)
	sq, err := scanSavedQuery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sq, err
}

func (s *Storage) ListSavedQueries(ctx context.Context) ([]*types.SavedQuery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+savedQueryColumns+` FROM saved_queries ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list saved queries: %w", err)
	}
	defer rows.Close()
	var out []*types.SavedQuery
	for rows.Next() {
		sq, err := scanSavedQuery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sq)
	}
	return out, rows.Err()
}

func (s *Storage) UpdateSavedQuery(ctx context.Context, query *types.SavedQuery) error {
	tags, err := json.Marshal(query.Tags)
	if err != nil {
		return fmt.Errorf("marshal saved query tags: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE saved_queries SET name=$2, description=$3, query=$4, tags=$5, updated_at=$6
		WHERE id=$1`,
		query.ID, query.Name, query.Description, query.Query, tags, query.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update saved query: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("saved query not found: %s", query.ID)
	}
	return nil
}

func (s *Storage) DeleteSavedQuery(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM saved_queries WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete saved query: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("saved query not found: %s", id)
	}
	return nil
}

func scanSavedQuery(sc scanner) (*types.SavedQuery, error) {
	var (
		sq   types.SavedQuery
		tags []byte
	)
	if err := sc.Scan(&sq.ID, &sq.Name, &sq.Description, &sq.Query, &tags, &sq.CreatedAt, &sq.UpdatedAt); err != nil {
		return nil, err
	}
	if len(tags) > 0 {
		if err := json.Unmarshal(tags, &sq.Tags); err != nil {
			return nil, fmt.Errorf("unmarshal saved query tags: %w", err)
		}
	}
	return &sq, nil
}

// ============================================================================
// Alert rules.
// ============================================================================

const alertRuleColumns = `id, name, description, query, threshold_operator, threshold_value, interval_seconds, severity, enabled, webhook_url, created_at, updated_at`

func (s *Storage) CreateAlertRule(ctx context.Context, rule *types.AlertRule) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO alert_rules (`+alertRuleColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		rule.ID, rule.Name, rule.Description, rule.Query, string(rule.ThresholdOperator),
		rule.ThresholdValue, rule.IntervalSeconds, string(rule.Severity), rule.Enabled,
		rule.WebhookURL, rule.CreatedAt, rule.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create alert rule: %w", err)
	}
	return nil
}

func (s *Storage) GetAlertRule(ctx context.Context, id string) (*types.AlertRule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+alertRuleColumns+` FROM alert_rules WHERE id = $1`, id)
	r, err := scanAlertRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Storage) ListAlertRules(ctx context.Context) ([]*types.AlertRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+alertRuleColumns+` FROM alert_rules ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer rows.Close()
	var out []*types.AlertRule
	for rows.Next() {
		r, err := scanAlertRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Storage) UpdateAlertRule(ctx context.Context, rule *types.AlertRule) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE alert_rules SET name=$2, description=$3, query=$4, threshold_operator=$5,
			threshold_value=$6, interval_seconds=$7, severity=$8, enabled=$9, webhook_url=$10, updated_at=$11
		WHERE id=$1`,
		rule.ID, rule.Name, rule.Description, rule.Query, string(rule.ThresholdOperator),
		rule.ThresholdValue, rule.IntervalSeconds, string(rule.Severity), rule.Enabled,
		rule.WebhookURL, rule.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update alert rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("alert rule not found: %s", rule.ID)
	}
	return nil
}

func (s *Storage) DeleteAlertRule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM alert_rules WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete alert rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("alert rule not found: %s", id)
	}
	return nil
}

func scanAlertRule(sc scanner) (*types.AlertRule, error) {
	r := &types.AlertRule{}
	var op, severity string
	if err := sc.Scan(&r.ID, &r.Name, &r.Description, &r.Query, &op, &r.ThresholdValue,
		&r.IntervalSeconds, &severity, &r.Enabled, &r.WebhookURL, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.ThresholdOperator = types.ThresholdOperator(op)
	r.Severity = types.AlertSeverity(severity)
	return r, nil
}
