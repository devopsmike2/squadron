// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// verdicts.go — the Postgres port of the proposer-learning verdict sweeps (ADR
// 0033 slice 6):
//   - ListAIVerdictsForGroup (#633): AI-originated rollouts on a group with a
//     terminal approval verdict after `since`, excluding per-rollout opt-outs,
//     newest-verdict-first;
//   - ListDiscoveryVerdicts (#655): the recommendation.pr_merged +
//     recommendation.pr_closed_not_merged audit rows matching a scope tuple;
//   - ListCrossScopeDiscoveryVerdicts (#671..): the same audit rows from scopes
//     OTHER than excludeScopeID, tagged with origin Provider + ScopeID.
//
// Semantics mirror the sqlite backend EXACTLY. The rollout sweep reads the
// rollouts table (no tenant column in the OSS port, so no tenant predicate,
// matching Get/ListRollouts). The audit-events sweeps read the already-tenant-
// scoped audit_events table and add the tenant predicate via tenantScope, exactly
// as the sqlite backend does. The audit payload is stored as raw TEXT here, so the
// scope match casts NULLIF(payload,'')::jsonb ->> 'key' (NULLIF guards the
// empty-payload rows a nil-Payload event writes); the projection reads the payload
// string and parses it in Go so the decode is byte-identical to sqlite.

