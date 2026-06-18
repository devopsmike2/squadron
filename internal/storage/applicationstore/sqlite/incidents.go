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

// CreateIncidentDraft inserts a new draft. The bridge calls this
// after the drafter returns a successful structured response; the
// API handler calls it when the operator drafts manually from the
// UI.
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
		INSERT INTO incident_drafts
		  (id, action_request_id, rollout_id, status, title, body_markdown,
		   draft_content_json, provider, external_id, external_url,
		   created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		d.ID, nullString(d.ActionRequestID), nullString(d.RolloutID),
		d.Status, d.Title, d.BodyMarkdown,
		nullString(d.DraftContentJSON), nullString(d.Provider),
		nullString(d.ExternalID), nullString(d.ExternalURL),
		d.CreatedAt, d.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create incident draft: %w", err)
	}
	return nil
}

// UpdateIncidentDraft overwrites the mutable fields and bumps
// updated_at. Used when the operator edits the draft body in the
// UI, when they publish it through a provider, and when they
// dismiss it.
func (s *Storage) UpdateIncidentDraft(ctx context.Context, d *types.IncidentDraft) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE incident_drafts
		   SET status = ?, title = ?, body_markdown = ?, draft_content_json = ?,
		       provider = ?, external_id = ?, external_url = ?, updated_at = ?
		 WHERE id = ?
	`,
		d.Status, d.Title, d.BodyMarkdown, nullString(d.DraftContentJSON),
		nullString(d.Provider), nullString(d.ExternalID), nullString(d.ExternalURL),
		now, d.ID,
	)
	if err != nil {
		return fmt.Errorf("update incident draft: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("incident draft not found: %s", d.ID)
	}
	d.UpdatedAt = now
	return nil
}

// GetIncidentDraft returns the draft by ID, or nil when missing.
func (s *Storage) GetIncidentDraft(ctx context.Context, id string) (*types.IncidentDraft, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, action_request_id, rollout_id, status, title, body_markdown,
		       draft_content_json, provider, external_id, external_url,
		       created_at, updated_at
		  FROM incident_drafts
		 WHERE id = ?
	`, id)
	return scanIncidentDraft(row)
}

// GetIncidentDraftByActionRequestID is the bridge's dedup path: at
// most one draft per action. Returns nil if no draft has been
// created for this action yet.
func (s *Storage) GetIncidentDraftByActionRequestID(ctx context.Context, actionRequestID string) (*types.IncidentDraft, error) {
	if actionRequestID == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, action_request_id, rollout_id, status, title, body_markdown,
		       draft_content_json, provider, external_id, external_url,
		       created_at, updated_at
		  FROM incident_drafts
		 WHERE action_request_id = ?
		 ORDER BY created_at DESC
		 LIMIT 1
	`, actionRequestID)
	return scanIncidentDraft(row)
}

// ListIncidentDrafts returns drafts that match the filter, newest
// first.
func (s *Storage) ListIncidentDrafts(ctx context.Context, filter types.IncidentDraftFilter) ([]*types.IncidentDraft, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `
		SELECT id, action_request_id, rollout_id, status, title, body_markdown,
		       draft_content_json, provider, external_id, external_url,
		       created_at, updated_at
		  FROM incident_drafts
		 WHERE 1=1
	`
	var args []any
	if filter.ActionRequestID != "" {
		query += ` AND action_request_id = ?`
		args = append(args, filter.ActionRequestID)
	}
	if filter.RolloutID != "" {
		query += ` AND rollout_id = ?`
		args = append(args, filter.RolloutID)
	}
	if filter.Status != "" {
		query += ` AND status = ?`
		args = append(args, filter.Status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
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
		if d != nil {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// scanIncidentDraft works for both *sql.Row and *sql.Rows.
func scanIncidentDraft(scanner interface {
	Scan(dest ...any) error
}) (*types.IncidentDraft, error) {
	var (
		d                                                              types.IncidentDraft
		actionRequestID, rolloutID, draftContentJSON, provider, extID, extURL sql.NullString
	)
	err := scanner.Scan(
		&d.ID, &actionRequestID, &rolloutID, &d.Status, &d.Title, &d.BodyMarkdown,
		&draftContentJSON, &provider, &extID, &extURL,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan incident draft: %w", err)
	}
	d.ActionRequestID = actionRequestID.String
	d.RolloutID = rolloutID.String
	d.DraftContentJSON = draftContentJSON.String
	d.Provider = provider.String
	d.ExternalID = extID.String
	d.ExternalURL = extURL.String
	return &d, nil
}

// nullString is a small helper: empty strings round-trip as SQL
// NULL rather than literal "" so list queries can use `WHERE col IS
// NULL` consistently.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
