// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// actions.go — the Postgres port of action runner registrations + action
// requests (ADR 0033 slice 5, the v0.53 "action runner" Move 2). Semantics
// mirror the sqlite backend EXACTLY:
//   - CreateActionRunnerRegistration defaults RegisteredAt to now and LastSeenAt
//     to RegisteredAt when zero;
//   - GetActionRunnerRegistration on a missing runner returns (nil, nil);
//   - ListActionRunnerRegistrations orders by registered_at DESC and INCLUDES
//     revoked runners (the UI splits them onto the inactive tab);
//   - UpdateActionRunnerRegistration / RevokeActionRunnerRegistration on a
//     missing runner return "action runner registration not found: <id>";
//   - CreateActionRequest defaults Status to "pending" when empty;
//   - GetActionRequest on a missing id returns (nil, nil);
//   - UpdateActionRequest on a missing id returns "action request not found:
//     <id>" and only touches the mutable result columns (status, denied_for,
//     outputs, started_at, completed_at) — the signed/immutable fields are left
//     as written at create time;
//   - ListActionRequests filters by proposal_id / runner_id / status, orders by
//     issued_at DESC, and defaults Limit to 100 (capped at 1000).
//
// public_key_pem, capabilities_json, parameters_json and signature are persisted
// VERBATIM (TEXT, never JSONB): the runner-side crypto verifies the exact bytes,
// so any reserialization would break signature/parse. As with the other OSS
// ports, tenant scoping is intentionally omitted (neither type carries a tenant
// field; the sqlite composite (tenant_id, runner_id) PK collapses to runner_id in
// single-tenant OSS).

// ============================================================================
// Action runner registrations.
// ============================================================================

func (s *Storage) CreateActionRunnerRegistration(ctx context.Context, r *types.ActionRunnerRegistration) error {
	if r.RegisteredAt.IsZero() {
		r.RegisteredAt = time.Now().UTC()
	}
	if r.LastSeenAt.IsZero() {
		r.LastSeenAt = r.RegisteredAt
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO action_runner_registrations
		  (runner_id, hostname, public_key_pem, capabilities_json, registered_at, last_seen_at, revoked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		r.RunnerID, r.Hostname, r.PublicKeyPEM, r.CapabilitiesJSON,
		r.RegisteredAt, r.LastSeenAt, r.RevokedAt)
	if err != nil {
		return fmt.Errorf("create action runner registration: %w", err)
	}
	return nil
}

func (s *Storage) UpdateActionRunnerRegistration(ctx context.Context, r *types.ActionRunnerRegistration) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE action_runner_registrations
		   SET hostname = $2, public_key_pem = $3, capabilities_json = $4, last_seen_at = $5, revoked_at = $6
		 WHERE runner_id = $1`,
		r.RunnerID, r.Hostname, r.PublicKeyPEM, r.CapabilitiesJSON, r.LastSeenAt, r.RevokedAt)
	if err != nil {
		return fmt.Errorf("update action runner registration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("action runner registration not found: %s", r.RunnerID)
	}
	return nil
}

func (s *Storage) GetActionRunnerRegistration(ctx context.Context, runnerID string) (*types.ActionRunnerRegistration, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT runner_id, hostname, public_key_pem, capabilities_json, registered_at, last_seen_at, revoked_at
		  FROM action_runner_registrations WHERE runner_id = $1`, runnerID)
	r, err := scanActionRunnerRegistration(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Storage) ListActionRunnerRegistrations(ctx context.Context) ([]*types.ActionRunnerRegistration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT runner_id, hostname, public_key_pem, capabilities_json, registered_at, last_seen_at, revoked_at
		  FROM action_runner_registrations ORDER BY registered_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list action runner registrations: %w", err)
	}
	defer rows.Close()
	var out []*types.ActionRunnerRegistration
	for rows.Next() {
		r, err := scanActionRunnerRegistration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Storage) RevokeActionRunnerRegistration(ctx context.Context, runnerID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE action_runner_registrations SET revoked_at = $2 WHERE runner_id = $1`, runnerID, at)
	if err != nil {
		return fmt.Errorf("revoke action runner registration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("action runner registration not found: %s", runnerID)
	}
	return nil
}

func scanActionRunnerRegistration(sc scanner) (*types.ActionRunnerRegistration, error) {
	r := &types.ActionRunnerRegistration{}
	var revokedAt sql.NullTime
	if err := sc.Scan(&r.RunnerID, &r.Hostname, &r.PublicKeyPEM, &r.CapabilitiesJSON,
		&r.RegisteredAt, &r.LastSeenAt, &revokedAt); err != nil {
		return nil, err
	}
	if revokedAt.Valid {
		t := revokedAt.Time
		r.RevokedAt = &t
	}
	return r, nil
}

// ============================================================================
// Action requests.
// ============================================================================

const actionRequestColumns = `id, proposal_id, runner_id, action_type, parameters_json, signature, ` +
	`phase, status, denied_for, dry_run_output_json, execution_output_json, ` +
	`issued_at, expires_at, started_at, completed_at`

func (s *Storage) CreateActionRequest(ctx context.Context, r *types.ActionRequest) error {
	if r.Status == "" {
		r.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO action_requests (`+actionRequestColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		r.ID, nullString(r.ProposalID), r.RunnerID, r.ActionType, r.ParametersJSON,
		r.Signature, r.Phase, r.Status, nullString(r.DeniedFor),
		nullString(r.DryRunOutputJSON), nullString(r.ExecutionOutputJSON),
		r.IssuedAt, r.ExpiresAt, r.StartedAt, r.CompletedAt)
	if err != nil {
		return fmt.Errorf("create action request: %w", err)
	}
	return nil
}

