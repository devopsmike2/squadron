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

	chain "github.com/devopsmike2/squadron/internal/audit/chain"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// audit_chain.go — the Postgres port of the ADR 0027 per-tenant tamper-evident
// audit hash-chain + retention checkpoints (ADR 0033 slice 4). HIGH-STAKES.
//
// The chain algorithm is NOT reimplemented here. RowHash + Verify live in the
// dependency-free internal/audit/chain package and are the single source of
// truth shared by the sqlite store, the memory store, and the offline verifier.
// This port's only job is to PERSIST and RETURN exactly what that package
// consumes so append + self-verify + checkpoint anchoring behave
// BYTE-IDENTICALLY to the sqlite backend:
//
//   - the immutable content columns (id, actor, event_type, target_type,
//     target_id, action, payload) + tenant_id + per-tenant seq feed RowHash;
//   - payload is persisted as the RAW hashed string (TEXT, not JSONB) so the
//     read-back matches what was hashed byte-for-byte;
//   - seq is the per-tenant monotonic ordering key the walk relies on; prev_hash
//     links to the prior row's row_hash; row_hash is the stored chain hash.
//
// The append read-head → insert critical section MUST be serialized per tenant:
// two concurrent appends that both read the same head would compute the same seq
// and fork the chain. The sqlite backend serializes with an in-process mutex
// (single writer, single file). Postgres may run multiple app instances, so
// this port takes a per-tenant transaction-scoped advisory lock instead — the
// DB-appropriate equivalent that serializes appends for a tenant across every
// instance, not just in-process. The hashing is unchanged; only the mutual
// exclusion mechanism differs.

// CreateAuditEvent appends one event to the caller's per-tenant hash-chain
// (ADR 0027 slice 1). Mirrors the sqlite backend exactly: tenant resolves via
// tenantScope (DefaultTenant when unstamped/system), the payload string is
// computed once and reused for BOTH the stored column and the hash so the two
// can never drift, seq = prevSeq+1, and row_hash chains from the prior row's
// row_hash. The critical section is serialized by a per-tenant advisory lock.
func (s *Storage) CreateAuditEvent(ctx context.Context, e *types.AuditEvent) error {
	var payloadJSON []byte
	if e.Payload != nil {
		var err error
		payloadJSON, err = json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("failed to marshal audit payload: %w", err)
		}
	}
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return err
	}
	if !apply {
		// System-context insert (background jobs) — mirror sqlite: land on
		// DefaultTenant so the chain is still per-tenant coherent.
		tenant = identity.DefaultTenant
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = e.CreatedAt
	}

	// payloadStr is computed once and reused for BOTH the stored payload column
	// and the hash so the two can never drift (ADR 0027 requirement).
	payloadStr := string(payloadJSON)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin audit tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Per-tenant transaction-scoped advisory lock: serializes the read-head →
	// insert critical section across ALL app instances so concurrent appends
	// can't fork the chain. Released automatically on commit/rollback.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "audit_chain:"+tenant); err != nil {
		return fmt.Errorf("acquire audit chain lock: %w", err)
	}

	var prevSeq int64
	var prevHash string
	err = tx.QueryRowContext(ctx,
		`SELECT seq, row_hash FROM audit_events WHERE tenant_id = $1 AND seq IS NOT NULL ORDER BY seq DESC LIMIT 1`,
		tenant,
	).Scan(&prevSeq, &prevHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			prevSeq = 0
			prevHash = "" // genesis: first row in this tenant's chain
		} else {
			return fmt.Errorf("failed to read audit chain head: %w", err)
		}
	}
	seq := prevSeq + 1
	rowHash := chain.RowHash(e.ID, e.Actor, e.EventType, e.TargetType, e.TargetID, e.Action, payloadStr, tenant, seq, prevHash)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events (id, timestamp, actor, event_type, target_type, target_id, action, payload, tenant_id, created_at, seq, prev_hash, row_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		e.ID,
		e.Timestamp,
		e.Actor,
		e.EventType,
		e.TargetType,
		nullString(e.TargetID),
		e.Action,
		payloadStr,
		tenant,
		e.CreatedAt,
		seq,
		prevHash,
		rowHash,
	); err != nil {
		return fmt.Errorf("failed to create audit event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit audit event: %w", err)
	}
	return nil
}

