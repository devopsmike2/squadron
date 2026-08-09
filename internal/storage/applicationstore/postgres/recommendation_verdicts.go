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

// recommendation_verdicts.go — the Postgres port of the iac_recommendation_verdicts
// row (ADR 0033 slice 6): the operator-set exclusion flag (v0.89.37 chunk 4) AND
// the durable GitHub Checks-API check-run state (v0.89.42 chunk 1), both keyed by
// recommendation_id. Semantics mirror the sqlite backend EXACTLY.
//
// SetRecommendationExclusion returns prevExcluded (the exclude_from_learning value
// BEFORE the upsert; false on insert) so the handler emits an audit event on
// transitions only. Stamp rules (played out in Go, not SQL, so a single upsert
// suffices):
//   - transition to excluded=true (insert-with-true OR false→true): stamp
//     excluded_at + excluded_by from rec;
//   - transition to excluded=false (true→false): clear both to NULL;
//   - fresh insert with excluded=false: no stamp;
//   - no-op toggle on an existing row: preserve the current stamps.
// updated_at is always refreshed; created_at is preserved across updates. The
// read-then-upsert is wrapped in a transaction so it can't race a concurrent
// toggle.
//
// SetCheckRunForRecommendation upserts the 5 check_run_* columns + updated_at via
// a read-then-UPDATE-or-INSERT (no ON CONFLICT). On INSERT the row is created with
// exclude_from_learning=FALSE and excluded_at/excluded_by NULL (the row's
// existence means "Squadron has a check run on this PR," not "operator has
// acted"). On UPDATE the invariant scope tuple is NOT overwritten so a concurrent
// chunk-4 exclusion write on the same recommendation_id is never clobbered. status
// / conclusion may be empty (stored NULL) during transient states.
//
// GetCheckRunForRecommendation returns exists=false (no error) when no row
// matches, and a zero-value CheckRunRef + empty status/conclusion when the row
// exists but no check run has been written yet.
//
// exclude_from_learning is a native BOOLEAN; check_run_id is BIGINT. OSS tenant
// scoping is omitted (types.ExcludedRecommendation carries no tenant field).

