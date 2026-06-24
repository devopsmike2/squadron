// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Span quality slice 2 §11 acceptance tests for the W3C trace context
// helpers (isWellFormedTraceparent + lookupTraceparent). Tests in this
// file pin format-edge-case behavior; the per-counter integration
// tests (acceptance #8-14) live in quality_test.go where they have
// access to the fakeClock + observeWith helpers.

// canonicalTraceparent is the §11 test 1 fixture: a known-good W3C
// trace context value with non-zero trace_id, non-zero parent_id,
// and the sampled flag set. Many tests below mutate one position of
// this string to exercise a single failure mode.
const canonicalTraceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"

// --- §11 test 1: canonical example ----------------------------------

func TestIsWellFormedTraceparent_CanonicalExample(t *testing.T) {
	assert.True(t, isWellFormedTraceparent(canonicalTraceparent))
}

// --- §11 test 2: wrong length ---------------------------------------

func TestIsWellFormedTraceparent_WrongLength_TooShort(t *testing.T) {
	assert.False(t, isWellFormedTraceparent("00-too-short"))
}

func TestIsWellFormedTraceparent_WrongLength_TooLong(t *testing.T) {
	// One extra trailing char past the canonical 55.
	assert.False(t, isWellFormedTraceparent(canonicalTraceparent+"x"))
}

func TestIsWellFormedTraceparent_Empty_Rejected(t *testing.T) {
	assert.False(t, isWellFormedTraceparent(""))
}

// --- §11 test 3: non-hex character in trace_id ----------------------

func TestIsWellFormedTraceparent_NonHexInTraceID(t *testing.T) {
	// Replace last char of trace_id (position 34) with 'g' — not hex.
	bad := "00-0123456789abcdef0123456789abcdeg-0123456789abcdef-01"
	assert.False(t, isWellFormedTraceparent(bad))
}

// W3C mandates lowercase hex; uppercase rejected as malformed.
func TestIsWellFormedTraceparent_UppercaseHex_Rejected(t *testing.T) {
	// Uppercase 'A' in the trace_id segment.
	bad := "00-0123456789ABCDEF0123456789abcdef-0123456789abcdef-01"
	assert.False(t, isWellFormedTraceparent(bad))
}

// --- §11 test 4: all-zero trace_id ----------------------------------

func TestIsWellFormedTraceparent_AllZeroTraceID_Rejected(t *testing.T) {
	bad := "00-00000000000000000000000000000000-0123456789abcdef-01"
	assert.False(t, isWellFormedTraceparent(bad))
}

// --- §11 test 5: all-zero parent_id ---------------------------------

func TestIsWellFormedTraceparent_AllZeroParentID_Rejected(t *testing.T) {
	bad := "00-0123456789abcdef0123456789abcdef-0000000000000000-01"
	assert.False(t, isWellFormedTraceparent(bad))
}

// --- §11 test 6: version "ff" (future reserved) ---------------------

func TestIsWellFormedTraceparent_VersionFF_Rejected(t *testing.T) {
	bad := "ff-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	assert.False(t, isWellFormedTraceparent(bad))
}

// --- §11 test 7: version "01" (next-version reserved) ---------------

func TestIsWellFormedTraceparent_VersionOne_Rejected(t *testing.T) {
	bad := "01-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	assert.False(t, isWellFormedTraceparent(bad))
}

// --- defensive: hyphen at wrong position -----------------------------

func TestIsWellFormedTraceparent_MissingHyphen(t *testing.T) {
	// Replace hyphen at position 2 with 'a' — same length so we
	// reach the hyphen-position check rather than the length check.
	bad := "00a0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	assert.False(t, isWellFormedTraceparent(bad))
}

func TestIsWellFormedTraceparent_HyphenAtWrongPosition(t *testing.T) {
	// Move the middle hyphen one position left into the trace_id
	// segment by swapping positions 34 and 35. Still 55 chars.
	bad := "00-0123456789abcdef0123456789abcde-f0123456789abcdef-01"
	assert.False(t, isWellFormedTraceparent(bad))
}

