// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package chain

import (
	"testing"
	"time"
)

// testKey builds a deterministic 32-byte audit HMAC key for the keyed tests.
func testKey(t *testing.T) *Key {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	k, err := NewKey(raw)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k
}

func TestNewKeyRejectsShort(t *testing.T) {
	if _, err := NewKey(make([]byte, 31)); err == nil {
		t.Fatal("NewKey must reject a key shorter than 32 bytes")
	}
	if _, err := NewKey(make([]byte, 32)); err != nil {
		t.Fatalf("NewKey must accept 32 bytes: %v", err)
	}
}

// buildKeyedChain builds n correctly linked KEYED rows (seq 1..n) with strictly
// increasing timestamps, sealed under key.
func buildKeyedChain(key *Key, tenant string, n int, base time.Time) []Row {
	rows := make([]Row, 0, n)
	prev := ""
	for i := 1; i <= n; i++ {
		ts := CanonicalTimestamp(base.Add(time.Duration(i) * time.Second))
		r := Row{
			ID:         "id" + itoa(i),
			Actor:      "actor",
			EventType:  "evt",
			TargetType: "tgt",
			TargetID:   "tid" + itoa(i),
			Action:     "act",
			Payload:    "{}",
			Tenant:     tenant,
			Seq:        int64(i),
			PrevHash:   prev,
			Timestamp:  ts,
			Algo:       AlgoHMACSHA256,
		}
		r.RowHash = RowHashV2(key, r.ID, r.Actor, r.EventType, r.TargetType, r.TargetID, r.Action, r.Payload, r.Tenant, r.Timestamp, r.Seq, r.PrevHash)
		rows = append(rows, r)
		prev = r.RowHash
	}
	return rows
}

func headFor(key *Key, rows []Row) *Head {
	last := rows[len(rows)-1]
	return &Head{Tenant: last.Tenant, HeadSeq: last.Seq, HeadRowHash: last.RowHash, MAC: key.HeadMAC(last.Tenant, last.Seq, last.RowHash)}
}

