// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

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

// memAuditRowHash duplicates sqlite.auditRowHash for the memory store. It is a
// deliberate duplicate (not a shared export): cross-store hash equality is NOT
// required — each store verifies its OWN chain — and duplicating avoids the
// memory package importing the sqlite package. Kept in lockstep with
// sqlite.auditRowHash by hand.
func memAuditRowHash(id, actor, eventType, targetType, targetID, action, payloadStr, tenant string, seq int64, prevHash string) string {
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

// VerifyAuditChain walks the caller's tenant hash-chain in the memory store
// (ADR 0027 slice 1). The memory store never edits or deletes audit rows, so a
// well-formed chain always verifies OK; the full walk is implemented anyway so
// the memory store satisfies the same contract the sqlite store does.
func (s *Store) VerifyAuditChain(ctx context.Context) (*types.AuditChainVerification, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tenant := identity.TenantFromContext(ctx)
	chain := s.auditChains[tenant]

	var (
		count       int
		coversFrom  int64
		prevSeq     int64
		prevRowHash string
		headSeq     int64
		headRowHash string
	)
	for i, row := range chain {
		if i == 0 {
			coversFrom = row.seq
			expected := memAuditRowHash(row.id, row.actor, row.eventType, row.targetType, row.targetID, row.action, row.payloadStr, tenant, row.seq, row.prevHash)
			if expected != row.rowHash {
				return &types.AuditChainVerification{OK: false, RowsVerified: count, FirstBreakSeq: row.seq, Detail: fmt.Sprintf("row_hash mismatch at chain-start seq %d", row.seq), CoversFromSeq: coversFrom}, nil
			}
		} else {
			if row.seq != prevSeq+1 {
				return &types.AuditChainVerification{OK: false, RowsVerified: count, FirstBreakSeq: row.seq, Detail: fmt.Sprintf("non-contiguous seq: expected %d, got %d", prevSeq+1, row.seq), CoversFromSeq: coversFrom}, nil
			}
			if row.prevHash != prevRowHash {
				return &types.AuditChainVerification{OK: false, RowsVerified: count, FirstBreakSeq: row.seq, Detail: fmt.Sprintf("prev_hash link broken at seq %d", row.seq), CoversFromSeq: coversFrom}, nil
			}
			expected := memAuditRowHash(row.id, row.actor, row.eventType, row.targetType, row.targetID, row.action, row.payloadStr, tenant, row.seq, row.prevHash)
			if expected != row.rowHash {
				return &types.AuditChainVerification{OK: false, RowsVerified: count, FirstBreakSeq: row.seq, Detail: fmt.Sprintf("row_hash mismatch at seq %d", row.seq), CoversFromSeq: coversFrom}, nil
			}
		}
		count++
		prevSeq = row.seq
		prevRowHash = row.rowHash
		headSeq = row.seq
		headRowHash = row.rowHash
	}

	return &types.AuditChainVerification{OK: true, RowsVerified: count, CoversFromSeq: coversFrom, HeadSeq: headSeq, HeadRowHash: headRowHash}, nil
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