// ListAuditEvents returns audit rows newest-first (by timestamp), applying the
// filter and the default/cap limit. Mirrors the sqlite backend: the tenant
// predicate is added only when tenantScope says apply.
func (s *Storage) ListAuditEvents(ctx context.Context, filter types.AuditEventFilter) ([]*types.AuditEvent, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}

	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	q := `SELECT id, timestamp, actor, event_type, target_type, target_id, action, payload, created_at, ai_explanation, ai_explanation_model, ai_explanation_generated_at FROM audit_events WHERE 1=1`
	var args []any
	n := 0
	if apply {
		n++
		q += fmt.Sprintf(" AND tenant_id = $%d", n)
		args = append(args, tenant)
	}
	if filter.EventType != "" {
		n++
		q += fmt.Sprintf(" AND event_type = $%d", n)
		args = append(args, filter.EventType)
	}
	if filter.TargetType != "" {
		n++
		q += fmt.Sprintf(" AND target_type = $%d", n)
		args = append(args, filter.TargetType)
	}
	if filter.TargetID != "" {
		n++
		q += fmt.Sprintf(" AND target_id = $%d", n)
		args = append(args, filter.TargetID)
	}
	if filter.Actor != "" {
		n++
		q += fmt.Sprintf(" AND actor = $%d", n)
		args = append(args, filter.Actor)
	}
	if !filter.Since.IsZero() {
		n++
		q += fmt.Sprintf(" AND timestamp >= $%d", n)
		args = append(args, filter.Since)
	}
	if !filter.Until.IsZero() {
		n++
		q += fmt.Sprintf(" AND timestamp < $%d", n)
		args = append(args, filter.Until)
	}
	n++
	q += fmt.Sprintf(" ORDER BY timestamp DESC LIMIT $%d", n)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list audit events: %w", err)
	}
	defer rows.Close()

	var out []*types.AuditEvent
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetAuditEvent fetches one audit row by ID. Returns (nil, nil) when no row
// matches so the caller can render a 404 distinct from a 500. Mirrors sqlite.
func (s *Storage) GetAuditEvent(ctx context.Context, id string) (*types.AuditEvent, error) {
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	q := `SELECT id, timestamp, actor, event_type, target_type, target_id, action, payload, created_at, ai_explanation, ai_explanation_model, ai_explanation_generated_at FROM audit_events WHERE id = $1`
	args := []any{id}
	if apply {
		q += ` AND tenant_id = $2`
		args = append(args, tenant)
	}
	e, err := scanAuditEvent(s.db.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get audit event: %w", err)
	}
	return e, nil
}

// scanAuditEvent decodes one audit row in the ListAuditEvents / GetAuditEvent
// projection order. Payload is decoded from the raw string back into the map
// (the raw column is the source of truth for the chain hash; this is only the
// human-facing view).
func scanAuditEvent(sc scanner) (*types.AuditEvent, error) {
	e := &types.AuditEvent{}
	var targetID, payload, aiExplanation, aiModel sql.NullString
	var aiGeneratedAt sql.NullTime
	if err := sc.Scan(
		&e.ID, &e.Timestamp, &e.Actor, &e.EventType, &e.TargetType,
		&targetID, &e.Action, &payload, &e.CreatedAt,
		&aiExplanation, &aiModel, &aiGeneratedAt,
	); err != nil {
		return nil, err
	}
	if targetID.Valid {
		e.TargetID = targetID.String
	}
	if payload.Valid && payload.String != "" {
		_ = json.Unmarshal([]byte(payload.String), &e.Payload)
	}
	if aiExplanation.Valid {
		e.AIExplanation = aiExplanation.String
	}
	if aiModel.Valid {
		e.AIExplanationModel = aiModel.String
	}
	if aiGeneratedAt.Valid {
		t := aiGeneratedAt.Time
		e.AIExplanationGeneratedAt = &t
	}
	return e, nil
}

// UpdateAuditEventExplanation writes a cached AI explanation onto the row. The
// row stays otherwise immutable; this is the one mutation the audit log allows.
// Returns an error if no row matches the supplied id. Mirrors sqlite.
func (s *Storage) UpdateAuditEventExplanation(ctx context.Context, id, explanation, model string, generatedAt time.Time) error {
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return err
	}
	stmt := `UPDATE audit_events SET ai_explanation = $1, ai_explanation_model = $2, ai_explanation_generated_at = $3 WHERE id = $4`
	args := []any{explanation, model, generatedAt, id}
	if apply {
		stmt += ` AND tenant_id = $5`
		args = append(args, tenant)
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("failed to update audit explanation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("audit event %q not found", id)
	}
	return nil
}

