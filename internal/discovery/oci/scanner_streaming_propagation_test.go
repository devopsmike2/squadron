// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanner_streaming_propagation_test.go covers slice 2 chunk 4 of the
// Event source tier arc (v0.89.106, #744 Stream 142): OCI Streaming
// retention propagation detection. The chunk extends the slice 1
// scanner_streaming.go scanner with a single-axis threshold check
// (retentionInHours >= 24h) that drives the snapshot's
// HasPropagationConfig + PropagationNotes fields.
//
// Tests in this file fall into two layers:
//
//   - Pure helper tests for streamPreservesPropagation — fast, no
//     httptest dependency, exhaustive across the boundary cases.
//   - Scanner-level integration tests reusing the chunk 1 fakeOCIStreaming
//     mock (scanner_streaming_test.go) to assert the threshold check
//     plumbs through ScanStreams correctly.
//
// See docs/proposals/event-source-tier-slice2.md §3.4 (detection
// surface) and §11 acceptance tests 13-14.

// --- streamPreservesPropagation helper tests -------------------------

// TestStreamPreservesPropagation_RetentionExactlyThreshold_Preserved
// asserts the boundary case: a stream with retentionInHours exactly
// equal to the OCIStreamingRetentionPropagationThresholdHours
// threshold should be classified as preserved (the rule is >=, not >).
// This is the load-bearing boundary case — operators routinely set
// retention to the threshold value itself; misclassifying the
// boundary would emit a spurious recommendation on a deliberately-
// configured stream.
func TestStreamPreservesPropagation_RetentionExactlyThreshold_Preserved(t *testing.T) {
	preserved, note := streamPreservesPropagation(OCIStreamingRetentionPropagationThresholdHours)
	assert.True(t, preserved,
		"retentionInHours == threshold (%d) should classify as preserved (>= rule, not >)",
		OCIStreamingRetentionPropagationThresholdHours)
	assert.Empty(t, note, "preserved streams should emit no propagation note")
}

// TestStreamPreservesPropagation_RetentionAboveThreshold_Preserved
// asserts a stream with retentionInHours well above the threshold
// (the common production posture) classifies as preserved. The 168h
// (one-week) value is the production default for many OCI Streaming
// deployments and a useful canonical above-threshold value.
func TestStreamPreservesPropagation_RetentionAboveThreshold_Preserved(t *testing.T) {
	preserved, note := streamPreservesPropagation(168)
	assert.True(t, preserved,
		"retentionInHours far above threshold should classify as preserved")
	assert.Empty(t, note, "preserved streams should emit no propagation note")
}

// TestStreamPreservesPropagation_RetentionBelowThreshold_BrokenNote
// (slice 2 §11 acceptance test 14): a stream with retentionInHours
// below the threshold should classify as broken and emit a
// human-readable note that names the actual retention value and the
// threshold so the proposer's chunk-5 recommendation reasoning has
// both values to surface.
func TestStreamPreservesPropagation_RetentionBelowThreshold_BrokenNote(t *testing.T) {
	preserved, note := streamPreservesPropagation(12)
	assert.False(t, preserved,
		"retentionInHours below threshold should classify as broken")
	assert.NotEmpty(t, note, "broken streams should emit a propagation note")
	assert.Contains(t, note, "retentionInHours=12",
		"note should name the actual retention value")
	assert.Contains(t, note, "threshold 24",
		"note should name the threshold value")
	assert.Contains(t, strings.ToLower(note), "kafka headers",
		"note should explain the propagation impact")
}

// TestStreamPreservesPropagation_RetentionZero_BrokenNote covers the
// defensive default: a zero retentionInHours (either a deliberately-
// tiny retention or a missing-field API response on a legacy stream)
// is classified as broken. The exclusion table absorbs false positives
// for tenancies whose response shape omits the field — see
// streamPreservesPropagation godoc.
func TestStreamPreservesPropagation_RetentionZero_BrokenNote(t *testing.T) {
	preserved, note := streamPreservesPropagation(0)
	assert.False(t, preserved,
		"zero retentionInHours should classify as broken (defensive default)")
	assert.NotEmpty(t, note,
		"zero retention should emit a propagation note so the operator sees the gap")
	assert.Contains(t, note, "retentionInHours=0",
		"note should name the zero retention value so the operator can disambiguate "+
			"deliberate-tiny-retention from missing-field cases")
}

// --- Scanner-level integration tests ---------------------------------