func (s *Storage) ListAIVerdictsForGroup(ctx context.Context, groupID string, since time.Time, limit int) ([]*types.Rollout, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+rolloutColumns+`
		FROM rollouts
		WHERE group_id = $1
		  AND proposed_by = 'ai'
		  AND (approved_at IS NOT NULL OR rejected_at IS NOT NULL)
		  AND COALESCE(approved_at, rejected_at) >= $2
		  AND exclude_from_learning = FALSE
		ORDER BY COALESCE(approved_at, rejected_at) DESC
		LIMIT $3`, groupID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list AI verdicts for group: %w", err)
	}
	defer rows.Close()
	var out []*types.Rollout
	for rows.Next() {
		r, err := scanRollout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Storage) ListDiscoveryVerdicts(
	ctx context.Context,
	connectionID, scopeID, region string,
	since time.Time, limit int,
) ([]*types.DiscoveryVerdict, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if connectionID == "" || scopeID == "" || region == "" {
		return nil, nil
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	q := `SELECT timestamp, event_type, payload FROM audit_events
		WHERE event_type IN ($1,$2)
		  AND timestamp >= $3
		  AND (NULLIF(payload,'')::jsonb ->> 'connection_id') = $4
		  AND ((NULLIF(payload,'')::jsonb ->> 'account_id') = $5
		       OR (NULLIF(payload,'')::jsonb ->> 'project_id') = $6
		       OR (NULLIF(payload,'')::jsonb ->> 'subscription_id') = $7
		       OR (NULLIF(payload,'')::jsonb ->> 'tenancy_ocid') = $8)
		  AND (NULLIF(payload,'')::jsonb ->> 'region') = $9`
	args := []any{
		"recommendation.pr_merged", "recommendation.pr_closed_not_merged",
		since, connectionID, scopeID, scopeID, scopeID, scopeID, region,
	}
	n := 9
	if apply {
		n++
		q += fmt.Sprintf(" AND tenant_id = $%d", n)
		args = append(args, tenant)
	}
	n++
	q += fmt.Sprintf(" ORDER BY timestamp DESC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list discovery verdicts: %w", err)
	}
	defer rows.Close()
	var out []*types.DiscoveryVerdict
	for rows.Next() {
		var (
			ts        time.Time
			eventType string
			payload   sql.NullString
		)
		if err := rows.Scan(&ts, &eventType, &payload); err != nil {
			return nil, fmt.Errorf("scan discovery verdict: %w", err)
		}
		rec, ok := decodeDiscoveryVerdict(ts, eventType, payload)
		if !ok {
			continue
		}
		// Same-scope rows skip anything whose branch didn't parse a kind cleanly.
		if rec.RecommendationKind == "" {
			continue
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Storage) ListCrossScopeDiscoveryVerdicts(
	ctx context.Context,
	excludeScopeID string,
	since time.Time, limit int,
) ([]*types.DiscoveryVerdict, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	q := `SELECT timestamp, event_type, payload FROM audit_events
		WHERE event_type IN ($1,$2)
		  AND timestamp >= $3
		  AND COALESCE(NULLIF(payload,'')::jsonb ->> 'account_id', '') != $4
		  AND COALESCE(NULLIF(payload,'')::jsonb ->> 'project_id', '') != $5
		  AND COALESCE(NULLIF(payload,'')::jsonb ->> 'subscription_id', '') != $6
		  AND COALESCE(NULLIF(payload,'')::jsonb ->> 'tenancy_ocid', '') != $7`
	args := []any{
		"recommendation.pr_merged", "recommendation.pr_closed_not_merged",
		since, excludeScopeID, excludeScopeID, excludeScopeID, excludeScopeID,
	}
	n := 7
	if apply {
		n++
		q += fmt.Sprintf(" AND tenant_id = $%d", n)
		args = append(args, tenant)
	}
	n++
	q += fmt.Sprintf(" ORDER BY timestamp DESC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list cross-scope discovery verdicts: %w", err)
	}
	defer rows.Close()
	var out []*types.DiscoveryVerdict
	for rows.Next() {
		var (
			ts        time.Time
			eventType string
			payload   sql.NullString
		)
		if err := rows.Scan(&ts, &eventType, &payload); err != nil {
			return nil, fmt.Errorf("scan cross-scope verdict: %w", err)
		}
		rec, ok := decodeDiscoveryVerdict(ts, eventType, payload)
		if !ok {
			continue
		}
		if rec.RecommendationKind == "" {
			continue
		}
		provider, scopeID := inferDiscoveryProviderScope(payload)
		if scopeID == "" {
			continue
		}
		rec.Provider = provider
		rec.ScopeID = scopeID
		out = append(out, rec)
	}
	return out, rows.Err()
}

// decodeDiscoveryVerdict maps one audit row into a DiscoveryVerdict, mirroring the
// sqlite backend's per-row Go decode. ok=false when the event_type is neither
// verdict type. The State discriminator selects which payload keys carry the
// timestamp + actor (merged_at/merged_by vs closed_at/closed_by); when the
// payload's explicit timestamp string parses it overrides the audit row's own
// timestamp, keeping the projection aligned with the GitHub-side truth.
func decodeDiscoveryVerdict(ts time.Time, eventType string, payload sql.NullString) (*types.DiscoveryVerdict, bool) {
	rec := &types.DiscoveryVerdict{PRMergedAt: ts}
	var actorKey, tsKey string
	switch eventType {
	case "recommendation.pr_merged":
		rec.State = "merged"
		actorKey, tsKey = "merged_by", "merged_at"
	case "recommendation.pr_closed_not_merged":
		rec.State = "closed_not_merged"
		actorKey, tsKey = "closed_by", "closed_at"
	default:
		return nil, false
	}
	if p, ok := unmarshalPayload(payload); ok {
		if v, ok := p["pr_url"].(string); ok {
			rec.PRURL = v
		}
		if v, ok := p["branch"].(string); ok {
			rec.Branch = v
		}
		if v, ok := p[actorKey].(string); ok {
			rec.MergedBy = v
		}
		if v, ok := p["recommendation_kind"].(string); ok {
			rec.RecommendationKind = v
		}
		if v, ok := p[tsKey].(string); ok && v != "" {
			if parsed, perr := time.Parse(time.RFC3339, v); perr == nil {
				rec.PRMergedAt = parsed
			}
		}
	}
	return rec, true
}

// inferDiscoveryProviderScope derives the origin cloud + scope id from a verdict
// payload by which scope key is set. Backs cross-cloud citations. Mirrors sqlite.
func inferDiscoveryProviderScope(payload sql.NullString) (provider, scopeID string) {
	p, ok := unmarshalPayload(payload)
	if !ok {
		return "", ""
	}
	if v, _ := p["account_id"].(string); v != "" {
		return "aws", v
	}
	if v, _ := p["project_id"].(string); v != "" {
		return "gcp", v
	}
	if v, _ := p["subscription_id"].(string); v != "" {
		return "azure", v
	}
	if v, _ := p["tenancy_ocid"].(string); v != "" {
		return "oci", v
	}
	return "", ""
}

func unmarshalPayload(payload sql.NullString) (map[string]any, bool) {
	if !payload.Valid || payload.String == "" {
		return nil, false
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(payload.String), &p); err != nil {
		return nil, false
	}
	return p, true
}
