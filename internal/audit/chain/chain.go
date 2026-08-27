// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package chain is the dependency-free source of truth for the audit
// tamper-evident hash chain (ADR 0027). Both application stores (sqlite,
// memory) and the offline verifier reuse RowHash + Verify so the canonical
// wire format and walk semantics can never silently diverge between them.
package chain

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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

	// ADR 0044 keyed-chain fields. Zero values describe a legacy (ADR 0027)
	// unkeyed row so every existing Row constructor keeps producing a legacy
	// row unchanged.
	//
	// Timestamp is the CANONICAL timestamp string (see CanonicalTimestamp) that
	// was folded into the keyed HMAC on append; it is empty and unused for
	// legacy rows (ADR 0027 deliberately excluded the timestamp). Algo names the
	// scheme this row was sealed under: "" = legacy unkeyed SHA-256,
	// AlgoHMACSHA256 = keyed HMAC-SHA256.
	Timestamp string
	Algo      string
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

// ============================================================================
// ADR 0044 — keyed tamper-evident chain (HMAC-SHA256 + high-water-mark head +
// timestamp coverage). This hardens the ADR 0027 unkeyed chain against a DB
// writer who can recompute the public SHA-256 hashes. It is layered ON TOP of
// the legacy scheme above: RowHash + Verify are UNCHANGED so pre-cutover rows
// keep verifying under the original algorithm and never get re-hashed (which
// would itself be indistinguishable from tampering).
// ============================================================================

// EnvVarAuditKey is the environment variable the control plane reads the audit
// HMAC key from. Per ADR 0044 (Q2) this is a DEDICATED key, distinct from
// SQUADRON_SECRETS_KEY — an app/DB compromise that leaks the data-encryption key
// must not also grant the ability to forge the audit trail. The value is
// base64-encoded raw bytes; the decoded length must be at least 32 (256-bit).
const EnvVarAuditKey = "SQUADRON_AUDIT_HMAC_KEY"

// auditKeyMinLen is the minimum decoded length of the audit HMAC key. HMAC keys
// shorter than the hash block are permitted by the construction but weaken it;
// we require a full 256-bit key to match the SHA-256 output.
const auditKeyMinLen = 32

// Algorithm markers persisted in audit_events.chain_algo (ADR 0044). A per-row
// marker (rather than a single global cutover seq) lets an upgraded chain carry
// a legacy prefix and a keyed suffix and self-describe which scheme each row was
// sealed under — the downgrade guard in VerifySealed enforces that the
// transition is one-way (legacy → keyed, never back).
const (
	// AlgoLegacySHA256 is the ADR 0027 unkeyed SHA-256 link hash (timestamp
	// excluded). Stored as NULL / "" for rows written before the keyed cutover
	// or while no audit key is configured.
	AlgoLegacySHA256 = ""
	// AlgoHMACSHA256 is the ADR 0044 keyed HMAC-SHA256 link hash (timestamp
	// included). Written for every append once an audit key is provided.
	AlgoHMACSHA256 = "hmac-sha256-v2"
)

// ErrAuditKeyMalformed is returned when SQUADRON_AUDIT_HMAC_KEY is set but does
// not base64-decode to at least 32 bytes. A typo'd key must fail loud rather
// than silently degrade the audit trail to unkeyed.
var ErrAuditKeyMalformed = errors.New(
	"chain: " + EnvVarAuditKey + " must be base64-encoded raw bytes, at least 32 (AES-256/HMAC-SHA256)",
)

// Key holds the audit HMAC secret. It is safe for concurrent use — hmac.New is
// called per operation, so no mutable state is shared.
type Key struct {
	raw []byte
}