// UpdateActionRequest overwrites only the mutable result columns; the signed /
// immutable fields (id, signature, parameters, phase, issued_at, expires_at) are
// left as written at create time, mirroring the sqlite backend.
func (s *Storage) UpdateActionRequest(ctx context.Context, r *types.ActionRequest) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE action_requests
		   SET status = $2, denied_for = $3,
		       dry_run_output_json = $4, execution_output_json = $5,
		       started_at = $6, completed_at = $7
		 WHERE id = $1`,
		r.ID, r.Status, nullString(r.DeniedFor),
		nullString(r.DryRunOutputJSON), nullString(r.ExecutionOutputJSON),
		r.StartedAt, r.CompletedAt)
	if err != nil {
		return fmt.Errorf("update action request: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("action request not found: %s", r.ID)
	}
	return nil
}

func (s *Storage) GetActionRequest(ctx context.Context, id string) (*types.ActionRequest, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+actionRequestColumns+` FROM action_requests WHERE id = $1`, id)
	r, err := scanActionRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Storage) ListActionRequests(ctx context.Context, filter types.ActionRequestFilter) ([]*types.ActionRequest, error) {
	query := `SELECT ` + actionRequestColumns + ` FROM action_requests WHERE 1=1`
	var args []any
	n := 0
	if filter.ProposalID != "" {
		n++
		query += fmt.Sprintf(" AND proposal_id = $%d", n)
		args = append(args, filter.ProposalID)
	}
	if filter.RunnerID != "" {
		n++
		query += fmt.Sprintf(" AND runner_id = $%d", n)
		args = append(args, filter.RunnerID)
	}
	if filter.Status != "" {
		n++
		query += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, filter.Status)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	n++
	query += fmt.Sprintf(" ORDER BY issued_at DESC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list action requests: %w", err)
	}
	defer rows.Close()
	var out []*types.ActionRequest
	for rows.Next() {
		r, err := scanActionRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanActionRequest(sc scanner) (*types.ActionRequest, error) {
	r := &types.ActionRequest{}
	var (
		proposalID          sql.NullString
		deniedFor           sql.NullString
		dryRunOutputJSON    sql.NullString
		executionOutputJSON sql.NullString
		startedAt           sql.NullTime
		completedAt         sql.NullTime
	)
	if err := sc.Scan(
		&r.ID, &proposalID, &r.RunnerID, &r.ActionType, &r.ParametersJSON, &r.Signature,
		&r.Phase, &r.Status, &deniedFor, &dryRunOutputJSON, &executionOutputJSON,
		&r.IssuedAt, &r.ExpiresAt, &startedAt, &completedAt,
	); err != nil {
		return nil, err
	}
	if proposalID.Valid {
		r.ProposalID = proposalID.String
	}
	if deniedFor.Valid {
		r.DeniedFor = deniedFor.String
	}
	if dryRunOutputJSON.Valid {
		r.DryRunOutputJSON = dryRunOutputJSON.String
	}
	if executionOutputJSON.Valid {
		r.ExecutionOutputJSON = executionOutputJSON.String
	}
	if startedAt.Valid {
		t := startedAt.Time
		r.StartedAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time
		r.CompletedAt = &t
	}
	return r, nil
}
