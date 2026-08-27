// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"strings"
	"testing"

	chain "github.com/devopsmike2/squadron/internal/audit/chain"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/require"
)

// keyedTestKey builds a deterministic 32-byte audit HMAC key for the store tests.
func keyedTestKey(t *testing.T) *chain.Key {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i*7 + 1)
	}
	k, err := chain.NewKey(raw)
	require.NoError(t, err)
	return k
}

// TestAuditChainKeyed_VerifiesAndReportsKeyed — a keyed append verifies OK and
// VerifyAuditChain reports Keyed=true with the high-water-mark at the tip.
func TestAuditChainKeyed_VerifiesAndReportsKeyed(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		store.SetAuditChainKey(keyedTestKey(t))
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.True(t, res.OK, "detail=%s", res.Detail)
		require.True(t, res.Keyed, "chain must report as keyed")
		require.Equal(t, 5, res.RowsVerified)
		require.Equal(t, int64(5), res.HighWaterMarkSeq)

		// Every row is sealed under the HMAC scheme.
		var nHMAC int
		require.NoError(t, store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audit_events WHERE chain_algo = ?`, chain.AlgoHMACSHA256).Scan(&nHMAC))
		require.Equal(t, 5, nHMAC)
	})
}

// TestAuditChainKeyed_DBWriterForgeDetected is the CRITICAL regression: a DB
// writer edits a row AND recomputes its row_hash with the PUBLIC unkeyed
// SHA-256 — what the ADR 0027 chain accepted. The keyed verify must FAIL.
func TestAuditChainKeyed_DBWriterForgeDetected(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		store.SetAuditChainKey(keyedTestKey(t))
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		// Read seq 3's columns to forge a public-SHA256 hash over edited content.
		var id, actor, eventType, targetType, targetID, action, payload, tenant, prevHash string
		require.NoError(t, store.db.QueryRowContext(ctx,
			`SELECT id, actor, event_type, target_type, target_id, action, payload, tenant_id, prev_hash FROM audit_events WHERE seq=3`).
			Scan(&id, &actor, &eventType, &targetType, &targetID, &action, &payload, &tenant, &prevHash))
		forged := chain.RowHash(id, actor, eventType, targetType, targetID, "tampered", payload, tenant, 3, prevHash)
		_, err := store.db.ExecContext(ctx,
			`UPDATE audit_events SET action='tampered', row_hash=? WHERE seq=3`, forged)
		require.NoError(t, err)

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.False(t, res.OK, "a public-SHA256 forge must be caught by the keyed verify")
		require.Equal(t, int64(3), res.FirstBreakSeq)
	})
}

// TestAuditChainKeyed_TailTruncationDetected — deleting the newest rows leaves a
// contiguous prefix the walk accepts; the high-water-mark catches it.
func TestAuditChainKeyed_TailTruncationDetected(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		store.SetAuditChainKey(keyedTestKey(t))
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		// Attacker deletes their two newest actions (seq 4, 5).
		_, err := store.db.ExecContext(ctx, `DELETE FROM audit_events WHERE seq IN (4,5)`)
		require.NoError(t, err)

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.False(t, res.OK, "tail truncation must be detected via the high-water-mark")
		require.Equal(t, int64(5), res.FirstBreakSeq)
		require.Contains(t, strings.ToLower(res.Detail), "truncation")
	})
}

// TestAuditChainKeyed_BackdateDetected — because the timestamp is folded into the
// keyed hash, editing ONLY the timestamp column breaks the row hash.
func TestAuditChainKeyed_BackdateDetected(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		store.SetAuditChainKey(keyedTestKey(t))
		ctx := context.Background()
		for i := 1; i <= 5; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		// Backdate seq 3 into an earlier window.
		_, err := store.db.ExecContext(ctx,
			`UPDATE audit_events SET timestamp='2020-01-01 00:00:00+00:00' WHERE seq=3`)
		require.NoError(t, err)

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.False(t, res.OK, "a backdated timestamp must be detected (timestamp is in the hash)")
		require.Equal(t, int64(3), res.FirstBreakSeq)
	})
}

// TestAuditChainKeyed_KeyAbsentNoCrashLegacy — with NO key configured the store
// keeps working in the unkeyed-legacy warn-window: appends succeed, verify is OK,
// and it reports Keyed=false (the operator-visible DEGRADED signal). No crash.
func TestAuditChainKeyed_KeyAbsentNoCrashLegacy(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		for i := 1; i <= 4; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.True(t, res.OK, "unkeyed legacy chain must still verify: detail=%s", res.Detail)
		require.False(t, res.Keyed, "no key -> reported as unkeyed/degraded")
		require.Equal(t, int64(0), res.HighWaterMarkSeq, "no HWM without a key")

		var nLegacy int
		require.NoError(t, store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audit_events WHERE chain_algo IS NULL OR chain_algo=''`).Scan(&nLegacy))
		require.Equal(t, 4, nLegacy)
	})
}

// TestAuditChainKeyed_MigrationLegacyPrefixThenKeyed — the migration path:
// pre-cutover legacy rows keep verifying alongside a keyed suffix, and editing a
// legacy row is caught because the first keyed row pins the boundary.
func TestAuditChainKeyed_MigrationLegacyPrefixThenKeyed(t *testing.T) {
	withSQLiteStore(t, func(s types.ApplicationStore) {
		store := s.(*Storage)
		ctx := context.Background()
		// Unkeyed legacy prefix.
		for i := 1; i <= 3; i++ {
			appendAuditEvent(t, store, ctx, i)
		}
		// Operator provides the key; the chain seals forward from seq 4.
		store.SetAuditChainKey(keyedTestKey(t))
		for i := 4; i <= 6; i++ {
			appendAuditEvent(t, store, ctx, i)
		}

		res, err := store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.True(t, res.OK, "mixed legacy+keyed chain must verify: detail=%s", res.Detail)
		require.True(t, res.Keyed)
		require.Equal(t, 6, res.RowsVerified)
		require.Equal(t, int64(6), res.HighWaterMarkSeq)

		// Two legacy rows survive as legacy (seq 1..3), three keyed (seq 4..6).
		var nLegacy, nKeyed int
		require.NoError(t, store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audit_events WHERE chain_algo IS NULL OR chain_algo=''`).Scan(&nLegacy))
		require.NoError(t, store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audit_events WHERE chain_algo=?`, chain.AlgoHMACSHA256).Scan(&nKeyed))
		require.Equal(t, 3, nLegacy)
		require.Equal(t, 3, nKeyed)

		// Tampering a legacy row is still detected (naive edit → stale row_hash at
		// seq 2). The stronger "attacker re-hashes the legacy row with public
		// SHA-256, boundary caught at the first keyed row" case is covered by the
		// pure chain test TestVerifySealedMixedLegacyThenKeyed.
		_, err = store.db.ExecContext(ctx, `UPDATE audit_events SET action='tampered' WHERE seq=2`)
		require.NoError(t, err)
		res, err = store.VerifyAuditChain(ctx)
		require.NoError(t, err)
		require.False(t, res.OK, "editing a legacy row must be caught")
		require.Equal(t, int64(2), res.FirstBreakSeq)
	})
}