// --- defensive: non-hex in trace_flags ------------------------------

func TestIsWellFormedTraceparent_NonHexInTraceFlags(t *testing.T) {
	// Trailing 'z' in flags segment.
	bad := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-0z"
	assert.False(t, isWellFormedTraceparent(bad))
}

// trace_flags all-zero ("00") IS valid per W3C — flags=00 means
// "not sampled" which is a legal observed state.
func TestIsWellFormedTraceparent_AllZeroFlags_Accepted(t *testing.T) {
	good := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-00"
	assert.True(t, isWellFormedTraceparent(good))
}

// --- isHexLowerOrDigit unit coverage --------------------------------

func TestIsHexLowerOrDigit_AllDigits(t *testing.T) {
	assert.True(t, isHexLowerOrDigit("0123456789"))
}

func TestIsHexLowerOrDigit_AllLowercase(t *testing.T) {
	assert.True(t, isHexLowerOrDigit("abcdef"))
}

func TestIsHexLowerOrDigit_UppercaseRejected(t *testing.T) {
	assert.False(t, isHexLowerOrDigit("ABCDEF"))
}

func TestIsHexLowerOrDigit_EmptyAccepted(t *testing.T) {
	// Vacuously true — zero-length input has no offending chars.
	assert.True(t, isHexLowerOrDigit(""))
}

// --- isAllZerosHex unit coverage ------------------------------------

func TestIsAllZerosHex_AllZeros_True(t *testing.T) {
	assert.True(t, isAllZerosHex("00000000000000000000000000000000"))
}

func TestIsAllZerosHex_OneNonZero_False(t *testing.T) {
	assert.False(t, isAllZerosHex("00000000000000000000000000000001"))
}

// --- lookupTraceparent ----------------------------------------------

func TestLookupTraceparent_DirectAttribute_Returns(t *testing.T) {
	attrs := map[string]string{
		TraceparentAttrName: canonicalTraceparent,
	}
	assert.Equal(t, canonicalTraceparent, lookupTraceparent(attrs))
}

func TestLookupTraceparent_HTTPHeaderAttribute_Returns(t *testing.T) {
	attrs := map[string]string{
		TraceparentHTTPHeaderAttrName: canonicalTraceparent,
	}
	assert.Equal(t, canonicalTraceparent, lookupTraceparent(attrs))
}

func TestLookupTraceparent_NeitherPresent_ReturnsEmpty(t *testing.T) {
	attrs := map[string]string{
		"service.name": "checkout",
	}
	assert.Equal(t, "", lookupTraceparent(attrs))
}

func TestLookupTraceparent_NilMap_ReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", lookupTraceparent(nil))
}

func TestLookupTraceparent_DirectTakesPrecedenceOverHTTP(t *testing.T) {
	// When both forms are present, the OTel semantic-convention key
	// wins — the http.request.header.* form is treated as a raw-
	// header fallback only.
	direct := canonicalTraceparent
	header := "00-fedcba9876543210fedcba9876543210-fedcba9876543210-00"
	attrs := map[string]string{
		TraceparentAttrName:           direct,
		TraceparentHTTPHeaderAttrName: header,
	}
	assert.Equal(t, direct, lookupTraceparent(attrs))
}

func TestLookupTraceparent_EmptyDirectFallsBackToHTTP(t *testing.T) {
	// An empty TraceparentAttrName value should fall through to the
	// HTTP-header form rather than returning "" — empty string is
	// indistinguishable from "key not present" in a map[string]string.
	attrs := map[string]string{
		TraceparentAttrName:           "",
		TraceparentHTTPHeaderAttrName: canonicalTraceparent,
	}
	assert.Equal(t, canonicalTraceparent, lookupTraceparent(attrs))
}
