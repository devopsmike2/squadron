// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"fmt"
	"time"
)

// retention.go — GC predicates for operator-activity tables that
// otherwise grow forever. cost_spike_events gains a row per detected
// anomaly and recommendation_outcomes a row per Apply click; neither
// had a prune path, so a long-lived deployment accumulates them without
// bound (and GET /savings/realized scans the full outcomes table). A
// background sweep in cmd/all-in-one calls these on a 24h ticker.

// DeleteClosedCostSpikeEventsBefore removes RESOLVED cost-spike rows
// (ended_at IS NOT NULL) whose ended_at predates the cutoff. Open spikes
// (ended_at IS NULL) are NEVER deleted regardless of age — an unresolved
// anomaly must stay visible on the alerts panel until the detector closes
// it. Returns the number of rows removed.
func (s *Storage) DeleteClosedCostSpikeEventsBefore(ctx context.Context, before time.Time) (int64, error) {
	// ADR 0011 slice 3b — GC runs under WithSystemContext (apply=false → no
	// predicate → fleet-wide prune). A non-system caller scopes to its own
	// tenant.
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return 0, err
	}
	stmt := `DELETE FROM cost_spike_events WHERE ended_at IS NOT NULL AND ended_at < ?`
	args := []any{before.UTC()}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("delete closed cost_spike_events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteRecommendationOutcomesBefore removes recommendation-outcome rows
// applied before the cutoff. These are the persisted "operator clicked
// Apply" records the Savings/realized surface reads; pruning old ones
// bounds both table size and that endpoint's full-table scan. Returns the
// number of rows removed.
func (s *Storage) DeleteRecommendationOutcomesBefore(ctx context.Context, before time.Time) (int64, error) {
	// ADR 0011 slice 3b — GC runs under WithSystemContext (apply=false → no
	// predicate → fleet-wide prune). A non-system caller scopes to its own
	// tenant.
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return 0, err
	}
	stmt := `DELETE FROM recommendation_outcomes WHERE applied_at < ?`
	args := []any{before.UTC()}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("delete recommendation_outcomes: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteDismissedIncidentDraftsBefore removes DISMISSED incident-draft
// rows (status = 'dismissed') whose updated_at (set at dismiss time)
// predates the cutoff. incident_drafts gains a row per AI-drafted
// incident and HandleDismissDraft only flips status to 'dismissed' — the
// row otherwise stays forever, so a long-lived deployment accumulates
// them without bound. Only dismissed drafts are pruned: 'draft' and
// 'published' rows are load-bearing (published ones are keyed by
// action_request_id for dedup / external-link lookup) and must survive
// regardless of age, mirroring the "closed cost-spikes only" invariant.
// Returns the number of rows removed.
func (s *Storage) DeleteDismissedIncidentDraftsBefore(ctx context.Context, before time.Time) (int64, error) {
	// ADR 0011 slice 3b — GC runs under WithSystemContext (apply=false → no
	// predicate → fleet-wide prune). A non-system caller scopes to its own
	// tenant.
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return 0, err
	}
	stmt := `DELETE FROM incident_drafts WHERE status = 'dismissed' AND updated_at < ?`
	args := []any{before.UTC()}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("delete dismissed incident_drafts: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteAuditEventsBefore removes audit_events rows whose timestamp (the
// event's logical time, indexed) predates the cutoff. Returns the number of
// rows removed.
//
// UNLIKE every other retention predicate, the sweep that calls this is
// OFF by default (see cmd/all-in-one audit-retention wiring): audit_events
// is the append-only compliance/evidence log, so silently pruning it is a
// product/compliance decision, not an engineering default. The predicate
// exists so operators whose regime permits (or requires) a bounded window
// can opt in to a configurable retention; with the switch unset the log
// grows unbounded and nothing here runs. This method itself performs the
// delete unconditionally — the enable/window gating lives entirely at the
// call site.
func (s *Storage) DeleteAuditEventsBefore(ctx context.Context, before time.Time) (int64, error) {
	// ADR 0011 slice 3b — GC runs under WithSystemContext (apply=false → no
	// predicate → fleet-wide prune). A non-system caller scopes to its own
	// tenant.
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return 0, err
	}
	stmt := `DELETE FROM audit_events WHERE timestamp < ?`
	args := []any{before.UTC()}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("delete audit_events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteIACRecommendationVerdictsBefore removes NON-EXCLUDED verdict rows
// (exclude_from_learning = 0) whose updated_at predates the cutoff. The
// discovery proposer's learning loop writes one iac_recommendation_verdicts
// row per recommendation an operator acts on, and the row survives even after
// the exclusion bit is cleared (kept so a re-toggle can report the prior state,
// and to carry the optional check-run back-signal columns) — so on a
// continuously-scanning deployment the table grows without bound and no other
// path prunes it. Returns the number of rows removed.
//
// ACTIVE exclusions (exclude_from_learning = 1) are NEVER deleted regardless of
// age: the bridge pulls exactly those rows to suppress "don't propose this
// again" recommendations from the learning context, so pruning one would
// silently resurrect a recommendation the operator declined. This mirrors the
// "closed cost-spikes only" / "dismissed drafts only" invariant of the other
// predicates — only rows that are no longer load-bearing are eligible.
//
// OPEN check-run rows are likewise protected: a row carrying a check_run_id
// whose check_run_conclusion is still NULL is load-bearing — the PR-merge/close
// webhook reads it to post the check run's final conclusion to GitHub. Deleting
// it (e.g. a PR that stays open, idle past the cutoff) would make the webhook
// fail-open and the check run would never resolve. Once the conclusion is set,
// or if the row never had a check run (check_run_id IS NULL), the back-signal is
// done and the row is prunable. updated_at is bumped on every check-run write,
// so the age filter only ever sees genuinely idle rows.
func (s *Storage) DeleteIACRecommendationVerdictsBefore(ctx context.Context, before time.Time) (int64, error) {
	// ADR 0011 slice 3b — GC runs under WithSystemContext (apply=false → no
	// predicate → fleet-wide prune). A non-system caller scopes to its own
	// tenant.
	tenant, apply, err := tenantScope(ctx)
	if err != nil {
		return 0, err
	}
	stmt := `DELETE FROM iac_recommendation_verdicts
		 WHERE exclude_from_learning = 0
		   AND updated_at < ?
		   AND (check_run_id IS NULL OR check_run_conclusion IS NOT NULL)`
	args := []any{before.UTC()}
	if apply {
		stmt += ` AND tenant_id = ?`
		args = append(args, tenant)
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, fmt.Errorf("delete iac_recommendation_verdicts: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
