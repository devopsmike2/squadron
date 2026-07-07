// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package chain

import "testing"

// TestRowHashGoldenVector pins the canonical wire format. The hex is computed
// once from RowHash and hardcoded so the offline verifier and any future change
// to the field concat can never silently drift from the stored on-disk hashes.
func TestRowHashGoldenVector(t *testing.T) {
	const want = "6bd9f814a828f15efd506149da9e666752ca615b5c0c0e50aa2d05e88923e78f"
	got := RowHash("id1", "actor", "evt", "tgt", "tid", "act", "{}", "tenant-a", 1, "")
	if got != want {
		t.Fatalf("golden vector drift:\n got  %s\n want %s", got, want)
	}
}

// buildChain constructs n correctly linked rows for tenant, seq 1..n.
func buildChain(tenant string, n int) []Row {
	rows := make([]Row, 0, n)
	prev := ""
	for i := 1; i <= n; i++ {
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
		}
		r.RowHash = RowHash(r.ID, r.Actor, r.EventType, r.TargetType, r.TargetID, r.Action, r.Payload, r.Tenant, r.Seq, r.PrevHash)
		rows = append(rows, r)
		prev = r.RowHash
	}
	return rows
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

func TestVerifyGoldenChainOK(t *testing.T) {
	res := Verify(buildChain("tenant-a", 5))
	if !res.OK {
		t.Fatalf("expected OK, got break at seq %d: %s", res.FirstBreakSeq, res.Detail)
	}
	if res.RowsVerified != 5 {
		t.Fatalf("RowsVerified = %d, want 5", res.RowsVerified)
	}
	if res.CoversFromSeq != 1 {
		t.Fatalf("CoversFromSeq = %d, want 1", res.CoversFromSeq)
	}
	if res.HeadSeq != 5 {
		t.Fatalf("HeadSeq = %d, want 5", res.HeadSeq)
	}
	if res.FirstBreakSeq != 0 {
		t.Fatalf("FirstBreakSeq = %d, want 0", res.FirstBreakSeq)
	}
}

func TestVerifyEmptyChainOK(t *testing.T) {
	res := Verify(nil)
	if !res.OK || res.RowsVerified != 0 {
		t.Fatalf("empty chain should verify OK with 0 rows, got %+v", res)
	}
}

func TestVerifyContentEditBreaks(t *testing.T) {
	rows := buildChain("tenant-a", 5)
	rows[2].Payload = `{"tampered":true}` // seq 3, row_hash left stale
	res := Verify(rows)
	if res.OK {
		t.Fatal("edited row must break the chain")
	}
	if res.FirstBreakSeq != 3 {
		t.Fatalf("FirstBreakSeq = %d, want 3", res.FirstBreakSeq)
	}
	if res.RowsVerified != 2 {
		t.Fatalf("RowsVerified = %d, want 2", res.RowsVerified)
	}
}

func TestVerifyChainStartEditBreaks(t *testing.T) {
	rows := buildChain("tenant-a", 3)
	rows[0].Payload = `{"tampered":true}` // seq 1 stored row_hash now stale
	res := Verify(rows)
	if res.OK {
		t.Fatal("edited chain-start row must break")
	}
	if res.FirstBreakSeq != 1 {
		t.Fatalf("FirstBreakSeq = %d, want 1", res.FirstBreakSeq)
	}
}

func TestVerifyMiddleDeleteBreaks(t *testing.T) {
	rows := buildChain("tenant-a", 5)
	// drop seq 3 -> [1,2,4,5]; the gap at 4 (expected 3) is a middle deletion.
	rows = append(rows[:2], rows[3:]...)
	res := Verify(rows)
	if res.OK {
		t.Fatal("middle deletion must break the chain")
	}
	if res.FirstBreakSeq != 4 {
		t.Fatalf("FirstBreakSeq = %d, want 4 (gap detected)", res.FirstBreakSeq)
	}
}

func TestVerifyReorderBreaks(t *testing.T) {
	rows := buildChain("tenant-a", 5)
	rows[2], rows[3] = rows[3], rows[2] // swap seq 3 and 4
	res := Verify(rows)
	if res.OK {
		t.Fatal("reorder must break the chain")
	}
	// After the swap, position index 2 now holds original seq 4 while prevSeq is
	// 2, so contiguity fails first (expected 3, got 4) at seq 4.
	if res.FirstBreakSeq != 4 {
		t.Fatalf("FirstBreakSeq = %d, want 4", res.FirstBreakSeq)
	}
}

func TestVerifyPrefixGCOK(t *testing.T) {
	// Simulate the opt-in retention sweep pruning seqs 1..2: the surviving
	// chain-start (seq 3) carries a non-empty prev_hash with no visible
	// predecessor. That is NOT a tamper signal — it must verify OK.
	full := buildChain("tenant-a", 5)
	pruned := full[2:] // seqs 3,4,5; pruned[0].PrevHash != ""
	if pruned[0].PrevHash == "" {
		t.Fatal("precondition: chain-start prev_hash should be non-empty")
	}
	res := Verify(pruned)
	if !res.OK {
		t.Fatalf("prefix GC must not be a false positive: break at seq %d: %s", res.FirstBreakSeq, res.Detail)
	}
	if res.CoversFromSeq != 3 {
		t.Fatalf("CoversFromSeq = %d, want 3", res.CoversFromSeq)
	}
	if res.RowsVerified != 3 {
		t.Fatalf("RowsVerified = %d, want 3", res.RowsVerified)
	}
}

func TestVerifyPrevHashLinkBreaks(t *testing.T) {
	rows := buildChain("tenant-a", 4)
	rows[2].PrevHash = "deadbeef" // break the link into seq 3 without a seq gap
	// recompute seq 3's row_hash so the row_hash check would pass; the link
	// check must fire first.
	rows[2].RowHash = RowHash(rows[2].ID, rows[2].Actor, rows[2].EventType, rows[2].TargetType, rows[2].TargetID, rows[2].Action, rows[2].Payload, rows[2].Tenant, rows[2].Seq, rows[2].PrevHash)
	res := Verify(rows)
	if res.OK {
		t.Fatal("broken prev_hash link must break the chain")
	}
	if res.FirstBreakSeq != 3 {
		t.Fatalf("FirstBreakSeq = %d, want 3", res.FirstBreakSeq)
	}
}
