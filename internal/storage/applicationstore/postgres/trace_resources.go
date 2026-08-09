// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/devopsmike2/squadron/internal/traceindex"
)

// trace_resources.go — the Postgres port of the trace_resource_seen storage layer
// (v0.89.74), ADR 0033 slice 6. Semantics mirror the sqlite backend EXACTLY:
//   - UpsertTraceResources applies each row via INSERT ... ON CONFLICT(resource_key)
//     DO UPDATE so span_count_24h + root_span_count_24h accumulate across
//     re-observations, last_seen_at + attributes_json + match_confidence +
//     updated_at refresh, resource_id_hint is preserved when the new value is
//     empty, and first_seen_at stays pinned. After the batch it counts rows and,
//     if over the SQUADRON_TRACEINDEX_MAX_ROWS cap (default 100K), DELETEs the
//     oldest last_seen_at rows down to the cap, returning the evicted count. The
//     batch + eviction run in ONE transaction. An empty batch is a (0, nil) no-op;
//   - GetTraceResource returns (nil, nil) when no row matches; empty key errors;
//   - ListTraceResourcesByScope returns rows for the (provider, scope_id) tuple
//     with last_seen_at >= since, newest-first, limit<=0||>100000 capped at 1000;
//     an empty provider returns nil;
//   - CountTraceResourcesByScope returns the row count for the same tuple; an empty
//     provider returns 0.
//
// The OSS port omits tenant scoping (traceindex.ResourceRow carries no tenant
// field): the conflict target is resource_key alone and the LRU eviction is
// global, which is exactly the single-tenant behavior the sqlite backend produces.

const defaultTraceIndexMaxRows = 100_000

// traceIndexMaxRows parses SQUADRON_TRACEINDEX_MAX_ROWS; falls through to the
// default on missing/invalid, mirroring the sqlite backend's env read.
func traceIndexMaxRows() int {
	raw := os.Getenv("SQUADRON_TRACEINDEX_MAX_ROWS")
	if raw == "" {
		return defaultTraceIndexMaxRows
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultTraceIndexMaxRows
	}
	return n
}

func (s *Storage) UpsertTraceResources(ctx context.Context, rows []traceindex.ResourceRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const stmt = `INSERT INTO trace_resource_seen (
		resource_key, provider, scope_id, resource_id_hint, service_name,
		first_seen_at, last_seen_at, span_count_24h, root_span_count_24h,
		attributes_json, match_confidence, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	ON CONFLICT (resource_key) DO UPDATE SET
		provider            = excluded.provider,
		scope_id            = excluded.scope_id,
		resource_id_hint    = COALESCE(NULLIF(excluded.resource_id_hint, ''), trace_resource_seen.resource_id_hint),
		service_name        = excluded.service_name,
		last_seen_at        = excluded.last_seen_at,
		span_count_24h      = trace_resource_seen.span_count_24h + excluded.span_count_24h,
		root_span_count_24h = trace_resource_seen.root_span_count_24h + excluded.root_span_count_24h,
		attributes_json     = excluded.attributes_json,
		match_confidence    = excluded.match_confidence,
		updated_at          = excluded.updated_at`

	for _, r := range rows {
		var scopeID, hint, attrJSON, svc any
		if r.ScopeID != "" {
			scopeID = r.ScopeID
		}
		if r.ResourceIDHint != "" {
			hint = r.ResourceIDHint
		}
		if r.AttributesJSON != "" {
			attrJSON = r.AttributesJSON
		}
		if r.ServiceName != "" {
			svc = r.ServiceName
		}
		conf := string(r.MatchConfidence)
		if conf == "" {
			conf = string(traceindex.MatchConfidenceWeak)
		}
		if _, err := tx.ExecContext(ctx, stmt,
			r.ResourceKey, r.Provider, scopeID, hint, svc,
			r.FirstSeenAt.UTC(), r.LastSeenAt.UTC(), r.SpanCount24h, r.RootSpanCount24h,
			attrJSON, conf, r.UpdatedAt.UTC()); err != nil {
			return 0, fmt.Errorf("upsert trace_resource_seen row %q: %w", r.ResourceKey, err)
		}
	}

	// LRU eviction: count rows, and if over budget delete the oldest last_seen_at
	// rows down to the cap. OSS single-tenant, so the sweep is global.
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM trace_resource_seen`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count trace_resource_seen: %w", err)
	}
	evicted := 0
	if budget := traceIndexMaxRows(); count > budget {
		over := count - budget
		res, err := tx.ExecContext(ctx, `DELETE FROM trace_resource_seen
			WHERE resource_key IN (
				SELECT resource_key FROM trace_resource_seen
				ORDER BY last_seen_at ASC
				LIMIT $1
			)`, over)
		if err != nil {
			return 0, fmt.Errorf("evict oldest trace_resource_seen rows: %w", err)
		}
		n, _ := res.RowsAffected()
		evicted = int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit trace_resource_seen upsert: %w", err)
	}
	return evicted, nil
}

const traceResourceSelectCols = `resource_key, provider,
	COALESCE(scope_id, ''), COALESCE(resource_id_hint, ''), COALESCE(service_name, ''),
	first_seen_at, last_seen_at, span_count_24h, root_span_count_24h,
	COALESCE(attributes_json, ''), match_confidence, updated_at`

func (s *Storage) GetTraceResource(ctx context.Context, key string) (*traceindex.ResourceRow, error) {
	if key == "" {
		return nil, fmt.Errorf("resource_key required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+traceResourceSelectCols+` FROM trace_resource_seen WHERE resource_key = $1`, key)
	r, err := scanTraceResourceRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read trace_resource_seen row: %w", err)
	}
	return r, nil
}

func (s *Storage) ListTraceResourcesByScope(
	ctx context.Context,
	provider, scopeID string,
	since time.Time,
	limit int,
) ([]traceindex.ResourceRow, error) {
	if provider == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100_000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+traceResourceSelectCols+`
		FROM trace_resource_seen
		WHERE provider = $1
		  AND (scope_id = $2 OR ($2 = '' AND scope_id IS NULL))
		  AND last_seen_at >= $3
		ORDER BY last_seen_at DESC
		LIMIT $4`, provider, scopeID, since.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list trace_resource_seen rows: %w", err)
	}
	defer rows.Close()
	var out []traceindex.ResourceRow
	for rows.Next() {
		r, err := scanTraceResourceRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan trace_resource_seen row: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (s *Storage) CountTraceResourcesByScope(ctx context.Context, provider, scopeID string) (int, error) {
	if provider == "" {
		return 0, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trace_resource_seen
		WHERE provider = $1
		  AND (scope_id = $2 OR ($2 = '' AND scope_id IS NULL))`, provider, scopeID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count trace_resource_seen: %w", err)
	}
	return n, nil
}

func scanTraceResourceRow(sc scanner) (*traceindex.ResourceRow, error) {
	var (
		r    traceindex.ResourceRow
		conf string
	)
	if err := sc.Scan(&r.ResourceKey, &r.Provider, &r.ScopeID, &r.ResourceIDHint, &r.ServiceName,
		&r.FirstSeenAt, &r.LastSeenAt, &r.SpanCount24h, &r.RootSpanCount24h,
		&r.AttributesJSON, &conf, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.MatchConfidence = traceindex.MatchConfidence(conf)
	r.FirstSeenAt = r.FirstSeenAt.UTC()
	r.LastSeenAt = r.LastSeenAt.UTC()
	r.UpdatedAt = r.UpdatedAt.UTC()
	return &r, nil
}
