// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
)

// --- production inventory query adapter --------------------------------
//
// Production wires the trace coverage handler's InventoryCountQuery
// against the same AuditQueryStore the unified summary handler already
// uses. The most-recent scan_completed event per (provider, scopeID)
// pair carries an InstanceCount the trace_coverage endpoint reads as
// inventory_count.
//
// Reusing the audit projection keeps the slice-1 chunk-3 surface
// disjoint from the per-provider scan endpoints (chunk 4 territory) —
// no new ApplicationStore method, no new scan-side wiring, just a slim
// adapter over the existing summary path's reads.
//
// A nil AuditQueryStore short-circuits to a zero count for every
// scope, which is the correct cold-start behavior (an operator who
// hasn't run a scan yet sees coverage_pct=0 across every provider).

type auditInventoryCountQueryAdapter struct {
	audit AuditQueryStore
}

// NewAuditInventoryCountQuery wraps an AuditQueryStore as the
// InventoryCountQuery surface the trace coverage handler consumes. nil
// audit yields nil so the trampoline can pass it straight through.
func NewAuditInventoryCountQuery(audit AuditQueryStore) InventoryCountQuery {
	if audit == nil {
		return nil
	}
	return &auditInventoryCountQueryAdapter{audit: audit}
}

// InventoryCountForScope reads the most-recent scan_completed event for
// the supplied (provider, scopeID) pair and returns its InstanceCount.
// Returns 0 with no error when the audit projection has no row for the
// scope — the trace coverage handler treats that as cold start.
func (a *auditInventoryCountQueryAdapter) InventoryCountForScope(
	ctx context.Context, provider, scopeID string,
) (int, error) {
	if a == nil || a.audit == nil {
		return 0, nil
	}
	scans, err := a.audit.ListRecentScanCompletedByProvider(ctx, provider)
	if err != nil {
		return 0, err
	}
	if s, ok := scans[scopeID]; ok {
		return s.InstanceCount, nil
	}
	return 0, nil
}
