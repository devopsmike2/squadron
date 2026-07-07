// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
)

// retentionGCContract mirrors the two interfaces cmd/all-in-one asserts the
// application store against to schedule retention GC (operatorTableRetentionGC
// + discoveryTableRetentionGC). Those assertions are done at RUNTIME via a type
// switch, so a signature drift on any one prune method would silently fail the
// assertion and disable the whole sweep — a table would leak in production with
// no compile error and no test failure. Pinning *Storage against the combined
// contract here turns that latent failure into a build error the moment a
// signature changes. Keep this list in lockstep with the two main.go interfaces.
type retentionGCContract interface {
	// operator-activity tables
	DeleteClosedCostSpikeEventsBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteRecommendationOutcomesBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteDismissedIncidentDraftsBefore(ctx context.Context, before time.Time) (int64, error)
	// audit log — pruned only when the operator opts in (default off)
	DeleteAuditEventsBefore(ctx context.Context, before time.Time) (int64, error)
	// discovery/serverless scan tables
	DeleteServerlessBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteEventSourceInstancesBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteOrchestrationInstancesBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteColdStartObservationsBefore(ctx context.Context, before time.Time) error
	DeleteErrorRateObservationsBefore(ctx context.Context, before time.Time) error
	DeleteDiscoveryScansBefore(ctx context.Context, before time.Time) (int64, error)
	DeleteIACRecommendationVerdictsBefore(ctx context.Context, before time.Time) (int64, error)
}

// Compile-time guard: the sqlite store must satisfy every retention predicate
// main.go schedules. If this stops compiling, a Delete*Before signature drifted
// and the corresponding GC sweep would silently no-op in production.
var _ retentionGCContract = (*Storage)(nil)

// TestRetention_DeleteDiscoveryScansBefore: persisted scan history older
// than the cutoff is pruned; recent scans survive. discovery_scans stores
// the full inventory blob per row, so without this GC it is the largest
// unbounded discovery table on a continuously-scanning deployment.
func TestRetention_DeleteDiscoveryScansBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-100 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-1 * 24 * time.Hour)

		require.NoError(t, store.SaveDiscoveryScan(ctx, &types.ScanRecord{
			ScanID: "scan-old", Provider: "aws", ScopeID: "acc-1",
			StartedAt: old, CompletedAt: old, CreatedAt: old,
			Summary: map[string]int{"ec2": 3}, ResultJSON: `{"instances":[]}`,
		}))
		require.NoError(t, store.SaveDiscoveryScan(ctx, &types.ScanRecord{
			ScanID: "scan-recent", Provider: "aws", ScopeID: "acc-1",
			StartedAt: recent, CompletedAt: recent, CreatedAt: recent,
			Summary: map[string]int{"ec2": 3}, ResultJSON: `{"instances":[]}`,
		}))

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		n, err := store.DeleteDiscoveryScansBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the 100-day-old scan should be pruned")

		// Old scan gone; recent scan (and its inventory) intact.
		gone, err := store.GetDiscoveryScan(ctx, "scan-old")
		require.NoError(t, err)
		require.Nil(t, gone, "scan-old must be pruned")
		kept, err := store.GetDiscoveryScan(ctx, "scan-recent")
		require.NoError(t, err)
		require.NotNil(t, kept, "scan-recent must survive")
	})
}

// TestRetention_DeleteAuditEventsBefore: the predicate prunes audit rows
// older than the cutoff by their logical timestamp and keeps recent ones.
// This backs the OPT-IN audit retention sweep (default disabled) — the
// predicate always deletes by cutoff; the enable/window gating lives at the
// cmd/all-in-one call site, not here.
func TestRetention_DeleteAuditEventsBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-400 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-10 * 24 * time.Hour)

		require.NoError(t, store.CreateAuditEvent(ctx, &types.AuditEvent{
			ID: "ae-old", Timestamp: old, Actor: "system",
			EventType: "rollout.succeeded", TargetType: "rollout", Action: "succeeded"}))
		require.NoError(t, store.CreateAuditEvent(ctx, &types.AuditEvent{
			ID: "ae-recent", Timestamp: recent, Actor: "system",
			EventType: "rollout.succeeded", TargetType: "rollout", Action: "succeeded"}))

		cutoff := time.Now().UTC().Add(-365 * 24 * time.Hour)
		n, err := store.DeleteAuditEventsBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the 400-day-old audit row should be pruned")

		gone, err := store.GetAuditEvent(ctx, "ae-old")
		require.NoError(t, err)
		require.Nil(t, gone, "ae-old must be pruned")
		kept, err := store.GetAuditEvent(ctx, "ae-recent")
		require.NoError(t, err)
		require.NotNil(t, kept, "ae-recent must survive")
	})
}

