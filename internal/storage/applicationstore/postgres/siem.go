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

// siem.go — the Postgres port of SIEM destinations (v0.50 audit export), ADR 0033
// slice 6. Semantics mirror the sqlite backend EXACTLY:
//   - CreateSiemDestination defaults CreatedAt/UpdatedAt to now and an empty
//     EventTypePrefixesJSON to "[]";
//   - GetSiemDestination returns (nil, nil) on a miss;
//   - ListSiemDestinations orders by name;
//   - UpdateSiemDestination overwrites the operator-editable fields (bumping
//     updated_at + defaulting prefixes to "[]") and returns "siem destination not
//     found" on a missing row;
//   - DeleteSiemDestination returns "siem destination not found" on a missing row;
//   - UpdateSiemDestinationStatus narrow-updates only the dispatcher-owned status
//     columns (no missing-row error) so it can't race an operator edit.
//
// secret is the encrypted-at-rest credential (BYTEA), never surfaced in plaintext
// to the API layer. enabled is a native BOOLEAN (sqlite stored 0/1). OSS tenant
// scoping is omitted (the type carries no tenant field).

const siemColumns = `id, name, type, url, secret, enabled, event_type_prefixes_json, ` +
	`last_event_sent_at, last_error, last_error_at, created_at, updated_at`

func (s *Storage) CreateSiemDestination(ctx context.Context, d *types.SiemDestination) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = d.CreatedAt
	}
	prefixes := d.EventTypePrefixesJSON
	if prefixes == "" {
		prefixes = "[]"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO siem_destinations
			(id, name, type, url, secret, enabled, event_type_prefixes_json, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		d.ID, d.Name, d.Type, d.URL, d.Secret, d.Enabled, prefixes, d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create siem destination: %w", err)
	}
	return nil
}

func (s *Storage) GetSiemDestination(ctx context.Context, id string) (*types.SiemDestination, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+siemColumns+` FROM siem_destinations WHERE id = $1`, id)
	d, err := scanSiemDestination(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (s *Storage) ListSiemDestinations(ctx context.Context) ([]*types.SiemDestination, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+siemColumns+` FROM siem_destinations ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list siem destinations: %w", err)
	}
	defer rows.Close()
	var out []*types.SiemDestination
	for rows.Next() {
		d, err := scanSiemDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Storage) UpdateSiemDestination(ctx context.Context, d *types.SiemDestination) error {
	d.UpdatedAt = time.Now().UTC()
	prefixes := d.EventTypePrefixesJSON
	if prefixes == "" {
		prefixes = "[]"
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE siem_destinations SET
			name = $2, type = $3, url = $4, secret = $5, enabled = $6,
			event_type_prefixes_json = $7, updated_at = $8
		WHERE id = $1`,
		d.ID, d.Name, d.Type, d.URL, d.Secret, d.Enabled, prefixes, d.UpdatedAt)
	if err != nil {
		return fmt.Errorf("update siem destination: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("siem destination not found: %s", d.ID)
	}
	return nil
}

func (s *Storage) DeleteSiemDestination(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM siem_destinations WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete siem destination: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("siem destination not found: %s", id)
	}
	return nil
}

// UpdateSiemDestinationStatus narrow-updates only the dispatcher-owned status
// columns so its writes don't race an operator editing the URL / secret. No
// missing-row error, matching the sqlite backend.
func (s *Storage) UpdateSiemDestinationStatus(ctx context.Context, id string, sentAt *time.Time, errMsg string, errAt *time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE siem_destinations
		SET last_event_sent_at = $2, last_error = $3, last_error_at = $4
		WHERE id = $1`,
		id, nullTime(sentAt), nullString(errMsg), nullTime(errAt))
	if err != nil {
		return fmt.Errorf("update siem destination status: %w", err)
	}
	return nil
}

func scanSiemDestination(sc scanner) (*types.SiemDestination, error) {
	d := &types.SiemDestination{}
	var (
		prefixes sql.NullString
		sentAt   sql.NullTime
		lastErr  sql.NullString
		errAt    sql.NullTime
	)
	if err := sc.Scan(&d.ID, &d.Name, &d.Type, &d.URL, &d.Secret, &d.Enabled, &prefixes,
		&sentAt, &lastErr, &errAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if prefixes.Valid {
		d.EventTypePrefixesJSON = prefixes.String
	}
	if sentAt.Valid {
		t := sentAt.Time
		d.LastEventSentAt = &t
	}
	if lastErr.Valid {
		d.LastError = lastErr.String
	}
	if errAt.Valid {
		t := errAt.Time
		d.LastErrorAt = &t
	}
	return d, nil
}
