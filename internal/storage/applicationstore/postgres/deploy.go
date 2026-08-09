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

// deploy.go — the Postgres port of deploy targets + deploy runs (v0.34 GitHub
// Actions integration) and expected agents (v0.32 inventory reconciliation),
// ADR 0033 slice 5. Semantics mirror the sqlite backend EXACTLY, including the
// places these entities DIVERGE from the CRUD norm:
//
//   - CreateDeployTarget requires a non-empty ID and defaults Provider="github",
//     GitHubBranch="main", DefaultInputs={}, and CreatedAt/UpdatedAt to now;
//   - UpdateDeployTarget keeps the existing credential when EncryptedCredential
//     is empty (so edit forms need not round-trip the secret) and — like sqlite —
//     does NOT report a missing row (update-missing is a no-op, not an error);
//   - GetDeployTarget on a missing id returns (nil, nil); it returns the
//     credential bytes + HasCredential, whereas ListDeployTargets STRIPS the
//     credential bytes (HasCredential only) and orders by name;
//   - DeleteDeployTarget, like sqlite, does NOT report a missing row;
//   - deploy runs mirror the same "update/delete never report missing" shape;
//     CreateDeployRun defaults Status="queued", RequestedAt=now, Inputs={},
//     ExpectedHosts=[]; ListDeployRuns filters by target_id / status, orders by
//     requested_at DESC, and applies Limit ONLY when > 0 (no implicit default);
//   - UpsertExpectedAgent upserts on the hostname key (defaults ExpectedSince=now
//     and always stamps UpdatedAt=now); DeleteExpectedAgent is a no-op on a
//     missing hostname; ListExpectedAgents filters by source and orders by
//     hostname; ReplaceExpectedAgentsForSource atomically drops the source's rows
//     and re-inserts the supplied set in one transaction.
//
// tenant_id is persisted + surfaced for deploy targets only (types.DeployTarget
// carries a TenantID field); deploy runs and expected agents carry no tenant
// field, so — as with the agents/configs ports — no tenant column exists for
// them and OSS stays single-tenant.

// ============================================================================
// Deploy targets.
// ============================================================================

func (s *Storage) CreateDeployTarget(ctx context.Context, t *types.DeployTarget) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("id required")
	}
	if t.Provider == "" {
		t.Provider = "github"
	}
	if t.GitHubBranch == "" {
		t.GitHubBranch = "main"
	}
	if t.DefaultInputs == nil {
		t.DefaultInputs = map[string]string{}
	}
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	inputsJSON, err := json.Marshal(t.DefaultInputs)
	if err != nil {
		return fmt.Errorf("marshal deploy target inputs: %w", err)
	}
	// OSS single-tenant: an empty TenantID persists as "default", mirroring the
	// sqlite system-context insert (identity.DefaultTenant).
	tenant := t.TenantID
	if tenant == "" {
		tenant = "default"
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO deploy_targets (
			id, name, provider, github_owner, github_repo, github_workflow,
			github_branch, encrypted_credential, default_inputs,
			config_id, inventory_path, tenant_id, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		t.ID, t.Name, t.Provider, t.GitHubOwner, t.GitHubRepo, t.GitHubWorkflow,
		t.GitHubBranch, t.EncryptedCredential, inputsJSON,
		t.ConfigID, t.InventoryPath, tenant, t.CreatedAt.UTC(), t.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("create deploy target: %w", err)
	}
	return nil
}

