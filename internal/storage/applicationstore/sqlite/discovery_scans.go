// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Discovery scan persistence (v0.89.250, continuous-discovery slice 1).
//
// One row per completed scan. The scanner.Result is self-describing, so
// SaveDiscoveryScan is mostly a projection: indexed columns for the list
// access path (provider, scope_id, started_at) plus two JSON blobs — summary
// (per-category counts, returned in listings) and result_json (the full
// marshaled inventory, returned only by GetDiscoveryScan). Regions is stored
// as a JSON array. Mirrors the existing instance-table persistence shape but at
// whole-scan grain, which is the natural primitive for "scan history".

// SaveDiscoveryScan upserts a scan record on scan_id.
func (s *Storage) SaveDiscoveryScan(ctx context.Context, rec *types.ScanRecord) error {
	if rec == nil || rec.ScanID == "" {
		return fmt.Errorf("sqlite: SaveDiscoveryScan requires a non-empty ScanID")
	}
	regionsJSON, err := json.Marshal(rec.Regions)
	if err != nil {
		return fmt.Errorf("sqlite: marshal regions: %w", err)
	}
	summary := rec.Summary
	if summary == nil {
		summary = map[string]int{}
	}
	summaryJSON, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("sqlite: marshal summary: %w", err)
	}
	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	partial := 0
	if rec.Partial {
		partial = 1
	}
	const stmt = `INSERT INTO discovery_scans (
		scan_id, provider, scope_id, regions, started_at, completed_at,
		partial, partial_reason, summary, result_json, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(scan_id) DO UPDATE SET
		provider=excluded.provider,
		scope_id=excluded.scope_id,
		regions=excluded.regions,
		started_at=excluded.started_at,
		completed_at=excluded.completed_at,
		partial=excluded.partial,
		partial_reason=excluded.partial_reason,
		summary=excluded.summary,
		result_json=excluded.result_json`
	_, err = s.db.ExecContext(ctx, stmt,
		rec.ScanID, rec.Provider, rec.ScopeID, string(regionsJSON),
		rec.StartedAt, rec.CompletedAt, partial, rec.PartialReason,
		string(summaryJSON), rec.ResultJSON, createdAt,
	)
	if err != nil {
		return fmt.Errorf("sqlite: insert discovery_scan: %w", err)
	}
	return nil
}

// ListDiscoveryScans returns newest-first scan history for a scope. result_json
// is omitted to keep list responses small. A blank scopeID lists every scan for
// the provider.
func (s *Storage) ListDiscoveryScans(ctx context.Context, provider, scopeID string, limit int) ([]*types.ScanRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	const stmt = `SELECT scan_id, provider, scope_id, regions, started_at,
		completed_at, partial, partial_reason, summary, created_at
		FROM discovery_scans
		WHERE provider = ? AND (? = '' OR scope_id = ?)
		ORDER BY started_at DESC
		LIMIT ?`
	rows, err := s.db.QueryContext(ctx, stmt, provider, scopeID, scopeID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list discovery_scans: %w", err)
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

// GetDiscoveryScan returns one scan including the full inventory (result_json).
// Returns (nil, nil) when no scan matches.
func (s *Storage) GetDiscoveryScan(ctx context.Context, scanID string) (*types.ScanRecord, error) {
	const stmt = `SELECT scan_id, provider, scope_id, regions, started_at,
		completed_at, partial, partial_reason, summary, result_json, created_at
		FROM discovery_scans WHERE scan_id = ?`
	row := s.db.QueryRowContext(ctx, stmt, scanID)
	rec, err := scanDiscoveryScanRow(row, true)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// rowScanner abstracts *sql.Row and *sql.Rows for the shared column scan.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDiscoveryScanRow(r rowScanner, withResult bool) (*types.ScanRecord, error) {
	var (
		rec           types.ScanRecord
		regionsJSON   string
		summaryJSON   string
		partialInt    int
		partialReason sql.NullString
		resultJSON    sql.NullString
	)
	var err error
	if withResult {
		err = r.Scan(&rec.ScanID, &rec.Provider, &rec.ScopeID, &regionsJSON,
			&rec.StartedAt, &rec.CompletedAt, &partialInt, &partialReason,
			&summaryJSON, &resultJSON, &rec.CreatedAt)
	} else {
		err = r.Scan(&rec.ScanID, &rec.Provider, &rec.ScopeID, &regionsJSON,
			&rec.StartedAt, &rec.CompletedAt, &partialInt, &partialReason,
			&summaryJSON, &rec.CreatedAt)
	}
	if err != nil {
		return nil, err
	}
	rec.Partial = partialInt != 0
	rec.PartialReason = partialReason.String
	if withResult {
		rec.ResultJSON = resultJSON.String
	}
	if regionsJSON != "" {
		_ = json.Unmarshal([]byte(regionsJSON), &rec.Regions)
	}
	rec.Summary = map[string]int{}
	if summaryJSON != "" {
		_ = json.Unmarshal([]byte(summaryJSON), &rec.Summary)
	}
	return &rec, nil
}
