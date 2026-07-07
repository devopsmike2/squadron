// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	chain "github.com/devopsmike2/squadron/internal/audit/chain"
)

// buildValidChain hand-crafts n correctly linked rows using chain.RowHash so the
// hashes are valid (mirrors the append path). Payloads are multi-key JSON with a
// deliberate key order + a nested object — byte-exactness matters, the recompute
// must see THESE exact bytes.
func buildValidChain(tenant string, n int) []chain.Row {
	rows := make([]chain.Row, 0, n)
	prev := ""
	for i := 1; i <= n; i++ {
		r := chain.Row{
			ID:         fmt.Sprintf("id-%d", i),
			Actor:      "operator:auditor@example.com",
			EventType:  "config.applied",
			TargetType: "config",
			TargetID:   fmt.Sprintf("cfg-%d", i),
			Action:     "applied",
			Payload:    fmt.Sprintf(`{"seq":%d,"zeta":2,"alpha":1,"note":"event-%d","nested":{"k":"v"}}`, i, i),
			Tenant:     tenant,
			Seq:        int64(i),
			PrevHash:   prev,
		}
		r.RowHash = chain.RowHash(r.ID, r.Actor, r.EventType, r.TargetType, r.TargetID, r.Action, r.Payload, r.Tenant, r.Seq, r.PrevHash)
		rows = append(rows, r)
		prev = r.RowHash
	}
	return rows
}

var chainHeader = []string{"tenant_id", "seq", "prev_hash", "row_hash", "id", "actor", "event_type", "target_type", "target_id", "action", "payload"}

// renderCSV renders rows in the EXACT column order the handler's chain export
// emits, so the CLI test round-trips against the real wire shape.
func renderCSV(rows []chain.Row) string {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write(chainHeader)
	for _, r := range rows {
		_ = w.Write([]string{r.Tenant, strconv.FormatInt(r.Seq, 10), r.PrevHash, r.RowHash, r.ID, r.Actor, r.EventType, r.TargetType, r.TargetID, r.Action, r.Payload})
	}
	w.Flush()
	return b.String()
}

// renderJSON renders rows as the JSON array the handler emits (payload as a raw
// JSON string, never a re-parsed object).
func renderJSON(rows []chain.Row) string {
	out := make([]exportRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, exportRow{
			TenantID: r.Tenant, Seq: r.Seq, PrevHash: r.PrevHash, RowHash: r.RowHash,
			ID: r.ID, Actor: r.Actor, EventType: r.EventType, TargetType: r.TargetType,
			TargetID: r.TargetID, Action: r.Action, Payload: r.Payload,
		})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func makeAttestation(tenant string, rows []chain.Row) Attestation {
	last := rows[len(rows)-1]
	return Attestation{
		Tenant: tenant, OK: true, RowsVerified: len(rows),
		HeadSeq: last.Seq, HeadRowHash: last.RowHash, CoversFromSeq: rows[0].Seq,
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp %s: %v", name, err)
	}
	return p
}

// TestRoundTrip_ExportVerifyPASS is the required safety net: build a valid
// chain, export it to a temp CSV + JSON, parse each back, and confirm the
// offline verifier PASSes. It ALSO asserts the payload round-trips byte-exact
// (the anti-re-marshal-drift proof).
func TestRoundTrip_ExportVerifyPASS(t *testing.T) {
	const tenant = "default"
	rows := buildValidChain(tenant, 5)
	att := makeAttestation(tenant, rows)

	csvPath := writeTemp(t, "audit-chain.csv", renderCSV(rows))
	jsonPath := writeTemp(t, "audit-chain.json", renderJSON(rows))

	for _, tc := range []struct{ name, path string }{{"csv", csvPath}, {"json", jsonPath}} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := parseExport(raw)
			if err != nil {
				t.Fatalf("parseExport: %v", err)
			}
			if len(parsed) != len(rows) {
				t.Fatalf("parsed %d rows, want %d", len(parsed), len(rows))
			}
			// Byte-exactness: every payload survived export+parse verbatim.
			for i := range rows {
				if parsed[i].Payload != rows[i].Payload {
					t.Fatalf("payload drift at seq %d:\n got  %q\n want %q", rows[i].Seq, parsed[i].Payload, rows[i].Payload)
				}
			}
			out := verifyExport(parsed, att)
			if !out.Pass {
				t.Fatalf("expected PASS, got %+v", out)
			}
			if !out.ChainOK || !out.HeadSeqMatch || !out.HeadHashMatch {
				t.Fatalf("expected all-green, got %+v", out)
			}
		})
	}
}

// TestTamperedPayload_FAIL edits a payload byte in the exported CSV and confirms
// the verifier detects the break at that seq (row_hash mismatch) and FAILs.
func TestTamperedPayload_FAIL(t *testing.T) {
	const tenant = "default"
	rows := buildValidChain(tenant, 5)
	att := makeAttestation(tenant, rows)

	csvText := renderCSV(rows)
	tampered := strings.Replace(csvText, "event-3", "hacked3", 1)
	if tampered == csvText {
		t.Fatal("tamper replacement did not apply")
	}
	parsed, err := parseExport([]byte(tampered))
	if err != nil {
		t.Fatalf("parseExport: %v", err)
	}
	out := verifyExport(parsed, att)
	if out.Pass {
		t.Fatal("expected FAIL on a tampered payload")
	}
	if out.ChainOK {
		t.Fatal("expected chain BROKEN")
	}
	if out.FirstBreakSeq != 3 {
		t.Fatalf("expected break at seq 3, got %d (detail=%s)", out.FirstBreakSeq, out.Detail)
	}
}

// TestHeadMismatch_FAIL keeps the chain intact but hands the verifier an
// attestation whose head_row_hash is wrong — the chain verifies OK but the tip
// does NOT match, so the overall verdict FAILs (distinct from a broken chain).
func TestHeadMismatch_FAIL(t *testing.T) {
	const tenant = "default"
	rows := buildValidChain(tenant, 5)
	att := makeAttestation(tenant, rows)
	att.HeadRowHash = "deadbeef" + att.HeadRowHash[8:] // corrupt the attested tip

	out := verifyExport(rows, att)
	if out.Pass {
		t.Fatal("expected FAIL on head_row_hash mismatch")
	}
	if !out.ChainOK {
		t.Fatal("chain itself should still verify OK")
	}
	if out.HeadHashMatch {
		t.Fatal("head hash should NOT match")
	}
	if !out.HeadSeqMatch {
		t.Fatal("head seq should still match (only the hash was corrupted)")
	}
}