func (s *Storage) UpdateDeployTarget(ctx context.Context, t *types.DeployTarget) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("id required")
	}
	t.UpdatedAt = time.Now().UTC()
	inputsJSON, err := json.Marshal(t.DefaultInputs)
	if err != nil {
		return fmt.Errorf("marshal deploy target inputs: %w", err)
	}
	// Only re-write the credential when it's been re-supplied; leaving it alone
	// lets the UI render edit forms without round-tripping the secret. Mirrors
	// sqlite, including the absence of a missing-row error.
	if len(t.EncryptedCredential) > 0 {
		_, err = s.db.ExecContext(ctx, `
			UPDATE deploy_targets SET
				name = $2, provider = $3, github_owner = $4, github_repo = $5, github_workflow = $6,
				github_branch = $7, encrypted_credential = $8, default_inputs = $9,
				config_id = $10, inventory_path = $11, updated_at = $12
			WHERE id = $1`,
			t.ID, t.Name, t.Provider, t.GitHubOwner, t.GitHubRepo, t.GitHubWorkflow,
			t.GitHubBranch, t.EncryptedCredential, inputsJSON,
			t.ConfigID, t.InventoryPath, t.UpdatedAt.UTC())
		if err != nil {
			return fmt.Errorf("update deploy target (with credential): %w", err)
		}
		return nil
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE deploy_targets SET
			name = $2, provider = $3, github_owner = $4, github_repo = $5, github_workflow = $6,
			github_branch = $7, default_inputs = $8,
			config_id = $9, inventory_path = $10, updated_at = $11
		WHERE id = $1`,
		t.ID, t.Name, t.Provider, t.GitHubOwner, t.GitHubRepo, t.GitHubWorkflow,
		t.GitHubBranch, inputsJSON, t.ConfigID, t.InventoryPath, t.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("update deploy target: %w", err)
	}
	return nil
}

const deployTargetColumns = `id, name, provider, github_owner, github_repo, github_workflow, ` +
	`github_branch, encrypted_credential, default_inputs, config_id, inventory_path, ` +
	`tenant_id, created_at, updated_at`

func (s *Storage) GetDeployTarget(ctx context.Context, id string) (*types.DeployTarget, error) {
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+deployTargetColumns+` FROM deploy_targets WHERE id = $1`, id)
	t, err := scanDeployTarget(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (s *Storage) ListDeployTargets(ctx context.Context) ([]*types.DeployTarget, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deployTargetColumns+` FROM deploy_targets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list deploy targets: %w", err)
	}
	defer rows.Close()
	out := []*types.DeployTarget{}
	for rows.Next() {
		// List strips the credential bytes; callers fetch them explicitly via
		// GetDeployTarget when a dispatch needs them.
		t, err := scanDeployTarget(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Storage) DeleteDeployTarget(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("id required")
	}
	// Mirrors sqlite: a missing row is not reported as an error.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM deploy_targets WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete deploy target: %w", err)
	}
	return nil
}

// scanDeployTarget reads one deploy target. keepCredential controls whether the
// credential bytes are surfaced (Get) or stripped (List); HasCredential is set
// either way from the presence of stored bytes.
func scanDeployTarget(sc scanner, keepCredential bool) (*types.DeployTarget, error) {
	t := &types.DeployTarget{}
	var (
		cred       []byte
		inputsJSON []byte
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.Provider, &t.GitHubOwner, &t.GitHubRepo, &t.GitHubWorkflow,
		&t.GitHubBranch, &cred, &inputsJSON, &t.ConfigID, &t.InventoryPath,
		&t.TenantID, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.HasCredential = len(cred) > 0
	if keepCredential {
		t.EncryptedCredential = cred
	}
	if len(inputsJSON) > 0 {
		if err := json.Unmarshal(inputsJSON, &t.DefaultInputs); err != nil {
			return nil, fmt.Errorf("unmarshal deploy target inputs: %w", err)
		}
	}
	return t, nil
}

// ============================================================================
// Deploy runs.
// ============================================================================

const deployRunColumns = `id, target_id, requested_by, requested_at, inputs, ` +
	`github_run_id, github_run_url, status, conclusion, completed_at, ` +
	`expected_hosts, verification_state, verified_at, notes`

func (s *Storage) CreateDeployRun(ctx context.Context, r *types.DeployRun) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("id required")
	}
	if r.Status == "" {
		r.Status = "queued"
	}
	if r.RequestedAt.IsZero() {
		r.RequestedAt = time.Now().UTC()
	}
	if r.Inputs == nil {
		r.Inputs = map[string]string{}
	}
	if r.ExpectedHosts == nil {
		r.ExpectedHosts = []string{}
	}
	inputsJSON, err := json.Marshal(r.Inputs)
	if err != nil {
		return fmt.Errorf("marshal deploy run inputs: %w", err)
	}
	hostsJSON, err := json.Marshal(r.ExpectedHosts)
	if err != nil {
		return fmt.Errorf("marshal deploy run expected hosts: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO deploy_runs (`+deployRunColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		r.ID, r.TargetID, r.RequestedBy, r.RequestedAt.UTC(), inputsJSON,
		r.GitHubRunID, r.GitHubRunURL, r.Status, r.Conclusion, nullTime(r.CompletedAt),
		hostsJSON, r.VerificationState, nullTime(r.VerifiedAt), r.Notes)
	if err != nil {
		return fmt.Errorf("create deploy run: %w", err)
	}
	return nil
}

