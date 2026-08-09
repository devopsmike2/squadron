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

// api_tokens.go — the Postgres port of API bearer tokens (ADR 0033 slice 6).
// Semantics mirror the sqlite backend EXACTLY:
//   - CreateAPIToken persists the tenant_id (types.APIToken carries TenantID); an
//     empty TenantID falls back to "default" (the OSS single tenant), matching the
//     sqlite resolution that lands on identity.DefaultTenant in the inert path;
//   - GetAPITokenByHash looks up BY HASH ONLY (the pre-auth path, before any
//     tenant is known) and returns (nil, nil) on a miss;
//   - ListAPITokens returns every token, revoked or not, newest-first;
//   - UpdateAPITokenLastUsed is best-effort (no missing-row error);
//   - RevokeAPIToken stamps revoked_at only when currently NULL and is idempotent
//     (no missing-row error).
//
// scopes is persisted VERBATIM as a JSON-array string ('[]' = legacy full-access)
// so the empty-array sentinel round-trips exactly as sqlite stores it. As with the
// deploy_targets port, tenant_id is persisted + surfaced but no tenant predicate
// is applied (OSS single-tenant).

const apiTokenColumns = `id, label, hash, scopes, created_at, last_used_at, revoked_at, expires_at, tenant_id`

func (s *Storage) CreateAPIToken(ctx context.Context, t *types.APIToken) error {
	scopesJSON, err := marshalScopes(t.Scopes)
	if err != nil {
		return err
	}
	tenantID := t.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO api_tokens (`+apiTokenColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		t.ID, t.Label, t.Hash, scopesJSON, t.CreatedAt, t.LastUsedAt, t.RevokedAt, t.ExpiresAt, tenantID)
	if err != nil {
		return fmt.Errorf("create api token: %w", err)
	}
	return nil
}

func (s *Storage) GetAPITokenByHash(ctx context.Context, hash string) (*types.APIToken, error) {
	// BY HASH ONLY — this is the pre-auth validation path, run before any tenant
	// is known; the tenant is read off the returned row. Mirrors sqlite.
	row := s.db.QueryRowContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE hash = $1`, hash)
	t, err := scanAPIToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (s *Storage) ListAPITokens(ctx context.Context) ([]*types.APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()
	var out []*types.APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateAPITokenLastUsed touches last_used_at. Best-effort: no missing-row error,
// matching the sqlite backend (the middleware fires this asynchronously).
func (s *Storage) UpdateAPITokenLastUsed(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = $2 WHERE id = $1`, id, at); err != nil {
		return fmt.Errorf("update api token last_used_at: %w", err)
	}
	return nil
}

// RevokeAPIToken stamps revoked_at only when currently NULL (already-revoked
// tokens keep their original stamp). Idempotent: revoking a missing or
// already-revoked token is not an error, matching the sqlite backend.
func (s *Storage) RevokeAPIToken(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, at); err != nil {
		return fmt.Errorf("revoke api token: %w", err)
	}
	return nil
}

func scanAPIToken(sc scanner) (*types.APIToken, error) {
	t := &types.APIToken{}
	var (
		scopesJSON                       string
		lastUsedAt, revokedAt, expiresAt sql.NullTime
	)
	if err := sc.Scan(&t.ID, &t.Label, &t.Hash, &scopesJSON, &t.CreatedAt,
		&lastUsedAt, &revokedAt, &expiresAt, &t.TenantID); err != nil {
		return nil, err
	}
	t.Scopes = unmarshalScopes(scopesJSON)
	if lastUsedAt.Valid {
		t.LastUsedAt = &lastUsedAt.Time
	}
	if revokedAt.Valid {
		t.RevokedAt = &revokedAt.Time
	}
	if expiresAt.Valid {
		t.ExpiresAt = &expiresAt.Time
	}
	return t, nil
}

// marshalScopes / unmarshalScopes mirror the sqlite helpers: nil and empty both
// serialize to '[]', and '[]' / "" both decode back to nil so the empty-equals-
// legacy-full-access contract stays byte-identical across backends.
func marshalScopes(s []string) (string, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal scopes: %w", err)
	}
	return string(b), nil
}

func unmarshalScopes(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