// TestStreamingScanner_StreamWith24HourRetention_HasPropagationConfig
// (slice 2 §11 acceptance test 13): a stream with retentionInHours
// exactly at the threshold should have HasPropagationConfig=true and
// no propagation notes after a full ScanStreams pass.
func TestStreamingScanner_StreamWith24HourRetention_HasPropagationConfig(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		{
			ID:               "ocid1.stream.oc1...preserved-stream",
			Name:             "preserved-stream",
			CompartmentID:    "ocid1.tenancy.oc1..aaa",
			LifecycleState:   "ACTIVE",
			RetentionInHours: 24,
		},
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.True(t, snap.HasPropagationConfig,
		"retentionInHours == threshold (24) should flip HasPropagationConfig=true")
	assert.Empty(t, snap.PropagationNotes,
		"a preserved stream should carry no propagation notes")
	// The Detail bag should denormalize the retention input for the
	// Inventory tab side panel.
	require.NotNil(t, snap.Detail)
	assert.Equal(t, 24, snap.Detail["retention_in_hours"],
		"Detail should carry the raw retention input for the Inventory tab")
}

// TestStreamingScanner_StreamWith12HourRetention_NoPropagationConfig
// (slice 2 §11 acceptance test 14): a stream with retentionInHours
// below the threshold should have HasPropagationConfig=false and a
// propagation note recording the gap. Mirrors the EventBridge chunk's
// per-rule broken-target test (see eventbridge_propagation_test.go
// in chunk 1 of slice 2).
func TestStreamingScanner_StreamWith12HourRetention_NoPropagationConfig(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		{
			ID:               "ocid1.stream.oc1...short-retention",
			Name:             "short-retention",
			CompartmentID:    "ocid1.tenancy.oc1..aaa",
			LifecycleState:   "ACTIVE",
			RetentionInHours: 12,
		},
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.False(t, snap.HasPropagationConfig,
		"retentionInHours below threshold (12 < 24) should leave HasPropagationConfig=false")
	require.Len(t, snap.PropagationNotes, 1,
		"a broken stream should carry exactly one propagation note (the per-stream rule)")
	assert.Contains(t, snap.PropagationNotes[0], "retentionInHours=12",
		"the note should name the actual retention value")
	assert.Contains(t, snap.PropagationNotes[0], "threshold 24",
		"the note should name the threshold value")
}

// TestStreamingScanner_MultipleStreamsMixedRetention_EachEvaluatedIndependently
// covers the per-stream evaluation contract: each stream's propagation
// axis is computed in isolation from every other stream's axis (unlike
// the EventBridge per-bus axis which ANDs across every rule on the
// bus). Streams are independent OCI resources; the slice 2 §3.4 design
// rule explicitly scopes the threshold to the stream level.
func TestStreamingScanner_MultipleStreamsMixedRetention_EachEvaluatedIndependently(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		{
			ID:               "ocid1.stream.oc1...preserved-a",
			Name:             "preserved-a",
			CompartmentID:    "ocid1.tenancy.oc1..aaa",
			LifecycleState:   "ACTIVE",
			RetentionInHours: 168,
		},
		{
			ID:               "ocid1.stream.oc1...broken-b",
			Name:             "broken-b",
			CompartmentID:    "ocid1.tenancy.oc1..aaa",
			LifecycleState:   "ACTIVE",
			RetentionInHours: 6,
		},
		{
			ID:               "ocid1.stream.oc1...preserved-c",
			Name:             "preserved-c",
			CompartmentID:    "ocid1.tenancy.oc1..aaa",
			LifecycleState:   "ACTIVE",
			RetentionInHours: 24,
		},
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 3, "three streams should produce three snapshots")

	byName := map[string]bool{}
	notesByName := map[string][]string{}
	for _, snap := range snaps {
		byName[snap.ResourceName] = snap.HasPropagationConfig
		notesByName[snap.ResourceName] = snap.PropagationNotes
	}

	assert.True(t, byName["preserved-a"],
		"preserved-a (168h) should have HasPropagationConfig=true")
	assert.Empty(t, notesByName["preserved-a"],
		"preserved-a should carry no propagation notes")

	assert.False(t, byName["broken-b"],
		"broken-b (6h < 24h) should have HasPropagationConfig=false "+
			"independent of the other streams' axes")
	require.Len(t, notesByName["broken-b"], 1,
		"broken-b should carry exactly one propagation note")
	assert.Contains(t, notesByName["broken-b"][0], "retentionInHours=6")

	assert.True(t, byName["preserved-c"],
		"preserved-c (24h == threshold) should have HasPropagationConfig=true")
	assert.Empty(t, notesByName["preserved-c"],
		"preserved-c should carry no propagation notes")
}

