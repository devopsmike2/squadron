// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// discovery_scans.go — the Postgres port of discovery scan persistence
// (v0.89.250, continuous-discovery slice 1), ADR 0033 slice 6. Semantics mirror
// the sqlite backend EXACTLY:
//   - SaveDiscoveryScan requires a non-empty ScanID and upserts on it; defaults
//     CreatedAt to now and a nil Summary to an empty map;
//   - ListDiscoveryScans returns newest-first (started_at DESC) history for a
//     scope, OMITTING result_json for cheap listing; a blank scopeID lists every
//     scan for the provider; limit<=0 defaults to 50;
//   - GetDiscoveryScan returns one scan INCLUDING result_json, or (nil, nil) on a
//     miss.
//
// regions + summary are structured → JSONB; result_json is the full marshaled
// inventory, an opaque string stored VERBATIM as TEXT. partial is a native
// BOOLEAN. OSS tenant scoping is omitted (the type carries no tenant field).

func (s *Storage) SaveDiscoveryScan(ctx context.Context, rec *types.ScanRecord) error {
	if rec == nil || rec.ScanID == "" {
		return fmt.Errorf("SaveDiscoveryScan requires a non-empty ScanID")
	}
	regionsJSON, err := json.Marshal(rec.Regions)
	if err != nil {
		return fmt.Errorf("marshal regions: %w", err)
	}
	summary := rec.Summary
	if summary == nil {
		summary = map[string]int{}
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("marshal summary: %w", err)
	}
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO discovery_scans (
			scan_id, provider, scope_id, regions, started_at, completed_at,
			partial, partial_reason, summary, result_json, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (scan_id) DO UPDATE SET
			provider       = excluded.provider,
			scope_id       = excluded.scope_id,
			regions        = excluded.regions,
			started_at     = excluded.started_at,
			completed_at   = excluded.completed_at,
			partial        = excluded.partial,
			partial_reason = excluded.partial_reason,
			summary        = excluded.summary,
			result_json    = excluded.result_json`,
		rec.ScanID, rec.Provider, rec.ScopeID, regionsJSON, rec.StartedAt.UTC(), rec.CompletedAt.UTC(),
		rec.Partial, nullString(rec.PartialReason), summaryJSON, nullString(rec.ResultJSON), createdAt.UTC())
	if err != nil {
		return fmt.Errorf("insert discovery scan: %w", err)
	}
	return nil
}

func (s *Storage) ListDiscoveryScans(ctx context.Context, provider, scopeID string, limit int) ([]*types.ScanRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT scan_id, provider, scope_id, regions, started_at, completed_at,
		       partial, partial_reason, summary, created_at
		FROM discovery_scans
		WHERE provider = $1 AND ($2 = '' OR scope_id = $2)
		ORDER BY started_at DESC
		LIMIT $3`, provider, scopeID, limit)
	if err != nil {
		return nil, fmt.Errorf("list discovery scans: %w", err)
	}
	defer rows.Close()
	var out []*types.ScanRecord
	for rows.Next() {
		rec, err := scanDiscoveryScanRow(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Storage) GetDiscoveryScan(ctx context.Context, scanID string) (*types.ScanRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT scan_id, provider, scope_id, regions, started_at, completed_at,
		       partial, partial_reason, summary, result_json, created_at
		FROM discovery_scans WHERE scan_id = $1`, scanID)
	rec, err := scanDiscoveryScanRow(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

func scanDiscoveryScanRow(sc scanner, withResult bool) (*types.ScanRecord, error) {
	var (
		rec           types.ScanRecord
		regionsJSON   []byte
		summaryJSON   []byte
		partialReason sql.NullString
		resultJSON    sql.NullString
	)
	var err error
	if withResult {
		err = sc.Scan(&rec.ScanID, &rec.Provider, &rec.ScopeID, &regionsJSON,
			&rec.StartedAt, &rec.CompletedAt, &rec.Partial, &partialReason,
			&summaryJSON, &resultJSON, &rec.CreatedAt)
	} else {
		err = sc.Scan(&rec.ScanID, &rec.Provider, &rec.ScopeID, &regionsJSON,
			&rec.StartedAt, &rec.CompletedAt, &rec.Partial, &partialReason,
			&summaryJSON, &rec.CreatedAt)
	}
	if err != nil {
		return nil, err
	}
	rec.PartialReason = partialReason.String
	if withResult {
		rec.ResultJSON = resultJSON.String
	}
	if len(regionsJSON) > 0 {
		_ = json.Unmarshal(regionsJSON, &rec.Regions)
	}
	rec.Summary = map[string]int{}
	if len(summaryJSON) > 0 {
		_ = json.Unmarshal(summaryJSON, &rec.Summary)
	}
	return &rec, nil
}