// UpdateDeployRun rewrites the mutable columns (target_id / requested_by /
// requested_at are immutable). Mirrors sqlite, including the absence of a
// missing-row error.
func (s *Storage) UpdateDeployRun(ctx context.Context, r *types.DeployRun) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("id required")
	}
	if r.Inputs == nil {
		r.Inputs = map[string]string{}
	}
	if r.ExpectedHosts == nil {
		r.ExpectedHosts = []string{}
	}
	inputsJSON, err := json.Marshal(r.Inputs)
	if err != nil {
		return fmt.Errorf("marshal deploy run inputs: %w", err)
	}
	hostsJSON, err := json.Marshal(r.ExpectedHosts)
	if err != nil {
		return fmt.Errorf("marshal deploy run expected hosts: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE deploy_runs SET
			inputs = $2, github_run_id = $3, github_run_url = $4, status = $5,
			conclusion = $6, completed_at = $7, expected_hosts = $8,
			verification_state = $9, verified_at = $10, notes = $11
		WHERE id = $1`,
		r.ID, inputsJSON, r.GitHubRunID, r.GitHubRunURL, r.Status,
		r.Conclusion, nullTime(r.CompletedAt), hostsJSON,
		r.VerificationState, nullTime(r.VerifiedAt), r.Notes)
	if err != nil {
		return fmt.Errorf("update deploy run: %w", err)
	}
	return nil
}

func (s *Storage) GetDeployRun(ctx context.Context, id string) (*types.DeployRun, error) {
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+deployRunColumns+` FROM deploy_runs WHERE id = $1`, id)
	r, err := scanDeployRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Storage) ListDeployRuns(ctx context.Context, filter types.DeployRunFilter) ([]*types.DeployRun, error) {
	query := `SELECT ` + deployRunColumns + ` FROM deploy_runs WHERE 1=1`
	var args []any
	n := 0
	if filter.TargetID != "" {
		n++
		query += fmt.Sprintf(" AND target_id = $%d", n)
		args = append(args, filter.TargetID)
	}
	if filter.Status != "" {
		n++
		query += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, filter.Status)
	}
	query += " ORDER BY requested_at DESC"
	// Unlike action requests / rollouts, ListDeployRuns applies Limit only when
	// explicitly set (no implicit default), matching sqlite.
	if filter.Limit > 0 {
		n++
		query += fmt.Sprintf(" LIMIT $%d", n)
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list deploy runs: %w", err)
	}
	defer rows.Close()
	out := []*types.DeployRun{}
	for rows.Next() {
		r, err := scanDeployRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanDeployRun(sc scanner) (*types.DeployRun, error) {
	r := &types.DeployRun{}
	var (
		inputsJSON  []byte
		hostsJSON   []byte
		completedAt sql.NullTime
		verifiedAt  sql.NullTime
	)
	if err := sc.Scan(&r.ID, &r.TargetID, &r.RequestedBy, &r.RequestedAt, &inputsJSON,
		&r.GitHubRunID, &r.GitHubRunURL, &r.Status, &r.Conclusion, &completedAt,
		&hostsJSON, &r.VerificationState, &verifiedAt, &r.Notes); err != nil {
		return nil, err
	}
	if completedAt.Valid {
		t := completedAt.Time
		r.CompletedAt = &t
	}
	if verifiedAt.Valid {
		t := verifiedAt.Time
		r.VerifiedAt = &t
	}
	if len(inputsJSON) > 0 {
		if err := json.Unmarshal(inputsJSON, &r.Inputs); err != nil {
			return nil, fmt.Errorf("unmarshal deploy run inputs: %w", err)
		}
	}
	if len(hostsJSON) > 0 {
		if err := json.Unmarshal(hostsJSON, &r.ExpectedHosts); err != nil {
			return nil, fmt.Errorf("unmarshal deploy run expected hosts: %w", err)
		}
	}
	return r, nil
}

// nullTime maps a *time.Time to a value pgx stores as NULL when nil or zero,
// mirroring the sqlite backend's nullableTime helper.
func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}

// ============================================================================
// Expected agents.
// ============================================================================

const expectedAgentColumns = `hostname, labels, source, expected_since, updated_at, notes`

func (s *Storage) UpsertExpectedAgent(ctx context.Context, e *types.ExpectedAgent) error {
	if e == nil || e.Hostname == "" {
		return fmt.Errorf("hostname required")
	}
	if e.ExpectedSince.IsZero() {
		e.ExpectedSince = time.Now().UTC()
	}
	e.UpdatedAt = time.Now().UTC()
	labelsJSON, err := json.Marshal(e.Labels)
	if err != nil {
		return fmt.Errorf("marshal expected agent labels: %w", err)
	}
	// Upsert on the hostname key: an existing row's labels/source/updated_at/notes
	// are overwritten (expected_since is preserved by the DO UPDATE list omitting
	// it, matching the sqlite ON CONFLICT set).
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO expected_agents (`+expectedAgentColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (hostname) DO UPDATE SET
			labels = excluded.labels,
			source = excluded.source,
			updated_at = excluded.updated_at,
			notes = excluded.notes`,
		e.Hostname, labelsJSON, e.Source, e.ExpectedSince.UTC(), e.UpdatedAt.UTC(), e.Notes)
	if err != nil {
		return fmt.Errorf("upsert expected agent: %w", err)
	}
	return nil
}

func (s *Storage) DeleteExpectedAgent(ctx context.Context, hostname string) error {
	if hostname == "" {
		return fmt.Errorf("hostname required")
	}
	// Mirrors sqlite: a missing hostname is not reported as an error.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM expected_agents WHERE hostname = $1`, hostname); err != nil {
		return fmt.Errorf("delete expected agent: %w", err)
	}
	return nil
}

