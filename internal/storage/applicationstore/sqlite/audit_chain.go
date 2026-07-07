// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	chain "github.com/devopsmike2/squadron/internal/audit/chain"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// VerifyAuditChain walks the caller's tenant hash-chain (ADR 0027 slice 1) and
// reports whether it is intact. Self-tenant ONLY: the tenant is resolved the
// same way the append path resolves it (tenantScope; DefaultTenant when the
// context is unstamped or system). Cross-tenant verification is an enterprise
// feature (a later slice) and is intentionally NOT implemented here.
//
// The pure walk (chain-start leniency + contiguity + prev_hash link + row_hash
// recompute) lives in internal/audit/chain so the offline verifier and both
// stores share one source of truth. This method layers the DB-coupled
// checkpoint anchoring on top of that pure result.
//
// Walk semantics:
//   - The FIRST surviving row's prev_hash is accepted as-is — its predecessor
//     may have been legitimately garbage-collected by the opt-in retention
//     sweep, so a non-empty prev_hash with no visible predecessor is NOT a
//     tamper signal. CoversFromSeq records that chain-start.
//   - Every subsequent row must be contiguous (seq == prev.seq+1; a gap is a
//     middle deletion), must link (prev_hash == the prior row's row_hash), and
//     must re-hash to its stored row_hash (content-edit / reorder detection).
//
// On the first break it returns {OK:false, RowsVerified:<verified so far>,
// FirstBreakSeq:<seq>, Detail, CoversFromSeq}. When every row passes it returns
// {OK:true, RowsVerified:n, CoversFromSeq:<first seq>}.
func (s *Storage) VerifyAuditChain(ctx context.Context) (*types.AuditChainVerification, error) {
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	if !apply {
		tenant = identity.DefaultTenant
	}

	dbRows, err := s.db.QueryContext(ctx,
		`SELECT id, actor, event_type, target_type, target_id, action, payload, seq, prev_hash, row_hash
		 FROM audit_events WHERE tenant_id = ? AND seq IS NOT NULL ORDER BY seq ASC`,
		tenant,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to read audit chain: %w", err)
	}
	defer dbRows.Close()

	var rows []chain.Row
	for dbRows.Next() {
		var (
			id, actor, eventType, targetType, action string
			targetID, payload                        sql.NullString
			seq                                      int64
			prevHash, rowHash                        sql.NullString
		)
		if err := dbRows.Scan(&id, &actor, &eventType, &targetType, &targetID, &action, &payload, &seq, &prevHash, &rowHash); err != nil {
			return nil, fmt.Errorf("failed to scan audit chain row: %w", err)
		}
		rows = append(rows, chain.Row{
			ID:         id,
			Actor:      actor,
			EventType:  eventType,
			TargetType: targetType,
			TargetID:   targetID.String,
			Action:     action,
			Payload:    payload.String,
			Tenant:     tenant,
			Seq:        seq,
			PrevHash:   prevHash.String,
			RowHash:    rowHash.String,
		})
	}
	if err := dbRows.Err(); err != nil {
		return nil, fmt.Errorf("audit chain iteration: %w", err)
	}

	// ADR 0027 slice 2 — load this tenant's latest retention checkpoint so the
	// first surviving row can be POSITIVELY anchored to a known-good boundary.
	// A missing checkpoint is fine (legacy prunes have none) — anchoring only
	// ever adds a positive signal, it never changes pass/fail.
	var (
		cpSeq     sql.NullInt64
		cpRowHash sql.NullString
	)
	_ = s.db.QueryRowContext(ctx,
		`SELECT checkpoint_seq, checkpoint_row_hash FROM audit_chain_checkpoints WHERE tenant_id = ? ORDER BY checkpoint_seq DESC LIMIT 1`,
		tenant,
	).Scan(&cpSeq, &cpRowHash)

	res := chain.Verify(rows)
	out := &types.AuditChainVerification{
		OK:            res.OK,
		RowsVerified:  res.RowsVerified,
		FirstBreakSeq: res.FirstBreakSeq,
		Detail:        res.Detail,
		CoversFromSeq: res.CoversFromSeq,
		HeadSeq:       res.HeadSeq,
		HeadRowHash:   res.HeadRowHash,
	}

	// Anchor the first surviving row to the latest checkpoint: the checkpoint
	// records the PRUNED head, so an intact prefix-prune leaves this row linking
	// straight to it. Anchoring only reports on an intact chain (matching the
	// prior in-loop behaviour, which only surfaced anchoring on the OK return).
	if res.OK && len(rows) > 0 {
		start := rows[0]
		if cpSeq.Valid && cpRowHash.Valid && start.PrevHash == cpRowHash.String && start.Seq == cpSeq.Int64+1 {
			out.AnchoredByCheckpoint = true
			out.CheckpointSeq = cpSeq.Int64
		}
	}

	return out, nil
}

// WriteAuditCheckpoint upserts a retention/chain reconciliation checkpoint
// (ADR 0027 slice 2), keyed by (tenant_id, checkpoint_seq). Re-recording the
// same pruned head (e.g. an idempotent re-run of a prune that pruned nothing
// new) overwrites the prior row's mutable columns rather than erroring.
func (s *Storage) WriteAuditCheckpoint(ctx context.Context, cp types.AuditCheckpoint) error {
	var sealed any
	if cp.SealedSig != "" {
		sealed = cp.SealedSig
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_chain_checkpoints
		    (tenant_id, checkpoint_seq, checkpoint_row_hash, rows_pruned, kind, created_at, sealed_sig)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, checkpoint_seq) DO UPDATE SET
		    checkpoint_row_hash = excluded.checkpoint_row_hash,
		    rows_pruned         = excluded.rows_pruned,
		    kind                = excluded.kind,
		    created_at          = excluded.created_at,
		    sealed_sig          = excluded.sealed_sig`,
		cp.Tenant, cp.CheckpointSeq, cp.CheckpointRowHash, cp.RowsPruned, cp.Kind, cp.CreatedAt.UTC(), sealed,
	)
	if err != nil {
		return fmt.Errorf("write audit checkpoint: %w", err)
	}
	return nil
}

// ListAuditCheckpoints returns a tenant's retention checkpoints, newest seq
// first (ADR 0027 slice 2). sealed_sig is nullable (unused in OSS).
func (s *Storage) ListAuditCheckpoints(ctx context.Context, tenant string) ([]types.AuditCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, checkpoint_seq, checkpoint_row_hash, rows_pruned, kind, created_at, sealed_sig
		 FROM audit_chain_checkpoints WHERE tenant_id = ? ORDER BY checkpoint_seq DESC`,
		tenant,
	)
	if err != nil {
		return nil, fmt.Errorf("list audit checkpoints: %w", err)
	}
	defer rows.Close()

	var out []types.AuditCheckpoint
	for rows.Next() {
		var (
			cp     types.AuditCheckpoint
			sealed sql.NullString
			ts     time.Time
		)
		if err := rows.Scan(&cp.Tenant, &cp.CheckpointSeq, &cp.CheckpointRowHash, &cp.RowsPruned, &cp.Kind, &ts, &sealed); err != nil {
			return nil, fmt.Errorf("scan audit checkpoint: %w", err)
		}
		cp.CreatedAt = ts
		cp.SealedSig = sealed.String
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate audit checkpoints: %w", err)
	}
	return out, nil
}
