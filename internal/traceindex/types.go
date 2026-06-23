// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package traceindex

import "time"

// MatchConfidence labels how reliable the resource_key projection
// was. "strong" means the index keyed by cloud.resource_id (tier 1)
// or by host.id / k8s.cluster.name / db.system+db.name combined
// with cloud.account.id (tiers 2-4); "weak" means the index keyed
// by host.name or service.name alone (tiers 5-6).
//
// The Discovery dashboard surfaces this distinction so an operator
// can hover on a weak-confidence match for the explanation —
// "Squadron matched this row by host.name only; if the scanner sees
// a different host.id with the same host.name, the match flips."
type MatchConfidence string

// Slice 1 keeps the confidence binary (strong vs weak). Design doc
// §13 Q3 names a future numeric confidence score as a slice-2
// candidate.
const (
	MatchConfidenceStrong MatchConfidence = "strong"
	MatchConfidenceWeak   MatchConfidence = "weak"
)

// ResourceObservation is the per-batch input the OTLP receiver
// hands the Index for one ResourceSpan. The receiver's hot path
// calls Index.Observe once per ResourceSpan after the unmarshal.
//
// Attributes is the resource-level attribute map (NOT span
// attributes — slice 1's threat-model §12 makes the explicit
// guarantee that span content stays in the DuckDB span store and
// never reaches the traceindex). Keys follow the OTel semantic
// convention names: cloud.resource_id, host.id, host.name,
// cloud.account.id, cloud.provider, k8s.cluster.name, db.system,
// db.name, service.name.
//
// SpanCount is the number of spans in this batch belonging to this
// resource. RootSpanCount is the subset of SpanCount with no
// parent_span_id — the receiver's hot path computes this once and
// passes it through so the index's per-batch increment stays O(1).
//
// Timestamp is the batch arrival time at the receiver — slice 1
// uses receiver-clock rather than span-clock so a malformed exporter
// can't backdate the index. Tests inject a fixed clock for
// determinism.
type ResourceObservation struct {
	Attributes    map[string]string
	SpanCount     int
	RootSpanCount int
	Timestamp     time.Time
}

// ResourceRow is the projected storage row the SQLite
// trace_resource_seen table carries. Field shapes mirror the table
// schema 1:1 so the storage layer's row-scan code stays trivial.
//
// AttributesJSON is the marshaled resource attribute map captured
// at the latest Observe call — slice 1's design doc §4 names this
// as a "diagnostic UI" surface so an operator can inspect the
// attributes that produced the row. Per §12 the field carries
// resource attributes only; no span content.
//
// SpanCount24h + RootSpanCount24h are rolling counters. Slice 1
// ships a coarse 24h window (design doc §4 makes the choice
// explicitly — finer-grained windowing is a slice-2 refinement) so
// the index accumulates into the same row across the flush cadence.
type ResourceRow struct {
	ResourceKey      string
	Provider         string
	ScopeID          string
	ResourceIDHint   string
	ServiceName      string
	FirstSeenAt      time.Time
	LastSeenAt       time.Time
	SpanCount24h     int64
	RootSpanCount24h int64
	AttributesJSON   string
	MatchConfidence  MatchConfidence
	UpdatedAt        time.Time
}

// Summary is the per-provider coverage result the Discovery
// dashboard's TRACE COVERAGE panel renders. InventoryCount is
// supplied by the caller (the discovery side joins its scanner
// snapshot against the traceindex at endpoint-call time per design
// doc §13 Q1); EmittingCount is what the index actually carries for
// the (provider, scopeID) tuple.
//
// CoveragePct is zero-safe: when InventoryCount is 0 the field is
// 0.0, NOT NaN. Acceptance test 11 ("cold-start parity preserved")
// pins this behavior — a fresh deployment with no spans observed
// must render without errors.
type Summary struct {
	Provider          string
	ScopeID           string
	InventoryCount    int
	EmittingCount     int
	CoveragePct       float64
	StrongMatchPct    float64
	WeakMatchPct      float64
	LastIndexUpdateAt time.Time
}