func TestVerifySealedKeyedOK(t *testing.T) {
	key := testKey(t)
	rows := buildKeyedChain(key, "tenant-a", 5, time.Unix(1_700_000_000, 0))
	res := VerifySealed(rows, key, headFor(key, rows))
	if !res.OK {
		t.Fatalf("keyed chain should verify: break at seq %d: %s", res.FirstBreakSeq, res.Detail)
	}
	if res.RowsVerified != 5 || res.HeadSeq != 5 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// TestVerifySealedForgeWithoutKeyFails is the CRITICAL regression: a DB writer
// edits a row and recomputes its row_hash with the PUBLIC unkeyed SHA-256
// (chain.RowHash) — exactly what the ADR 0027 chain could not catch. Under the
// keyed scheme, verify must FAIL because the attacker lacks the HMAC key.
func TestVerifySealedForgeWithoutKeyFails(t *testing.T) {
	key := testKey(t)
	rows := buildKeyedChain(key, "tenant-a", 5, time.Unix(1_700_000_000, 0))
	// Attacker edits seq 3's action and recomputes with the public algorithm.
	rows[2].Action = "tampered"
	rows[2].RowHash = RowHash(rows[2].ID, rows[2].Actor, rows[2].EventType, rows[2].TargetType, rows[2].TargetID, rows[2].Action, rows[2].Payload, rows[2].Tenant, rows[2].Seq, rows[2].PrevHash)
	res := VerifySealed(rows, key, nil)
	if res.OK {
		t.Fatal("a DB-writer forge with the public SHA-256 must be caught by the keyed verify")
	}
	if res.FirstBreakSeq != 3 {
		t.Fatalf("FirstBreakSeq = %d, want 3", res.FirstBreakSeq)
	}
}

// TestVerifySealedContentEditFails — editing content without touching the stored
// hash (a naive edit) fails too.
func TestVerifySealedContentEditFails(t *testing.T) {
	key := testKey(t)
	rows := buildKeyedChain(key, "tenant-a", 4, time.Unix(1_700_000_000, 0))
	rows[1].Payload = `{"tampered":true}`
	res := VerifySealed(rows, key, nil)
	if res.OK || res.FirstBreakSeq != 2 {
		t.Fatalf("edit must break at seq 2, got %+v", res)
	}
}

// TestVerifySealedBackdatingMonotonicity — even a key-holder who re-seals a row
// with a valid HMAC but an EARLIER timestamp is caught by the monotonicity rule.
func TestVerifySealedBackdatingMonotonicity(t *testing.T) {
	key := testKey(t)
	base := time.Unix(1_700_000_000, 0)
	rows := buildKeyedChain(key, "tenant-a", 4, base)
	// Re-seal seq 3 with a timestamp BEFORE seq 2 (a valid hash, wrong order).
	earlier := CanonicalTimestamp(base.Add(-time.Hour))
	rows[2].Timestamp = earlier
	rows[2].RowHash = RowHashV2(key, rows[2].ID, rows[2].Actor, rows[2].EventType, rows[2].TargetType, rows[2].TargetID, rows[2].Action, rows[2].Payload, rows[2].Tenant, rows[2].Timestamp, rows[2].Seq, rows[2].PrevHash)
	// Fix the downstream link so the ONLY remaining break is the timestamp order.
	rows[3].PrevHash = rows[2].RowHash
	rows[3].RowHash = RowHashV2(key, rows[3].ID, rows[3].Actor, rows[3].EventType, rows[3].TargetType, rows[3].TargetID, rows[3].Action, rows[3].Payload, rows[3].Tenant, rows[3].Timestamp, rows[3].Seq, rows[3].PrevHash)
	res := VerifySealed(rows, key, nil)
	if res.OK {
		t.Fatal("a backdated (valid-hash) row must be caught by timestamp monotonicity")
	}
	if res.FirstBreakSeq != 3 {
		t.Fatalf("FirstBreakSeq = %d, want 3", res.FirstBreakSeq)
	}
}

// TestVerifySealedTailTruncationHWM — deleting the newest rows leaves a shorter
// contiguous chain that the walk alone accepts; the high-water-mark catches it.
func TestVerifySealedTailTruncationHWM(t *testing.T) {
	key := testKey(t)
	full := buildKeyedChain(key, "tenant-a", 5, time.Unix(1_700_000_000, 0))
	head := headFor(key, full) // records seq 5
	truncated := full[:3]      // attacker deleted seq 4 + 5
	res := VerifySealed(truncated, key, head)
	if res.OK {
		t.Fatal("tail truncation must be caught by the high-water-mark")
	}
	if res.FirstBreakSeq != 5 {
		t.Fatalf("FirstBreakSeq = %d, want 5 (HWM seq)", res.FirstBreakSeq)
	}
}

// TestVerifySealedForgedHWMFails — a DB writer who lowers the HWM to match a
// truncated chain can't forge its MAC.
func TestVerifySealedForgedHWMFails(t *testing.T) {
	key := testKey(t)
	full := buildKeyedChain(key, "tenant-a", 5, time.Unix(1_700_000_000, 0))
	truncated := full[:3]
	// Attacker rewrites the HWM to seq 3 but has no key to MAC it.
	forged := &Head{Tenant: "tenant-a", HeadSeq: 3, HeadRowHash: truncated[2].RowHash, MAC: "deadbeef"}
	res := VerifySealed(truncated, key, forged)
	if res.OK {
		t.Fatal("a forged high-water-mark MAC must be rejected")
	}
}

// TestVerifySealedDowngradeGuard — once keyed, the chain may not revert to the
// forgeable legacy scheme.
func TestVerifySealedDowngradeGuard(t *testing.T) {
	key := testKey(t)
	rows := buildKeyedChain(key, "tenant-a", 3, time.Unix(1_700_000_000, 0))
	// Attacker appends a legacy (unkeyed) row after the keyed prefix.
	legacy := Row{
		ID: "id4", Actor: "actor", EventType: "evt", TargetType: "tgt", TargetID: "tid4",
		Action: "act", Payload: "{}", Tenant: "tenant-a", Seq: 4, PrevHash: rows[2].RowHash, Algo: AlgoLegacySHA256,
	}
	legacy.RowHash = RowHash(legacy.ID, legacy.Actor, legacy.EventType, legacy.TargetType, legacy.TargetID, legacy.Action, legacy.Payload, legacy.Tenant, legacy.Seq, legacy.PrevHash)
	rows = append(rows, legacy)
	res := VerifySealed(rows, key, nil)
	if res.OK || res.FirstBreakSeq != 4 {
		t.Fatalf("downgrade to legacy after keyed must fail at seq 4, got %+v", res)
	}
}

// TestVerifySealedLegacyStillVerifies — a pure-legacy chain (no key) verifies OK
// under VerifySealed, identical to the ADR 0027 Verify. This is the migration
// guarantee: existing unkeyed chains keep verifying.
func TestVerifySealedLegacyStillVerifies(t *testing.T) {
	rows := buildChain("tenant-a", 5) // legacy rows, Algo == ""
	res := VerifySealed(rows, nil, nil)
	if !res.OK || res.RowsVerified != 5 {
		t.Fatalf("legacy chain must still verify under VerifySealed: %+v", res)
	}
}

// TestVerifySealedMixedLegacyThenKeyed — the migration case: a legacy prefix
// followed by a keyed suffix that links to it verifies OK, and editing a legacy
// row is caught because the first keyed row pins the boundary hash.
func TestVerifySealedMixedLegacyThenKeyed(t *testing.T) {
	key := testKey(t)
	// Legacy prefix seqs 1..2.
	legacy := buildChain("tenant-a", 2)
	// Keyed suffix seqs 3..4 linking to the legacy head.
	base := time.Unix(1_700_000_000, 0)
	prev := legacy[len(legacy)-1].RowHash
	all := append([]Row{}, legacy...)
	for i := 3; i <= 4; i++ {
		ts := CanonicalTimestamp(base.Add(time.Duration(i) * time.Second))
		r := Row{
			ID: "id" + itoa(i), Actor: "actor", EventType: "evt", TargetType: "tgt",
			TargetID: "tid" + itoa(i), Action: "act", Payload: "{}", Tenant: "tenant-a",
			Seq: int64(i), PrevHash: prev, Timestamp: ts, Algo: AlgoHMACSHA256,
		}
		r.RowHash = RowHashV2(key, r.ID, r.Actor, r.EventType, r.TargetType, r.TargetID, r.Action, r.Payload, r.Tenant, r.Timestamp, r.Seq, r.PrevHash)
		all = append(all, r)
		prev = r.RowHash
	}
	if res := VerifySealed(all, key, nil); !res.OK {
		t.Fatalf("mixed legacy+keyed chain must verify: break at seq %d: %s", res.FirstBreakSeq, res.Detail)
	}

	// Editing a legacy row breaks the link into the first keyed row (whose HMAC
	// the attacker cannot recompute), so the boundary is sealed.
	all[1].Payload = `{"tampered":true}`
	all[1].RowHash = RowHash(all[1].ID, all[1].Actor, all[1].EventType, all[1].TargetType, all[1].TargetID, all[1].Action, all[1].Payload, all[1].Tenant, all[1].Seq, all[1].PrevHash)
	res := VerifySealed(all, key, nil)
	if res.OK {
		t.Fatal("editing a legacy row must break the keyed boundary link")
	}
}

// TestVerifySealedKeyedRowNoKeyFailsClosed — a keyed chain cannot be verified
// (and must not pass) without the key.
func TestVerifySealedKeyedRowNoKeyFailsClosed(t *testing.T) {
	key := testKey(t)
	rows := buildKeyedChain(key, "tenant-a", 3, time.Unix(1_700_000_000, 0))
	res := VerifySealed(rows, nil, nil)
	if res.OK {
		t.Fatal("a keyed chain must fail closed when no key is configured")
	}
}

func TestCanonicalTimestampStableAndMonotonic(t *testing.T) {
	a := time.Unix(1_700_000_000, 123456789).UTC() // nanos
	got := CanonicalTimestamp(a)
	// Idempotent across a micro round-trip.
	if again := CanonicalTimestamp(a.Truncate(time.Microsecond)); again != got {
		t.Fatalf("CanonicalTimestamp not stable across micro truncation: %q vs %q", got, again)
	}
	// Lexical order matches chronological order (fixed-width fraction).
	earlier := CanonicalTimestamp(time.Unix(1_700_000_000, 500000000).UTC()) // .5s
	later := CanonicalTimestamp(time.Unix(1_700_000_001, 0).UTC())           // next whole second
	if !(earlier < later) {
		t.Fatalf("lexical order must match time order: %q !< %q", earlier, later)
	}
}