// auditChainRows reads the caller's tenant chain rows (seq ASC) with the RAW
// payload column string, byte-identical to what chain.RowHash consumed on the
// append path. Shared by VerifyAuditChain and ListAuditChainRows so the two can
// never diverge on what a "row" is — exactly the sqlite backend's invariant.
func (s *Storage) auditChainRows(ctx context.Context, tenant string) ([]chain.Row, error) {
	dbRows, err := s.db.QueryContext(ctx,
		`SELECT id, actor, event_type, target_type, target_id, action, payload, seq, prev_hash, row_hash
		 FROM audit_events WHERE tenant_id = $1 AND seq IS NOT NULL ORDER BY seq ASC`,
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
	return rows, nil
}

// VerifyAuditChain walks the caller's tenant hash-chain (ADR 0027) and reports
// whether it is intact. Self-tenant ONLY: the tenant resolves the same way the
// append path resolves it (tenantScope; DefaultTenant when unstamped/system).
//
// The pure walk (chain-start leniency + contiguity + prev_hash link + row_hash
// recompute) lives in internal/audit/chain — the SAME package the sqlite store
// calls — so the two backends can never diverge on the verdict. This method
// layers the DB-coupled checkpoint anchoring on top of that pure result,
// mirroring the sqlite backend exactly.
func (s *Storage) VerifyAuditChain(ctx context.Context) (*types.AuditChainVerification, error) {
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	if !apply {
		tenant = identity.DefaultTenant
	}

	rows, err := s.auditChainRows(ctx, tenant)
	if err != nil {
		return nil, err
	}

	// ADR 0027 slice 2 — load this tenant's latest checkpoint so the first
	// surviving row can be POSITIVELY anchored to a known-good boundary. A
	// missing checkpoint is fine (anchoring only ever adds a positive signal, it
	// never changes pass/fail).
	var (
		cpSeq     sql.NullInt64
		cpRowHash sql.NullString
	)
	_ = s.db.QueryRowContext(ctx,
		`SELECT checkpoint_seq, checkpoint_row_hash FROM audit_chain_checkpoints WHERE tenant_id = $1 ORDER BY checkpoint_seq DESC LIMIT 1`,
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
	// straight to it. Only reported on an intact chain, matching sqlite.
	if res.OK && len(rows) > 0 {
		start := rows[0]
		if cpSeq.Valid && cpRowHash.Valid && start.PrevHash == cpRowHash.String && start.Seq == cpSeq.Int64+1 {
			out.AnchoredByCheckpoint = true
			out.CheckpointSeq = cpSeq.Int64
		}
	}

	return out, nil
}

// ListAuditChainRows returns the caller's tenant chain rows (seq ASC) with the
// RAW payload column string (byte-identical to what was hashed), for evidence
// export + offline verification (ADR 0027). Tenant resolves exactly the way
// VerifyAuditChain resolves it, and it runs the SAME SELECT, so the two can
// never diverge on what a "row" is. Mirrors the sqlite backend.
func (s *Storage) ListAuditChainRows(ctx context.Context) ([]chain.Row, error) {
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return nil, err
	}
	if !apply {
		tenant = identity.DefaultTenant
	}
	return s.auditChainRows(ctx, tenant)
}

// WriteAuditCheckpoint upserts a retention/chain reconciliation checkpoint
// (ADR 0027 slice 2), keyed by (tenant_id, checkpoint_seq). Re-recording the
// same pruned head overwrites the prior row's mutable columns rather than
// erroring. Mirrors the sqlite backend's ON CONFLICT upsert.
func (s *Storage) WriteAuditCheckpoint(ctx context.Context, cp types.AuditCheckpoint) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_chain_checkpoints
		    (tenant_id, checkpoint_seq, checkpoint_row_hash, rows_pruned, kind, created_at, sealed_sig)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (tenant_id, checkpoint_seq) DO UPDATE SET
		    checkpoint_row_hash = excluded.checkpoint_row_hash,
		    rows_pruned         = excluded.rows_pruned,
		    kind                = excluded.kind,
		    created_at          = excluded.created_at,
		    sealed_sig          = excluded.sealed_sig`,
		cp.Tenant, cp.CheckpointSeq, cp.CheckpointRowHash, cp.RowsPruned, cp.Kind, cp.CreatedAt.UTC(), nullString(cp.SealedSig),
	)
	if err != nil {
		return fmt.Errorf("write audit checkpoint: %w", err)
	}
	return nil
}

// ListAuditCheckpoints returns a tenant's retention checkpoints, newest seq
// first (ADR 0027 slice 2). sealed_sig is nullable (unused in OSS). Mirrors
// the sqlite backend.
func (s *Storage) ListAuditCheckpoints(ctx context.Context, tenant string) ([]types.AuditCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, checkpoint_seq, checkpoint_row_hash, rows_pruned, kind, created_at, sealed_sig
		 FROM audit_chain_checkpoints WHERE tenant_id = $1 ORDER BY checkpoint_seq DESC`,
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

// defaultAuditLimit / maxAuditLimit mirror the sqlite backend's list bounds.
const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)
