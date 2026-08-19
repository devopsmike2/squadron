// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// queries_automations.go — the Postgres port of automations (ADR 0038, first
// slice). CRUD-only entity whose semantics mirror the sqlite backend EXACTLY:
//   - a missing Get returns (nil, nil);
//   - Update / Delete on a missing row returns "automation not found";
//   - ListAutomations orders by name ASC.
//
// As with alert rules, OSS tenant scoping is omitted (single-tenant). Nullable
// agent_id / group_id round-trip through sql.NullString ⇄ *string.

const automationColumns = `id, name, enabled, agent_id, group_id, trigger_type, action_type, cooldown_seconds, max_attempts, dry_run, created_at, updated_at`

func (s *Storage) CreateAutomation(ctx context.Context, a *types.Automation) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO automations (`+automationColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		a.ID, a.Name, a.Enabled, strPtrToNull(a.AgentID), strPtrToNull(a.GroupID),
		string(a.Trigger), string(a.Action), a.CooldownSeconds, a.MaxAttempts, a.DryRun,
		a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create automation: %w", err)
	}
	return nil
}

func (s *Storage) GetAutomation(ctx context.Context, id string) (*types.Automation, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+automationColumns+` FROM automations WHERE id = $1`, id)
	a, err := scanAutomation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (s *Storage) ListAutomations(ctx context.Context) ([]*types.Automation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+automationColumns+` FROM automations ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list automations: %w", err)
	}
	defer rows.Close()
	var out []*types.Automation
	for rows.Next() {
		a, err := scanAutomation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Storage) UpdateAutomation(ctx context.Context, a *types.Automation) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE automations SET name=$2, enabled=$3, agent_id=$4, group_id=$5, trigger_type=$6,
			action_type=$7, cooldown_seconds=$8, max_attempts=$9, dry_run=$10, updated_at=$11
		WHERE id=$1`,
		a.ID, a.Name, a.Enabled, strPtrToNull(a.AgentID), strPtrToNull(a.GroupID),
		string(a.Trigger), string(a.Action), a.CooldownSeconds, a.MaxAttempts, a.DryRun,
		a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update automation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("automation not found: %s", a.ID)
	}
	return nil
}

func (s *Storage) DeleteAutomation(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM automations WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("delete automation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("automation not found: %s", id)
	}
	return nil
}

func scanAutomation(sc scanner) (*types.Automation, error) {
	a := &types.Automation{}
	var (
		trigger, action  string
		agentID, groupID sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.Name, &a.Enabled, &agentID, &groupID, &trigger, &action,
		&a.CooldownSeconds, &a.MaxAttempts, &a.DryRun, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Trigger = types.AutomationTrigger(trigger)
	a.Action = types.AutomationAction(action)
	a.AgentID = strPtrFromNull(agentID)
	a.GroupID = strPtrFromNull(groupID)
	return a, nil
}

// strPtrToNull maps a *string to a driver value: NULL when nil, else the value.
func strPtrToNull(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// strPtrFromNull maps a scanned sql.NullString back to a *string.
func strPtrFromNull(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}