func (s *Storage) SetRecommendationExclusion(
	ctx context.Context,
	rec types.ExcludedRecommendation,
	excluded bool,
) (bool, error) {
	if rec.RecommendationID == "" {
		return false, fmt.Errorf("recommendation_id required")
	}
	if rec.ConnectionID == "" || rec.AccountID == "" || rec.Region == "" {
		return false, fmt.Errorf("scope tuple (connection_id, account_id, region) required")
	}
	if rec.RecommendationKind == "" {
		return false, fmt.Errorf("recommendation_kind required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Step 1: read prior exclude_from_learning + created_at (row-existence probe).
	var (
		hadRow       bool
		prevExcluded bool
		prevCreated  time.Time
	)
	err = tx.QueryRowContext(ctx,
		`SELECT exclude_from_learning, created_at FROM iac_recommendation_verdicts WHERE recommendation_id = $1`,
		rec.RecommendationID).Scan(&prevExcluded, &prevCreated)
	switch {
	case err == nil:
		hadRow = true
	case errors.Is(err, sql.ErrNoRows):
		hadRow = false
	default:
		return false, fmt.Errorf("read prior exclusion: %w", err)
	}

	now := time.Now().UTC()
	excludedAt := rec.ExcludedAt
	if excluded && excludedAt.IsZero() {
		excludedAt = now
	}
	createdAt := now
	if hadRow {
		createdAt = prevCreated
	}

	// Compute the stamps to write per the transition rules.
	var stampAt any
	var stampBy any
	switch {
	case excluded && (!hadRow || !prevExcluded):
		stampAt = excludedAt
		stampBy = rec.ExcludedBy
	case !excluded && hadRow && prevExcluded:
		stampAt = nil
		stampBy = nil
	case !excluded && !hadRow:
		stampAt = nil
		stampBy = nil
	default:
		// No-op toggle on an existing row: preserve the current stamps.
		var curAt sql.NullTime
		var curBy sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT excluded_at, excluded_by FROM iac_recommendation_verdicts WHERE recommendation_id = $1`,
			rec.RecommendationID).Scan(&curAt, &curBy); err != nil {
			return false, fmt.Errorf("read prior stamps: %w", err)
		}
		if curAt.Valid {
			stampAt = curAt.Time
		}
		if curBy.Valid {
			stampBy = curBy.String
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO iac_recommendation_verdicts (
			recommendation_id, connection_id, account_id, region,
			recommendation_kind, resource_id, exclude_from_learning,
			excluded_at, excluded_by, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (recommendation_id) DO UPDATE SET
			connection_id         = excluded.connection_id,
			account_id            = excluded.account_id,
			region                = excluded.region,
			recommendation_kind   = excluded.recommendation_kind,
			resource_id           = excluded.resource_id,
			exclude_from_learning = excluded.exclude_from_learning,
			excluded_at           = excluded.excluded_at,
			excluded_by           = excluded.excluded_by,
			updated_at            = excluded.updated_at`,
		rec.RecommendationID, rec.ConnectionID, rec.AccountID, rec.Region,
		rec.RecommendationKind, nullString(rec.ResourceID), excluded,
		stampAt, stampBy, createdAt, now); err != nil {
		return false, fmt.Errorf("upsert recommendation exclusion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit exclusion upsert: %w", err)
	}
	return prevExcluded, nil
}

func (s *Storage) ListExcludedRecommendations(
	ctx context.Context,
	connectionID, accountID, region string,
	limit int,
) ([]types.ExcludedRecommendation, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if connectionID == "" || accountID == "" || region == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT recommendation_id, connection_id, account_id, region,
		       recommendation_kind, COALESCE(resource_id, ''),
		       excluded_at, COALESCE(excluded_by, '')
		FROM iac_recommendation_verdicts
		WHERE connection_id = $1 AND account_id = $2 AND region = $3
		  AND exclude_from_learning = TRUE
		ORDER BY excluded_at DESC
		LIMIT $4`, connectionID, accountID, region, limit)
	if err != nil {
		return nil, fmt.Errorf("list excluded recommendations: %w", err)
	}
	defer rows.Close()
	var out []types.ExcludedRecommendation
	for rows.Next() {
		var (
			rec        types.ExcludedRecommendation
			excludedAt sql.NullTime
		)
		if err := rows.Scan(&rec.RecommendationID, &rec.ConnectionID, &rec.AccountID, &rec.Region,
			&rec.RecommendationKind, &rec.ResourceID, &excludedAt, &rec.ExcludedBy); err != nil {
			return nil, fmt.Errorf("scan excluded recommendation row: %w", err)
		}
		if excludedAt.Valid {
			rec.ExcludedAt = excludedAt.Time.UTC()
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Storage) SetCheckRunForRecommendation(
	ctx context.Context,
	rec types.ExcludedRecommendation,
	ref types.CheckRunRef,
	status, conclusion string,
) error {
	if rec.RecommendationID == "" {
		return fmt.Errorf("recommendation_id required")
	}
	if rec.ConnectionID == "" || rec.AccountID == "" || rec.Region == "" {
		return fmt.Errorf("scope tuple (connection_id, account_id, region) required")
	}
	if rec.RecommendationKind == "" {
		return fmt.Errorf("recommendation_kind required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var hadRow bool
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM iac_recommendation_verdicts WHERE recommendation_id = $1`, rec.RecommendationID).Scan(&one)
	switch {
	case err == nil:
		hadRow = true
	case errors.Is(err, sql.ErrNoRows):
		hadRow = false
	default:
		return fmt.Errorf("read row presence: %w", err)
	}

	now := time.Now().UTC()
	// Nullable columns: empty values become SQL NULL so "no check run yet" stays
	// distinguishable from a future empty-string write.
	var owner, repo, checkID, headSHA, statusVal, conclusionVal any
	if ref.Owner != "" {
		owner = ref.Owner
	}
	if ref.Repo != "" {
		repo = ref.Repo
	}
	if ref.CheckID != 0 {
		checkID = ref.CheckID
	}
	if ref.HeadSHA != "" {
		headSHA = ref.HeadSHA
	}
	if status != "" {
		statusVal = status
	}
	if conclusion != "" {
		conclusionVal = conclusion
	}

	if hadRow {
		if _, err := tx.ExecContext(ctx, `
			UPDATE iac_recommendation_verdicts SET
				check_run_owner      = $2,
				check_run_repo       = $3,
				check_run_id         = $4,
				check_run_head_sha   = $5,
				check_run_status     = $6,
				check_run_conclusion = $7,
				check_run_updated_at = $8,
				updated_at           = $9
			WHERE recommendation_id = $1`,
			rec.RecommendationID, owner, repo, checkID, headSHA, statusVal, conclusionVal, now, now); err != nil {
			return fmt.Errorf("update check run state: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO iac_recommendation_verdicts (
				recommendation_id, connection_id, account_id, region,
				recommendation_kind, resource_id, exclude_from_learning,
				excluded_at, excluded_by,
				check_run_owner, check_run_repo, check_run_id, check_run_head_sha,
				check_run_status, check_run_conclusion, check_run_updated_at,
				created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,FALSE,NULL,NULL,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			rec.RecommendationID, rec.ConnectionID, rec.AccountID, rec.Region,
			rec.RecommendationKind, nullString(rec.ResourceID),
			owner, repo, checkID, headSHA, statusVal, conclusionVal, now, now, now); err != nil {
			return fmt.Errorf("insert check run state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit check run upsert: %w", err)
	}
	return nil
}

func (s *Storage) GetCheckRunForRecommendation(
	ctx context.Context,
	recommendationID string,
) (types.CheckRunRef, string, string, bool, error) {
	if recommendationID == "" {
		return types.CheckRunRef{}, "", "", false, fmt.Errorf("recommendation_id required")
	}
	var (
		ref        types.CheckRunRef
		status     string
		conclusion string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(check_run_owner, ''), COALESCE(check_run_repo, ''),
		       COALESCE(check_run_id, 0), COALESCE(check_run_head_sha, ''),
		       COALESCE(check_run_status, ''), COALESCE(check_run_conclusion, '')
		FROM iac_recommendation_verdicts WHERE recommendation_id = $1`, recommendationID).Scan(
		&ref.Owner, &ref.Repo, &ref.CheckID, &ref.HeadSHA, &status, &conclusion)
	if errors.Is(err, sql.ErrNoRows) {
		return types.CheckRunRef{}, "", "", false, nil
	}
	if err != nil {
		return types.CheckRunRef{}, "", "", false, fmt.Errorf("read check run state: %w", err)
	}
	return ref, status, conclusion, true, nil
}
