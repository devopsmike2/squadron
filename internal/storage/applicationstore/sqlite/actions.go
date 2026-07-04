// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// CreateActionRunnerRegistration inserts a new runner registration.
// Squadron calls this once per runner enrollment; the operator-side
// install flow generates the keypair and posts the public key plus
// declared capabilities to /api/v1/runners/register.
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
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, r.RunnerID, r.Hostname, r.PublicKeyPEM, r.CapabilitiesJSON, r.RegisteredAt, r.LastSeenAt, r.RevokedAt)
	if err != nil {
		return fmt.Errorf("create action runner registration: %w", err)
	}
	return nil
}

// UpdateActionRunnerRegistration overwrites a runner's fields. Used
// by the runner's periodic re-registration (capabilities change)
// and by Squadron's last-seen tracking.
func (s *Storage) UpdateActionRunnerRegistration(ctx context.Context, r *types.ActionRunnerRegistration) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE action_runner_registrations
		   SET hostname = ?, public_key_pem = ?, capabilities_json = ?, last_seen_at = ?, revoked_at = ?
		 WHERE runner_id = ?
	`, r.Hostname, r.PublicKeyPEM, r.CapabilitiesJSON, r.LastSeenAt, r.RevokedAt, r.RunnerID)
	if err != nil {
		return fmt.Errorf("update action runner registration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("action runner registration not found: %s", r.RunnerID)
	}
	return nil
}

// GetActionRunnerRegistration returns the registration for the
// supplied runner_id or nil when none exists.
func (s *Storage) GetActionRunnerRegistration(ctx context.Context, runnerID string) (*types.ActionRunnerRegistration, error) {
	r := &types.ActionRunnerRegistration{}
	var revokedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT runner_id, hostname, public_key_pem, capabilities_json, registered_at, last_seen_at, revoked_at
		  FROM action_runner_registrations
		 WHERE runner_id = ?
	`, runnerID).Scan(&r.RunnerID, &r.Hostname, &r.PublicKeyPEM, &r.CapabilitiesJSON, &r.RegisteredAt, &r.LastSeenAt, &revokedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get action runner registration: %w", err)
	}
	if revokedAt.Valid {
		t := revokedAt.Time
		r.RevokedAt = &t
	}
	return r, nil
}

// ListActionRunnerRegistrations returns every runner registration,
// newest registered_at first. Revoked runners are included; the
// UI distinguishes them on the active/inactive tab.
func (s *Storage) ListActionRunnerRegistrations(ctx context.Context) ([]*types.ActionRunnerRegistration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT runner_id, hostname, public_key_pem, capabilities_json, registered_at, last_seen_at, revoked_at
		  FROM action_runner_registrations
		 ORDER BY registered_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list action runner registrations: %w", err)
	}
	defer rows.Close()
	var out []*types.ActionRunnerRegistration
	for rows.Next() {
		r := &types.ActionRunnerRegistration{}
		var revokedAt sql.NullTime
		if err := rows.Scan(&r.RunnerID, &r.Hostname, &r.PublicKeyPEM, &r.CapabilitiesJSON, &r.RegisteredAt, &r.LastSeenAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("scan action runner registration: %w", err)
		}
		if revokedAt.Valid {
			t := revokedAt.Time
			r.RevokedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeActionRunnerRegistration marks the runner as revoked.
// Squadron refuses to dispatch new requests to revoked runners;
// the row stays for audit history.
func (s *Storage) RevokeActionRunnerRegistration(ctx context.Context, runnerID string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE action_runner_registrations SET revoked_at = ? WHERE runner_id = ?
	`, at, runnerID)
	if err != nil {
		return fmt.Errorf("revoke action runner registration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("action runner registration not found: %s", runnerID)
	}
	return nil
}

// CreateActionRequest persists a freshly signed action request.
// The request enters status=pending; UpdateActionRequest writes
// the final status and output once the runner responds.
func (s *Storage) CreateActionRequest(ctx context.Context, r *types.ActionRequest) error {
	if r.Status == "" {
		r.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO action_requests
		  (id, proposal_id, runner_id, action_type, parameters_json, signature,
		   phase, status, denied_for, dry_run_output_json, execution_output_json,
		   issued_at, expires_at, started_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		r.ID, nullableString(r.ProposalID), r.RunnerID, r.ActionType, r.ParametersJSON,
		r.Signature, r.Phase, r.Status, nullableString(r.DeniedFor),
		nullableString(r.DryRunOutputJSON), nullableString(r.ExecutionOutputJSON),
		r.IssuedAt, r.ExpiresAt, r.StartedAt, r.CompletedAt,
	)
	if err != nil {
		return fmt.Errorf("create action request: %w", err)
	}
	return nil
}

// UpdateActionRequest overwrites the mutable fields of an action
// request. The runner's result (status, output, completed_at) lands
// here; the immutable fields (id, signature, parameters, phase,
// issued_at) stay as they were at create time.
func (s *Storage) UpdateActionRequest(ctx context.Context, r *types.ActionRequest) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE action_requests
		   SET status = ?, denied_for = ?,
		       dry_run_output_json = ?, execution_output_json = ?,
		       started_at = ?, completed_at = ?
		 WHERE id = ?
	`,
		r.Status, nullableString(r.DeniedFor),
		nullableString(r.DryRunOutputJSON), nullableString(r.ExecutionOutputJSON),
		r.StartedAt, r.CompletedAt, r.ID,
	)
	if err != nil {
		return fmt.Errorf("update action request: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("action request not found: %s", r.ID)
	}
	return nil
}

// GetActionRequest returns one request by ID, or nil.
func (s *Storage) GetActionRequest(ctx context.Context, id string) (*types.ActionRequest, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, proposal_id, runner_id, action_type, parameters_json, signature,
		       phase, status, denied_for, dry_run_output_json, execution_output_json,
		       issued_at, expires_at, started_at, completed_at
		  FROM action_requests WHERE id = ?
	`, id)
	return scanActionRequest(row)
}

// ListActionRequests returns requests matching the filter. Default
// sort is newest first.
func (s *Storage) ListActionRequests(ctx context.Context, filter types.ActionRequestFilter) ([]*types.ActionRequest, error) {
	q := `SELECT id, proposal_id, runner_id, action_type, parameters_json, signature,
	             phase, status, denied_for, dry_run_output_json, execution_output_json,
	             issued_at, expires_at, started_at, completed_at
	        FROM action_requests WHERE 1=1`
	var args []any
	if filter.ProposalID != "" {
		q += " AND proposal_id = ?"
		args = append(args, filter.ProposalID)
	}
	if filter.RunnerID != "" {
		q += " AND runner_id = ?"
		args = append(args, filter.RunnerID)
	}
	if filter.Status != "" {
		q += " AND status = ?"
		args = append(args, filter.Status)
	}
	q += " ORDER BY issued_at DESC"
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	q += " LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
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

// scanActionRequest is the shared scanner used by both row and rows
// paths. Mirrors the column order in the SELECTs above.
func scanActionRequest(sc interface{ Scan(...any) error }) (*types.ActionRequest, error) {
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
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan action request: %w", err)
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
