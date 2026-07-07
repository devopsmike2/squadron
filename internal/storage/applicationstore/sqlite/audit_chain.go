// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// auditRowHash computes the tamper-evident chain hash for one audit row
// (ADR 0027 slice 1). It is a length-prefixed field concatenation
// (injection-safe: because every field is prefixed with its byte length, no
// content an attacker can smuggle into a field can shift the field boundaries)
// over the immutable content columns + the per-tenant seq, chained with the
// previous row's row_hash.
//
// Timestamp / created_at are DELIBERATELY excluded: they round-trip through a
// DB-dependent time format that could drift between append and verify, and the
// immutable content columns + per-tenant seq already uniquely pin the event.
// Any edit, middle-deletion, or reorder still breaks the chain because seq is
// hashed and prev_hash links each row to its predecessor.
//
// payloadStr MUST be byte-identical to what is written to (and read back from)
// the payload column. The append path passes the exact string it INSERTs; the
// verify path passes the DB payload string. Taking the raw string (not the
// map) removes any chance of re-marshal drift between append and verify.
func auditRowHash(id, actor, eventType, targetType, targetID, action, payloadStr, tenant string, seq int64, prevHash string) string {
	h := sha256.New()
	writeField := func(s string) {
		fmt.Fprintf(h, "%d:", len(s))
		fmt.Fprint(h, s)
	}
	writeField(id)
	writeField(actor)
	writeField(eventType)
	writeField(targetType)
	writeField(targetID)
	writeField(action)
	writeField(payloadStr)
	writeField(tenant)
	writeField(strconv.FormatInt(seq, 10))
	fmt.Fprint(h, prevHash)
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyAuditChain walks the caller's tenant hash-chain (ADR 0027 slice 1) and
// reports whether it is intact. Self-tenant ONLY: the tenant is resolved the
// same way the append path resolves it (tenantScope; DefaultTenant when the
// context is unstamped or system). Cross-tenant verification is an enterprise
// feature (a later slice) and is intentionally NOT implemented here.
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

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, actor, event_type, target_type, target_id, action, payload, seq, prev_hash, row_hash
		 FROM audit_events WHERE tenant_id = ? AND seq IS NOT NULL ORDER BY seq ASC`,
		tenant,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to read audit chain: %w", err)
	}
	defer rows.Close()

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

	var (
		count       int
		first       = true
		coversFrom  int64
		prevSeq     int64
		prevRowHash string
		headSeq     int64
		headRowHash string
		anchored    bool
		anchorCPSeq int64
	)
	for rows.Next() {
		var (
			id, actor, eventType, targetType, action string
			targetID, payload                        sql.NullString
			seq                                      int64
			prevHash, rowHash                        sql.NullString
		)
		if err := rows.Scan(&id, &actor, &eventType, &targetType, &targetID, &action, &payload, &seq, &prevHash, &rowHash); err != nil {
			return nil, fmt.Errorf("failed to scan audit chain row: %w", err)
		}
		ph := prevHash.String

		if first {
			coversFrom = seq
			expected := auditRowHash(id, actor, eventType, targetType, targetID.String, action, payload.String, tenant, seq, ph)
			if expected != rowHash.String {
				return &types.AuditChainVerification{
					OK:            false,
					RowsVerified:  count,
					FirstBreakSeq: seq,
					Detail:        fmt.Sprintf("row_hash mismatch at chain-start seq %d (row content edited)", seq),
					CoversFromSeq: coversFrom,
				}, nil
			}
			// Anchor the first surviving row to the latest checkpoint: the
			// checkpoint records the PRUNED head, so an intact prefix-prune
			// leaves this row linking straight to it.
			if cpSeq.Valid && cpRowHash.Valid && ph == cpRowHash.String && seq == cpSeq.Int64+1 {
				anchored = true
				anchorCPSeq = cpSeq.Int64
			}
			first = false
		} else {
			if seq != prevSeq+1 {
				return &types.AuditChainVerification{
					OK:            false,
					RowsVerified:  count,
					FirstBreakSeq: seq,
					Detail:        fmt.Sprintf("non-contiguous seq: expected %d, got %d (middle deletion)", prevSeq+1, seq),
					CoversFromSeq: coversFrom,
				}, nil
			}
			if ph != prevRowHash {
				return &types.AuditChainVerification{
					OK:            false,
					RowsVerified:  count,
					FirstBreakSeq: seq,
					Detail:        fmt.Sprintf("prev_hash link broken at seq %d", seq),
					CoversFromSeq: coversFrom,
				}, nil
			}
			expected := auditRowHash(id, actor, eventType, targetType, targetID.String, action, payload.String, tenant, seq, ph)
			if expected != rowHash.String {
				return &types.AuditChainVerification{
					OK:            false,
					RowsVerified:  count,
					FirstBreakSeq: seq,
					Detail:        fmt.Sprintf("row_hash mismatch at seq %d (row content edited or reordered)", seq),
					CoversFromSeq: coversFrom,
				}, nil
			}
		}
		count++
		prevSeq = seq
		prevRowHash = rowHash.String
		headSeq = seq
		headRowHash = rowHash.String
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit chain iteration: %w", err)
	}

	return &types.AuditChainVerification{
		OK:                   true,
		RowsVerified:         count,
		CoversFromSeq:        coversFrom,
		HeadSeq:              headSeq,
		HeadRowHash:          headRowHash,
		AnchoredByCheckpoint: anchored,
		CheckpointSeq:        anchorCPSeq,
	}, nil
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
