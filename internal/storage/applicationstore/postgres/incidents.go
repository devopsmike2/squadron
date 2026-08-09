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

// incidents.go — the Postgres port of incident drafts (SQ-3), ADR 0033 slice 6.
// Semantics mirror the sqlite backend EXACTLY:
//   - CreateIncidentDraft defaults CreatedAt/UpdatedAt to now and Status to
//     "draft"; optional string fields persist as NULL via nullString;
//   - UpdateIncidentDraft overwrites the mutable fields, bumps updated_at, and
//     returns "incident draft not found" on a missing row;
//   - GetIncidentDraft returns (nil, nil) on a miss;
//   - GetIncidentDraftByActionRequestID short-circuits to (nil, nil) on an empty
//     id, else returns the newest matching draft (the bridge's dedup path);
//   - ListIncidentDrafts filters by action_request_id / rollout_id / status,
//     orders created_at DESC, defaults Limit to 100 (capped at 1000).
//
// OSS tenant scoping is omitted (the type carries no tenant field).

const incidentDraftColumns = `id, action_request_id, rollout_id, status, title, body_markdown, ` +
	`draft_content_json, provider, external_id, external_url, created_at, updated_at`

func (s *Storage) CreateIncidentDraft(ctx context.Context, d *types.IncidentDraft) error {
	now := time.Now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = now
	}
	if d.Status == "" {
		d.Status = "draft"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO incident_drafts (`+incidentDraftColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		d.ID, nullString(d.ActionRequestID), nullString(d.RolloutID), d.Status, d.Title, d.BodyMarkdown,
		nullString(d.DraftContentJSON), nullString(d.Provider), nullString(d.ExternalID), nullString(d.ExternalURL),
		d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create incident draft: %w", err)
	}
	return nil
}

func (s *Storage) UpdateIncidentDraft(ctx context.Context, d *types.IncidentDraft) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE incident_drafts SET
			status = $2, title = $3, body_markdown = $4, draft_content_json = $5,
			provider = $6, external_id = $7, external_url = $8, updated_at = $9
		WHERE id = $1`,
		d.ID, d.Status, d.Title, d.BodyMarkdown, nullString(d.DraftContentJSON),
		nullString(d.Provider), nullString(d.ExternalID), nullString(d.ExternalURL), now)
	if err != nil {
		return fmt.Errorf("update incident draft: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("incident draft not found: %s", d.ID)
	}
	d.UpdatedAt = now
	return nil
}

func (s *Storage) GetIncidentDraft(ctx context.Context, id string) (*types.IncidentDraft, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+incidentDraftColumns+` FROM incident_drafts WHERE id = $1`, id)
	d, err := scanIncidentDraft(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (s *Storage) GetIncidentDraftByActionRequestID(ctx context.Context, actionRequestID string) (*types.IncidentDraft, error) {
	if actionRequestID == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+incidentDraftColumns+`
		FROM incident_drafts WHERE action_request_id = $1 ORDER BY created_at DESC LIMIT 1`, actionRequestID)
	d, err := scanIncidentDraft(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (s *Storage) ListIncidentDrafts(ctx context.Context, filter types.IncidentDraftFilter) ([]*types.IncidentDraft, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT ` + incidentDraftColumns + ` FROM incident_drafts WHERE 1=1`
	var args []any
	n := 0
	if filter.ActionRequestID != "" {
		n++
		query += fmt.Sprintf(" AND action_request_id = $%d", n)
		args = append(args, filter.ActionRequestID)
	}
	if filter.RolloutID != "" {
		n++
		query += fmt.Sprintf(" AND rollout_id = $%d", n)
		args = append(args, filter.RolloutID)
	}
	if filter.Status != "" {
		n++
		query += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, filter.Status)
	}
	n++
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list incident drafts: %w", err)
	}
	defer rows.Close()
	var out []*types.IncidentDraft
	for rows.Next() {
		d, err := scanIncidentDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanIncidentDraft(sc scanner) (*types.IncidentDraft, error) {
	d := &types.IncidentDraft{}
	var actionRequestID, rolloutID, draftContentJSON, provider, extID, extURL sql.NullString
	if err := sc.Scan(&d.ID, &actionRequestID, &rolloutID, &d.Status, &d.Title, &d.BodyMarkdown,
		&draftContentJSON, &provider, &extID, &extURL, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	d.ActionRequestID = actionRequestID.String
	d.RolloutID = rolloutID.String
	d.DraftContentJSON = draftContentJSON.String
	d.Provider = provider.String
	d.ExternalID = extID.String
	d.ExternalURL = extURL.String
	return d, nil
}