// NewKey builds a Key from raw bytes (>= 32). Callers that already hold the raw
// key (tests, an external secret manager fetch) use this directly.
func NewKey(raw []byte) (*Key, error) {
	if len(raw) < auditKeyMinLen {
		return nil, fmt.Errorf("%w: got %d bytes, need >= %d", ErrAuditKeyMalformed, len(raw), auditKeyMinLen)
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return &Key{raw: cp}, nil
}

// LoadKeyFromEnv reads SQUADRON_AUDIT_HMAC_KEY and returns the resolved key.
//
//	present=false, err=nil  -> the env var is unset/empty. The caller runs in the
//	                           DEGRADED warn-window (ADR 0044/0045): emit the loud
//	                           startup warning and continue in unkeyed-legacy mode.
//	present=true,  err!=nil -> the env var is set but malformed. This is FATAL —
//	                           a typo'd key must not silently orphan the seal.
//	present=true,  err=nil  -> a valid key; the chain is sealed with HMAC.
func LoadKeyFromEnv() (key *Key, present bool, err error) {
	raw := strings.TrimSpace(os.Getenv(EnvVarAuditKey))
	if raw == "" {
		return nil, false, nil
	}
	decoded, derr := base64.StdEncoding.DecodeString(raw)
	if derr != nil {
		return nil, true, fmt.Errorf("%w: not base64: %v", ErrAuditKeyMalformed, derr)
	}
	k, kerr := NewKey(decoded)
	return k, true, kerr
}

// CanonicalTimestamp renders a timestamp as the fixed-width UTC string folded
// into the keyed hash. It is:
//   - UTC-normalized (so the driver's read-back zone can't shift it),
//   - truncated to MICROSECONDS (Postgres timestamptz keeps micros, SQLite keeps
//     nanos — truncating to the coarser precision makes the append-time value and
//     the read-back value byte-identical on BOTH backends), and
//   - fixed-width with a zero-padded 6-digit fraction and a literal trailing "Z"
//     so lexical string comparison is equivalent to chronological order (which
//     the monotonicity check relies on).
func CanonicalTimestamp(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format("2006-01-02T15:04:05.000000") + "Z"
}

// RowHashV2 is the ADR 0044 keyed link hash: HMAC-SHA256 (keyed by the
// server-held secret) over the SAME injection-safe length-prefixed field concat
// as RowHash, PLUS the canonical timestamp (closing the ADR 0027 backdating
// gap), chained with the previous row's row_hash. A DB writer who lacks the key
// cannot recompute any row_hash, so in-place edits, reorders, and forged
// appends are all detectable — the property the unkeyed chain never had.
//
// timestamp MUST be CanonicalTimestamp(row.Timestamp) — the exact string the
// verify path reconstructs from the stored column.
func RowHashV2(key *Key, id, actor, eventType, targetType, targetID, action, payload, tenant, timestamp string, seq int64, prevHash string) string {
	h := hmac.New(sha256.New, key.raw)
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
	writeField(timestamp) // ADR 0044 — timestamp coverage (backdating detection)
	writeField(strconv.FormatInt(seq, 10))
	fmt.Fprint(h, prevHash)
	return hex.EncodeToString(h.Sum(nil))
}

// computeExpected recomputes a row's link hash under the scheme the row declares
// (Algo). Legacy rows use the unkeyed RowHash; keyed rows use RowHashV2. key may
// be nil only when every row is legacy.
func computeExpected(key *Key, r Row) string {
	if r.Algo == AlgoHMACSHA256 {
		return RowHashV2(key, r.ID, r.Actor, r.EventType, r.TargetType, r.TargetID, r.Action, r.Payload, r.Tenant, r.Timestamp, r.Seq, r.PrevHash)
	}
	return RowHash(r.ID, r.Actor, r.EventType, r.TargetType, r.TargetID, r.Action, r.Payload, r.Tenant, r.Seq, r.PrevHash)
}

// Head is the persisted high-water-mark for a tenant's chain (ADR 0044): the
// latest sealed {seq, row_hash}, authenticated by MAC so a DB writer without the
// key cannot forge or lower it. VerifySealed uses it to detect TAIL truncation —
// the case the ADR 0027 self-verify structurally could not catch (deleting the
// newest rows leaves a shorter-but-contiguous chain that still passes).
type Head struct {
	Tenant      string
	HeadSeq     int64
	HeadRowHash string
	MAC         string // hex HMAC over {domain, tenant, seq, row_hash}
}

// HeadMAC authenticates a high-water-mark. The "audit_chain_head" domain tag
// separates it from row hashes so a row_hash can never be replayed as a HWM MAC.
func (k *Key) HeadMAC(tenant string, seq int64, rowHash string) string {
	h := hmac.New(sha256.New, k.raw)
	writeField := func(s string) {
		fmt.Fprintf(h, "%d:", len(s))
		fmt.Fprint(h, s)
	}
	writeField("audit_chain_head")
	writeField(tenant)
	writeField(strconv.FormatInt(seq, 10))
	writeField(rowHash)
	return hex.EncodeToString(h.Sum(nil))
}

// verifyHeadMAC is a constant-time check of a stored HWM's MAC.
func (k *Key) verifyHeadMAC(h Head) bool {
	want := k.HeadMAC(h.Tenant, h.HeadSeq, h.HeadRowHash)
	return subtle.ConstantTimeCompare([]byte(want), []byte(h.MAC)) == 1
}

// VerifySealed is the ADR 0044 keyed verification. It performs the full ADR 0027
// walk (chain-start leniency + contiguity + prev_hash link + row_hash recompute)
// and adds, end-to-end:
//
//   - Per-row scheme dispatch: legacy rows recompute under unkeyed SHA-256, keyed
//     rows under HMAC-SHA256. A DB writer without the key cannot forge a keyed
//     row_hash, so edits/reorders/forged appends fail here.
//   - Downgrade guard: once the chain is keyed it may NEVER revert to the
//     forgeable legacy scheme (a keyed row followed by a legacy row is an attack).
//     The legacy prefix is itself pinned by the first keyed row, whose HMAC binds
//     prev_hash = the last legacy row_hash — an attacker can't rewrite legacy
//     history without breaking that keyed link they cannot recompute.
//   - Timestamp monotonicity (backdating): adjacent keyed rows must have
//     non-decreasing canonical timestamps.
//   - High-water-mark (tail truncation): the live head must be >= the recorded,
//     MAC-authenticated HWM, and the row at the HWM seq must still hash to the
//     recorded head hash.
//
// key may be nil only for a pure-legacy chain (degraded/warn-window). A keyed row
// or a present HWM with a nil key fails closed. head may be nil (no HWM recorded
// yet — truncation cannot be asserted; the walk still runs).
func VerifySealed(rows []Row, key *Key, head *Head) Result {
	var (
		count       int
		coversFrom  int64
		prevSeq     int64
		prevRowHash string
		prevTS      string
		sawKeyed    bool
		headSeq     int64
		headRowHash string
	)
	fail := func(seq int64, detail string) Result {
		return Result{OK: false, RowsVerified: count, FirstBreakSeq: seq, Detail: detail, CoversFromSeq: coversFrom}
	}
	for i, row := range rows {
		keyed := row.Algo == AlgoHMACSHA256
		if sawKeyed && !keyed {
			return fail(row.Seq, fmt.Sprintf("downgrade attack at seq %d: reverts to unkeyed legacy scheme after a keyed entry", row.Seq))
		}
		if keyed && key == nil {
			return fail(row.Seq, fmt.Sprintf("seq %d is HMAC-keyed but no audit key is configured to verify it", row.Seq))
		}
		expected := computeExpected(key, row)
		if i == 0 {
			coversFrom = row.Seq
			if expected != row.RowHash {
				return fail(row.Seq, fmt.Sprintf("row_hash mismatch at chain-start seq %d (row content edited or key mismatch)", row.Seq))
			}
		} else {
			if row.Seq != prevSeq+1 {
				return fail(row.Seq, fmt.Sprintf("non-contiguous seq: expected %d, got %d (middle deletion)", prevSeq+1, row.Seq))
			}
			if row.PrevHash != prevRowHash {
				return fail(row.Seq, fmt.Sprintf("prev_hash link broken at seq %d", row.Seq))
			}
			if expected != row.RowHash {
				return fail(row.Seq, fmt.Sprintf("row_hash mismatch at seq %d (row content edited or reordered)", row.Seq))
			}
		}
		// Timestamp monotonicity — only across adjacent KEYED rows (legacy rows
		// don't cover the timestamp, so it is neither available nor enforceable
		// for them; this also never fires across the legacy→keyed boundary).
		if keyed && prevTS != "" && row.Timestamp != "" && row.Timestamp < prevTS {
			return fail(row.Seq, fmt.Sprintf("timestamp regression at seq %d (%s < %s) — backdating", row.Seq, row.Timestamp, prevTS))
		}
		count++
		prevSeq = row.Seq
		prevRowHash = row.RowHash
		if keyed {
			sawKeyed = true
			prevTS = row.Timestamp
		} else {
			prevTS = ""
		}
		headSeq = row.Seq
		headRowHash = row.RowHash
	}

	// High-water-mark: the recorded head pins the tail. A live head below the
	// recorded HWM means the newest rows were deleted (tail truncation) — the
	// exact gap the unkeyed self-verify could not close.
	if head != nil {
		if key == nil {
			return fail(head.HeadSeq, "high-water-mark present but no audit key configured to verify it")
		}
		if !key.verifyHeadMAC(*head) {
			return fail(head.HeadSeq, "high-water-mark MAC invalid (HWM forged or wrong key)")
		}
		if headSeq < head.HeadSeq {
			return fail(head.HeadSeq, fmt.Sprintf("tail truncation: live head seq %d is below the recorded high-water-mark seq %d", headSeq, head.HeadSeq))
		}
		if headSeq == head.HeadSeq && headRowHash != head.HeadRowHash {
			return fail(head.HeadSeq, fmt.Sprintf("head row_hash mismatch at high-water-mark seq %d", head.HeadSeq))
		}
	}

	return Result{
		OK:            true,
		RowsVerified:  count,
		CoversFromSeq: coversFrom,
		HeadSeq:       headSeq,
		HeadRowHash:   headRowHash,
	}
}
