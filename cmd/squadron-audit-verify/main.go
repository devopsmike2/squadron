// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Command squadron-audit-verify is the OFFLINE attestation verifier (ADR 0027).
//
// It lets an auditor independently re-verify a Squadron tamper-evidence
// attestation with ZERO secrets: given the chain-column audit export (the CSV
// or JSON produced by GET /api/v1/audit/events?include_chain=1) and the
// attestation JSON (downloaded from /audit-verify/tenants/{t}/attest), it
// recomputes the hash-chain OFFLINE with internal/audit/chain and confirms the
// recomputed head (HeadSeq + HeadRowHash) matches the attestation's tip.
//
// The core auditor check needs no key material at all — it re-hashes the
// exported rows and compares the chain tip to what Squadron attested. If the
// SQUADRON_SECRETS_KEY is ALSO available and the attestation carries a sealed
// signature, an OPTIONAL second step opens the seal to confirm the same key
// that runs Squadron vouches for this exact head; a missing/rotated key never
// fails the primary zero-secret result.
//
// Dependency-light on purpose: only internal/audit/chain +
// internal/discovery/credstore + the standard library.
package main

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	chain "github.com/devopsmike2/squadron/internal/audit/chain"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// Attestation mirrors the enterprise auditverify.Attestation wire shape (the
// enterprise module is NOT importable from the open core, so the fields are
// mirrored locally with the SAME json tags). Only the tamper-evident anchor
// fields are consumed here; the rest are parsed-and-ignored for forward compat.
type Attestation struct {
	Tenant        string `json:"tenant"`
	OK            bool   `json:"ok"`
	RowsVerified  int    `json:"rows_verified"`
	HeadSeq       int64  `json:"head_seq"`
	HeadRowHash   string `json:"head_row_hash"`
	CoversFromSeq int64  `json:"covers_from_seq"`
	FirstBreakSeq int64  `json:"first_break_seq"`
	Detail        string `json:"detail"`
	SealedSig     string `json:"sealed_sig"`
	Envelope      string `json:"envelope"`
}

// sealPayload mirrors the enterprise auditverify.SealPayload EXACTLY (field
// order tenant, head_seq, head_row_hash + json tags). canonical() reproduces
// SealPayload.Canonical() — Go marshals struct fields in declaration order, so
// the bytes are byte-identical to what the enterprise wire sealed.
type sealPayload struct {
	Tenant      string `json:"tenant"`
	HeadSeq     int64  `json:"head_seq"`
	HeadRowHash string `json:"head_row_hash"`
}

func (p sealPayload) canonical() ([]byte, error) { return json.Marshal(p) }

