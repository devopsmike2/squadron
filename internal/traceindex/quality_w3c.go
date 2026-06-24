// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

// W3C trace context parsing helpers — see
// docs/proposals/span-quality-slice2.md §3.1 (malformed traceparent
// detection) and §3.2 (missing-on-child detection).
//
// Kept in a sibling file (rather than inline in quality.go) for the
// same reason quality_rules.go is split: the chunk-2 proposer prompt
// and the chunk-3 operator runbook both reference the canonical W3C
// format here, and a single edit ripples to the receiver hot path
// without disturbing unrelated quality logic.
//
// All helpers are allocation-free on the hot path (no string slicing
// that escapes the stack, no map allocation). The benchmark in
// quality_bench_test.go pins the per-call cost under the §5 budget.

// TraceparentExpectedLen is the canonical W3C trace context header
// length: version(2) + "-" + trace_id(32) + "-" + parent_id(16) +
// "-" + trace_flags(2) = 55 chars.
const TraceparentExpectedLen = 55

// TraceparentVersionSupported is the only W3C version slice 2 accepts.
// Future versions ("01", "ff", etc.) trigger malformed detection per
// §3.1. Slice 3 may relax for forward-compat once the W3C working
// group ships version 01 — until then, accepting "01" silently would
// mask real version-mismatch bugs in upstream SDKs (see §1 case B).
const TraceparentVersionSupported = "00"

// TraceparentAttrName is the canonical OTel attribute key for the
// W3C trace context header value. Most OTel SDKs attach this when
// they extract context on inbound RPC/HTTP.
const TraceparentAttrName = "traceparent"

// TraceparentHTTPHeaderAttrName is the alternative attribute key
// used by some SDKs that preserve the raw HTTP header (rather than
// re-emitting under the OTel semantic-convention key). Slice 2 checks
// both forms; lookupTraceparent picks the canonical key first.
const TraceparentHTTPHeaderAttrName = "http.request.header.traceparent"

// isWellFormedTraceparent reports whether the given value is a
// well-formed W3C trace context value per the slice 2 design doc
// §3.1. The check is strict on version ("00" only), hex casing
// (lowercase only — the W3C spec mandates it), and the all-zero
// prohibitions on trace_id and parent_id.
//
// Hot-path budget: ~50ns per call (see BenchmarkQuality_Observe_*
// in quality_bench_test.go). Avoids allocation by indexing the
// underlying byte array directly rather than slicing into temporary
// substrings.
//
// PII posture: this function inspects format only. The value is
// never logged, never returned, never reaches audit. Slice 1's
// threat-model §11 carries through.
func isWellFormedTraceparent(value string) bool {
	if len(value) != TraceparentExpectedLen {
		return false
	}
	// Hyphen positions: 2, 35, 52 per the W3C grammar.
	if value[2] != '-' || value[35] != '-' || value[52] != '-' {
		return false
	}
	// Version segment must be exactly "00". Future versions ("01",
	// "ff", etc.) are rejected per slice 2 §3.1; slice 3 may relax
	// once W3C ships v01.
	if value[0] != '0' || value[1] != '0' {
		return false
	}
	// trace_id segment: positions 3-34 inclusive (32 chars). Must be
	// lowercase hex AND not all zeros.
	if !isHexLowerOrDigit(value[3:35]) {
		return false
	}
	if isAllZerosHex(value[3:35]) {
		return false
	}
	// parent_id segment: positions 36-51 inclusive (16 chars). Same
	// hex + non-zero requirement.
	if !isHexLowerOrDigit(value[36:52]) {
		return false
	}
	if isAllZerosHex(value[36:52]) {
		return false
	}
	// trace_flags segment: positions 53-54 (2 chars). Hex-only —
	// the all-zero case is permitted here (flags=00 means "not
	// sampled" which is a perfectly legal W3C value).
	if !isHexLowerOrDigit(value[53:55]) {
		return false
	}
	return true
}

// isHexLowerOrDigit reports whether s contains only the lowercase
// hex characters 0-9 and a-f. The W3C spec mandates lowercase;
// uppercase A-F is rejected as malformed per §3.1.
//
// Loop body kept branch-light: a single conjunction per character
// keeps the function inlinable and the per-character cost in the
// low-ns range on amd64/arm64.
func isHexLowerOrDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// isAllZerosHex reports whether s is all '0' characters. The W3C
// spec prohibits all-zero trace_id and parent_id (a span carrying
// either is by definition malformed because the all-zero value is
// reserved as a sentinel for "no context").
func isAllZerosHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// lookupTraceparent returns the traceparent attribute value from
// the span's attribute map, checking both common attribute names.
// Returns "" if neither is present.
//
// Precedence: TraceparentAttrName ("traceparent") wins over
// TraceparentHTTPHeaderAttrName. Rationale — when an SDK attaches
// both forms (rare but observed), the OTel semantic-convention key
// is the authoritative one; the http.request.header.* form is a
// raw-header copy that may carry stale data if the SDK rewrote the
// outbound value mid-flight.
//
// Hot-path budget: 1-2 map lookups, no allocation.
func lookupTraceparent(attrs map[string]string) string {
	if v := attrs[TraceparentAttrName]; v != "" {
		return v
	}
	if v := attrs[TraceparentHTTPHeaderAttrName]; v != "" {
		return v
	}
	return ""
}