func (s *Storage) ListExpectedAgents(ctx context.Context, source string) ([]*types.ExpectedAgent, error) {
	query := `SELECT ` + expectedAgentColumns + ` FROM expected_agents WHERE 1=1`
	var args []any
	if source != "" {
		query += " AND source = $1"
		args = append(args, source)
	}
	query += " ORDER BY hostname"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list expected agents: %w", err)
	}
	defer rows.Close()
	out := []*types.ExpectedAgent{}
	for rows.Next() {
		e, err := scanExpectedAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReplaceExpectedAgentsForSource atomically drops every row tagged with source
// and re-inserts the supplied set, all in one transaction so a partial failure
// leaves the previous inventory intact. Mirrors the sqlite bulk-rotate.
func (s *Storage) ReplaceExpectedAgentsForSource(ctx context.Context, source string, entries []*types.ExpectedAgent) error {
	if source == "" {
		return fmt.Errorf("source required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM expected_agents WHERE source = $1`, source); err != nil {
		return fmt.Errorf("delete by source: %w", err)
	}

	now := time.Now().UTC()
	for _, e := range entries {
		if e == nil || e.Hostname == "" {
			continue
		}
		labelsJSON, err := json.Marshal(e.Labels)
		if err != nil {
			return fmt.Errorf("marshal expected agent labels: %w", err)
		}
		expected := e.ExpectedSince
		if expected.IsZero() {
			expected = now
		}
		// INSERT ... ON CONFLICT DO UPDATE mirrors sqlite's INSERT OR REPLACE:
		// a hostname that survived under a different source is overwritten by PK.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO expected_agents (`+expectedAgentColumns+`)
			VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT (hostname) DO UPDATE SET
				labels = excluded.labels,
				source = excluded.source,
				expected_since = excluded.expected_since,
				updated_at = excluded.updated_at,
				notes = excluded.notes`,
			e.Hostname, labelsJSON, source, expected.UTC(), now, e.Notes); err != nil {
			return fmt.Errorf("insert %s: %w", e.Hostname, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func scanExpectedAgent(sc scanner) (*types.ExpectedAgent, error) {
	e := &types.ExpectedAgent{}
	var labelsJSON []byte
	if err := sc.Scan(&e.Hostname, &labelsJSON, &e.Source,
		&e.ExpectedSince, &e.UpdatedAt, &e.Notes); err != nil {
		return nil, err
	}
	if len(labelsJSON) > 0 {
		if err := json.Unmarshal(labelsJSON, &e.Labels); err != nil {
			return nil, fmt.Errorf("unmarshal expected agent labels: %w", err)
		}
	}
	return e, nil
}