// TestRetention_DeleteClosedCostSpikeEventsBefore: closed spikes older
// than the cutoff are pruned; recent closed spikes AND open spikes (any
// age) survive — an unresolved anomaly must never be GC'd.
func TestRetention_DeleteClosedCostSpikeEventsBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-100 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-1 * 24 * time.Hour)

		oldEnded := old
		require.NoError(t, store.CreateCostSpikeEvent(ctx, &types.CostSpikeEvent{
			ID: "closed-old", StartedAt: old.Add(-time.Hour), EndedAt: &oldEnded, Severity: "warn"}))
		recentEnded := recent
		require.NoError(t, store.CreateCostSpikeEvent(ctx, &types.CostSpikeEvent{
			ID: "closed-recent", StartedAt: recent.Add(-time.Hour), EndedAt: &recentEnded, Severity: "warn"}))
		// Open (ended_at NULL) + very old — must NOT be pruned.
		require.NoError(t, store.CreateCostSpikeEvent(ctx, &types.CostSpikeEvent{
			ID: "open-old", StartedAt: old, Severity: "critical"}))

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		n, err := store.DeleteClosedCostSpikeEventsBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the closed+old spike should be deleted")

		gone, err := store.GetCostSpikeEvent(ctx, "closed-old")
		require.NoError(t, err)
		require.Nil(t, gone, "closed-old should be pruned")
		for _, id := range []string{"closed-recent", "open-old"} {
			kept, err := store.GetCostSpikeEvent(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, kept, "%s should survive the sweep", id)
		}
	})
}

// TestRetention_DeleteRecommendationOutcomesBefore: outcomes applied
// before the cutoff are pruned; newer ones survive.
func TestRetention_DeleteRecommendationOutcomesBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-100 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-1 * 24 * time.Hour)

		require.NoError(t, store.CreateRecommendationOutcome(ctx, &types.RecommendationOutcome{
			ID: "o-old", RecommendationID: "r1", AppliedAt: old, Title: "t", Category: "c"}))
		require.NoError(t, store.CreateRecommendationOutcome(ctx, &types.RecommendationOutcome{
			ID: "o-recent", RecommendationID: "r2", AppliedAt: recent, Title: "t", Category: "c"}))

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		n, err := store.DeleteRecommendationOutcomesBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the old outcome should be deleted")

		remaining, err := store.ListRecommendationOutcomes(ctx)
		require.NoError(t, err)
		require.Len(t, remaining, 1, "only the recent outcome should survive")
		require.Equal(t, "o-recent", remaining[0].ID)
	})
}

// TestRetention_DeleteDismissedIncidentDraftsBefore: dismissed drafts
// older than the cutoff are pruned; recent dismissed drafts AND non-
// dismissed drafts (draft / published, any age) survive — a
// still-actionable or published-for-dedup draft must never be GC'd.
func TestRetention_DeleteDismissedIncidentDraftsBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-100 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-1 * 24 * time.Hour)

		// Dismissed + old → pruned.
		require.NoError(t, store.CreateIncidentDraft(ctx, &types.IncidentDraft{
			ID: "dismissed-old", Status: "dismissed", Title: "t", BodyMarkdown: "b",
			CreatedAt: old.Add(-time.Hour), UpdatedAt: old}))
		// Dismissed + recent → survives.
		require.NoError(t, store.CreateIncidentDraft(ctx, &types.IncidentDraft{
			ID: "dismissed-recent", Status: "dismissed", Title: "t", BodyMarkdown: "b",
			CreatedAt: recent.Add(-time.Hour), UpdatedAt: recent}))
		// Draft + old → survives (not dismissed).
		require.NoError(t, store.CreateIncidentDraft(ctx, &types.IncidentDraft{
			ID: "draft-old", Status: "draft", Title: "t", BodyMarkdown: "b",
			CreatedAt: old.Add(-time.Hour), UpdatedAt: old}))
		// Published + old → survives (load-bearing dedup/link record).
		require.NoError(t, store.CreateIncidentDraft(ctx, &types.IncidentDraft{
			ID: "published-old", Status: "published", Title: "t", BodyMarkdown: "b",
			ActionRequestID: "ar-1", CreatedAt: old.Add(-time.Hour), UpdatedAt: old}))

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		n, err := store.DeleteDismissedIncidentDraftsBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "only the dismissed+old draft should be deleted")

		gone, err := store.GetIncidentDraft(ctx, "dismissed-old")
		require.NoError(t, err)
		require.Nil(t, gone, "dismissed-old should be pruned")
		for _, id := range []string{"dismissed-recent", "draft-old", "published-old"} {
			kept, err := store.GetIncidentDraft(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, kept, "%s should survive the sweep", id)
		}
	})
}

