// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// readTraceIndexMaxRowsEnv parses SQUADRON_TRACEINDEX_MAX_ROWS; falls
// through to defaultTraceIndexMaxRows on missing/invalid. Operators
// override the LRU cap via this env var per design doc §12.
func readTraceIndexMaxRowsEnv() int {
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

// UpsertTraceResources — v0.89.74 (#705 Stream 103, slice 1 chunk 1).
// Batched flush sink for the traceindex Index. Each row is applied
// via INSERT ... ON CONFLICT(tenant_id, resource_key) DO UPDATE (the
// ADR 0011 slice 3b′ composite key, so two tenants can hold the same
// resource_key) so the row's span_count_24h accumulates across
// re-observations, last_seen_at + attributes_json + updated_at refresh
// to the new values, and first_seen_at stays pinned at the original
// observation.
//
// After the upsert pass the method counts rows in trace_resource_seen
// and, if over the maxRows cap (set from SQUADRON_TRACEINDEX_MAX_ROWS
// at NewSQLiteStorage time, default 100K per design doc §12), DELETEs
// the oldest last_seen_at rows until the count matches the cap.
// Returns the evicted count — non-zero feeds the chunk-2 flush audit
// payload's eviction_count field so operators can detect high-
// cardinality attribute amplification (§12 threat).
//
// The whole batch + eviction sweep runs in one transaction so a
// concurrent Coverage / LastSeenAt read can't observe a partial
// flush.
func (s *Storage) UpsertTraceResources(
	ctx context.Context,
	rows []traceindex.ResourceRow,
) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	// ADR 0011 slice 3b — the trace flush runs under WithSystemContext in
	// production (apply=false → rowTenant=DefaultTenant, no eviction
	// predicate → fleet-wide LRU). A non-system caller stamps the rows with
	// its tenant. The upsert's ON CONFLICT target is the slice 3b′ composite
	// (tenant_id, resource_key) key, so two tenants can hold the same
	// resource_key without collision.
	//
	// TODO(ADR 0011 slice 3b′): the LRU cap (count + evict-oldest below) is
	// deliberately GLOBAL, not per-tenant. The production flush runs under a
	// system context (no tenant to scope by), and SQUADRON_TRACEINDEX_MAX_ROWS
	// is a fleet-wide storage budget, so a fleet-wide LRU is the correct
	// behavior for the flush path. Making the cap per-tenant would need a
	// per-tenant budget + a tenant to evict within, which the system-context
	// flush does not carry; deferred until a per-tenant trace budget exists.
	// Inert in OSS (single tenant ⇒ global == per-tenant).
	scopeTenant, apply, err := tenantScope(ctx)
	if err != nil {
		return 0, err
	}
	rowTenant := scopeTenant
	if !apply {
		rowTenant = identity.DefaultTenant
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const stmt = `INSERT INTO trace_resource_seen (
		resource_key, provider, scope_id, resource_id_hint, service_name,
		first_seen_at, last_seen_at, span_count_24h, root_span_count_24h,
		attributes_json, match_confidence, updated_at, tenant_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(tenant_id, resource_key) DO UPDATE SET
		provider             = excluded.provider,
		scope_id             = excluded.scope_id,
		resource_id_hint     = COALESCE(NULLIF(excluded.resource_id_hint, ''), trace_resource_seen.resource_id_hint),
		service_name         = excluded.service_name,
		last_seen_at         = excluded.last_seen_at,
		span_count_24h       = trace_resource_seen.span_count_24h + excluded.span_count_24h,
		root_span_count_24h  = trace_resource_seen.root_span_count_24h + excluded.root_span_count_24h,
		attributes_json      = excluded.attributes_json,
		match_confidence     = excluded.match_confidence,
		updated_at           = excluded.updated_at,
		tenant_id            = excluded.tenant_id`

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
			r.ResourceKey,
			r.Provider,
			scopeID,
			hint,
			svc,
			r.FirstSeenAt.UTC(),
			r.LastSeenAt.UTC(),
			r.SpanCount24h,
			r.RootSpanCount24h,
			attrJSON,
			conf,
			r.UpdatedAt.UTC(),
			rowTenant,
		); err != nil {
			return 0, fmt.Errorf("failed to upsert trace_resource_seen row %q: %w", r.ResourceKey, err)
		}
	}

	cap := s.traceIndexMaxRow
	if cap <= 0 {
		cap = defaultTraceIndexMaxRows
	}

	// Count rows after the batch. If over cap, sweep the oldest
	// last_seen_at rows. The DELETE uses the idx_trace_resource_seen_last_seen
	// index so the sweep stays a single ranged read.
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM trace_resource_seen`).Scan(&total); err != nil {
		return 0, fmt.Errorf("failed to count trace_resource_seen: %w", err)
	}
	evicted := 0
	if total > cap {
		over := total - cap
		res, err := tx.ExecContext(ctx, `DELETE FROM trace_resource_seen
			WHERE resource_key IN (
				SELECT resource_key FROM trace_resource_seen
				ORDER BY last_seen_at ASC
				LIMIT ?
			)`, over)
		if err != nil {
			return 0, fmt.Errorf("failed to evict oldest trace_resource_seen rows: %w", err)
		}
		n, _ := res.RowsAffected()
		evicted = int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit trace_resource_seen upsert: %w", err)
	}
	return evicted, nil
}

// GetTraceResource — v0.89.74. Returns the row for resource_key, or
// (nil, nil) when no row matches. The per-provider Inventory tab
// (chunk 4) reads through this for the last_seen_at column.
func (s *Storage) GetTraceResource(
	ctx context.Context,
	key string,
) (*traceindex.ResourceRow, error) {
	if key == "" {
		return nil, fmt.Errorf("resource_key required")
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	stmt := `SELECT
		resource_key, provider,
		COALESCE(scope_id, ''),
		COALESCE(resource_id_hint, ''),
		COALESCE(service_name, ''),
		first_seen_at, last_seen_at,
		span_count_24h, root_span_count_24h,
		COALESCE(attributes_json, ''),
		match_confidence, updated_at
		FROM trace_resource_seen
		WHERE resource_key = ?`
	args := []any{key}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	var (
		r    traceindex.ResourceRow
		conf string
	)
	err = s.db.QueryRowContext(ctx, stmt, args...).Scan(
		&r.ResourceKey, &r.Provider,
		&r.ScopeID, &r.ResourceIDHint, &r.ServiceName,
		&r.FirstSeenAt, &r.LastSeenAt,
		&r.SpanCount24h, &r.RootSpanCount24h,
		&r.AttributesJSON,
		&conf, &r.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read trace_resource_seen row: %w", err)
	}
	r.MatchConfidence = traceindex.MatchConfidence(conf)
	r.FirstSeenAt = r.FirstSeenAt.UTC()
	r.LastSeenAt = r.LastSeenAt.UTC()
	r.UpdatedAt = r.UpdatedAt.UTC()
	return &r, nil
}

// ListTraceResourcesByScope — v0.89.74. Returns rows for the
// (provider, scope_id) tuple with last_seen_at >= since, ordered
// newest-first. limit<=0 caps at 1000 (sized to the dashboard's per-
// provider cap). The Discovery dashboard's per-card breakdown reads
// through this for the strong/weak confidence split.
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
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	stmt := `SELECT
		resource_key, provider,
		COALESCE(scope_id, ''),
		COALESCE(resource_id_hint, ''),
		COALESCE(service_name, ''),
		first_seen_at, last_seen_at,
		span_count_24h, root_span_count_24h,
		COALESCE(attributes_json, ''),
		match_confidence, updated_at
		FROM trace_resource_seen
		WHERE provider = ?
		  AND (scope_id = ? OR (? = '' AND scope_id IS NULL))
		  AND last_seen_at >= ?`
	args := []any{provider, scopeID, scopeID, since.UTC()}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	stmt += `
		ORDER BY last_seen_at DESC
		LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list trace_resource_seen rows: %w", err)
	}
	defer rows.Close()
	var out []traceindex.ResourceRow
	for rows.Next() {
		var (
			r    traceindex.ResourceRow
			conf string
		)
		if err := rows.Scan(
			&r.ResourceKey, &r.Provider,
			&r.ScopeID, &r.ResourceIDHint, &r.ServiceName,
			&r.FirstSeenAt, &r.LastSeenAt,
			&r.SpanCount24h, &r.RootSpanCount24h,
			&r.AttributesJSON,
			&conf, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan trace_resource_seen row: %w", err)
		}
		r.MatchConfidence = traceindex.MatchConfidence(conf)
		r.FirstSeenAt = r.FirstSeenAt.UTC()
		r.LastSeenAt = r.LastSeenAt.UTC()
		r.UpdatedAt = r.UpdatedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountTraceResourcesByScope — v0.89.74. Returns the row count for
// the (provider, scope_id) tuple, the numerator for the dashboard's
// coverage_pct.
func (s *Storage) CountTraceResourcesByScope(
	ctx context.Context,
	provider, scopeID string,
) (int, error) {
	if provider == "" {
		return 0, nil
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return 0, err
	}
	stmt := `SELECT COUNT(*) FROM trace_resource_seen
		WHERE provider = ?
		  AND (scope_id = ? OR (? = '' AND scope_id IS NULL))`
	args := []any{provider, scopeID, scopeID}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, stmt, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count trace_resource_seen: %w", err)
	}
	return n, nil
}