// exportRow mirrors the JSON chain-export object shape (handler
// auditChainExportRow). payload is the RAW stored string.
type exportRow struct {
	TenantID   string `json:"tenant_id"`
	Seq        int64  `json:"seq"`
	PrevHash   string `json:"prev_hash"`
	RowHash    string `json:"row_hash"`
	ID         string `json:"id"`
	Actor      string `json:"actor"`
	EventType  string `json:"event_type"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Action     string `json:"action"`
	Payload    string `json:"payload"`
}

func (e exportRow) toChainRow() chain.Row {
	return chain.Row{
		ID:         e.ID,
		Actor:      e.Actor,
		EventType:  e.EventType,
		TargetType: e.TargetType,
		TargetID:   e.TargetID,
		Action:     e.Action,
		Payload:    e.Payload,
		Tenant:     e.TenantID,
		Seq:        e.Seq,
		PrevHash:   e.PrevHash,
		RowHash:    e.RowHash,
	}
}

// Outcome is the testable result of the zero-secret core check.
type Outcome struct {
	RowsVerified  int
	ChainOK       bool
	FirstBreakSeq int64
	Detail        string
	CoversFromSeq int64
	RecomputedSeq int64
	RecomputedTip string
	HeadSeqMatch  bool
	HeadHashMatch bool
	// Pass is the overall auditor verdict: the chain recomputes intact AND its
	// tip (seq + hash) matches the attestation. This is the zero-secret result.
	Pass bool
}

// verifyExport is the pure, testable core: recompute the chain over the exported
// rows and compare its tip to the attestation. No I/O, no secrets. rows should
// be sorted by Seq ASC (verifyExport sorts defensively).
func verifyExport(rows []chain.Row, att Attestation) Outcome {
	sorted := make([]chain.Row, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })

	res := chain.Verify(sorted)
	out := Outcome{
		RowsVerified:  res.RowsVerified,
		ChainOK:       res.OK,
		FirstBreakSeq: res.FirstBreakSeq,
		Detail:        res.Detail,
		CoversFromSeq: res.CoversFromSeq,
		RecomputedSeq: res.HeadSeq,
		RecomputedTip: res.HeadRowHash,
	}
	out.HeadSeqMatch = res.OK && res.HeadSeq == att.HeadSeq
	out.HeadHashMatch = res.OK && res.HeadRowHash == att.HeadRowHash
	out.Pass = res.OK && out.HeadSeqMatch && out.HeadHashMatch
	return out
}

// parseExport auto-detects the export format. A leading '[' or '{' (after
// whitespace) is JSON — a top-level array, or newline-delimited objects
// (NDJSON). Anything else is treated as CSV whose header names the columns.
func parseExport(data []byte) ([]chain.Row, error) {
	trimmed := strings.TrimLeftFunc(string(data), func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\r' || r == '\t' || r == '\uFEFF'
	})
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty export")
	}
	switch trimmed[0] {
	case '[', '{':
		return parseExportJSON(trimmed)
	default:
		return parseExportCSV(data)
	}
}

func parseExportJSON(s string) ([]chain.Row, error) {
	if strings.HasPrefix(strings.TrimSpace(s), "[") {
		var rows []exportRow
		if err := json.Unmarshal([]byte(s), &rows); err != nil {
			return nil, fmt.Errorf("parse JSON array export: %w", err)
		}
		out := make([]chain.Row, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.toChainRow())
		}
		return out, nil
	}
	// NDJSON: one object per non-empty line.
	var out []chain.Row
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r exportRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("parse NDJSON export line %d: %w", i+1, err)
		}
		out = append(out, r.toChainRow())
	}
	return out, nil
}

func parseExportCSV(data []byte) ([]chain.Row, error) {
	r := csv.NewReader(strings.NewReader(string(data)))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse CSV export: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("empty CSV export")
	}
	// Map columns by header name so column order is not load-bearing.
	idx := map[string]int{}
	for i, name := range records[0] {
		idx[strings.TrimSpace(strings.TrimPrefix(name, "\uFEFF"))] = i
	}
	for _, req := range []string{"tenant_id", "seq", "prev_hash", "row_hash", "id", "actor", "event_type", "target_type", "target_id", "action", "payload"} {
		if _, ok := idx[req]; !ok {
			return nil, fmt.Errorf("CSV export missing required column %q", req)
		}
	}
	get := func(rec []string, name string) string {
		i := idx[name]
		if i < len(rec) {
			return rec[i]
		}
		return ""
	}
	var out []chain.Row
	for lineNo, rec := range records[1:] {
		seq, err := strconv.ParseInt(strings.TrimSpace(get(rec, "seq")), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("CSV export row %d: bad seq: %w", lineNo+2, err)
		}
		out = append(out, chain.Row{
			ID:         get(rec, "id"),
			Actor:      get(rec, "actor"),
			EventType:  get(rec, "event_type"),
			TargetType: get(rec, "target_type"),
			TargetID:   get(rec, "target_id"),
			Action:     get(rec, "action"),
			Payload:    get(rec, "payload"),
			Tenant:     get(rec, "tenant_id"),
			Seq:        seq,
			PrevHash:   get(rec, "prev_hash"),
			RowHash:    get(rec, "row_hash"),
		})
	}
	return out, nil
}

func main() {
	exportPath := flag.String("export", "", "path to the chain-column audit export (CSV or JSON) from ?include_chain=1")
	attestPath := flag.String("attestation", "", "path to the attestation JSON from /audit-verify/tenants/{t}/attest")
	tenantFlag := flag.String("tenant", "", "optional expected tenant id to cross-check against the export + attestation")
	flag.Parse()

	if *exportPath == "" || *attestPath == "" {
		fmt.Fprintln(os.Stderr, "usage: squadron-audit-verify -export <path> -attestation <path> [-tenant <id>]")
		os.Exit(1)
	}

	exportBytes, err := os.ReadFile(*exportPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read export: %v\n", err)
		os.Exit(1)
	}
	attBytes, err := os.ReadFile(*attestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read attestation: %v\n", err)
		os.Exit(1)
	}

	rows, err := parseExport(exportBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse export: %v\n", err)
		os.Exit(1)
	}
	var att Attestation
	if err := json.Unmarshal(attBytes, &att); err != nil {
		fmt.Fprintf(os.Stderr, "parse attestation: %v\n", err)
		os.Exit(1)
	}

	out := verifyExport(rows, att)

	fmt.Println("Squadron offline attestation verifier (ADR 0027)")
	fmt.Printf("  export rows:        %d\n", len(rows))
	fmt.Printf("  attestation tenant: %s\n", att.Tenant)
	fmt.Printf("  rows verified:      %d\n", out.RowsVerified)
	if out.ChainOK {
		fmt.Printf("  chain:              OK (covers from seq %d)\n", out.CoversFromSeq)
	} else {
		fmt.Printf("  chain:              BROKEN at seq %d — %s\n", out.FirstBreakSeq, out.Detail)
	}
	fmt.Printf("  recomputed head:    seq=%d hash=%s\n", out.RecomputedSeq, out.RecomputedTip)
	fmt.Printf("  attested head:      seq=%d hash=%s\n", att.HeadSeq, att.HeadRowHash)

	failed := false

	// Optional tenant cross-check. A mismatch means the export/attestation is
	// not for the tenant the auditor expected — a hard FAIL.
	if *tenantFlag != "" {
		if att.Tenant != *tenantFlag {
			fmt.Printf("  tenant cross-check: FAIL (attestation tenant %q != expected %q)\n", att.Tenant, *tenantFlag)
			failed = true
		} else {
			mismatch := ""
			for _, r := range rows {
				if r.Tenant != *tenantFlag {
					mismatch = r.Tenant
					break
				}
			}
			if mismatch != "" {
				fmt.Printf("  tenant cross-check: FAIL (export row tenant %q != expected %q)\n", mismatch, *tenantFlag)
				failed = true
			} else {
				fmt.Printf("  tenant cross-check: OK (%s)\n", *tenantFlag)
			}
		}
	}

	// Core zero-secret verdict.
	if out.Pass {
		fmt.Println("  head match:         PASS (recomputed tip matches the attestation)")
	} else {
		failed = true
		switch {
		case !out.ChainOK:
			fmt.Println("  head match:         FAIL (chain is broken — see break above)")
		case !out.HeadSeqMatch:
			fmt.Printf("  head match:         FAIL (tip mismatch: recomputed seq %d != attested %d)\n", out.RecomputedSeq, att.HeadSeq)
		case !out.HeadHashMatch:
			fmt.Println("  head match:         FAIL (tip mismatch: recomputed head hash != attested head_row_hash)")
		default:
			fmt.Println("  head match:         FAIL")
		}
	}

	// OPTIONAL sealed-signature path (key vouches for this head). NEVER fails the
	// overall run — the zero-secret head match above is the primary result. A
	// seal-open failure (wrong/rotated key) is clearly distinguished from a tip
	// mismatch.
	runSealCheck(att)

	if failed {
		os.Exit(2)
	}
}

// runSealCheck is the optional key-backed confirmation. It only runs when both
// SQUADRON_SECRETS_KEY is set AND the attestation carries a sealed_sig. It never
// changes the process exit code — it only prints an extra line of assurance (or
// a clearly-labeled seal-open failure), keeping the zero-secret path primary.
func runSealCheck(att Attestation) {
	if os.Getenv("SQUADRON_SECRETS_KEY") == "" || att.SealedSig == "" {
		if att.SealedSig != "" {
			fmt.Println("  seal check:         skipped (SQUADRON_SECRETS_KEY not set — zero-secret result stands)")
		}
		return
	}
	blob, err := base64.StdEncoding.DecodeString(att.SealedSig)
	if err != nil {
		fmt.Printf("  seal check:         seal could not be decoded (bad base64): %v\n", err)
		return
	}
	key, err := credstore.LoadKeyFromEnv()
	if err != nil {
		fmt.Printf("  seal check:         key unavailable (%v — zero-secret result stands)\n", err)
		return
	}
	pt, err := credstore.UnsealAuditCheckpoint(key, blob)
	if err != nil {
		fmt.Println("  seal check:         seal could not be opened (wrong/rotated key) — NOT a tip mismatch")
		return
	}
	want, err := sealPayload{Tenant: att.Tenant, HeadSeq: att.HeadSeq, HeadRowHash: att.HeadRowHash}.canonical()
	if err != nil {
		fmt.Printf("  seal check:         could not build canonical seal payload: %v\n", err)
		return
	}
	if string(pt) == string(want) {
		fmt.Println("  seal check:         seal opened + verified (Squadron key vouches for this head)")
	} else {
		fmt.Println("  seal check:         seal opened but payload does NOT match the attested head (tip differs from sealed tip)")
	}
}
