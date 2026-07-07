// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package chain is the dependency-free source of truth for the audit
// tamper-evident hash chain (ADR 0027). Both application stores (sqlite,
// memory) and the offline verifier reuse RowHash + Verify so the canonical
// wire format and walk semantics can never silently diverge between them.
package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// RowHash is the canonical tamper-evident chain hash for one audit row (ADR 0027).
// Length-prefixed field concat (injection-safe: because every field is prefixed
// with its byte length, no content an attacker can smuggle into a field can
// shift the field boundaries) over the immutable content columns + per-tenant
// seq, chained with the previous row's row_hash. Timestamp is deliberately
// excluded. This is the SINGLE source of truth reused by both stores and the
// offline verifier.
//
// payload MUST be byte-identical to what is written to (and read back from) the
// payload column: the append path passes the exact string it INSERTs; the
// verify path passes the DB payload string. Taking the raw string (not the map)
// removes any chance of re-marshal drift between append and verify.
func RowHash(id, actor, eventType, targetType, targetID, action, payload, tenant string, seq int64, prevHash string) string {
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
	writeField(payload)
	writeField(tenant)
	writeField(strconv.FormatInt(seq, 10))
	fmt.Fprint(h, prevHash)
	return hex.EncodeToString(h.Sum(nil))
}

// Row is one row of a tenant's chain, ordered by Seq ascending.
type Row struct {
	ID, Actor, EventType, TargetType, TargetID, Action, Payload, Tenant string
	Seq                                                                 int64
	PrevHash                                                            string
	RowHash                                                             string // the stored hash, compared against the recompute
}

// Result is the pure verification outcome (a leaf type — the stores translate
// this into types.AuditChainVerification, adding DB-only anchoring fields).
type Result struct {
	OK            bool
	RowsVerified  int
	FirstBreakSeq int64
	Detail        string
	CoversFromSeq int64
	HeadSeq       int64
	HeadRowHash   string
}

// Verify walks rows (MUST be pre-sorted by Seq ASC) applying the chain-start
// leniency + contiguity + prev_hash link + row_hash recompute. Pure; no DB.
//
// The FIRST surviving row's prev_hash is accepted as-is — its predecessor may
// have been legitimately garbage-collected by the opt-in retention sweep, so a
// non-empty prev_hash with no visible predecessor is NOT a tamper signal;
// CoversFromSeq records that chain-start. Every subsequent row must be
// contiguous (seq == prev.seq+1; a gap is a middle deletion), must link
// (prev_hash == the prior row's row_hash), and must re-hash to its stored
// row_hash (content-edit / reorder detection).
func Verify(rows []Row) Result {
	var (
		count       int
		coversFrom  int64
		prevSeq     int64
		prevRowHash string
		headSeq     int64
		headRowHash string
	)
	for i, row := range rows {
		if i == 0 {
			coversFrom = row.Seq
			expected := RowHash(row.ID, row.Actor, row.EventType, row.TargetType, row.TargetID, row.Action, row.Payload, row.Tenant, row.Seq, row.PrevHash)
			if expected != row.RowHash {
				return Result{
					OK:            false,
					RowsVerified:  count,
					FirstBreakSeq: row.Seq,
					Detail:        fmt.Sprintf("row_hash mismatch at chain-start seq %d (row content edited)", row.Seq),
					CoversFromSeq: coversFrom,
				}
			}
		} else {
			if row.Seq != prevSeq+1 {
				return Result{
					OK:            false,
					RowsVerified:  count,
					FirstBreakSeq: row.Seq,
					Detail:        fmt.Sprintf("non-contiguous seq: expected %d, got %d (middle deletion)", prevSeq+1, row.Seq),
					CoversFromSeq: coversFrom,
				}
			}
			if row.PrevHash != prevRowHash {
				return Result{
					OK:            false,
					RowsVerified:  count,
					FirstBreakSeq: row.Seq,
					Detail:        fmt.Sprintf("prev_hash link broken at seq %d", row.Seq),
					CoversFromSeq: coversFrom,
				}
			}
			expected := RowHash(row.ID, row.Actor, row.EventType, row.TargetType, row.TargetID, row.Action, row.Payload, row.Tenant, row.Seq, row.PrevHash)
			if expected != row.RowHash {
				return Result{
					OK:            false,
					RowsVerified:  count,
					FirstBreakSeq: row.Seq,
					Detail:        fmt.Sprintf("row_hash mismatch at seq %d (row content edited or reordered)", row.Seq),
					CoversFromSeq: coversFrom,
				}
			}
		}
		count++
		prevSeq = row.Seq
		prevRowHash = row.RowHash
		headSeq = row.Seq
		headRowHash = row.RowHash
	}

	return Result{
		OK:            true,
		RowsVerified:  count,
		CoversFromSeq: coversFrom,
		HeadSeq:       headSeq,
		HeadRowHash:   headRowHash,
	}
}