// TestStreamingScanner_RetentionFieldMissing_DefaultsBrokenWithNote
// covers the defensive case: when the OCI Streaming API response shape
// omits the retentionInHours field entirely (a legacy stream, a
// hypothetical future API version that drops the field, etc.), Go's
// JSON unmarshalling leaves the int field at its zero value. The slice
// 2 rule treats zero as below-threshold and surfaces a propagation
// note so the operator sees the gap rather than the scanner silently
// classifying the stream as preserved.
//
// See streamPreservesPropagation godoc for the defensive-default
// rationale and the false-positive risk note in design doc §12.
func TestStreamingScanner_RetentionFieldMissing_DefaultsBrokenWithNote(t *testing.T) {
	fake := newFakeOCIStreaming()
	// Construct a stream without setting RetentionInHours — the Go
	// zero value (0) emulates the wire shape of a response that
	// omits the field. The JSON mock encodes the struct as-is, and
	// the scanner's listStreamsPage unmarshals it back symmetrically;
	// the round-trip lands at retentionInHours=0 in the projection
	// step.
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		{
			ID:             "ocid1.stream.oc1...legacy-stream",
			Name:           "legacy-stream",
			CompartmentID:  "ocid1.tenancy.oc1..aaa",
			LifecycleState: "ACTIVE",
			// RetentionInHours intentionally unset -> zero value.
		},
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	snap := snaps[0]
	assert.False(t, snap.HasPropagationConfig,
		"a missing retentionInHours field should leave HasPropagationConfig=false "+
			"(defensive default — Squadron cannot prove the threshold is met)")
	require.Len(t, snap.PropagationNotes, 1,
		"a missing-field stream should carry exactly one propagation note")
	assert.Contains(t, snap.PropagationNotes[0], "retentionInHours=0",
		"the note should name the zero retention value so the operator can "+
			"disambiguate deliberate-tiny-retention from missing-field cases")
}

// TestStreamingScanner_PropagationIndependentOfLogAxis covers the
// composition contract: the slice 2 propagation axis is computed from
// the list-call response shape alone and does NOT depend on the slice
// 1 OCI Logging proxy axis. A stream with no log group can still be
// propagation-preserved if its retention is above threshold; a stream
// with a log group can still be propagation-broken if its retention
// is below. Tests both halves of the matrix.
func TestStreamingScanner_PropagationIndependentOfLogAxis(t *testing.T) {
	fake := newFakeOCIStreaming()
	fake.StreamsByCompartment["ocid1.tenancy.oc1..aaa"] = []ociStream{
		{
			ID:               "ocid1.stream.oc1...nolog-preserved",
			Name:             "nolog-preserved",
			CompartmentID:    "ocid1.tenancy.oc1..aaa",
			LifecycleState:   "ACTIVE",
			RetentionInHours: 48,
		},
		{
			ID:               "ocid1.stream.oc1...log-broken",
			Name:             "log-broken",
			CompartmentID:    "ocid1.tenancy.oc1..aaa",
			LifecycleState:   "ACTIVE",
			RetentionInHours: 6,
		},
	}
	// Only the log-broken stream has a matching Logging entry.
	fake.LogsByStream["ocid1.stream.oc1...log-broken"] = []ociLogResource{
		makeOCILogResource("ocid1.stream.oc1...log-broken"),
	}

	s := newStreamingScannerWithFake(t, fake, "us-phoenix-1")
	snaps, err := s.ScanStreams(context.Background(), scopeRoot())
	require.NoError(t, err)
	require.Len(t, snaps, 2)

	byName := map[string]scannerSnapshotProbe{}
	for _, snap := range snaps {
		byName[snap.ResourceName] = scannerSnapshotProbe{
			HasLogAxis:           snap.HasLogAxis,
			HasPropagationConfig: snap.HasPropagationConfig,
		}
	}

	// nolog-preserved: no log group but retention >= threshold.
	assert.False(t, byName["nolog-preserved"].HasLogAxis,
		"nolog-preserved has no log entry -> HasLogAxis=false")
	assert.True(t, byName["nolog-preserved"].HasPropagationConfig,
		"nolog-preserved has retention=48 -> HasPropagationConfig=true "+
			"(propagation axis is independent of log axis)")

	// log-broken: log group present but retention below threshold.
	assert.True(t, byName["log-broken"].HasLogAxis,
		"log-broken has a matching log entry -> HasLogAxis=true")
	assert.False(t, byName["log-broken"].HasPropagationConfig,
		"log-broken has retention=6 -> HasPropagationConfig=false "+
			"(log axis presence does not imply propagation preserved)")
}

// scannerSnapshotProbe is a small per-test struct used to thread two
// boolean axes through a map under a stream name key. Kept local to
// this file so the chunk 4 test surface does not bleed into the chunk
// 1 file's helpers.
type scannerSnapshotProbe struct {
	HasLogAxis           bool
	HasPropagationConfig bool
}
