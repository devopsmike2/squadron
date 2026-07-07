// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// auditChainContract pins the new store method signature (ADR 0027 slice 1),
// mirroring retentionGCContract: a compile-time guard so a signature drift on
// VerifyAuditChain becomes a build error rather than a silent contract break.
type auditChainContract interface {
	VerifyAuditChain(ctx context.Context) (*types.AuditChainVerification, error)
}

var _ auditChainContract = (*Storage)(nil)

// appendAuditEvent inserts one event with distinct, verifiable content.
func appendAuditEvent(t *testing.T, store *Storage, ctx context.Context, n int) {
	t.Helper()
	require.NoError(t, store.CreateAuditEvent(ctx, &types.AuditEvent{
		ID:         uuid.NewString(),
		Actor:      fmt.Sprintf("operator:user%d@example.com", n),
		EventType:  "config.applied",
		TargetType: "config",
		TargetID:   fmt.Sprintf("cfg-%d", n),
		Action:     "applied",
		Payload:    map[string]any{"n": n, "note": fmt.Sprintf("event-%d", n)},
	}))
}

func TestAuditChain_GoldenVerifies(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.True(t, res.OK, "detail=%s", res.Detail)
		require.Equal(t, 5, res.RowsVerified)
		require.Equal(t, int64(1), res.CoversFromSeq)
		require.Equal(t, int64(0), res.FirstBreakSeq)
	})
}

func TestAuditChain_EditDetected(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		_, err := store.db.ExecContext(ctx, `UPDATE audit_events SET action='tampered' WHERE seq=3`)
		require.NoError(t, err)

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.False(t, res.OK)
		require.Equal(t, int64(3), res.FirstBreakSeq)
	})
}

func TestAuditChain_MiddleDeleteDetected(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		_, err := store.db.ExecContext(ctx, `DELETE FROM audit_events WHERE seq=3`)
		require.NoError(t, err)

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.False(t, res.OK)
		require.Equal(t, int64(4), res.FirstBreakSeq, "gap should be detected at seq 4")
	})
}

func TestAuditChain_ReorderDetected(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		// Swap the content (payload + action) of two rows without moving their
		// seq: each row now hashes to a value that no longer matches its stored
		// row_hash at that seq — a reorder is indistinguishable from a swap.
		_, err := store.db.ExecContext(ctx, `UPDATE audit_events SET payload='{"reordered":true}' WHERE seq=2`)
		require.NoError(t, err)

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.False(t, res.OK)
		require.Equal(t, int64(2), res.FirstBreakSeq)
	})
}

func TestAuditChain_PrefixGCNoFalsePositive(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		// Legitimate prefix garbage-collection: the earliest row is pruned. The
		// new chain-start carries a non-empty prev_hash with no visible
		// predecessor — this must NOT be flagged.
		_, err := store.db.ExecContext(ctx, `DELETE FROM audit_events WHERE seq=1`)
		require.NoError(t, err)

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.True(t, res.OK, "prefix GC must not be a false positive: detail=%s", res.Detail)
		require.Equal(t, 4, res.RowsVerified)
		require.Equal(t, int64(2), res.CoversFromSeq)
	})
}

func TestAuditChain_ConcurrencyNoFork(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()

		const goroutines = 10
		const perGoroutine = 5
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(base int) {
				defer wg.Done()
				for i := 0; i < perGoroutine; i++ {
					require.NoError(t, store.CreateAuditEvent(ctx, &types.AuditEvent{
						ID:         uuid.NewString(),
						Actor:      "system",
						EventType:  "concurrent.append",
						TargetType: "config",
						TargetID:   fmt.Sprintf("g%d-i%d", base, i),
						Action:     "applied",
						Payload:    map[string]any{"g": base, "i": i},
					}))
				}
			}(g)
		}
		wg.Wait()

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.True(t, res.OK, "concurrent appends must not fork the chain: detail=%s", res.Detail)
		require.Equal(t, goroutines*perGoroutine, res.RowsVerified)

		// Independently confirm seqs are 1..50 with no gap or duplicate.
		var distinctCount, maxSeq, total int64
		require.NoError(t, store.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT seq), COALESCE(MAX(seq),0), COUNT(*) FROM audit_events WHERE seq IS NOT NULL`).
			Scan(&distinctCount, &maxSeq, &total))
		require.Equal(t, int64(goroutines*perGoroutine), distinctCount, "no duplicate seqs")
		require.Equal(t, int64(goroutines*perGoroutine), maxSeq, "contiguous up to N")
		require.Equal(t, int64(goroutines*perGoroutine), total, "no forked rows")
	})
}

func TestAuditChain_PerTenantIsolation(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctxA := identity.WithTenant(context.Background(), "tenant-a")
		ctxB := identity.WithTenant(context.Background(), "tenant-b")

		for i := 1; i <= 3; i++ {
			appendAuditEvent(t, store, ctxA, i)
		}
		for i := 1; i <= 4; i++ {
			appendAuditEvent(t, store, ctxB, i)
		}

		resA, err := store.VerifyAuditChain(ctxA)
		require.NoError(t, err)
		require.True(t, resA.OK, "detail=%s", resA.Detail)
		require.Equal(t, 3, resA.RowsVerified)
		require.Equal(t, int64(1), resA.CoversFromSeq, "tenant A seqs restart at 1")

		resB, err := store.VerifyAuditChain(ctxB)
		require.NoError(t, err)
		require.True(t, resB.OK, "detail=%s", resB.Detail)
		require.Equal(t, 4, resB.RowsVerified)
		require.Equal(t, int64(1), resB.CoversFromSeq, "tenant B seqs restart at 1")
	})
}
