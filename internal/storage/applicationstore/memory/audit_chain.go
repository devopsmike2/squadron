// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"sort"

	chain "github.com/devopsmike2/squadron/internal/audit/chain"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// memAuditChainRow is the memory-store mirror of one audit hash-chain row
// (ADR 0027 slice 1). The memory store is test-only; it keeps just enough per
// tenant to re-verify the chain.
type memAuditChainRow struct {
	id, actor, eventType, targetType, targetID, action, payloadStr string
	seq                                                            int64
	prevHash                                                       string
	rowHash                                                        string
}

// VerifyAuditChain walks the caller's tenant hash-chain in the memory store
// (ADR 0027 slice 1). The memory store never edits or deletes audit rows, so a
// well-formed chain always verifies OK; the full walk is delegated to the pure
// internal/audit/chain package so the memory store satisfies the same contract
// the sqlite store does off the one shared source of truth. The memory store
// has no retention checkpoints table, so it never sets the anchoring fields.
func (s *Store) VerifyAuditChain(ctx context.Context) (*types.AuditChainVerification, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tenant := identity.TenantFromContext(ctx)
	memChain := s.auditChains[tenant]

	rows := make([]chain.Row, 0, len(memChain))
	for _, row := range memChain {
		rows = append(rows, chain.Row{
			ID:         row.id,
			Actor:      row.actor,
			EventType:  row.eventType,
			TargetType: row.targetType,
			TargetID:   row.targetID,
			Action:     row.action,
			Payload:    row.payloadStr,
			Tenant:     tenant,
			Seq:        row.seq,
			PrevHash:   row.prevHash,
			RowHash:    row.rowHash,
		})
	}

	res := chain.Verify(rows)
	return &types.AuditChainVerification{
		OK:            res.OK,
		RowsVerified:  res.RowsVerified,
		FirstBreakSeq: res.FirstBreakSeq,
		Detail:        res.Detail,
		CoversFromSeq: res.CoversFromSeq,
		HeadSeq:       res.HeadSeq,
		HeadRowHash:   res.HeadRowHash,
	}, nil
}

// WriteAuditCheckpoint upserts a retention/chain reconciliation checkpoint in
// the memory store (ADR 0027 slice 2), keyed by (tenant, checkpoint_seq).
func (s *Store) WriteAuditCheckpoint(_ context.Context, cp types.AuditCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	byTenant := s.auditCheckpoints[cp.Tenant]
	if byTenant == nil {
		byTenant = make(map[int64]types.AuditCheckpoint)
		s.auditCheckpoints[cp.Tenant] = byTenant
	}
	byTenant[cp.CheckpointSeq] = cp
	return nil
}

// ListAuditCheckpoints returns a tenant's checkpoints, newest seq first
// (ADR 0027 slice 2).
func (s *Store) ListAuditCheckpoints(_ context.Context, tenant string) ([]types.AuditCheckpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byTenant := s.auditCheckpoints[tenant]
	out := make([]types.AuditCheckpoint, 0, len(byTenant))
	for _, cp := range byTenant {
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CheckpointSeq > out[j].CheckpointSeq })
	return out, nil
}

// ListAuditChainRows returns the caller's tenant chain rows (seq ASC) with the
// raw payload string, for evidence export + offline verification (ADR 0027).
// Mirrors VerifyAuditChain's row build exactly; the tenant resolves the same
// way. The memory store is test-only but satisfies the same store contract the
// sqlite store does off the one shared source of truth.
func (s *Store) ListAuditChainRows(ctx context.Context) ([]chain.Row, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tenant := identity.TenantFromContext(ctx)
	memChain := s.auditChains[tenant]

	rows := make([]chain.Row, 0, len(memChain))
	for _, row := range memChain {
		rows = append(rows, chain.Row{
			ID:         row.id,
			Actor:      row.actor,
			EventType:  row.eventType,
			TargetType: row.targetType,
			TargetID:   row.targetID,
			Action:     row.action,
			Payload:    row.payloadStr,
			Tenant:     tenant,
			Seq:        row.seq,
			PrevHash:   row.prevHash,
			RowHash:    row.rowHash,
		})
	}
	return rows, nil
}