// TestRetention_DeleteIACRecommendationVerdictsBefore: cleared verdict rows
// (exclude_from_learning=0) older than the cutoff are pruned, while ACTIVE
// exclusions (exclude_from_learning=1) survive regardless of age and recent
// cleared rows survive — mirroring the "closed/dismissed only" invariant.
func TestRetention_DeleteIACRecommendationVerdictsBefore(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		old := time.Now().UTC().Add(-100 * 24 * time.Hour)
		recent := time.Now().UTC().Add(-1 * 24 * time.Hour)

		insert := func(id string, excluded int, updated time.Time) {
			_, err := store.db.ExecContext(ctx,
				`INSERT INTO iac_recommendation_verdicts
				 (recommendation_id, connection_id, account_id, region, recommendation_kind,
				  exclude_from_learning, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				id, "conn-1", "acc-1", "us-east-1", "ec2-adot",
				excluded, updated, updated)
			require.NoError(t, err)
		}
		insertCheckRun := func(id string, excluded int, updated time.Time, runID *int64, conclusion *string) {
			_, err := store.db.ExecContext(ctx,
				`INSERT INTO iac_recommendation_verdicts
				 (recommendation_id, connection_id, account_id, region, recommendation_kind,
				  exclude_from_learning, created_at, updated_at, check_run_id, check_run_conclusion)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, "conn-1", "acc-1", "us-east-1", "ec2-adot",
				excluded, updated, updated, runID, conclusion)
			require.NoError(t, err)
		}
		runID := int64(4242)
		concluded := "success"

		insert("old-cleared", 0, old)       // prunable
		insert("old-active", 1, old)        // active exclusion — must survive
		insert("recent-cleared", 0, recent) // recent — must survive
		// OPEN check-run row aged past the cutoff: check_run_id set, conclusion
		// NULL. Load-bearing — the merge/close webhook still needs it to post the
		// final conclusion — so it must survive despite being old + cleared.
		insertCheckRun("old-open-checkrun", 0, old, &runID, nil)
		// RESOLVED check-run row aged past the cutoff: back-signal already
		// delivered (conclusion set), so it is prunable like any old cleared row.
		insertCheckRun("old-resolved-checkrun", 0, old, &runID, &concluded)

		cutoff := time.Now().UTC().Add(-90 * 24 * time.Hour)
		n, err := store.DeleteIACRecommendationVerdictsBefore(ctx, cutoff)
		require.NoError(t, err)
		require.Equal(t, int64(2), n,
			"old cleared verdict + resolved check-run row pruned; open check-run row protected")

		var got []string
		rows, err := store.db.QueryContext(ctx,
			`SELECT recommendation_id FROM iac_recommendation_verdicts ORDER BY recommendation_id`)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			got = append(got, id)
		}
		require.NoError(t, rows.Err())
		require.Equal(t, []string{"old-active", "old-open-checkrun", "recent-cleared"}, got,
			"active exclusion, OPEN check-run row, and recent cleared row must survive")
	})
}

// appendAuditEventAt inserts one chained audit event with an explicit logical
// timestamp (seq is still assigned by insert order, independent of ts) — the
// seq/timestamp independence that ADR 0027 slice 2 reconciles.
func appendAuditEventAt(t *testing.T, store *Storage, ctx context.Context, n int, ts time.Time) {
	t.Helper()
	require.NoError(t, store.CreateAuditEvent(ctx, &types.AuditEvent{
		ID:         uuid.NewString(),
		Timestamp:  ts,
		Actor:      fmt.Sprintf("operator:user%d@example.com", n),
		EventType:  "config.applied",
		TargetType: "config",
		TargetID:   fmt.Sprintf("cfg-%d", n),
		Action:     "applied",
		Payload:    map[string]any{"n": n},
	}))
}

func auditRowHashAtSeq(t *testing.T, store *Storage, ctx context.Context, tenant string, seq int64) string {
	t.Helper()
	var h string
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT row_hash FROM audit_events WHERE tenant_id = ? AND seq = ?`, tenant, seq).Scan(&h))
	return h
}

func remainingSeqs(t *testing.T, store *Storage, ctx context.Context, tenant string) []int64 {
	t.Helper()
	rows, err := store.db.QueryContext(ctx,
		`SELECT seq FROM audit_events WHERE tenant_id = ? AND seq IS NOT NULL ORDER BY seq ASC`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var q int64
		require.NoError(t, rows.Scan(&q))
		out = append(out, q)
	}
	return out
}

// TestRetention_PruneThenVerifyGreen_Prefix — append 5, prune the oldest 2 by
// timestamp; the chain stays OK, the survivor is anchored to the checkpoint,
// and CoversFromSeq advances to the first survivor. (ADR 0027 slice 2 (a).)
func TestRetention_PruneThenVerifyGreen_Prefix(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		now := time.Now().UTC()
		// seq 1,2 are old; seq 3,4,5 are recent.
		appendAuditEventAt(t, store, ctx, 1, now.Add(-10*24*time.Hour))
		appendAuditEventAt(t, store, ctx, 2, now.Add(-9*24*time.Hour))
		appendAuditEventAt(t, store, ctx, 3, now.Add(-1*24*time.Hour))
		appendAuditEventAt(t, store, ctx, 4, now.Add(-1*24*time.Hour))
		appendAuditEventAt(t, store, ctx, 5, now.Add(-1*24*time.Hour))

		n, err := store.DeleteAuditEventsBefore(ctx, now.Add(-5*24*time.Hour))
		require.NoError(t, err)
		require.Equal(t, int64(2), n, "the oldest contiguous seq-prefix (seq<=2) is pruned")

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.True(t, res.OK, "detail=%s", res.Detail)
		require.Equal(t, 3, res.RowsVerified)
		require.Equal(t, int64(3), res.CoversFromSeq)
		require.True(t, res.AnchoredByCheckpoint, "first survivor must anchor to the retention checkpoint")
		require.Equal(t, int64(2), res.CheckpointSeq)
	})
}

// TestRetention_MidChainHolePrevented — THE test that proves the fix. A MIDDLE
// seq carries a backdated (old) timestamp while its neighbors are recent. A
// naive `timestamp < cutoff` prune would delete only that middle seq → a
// mid-chain hole → a VerifyAuditChain false-positive. The seq-prefix prune
// instead removes the contiguous prefix (seq<=cutSeq), so survivors stay
// contiguous and the chain verifies OK. (ADR 0027 slice 2 (b).)
func TestRetention_MidChainHolePrevented(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		now := time.Now().UTC()
		// seq1 recent, seq2 OLD (backdated), seq3 recent — the inversion.
		appendAuditEventAt(t, store, ctx, 1, now.Add(-1*24*time.Hour))
		appendAuditEventAt(t, store, ctx, 2, now.Add(-100*24*time.Hour))
		appendAuditEventAt(t, store, ctx, 3, now.Add(-1*24*time.Hour))

		// cutoff between the old and recent timestamps → cutSeq = 2.
		n, err := store.DeleteAuditEventsBefore(ctx, now.Add(-50*24*time.Hour))
		require.NoError(t, err)
		require.Equal(t, int64(2), n, "the contiguous prefix seq<=2 is pruned, NOT just the backdated middle row")

		// Survivors must be a contiguous seq-suffix ({3}), never {1,3} (a hole).
		require.Equal(t, []int64{3}, remainingSeqs(t, store, ctx, identity.DefaultTenant),
			"prune must remove a contiguous seq-prefix, not punch a mid-chain hole")

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.True(t, res.OK, "no false tamper after a boundary-inversion prune: detail=%s", res.Detail)
		require.Equal(t, int64(3), res.CoversFromSeq)
	})
}

// TestRetention_CheckpointWritten — a prune records exactly one checkpoint for
// the tenant whose seq/row_hash equal the pruned head and whose kind is
// retention-cut. (ADR 0027 slice 2.)
func TestRetention_CheckpointWritten(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		now := time.Now().UTC()
		appendAuditEventAt(t, store, ctx, 1, now.Add(-10*24*time.Hour))
		appendAuditEventAt(t, store, ctx, 2, now.Add(-9*24*time.Hour))
		appendAuditEventAt(t, store, ctx, 3, now.Add(-1*24*time.Hour))

		wantHash := auditRowHashAtSeq(t, store, ctx, identity.DefaultTenant, 2)

		_, err := store.DeleteAuditEventsBefore(ctx, now.Add(-5*24*time.Hour))
		require.NoError(t, err)

		cps, err := store.ListAuditCheckpoints(ctx, identity.DefaultTenant)
		require.NoError(t, err)
		require.Len(t, cps, 1)
		require.Equal(t, int64(2), cps[0].CheckpointSeq)
		require.Equal(t, wantHash, cps[0].CheckpointRowHash)
		require.Equal(t, int64(2), cps[0].RowsPruned)
		require.Equal(t, "retention-cut", cps[0].Kind)
	})
}

// TestRetention_PerTenantPruneIsolation — a fleet-wide prune (WithSystemContext)
// prunes each tenant's own contiguous prefix and writes each tenant its own
// checkpoint; tenants never bleed into one another. (ADR 0027 slice 2.)
func TestRetention_PerTenantPruneIsolation(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		now := time.Now().UTC()
		ctxA := identity.WithTenant(context.Background(), "tenant-a")
		ctxB := identity.WithTenant(context.Background(), "tenant-b")
		// tenant-a: seq1,2 old; seq3 recent. tenant-b: seq1 old; seq2,3 recent.
		appendAuditEventAt(t, store, ctxA, 1, now.Add(-10*24*time.Hour))
		appendAuditEventAt(t, store, ctxA, 2, now.Add(-9*24*time.Hour))
		appendAuditEventAt(t, store, ctxA, 3, now.Add(-1*24*time.Hour))
		appendAuditEventAt(t, store, ctxB, 1, now.Add(-10*24*time.Hour))
		appendAuditEventAt(t, store, ctxB, 2, now.Add(-1*24*time.Hour))
		appendAuditEventAt(t, store, ctxB, 3, now.Add(-1*24*time.Hour))

		n, err := store.DeleteAuditEventsBefore(identity.WithSystemContext(context.Background()), now.Add(-5*24*time.Hour))
		require.NoError(t, err)
		require.Equal(t, int64(3), n, "2 from tenant-a + 1 from tenant-b")

		require.Equal(t, []int64{3}, remainingSeqs(t, store, ctxA, "tenant-a"))
		require.Equal(t, []int64{2, 3}, remainingSeqs(t, store, ctxB, "tenant-b"))

		resA, err := store.VerifyAuditChain(ctxA)
		require.NoError(t, err)
		require.True(t, resA.OK, "detail=%s", resA.Detail)
		require.True(t, resA.AnchoredByCheckpoint)
		require.Equal(t, int64(2), resA.CheckpointSeq)

		resB, err := store.VerifyAuditChain(ctxB)
		require.NoError(t, err)
		require.True(t, resB.OK, "detail=%s", resB.Detail)
		require.True(t, resB.AnchoredByCheckpoint)
		require.Equal(t, int64(1), resB.CheckpointSeq)

		cpsA, err := store.ListAuditCheckpoints(context.Background(), "tenant-a")
		require.NoError(t, err)
		require.Len(t, cpsA, 1)
		require.Equal(t, int64(2), cpsA[0].CheckpointSeq)
		cpsB, err := store.ListAuditCheckpoints(context.Background(), "tenant-b")
		require.NoError(t, err)
		require.Len(t, cpsB, 1)
		require.Equal(t, int64(1), cpsB[0].CheckpointSeq)
	})
}
