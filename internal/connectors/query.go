// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"errors"
	"time"
)

// TimeRange is the window a query (or a burn-rate evaluation) covers.
// A zero Step means "instant / let the backend pick its default
// resolution"; a non-zero Step requests a stepped timeseries at that
// resolution.
type TimeRange struct {
	// Start is the inclusive lower bound of the window.
	Start time.Time

	// End is the inclusive upper bound of the window.
	End time.Time

	// Step is the timeseries resolution for metric queries. Zero means
	// instant/backend default; ignored for log and scalar queries.
	Step time.Duration
}

// Duration is the End-Start span. Returns a negative duration for an
// inverted range; callers validate via NormalizedQuery.Validate first.
func (tr TimeRange) Duration() time.Duration {
	return tr.End.Sub(tr.Start)
}

// MatchOp is a label/field matcher operator. The set mirrors the
// operators SquadronQL's parser already understands (=, !=, =~, !~) so
// the normalized model does not introduce a second matcher vocabulary.
type MatchOp string

const (
	// MatchEqual is exact-match equality (label = "value").
	MatchEqual MatchOp = "="
	// MatchNotEqual is exact-match inequality (label != "value").
	MatchNotEqual MatchOp = "!="
	// MatchRegex is regex match (label =~ "re").
	MatchRegex MatchOp = "=~"
	// MatchNotRegex is negated regex match (label !~ "re").
	MatchNotRegex MatchOp = "!~"
)

// valid reports whether op is one of the four supported operators.
func (op MatchOp) valid() bool {
	switch op {
	case MatchEqual, MatchNotEqual, MatchRegex, MatchNotRegex:
		return true
	default:
		return false
	}
}

// LabelMatcher filters a query by a label (metrics) or field (logs).
// Each connector translates the matcher into its backend's filter
// syntax (a Prometheus/Loki stream selector, an SPL search term, a SQL
// WHERE predicate, ...).
type LabelMatcher struct {
	// Label is the label or field name to match on.
	Label string
	// Op is the match operator.
	Op MatchOp
	// Value is the literal or regex the operator compares against.
	Value string
}

// Aggregation is an optional reduction the caller asks the backend to
// push down. An empty Aggregation (AggNone) means "return raw series /
// rows". A connector only honors an aggregation it advertised via
// Capabilities.SupportsAggregation.
type Aggregation string

const (
	// AggNone requests no aggregation (raw series or rows).
	AggNone Aggregation = ""
	// AggSum sums grouped values.
	AggSum Aggregation = "sum"
	// AggAvg averages grouped values.
	AggAvg Aggregation = "avg"
	// AggMin takes the minimum of grouped values.
	AggMin Aggregation = "min"
	// AggMax takes the maximum of grouped values.
	AggMax Aggregation = "max"
	// AggCount counts grouped values / rows.
	AggCount Aggregation = "count"
	// AggRate computes the per-second rate of a counter over the step.
	AggRate Aggregation = "rate"
)

// valid reports whether a is a known aggregation (AggNone included).
func (a Aggregation) valid() bool {
	switch a {
	case AggNone, AggSum, AggAvg, AggMin, AggMax, AggCount, AggRate:
		return true
	default:
		return false
	}
}

// Validation sentinels. Callers errors.Is against these to distinguish a
// malformed query from a backend failure.
var (
	// ErrEmptySignal is returned when a NormalizedQuery has no Signal.
	ErrEmptySignal = errors.New("connectors: query signal is required")
	// ErrInvalidTimeRange is returned when the time range is unset or
	// inverted (End before Start).
	ErrInvalidTimeRange = errors.New("connectors: query time range is invalid (End must be after Start)")
	// ErrInvalidMatcher is returned when a matcher has an empty label or
	// an unknown operator.
	ErrInvalidMatcher = errors.New("connectors: query has an invalid label matcher")
	// ErrInvalidAggregation is returned when Aggregation is not a known
	// value.
	ErrInvalidAggregation = errors.New("connectors: query aggregation is not recognized")
)

// NormalizedQuery is the vendor-neutral representation of "query backend
// X for signal Y over time window Z with filters." Each connector
// translates it into the backend's own query language; the translation
// is the vendor-specific work, this shape is the contract between
// SquadronQL and every connector.
type NormalizedQuery struct {
	// Signal is the telemetry class to query (metrics/logs/traces).
	Signal Signal

	// Selector is the primary target: a metric/measurement name for
	// SignalMetrics, or a base stream/index selector for SignalLogs.
	// Its interpretation is connector-specific; it is required.
	Selector string

	// TimeRange is the window to query over.
	TimeRange TimeRange

	// Matchers are the label/field filters, ANDed together. Empty means
	// "no filter beyond the selector".
	Matchers []LabelMatcher

	// GroupBy lists the labels to group aggregated results by. Ignored
	// when Aggregation is AggNone.
	GroupBy []string

	// Aggregation is the optional push-down reduction. AggNone returns
	// raw series/rows.
	Aggregation Aggregation

	// Limit caps the number of series (metrics) or rows (logs) returned.
	// Zero means "connector/back-end default limit".
	Limit int

	// Raw is an optional escape hatch carrying the original SquadronQL
	// text for diagnostics and for connectors that prefer to pass a
	// backend-native fragment through untranslated. Never required.
	Raw string
}

// Validate checks the query is well-formed before a connector attempts
// to translate it. It returns one of the Err* sentinels (errors.Is-
// comparable) on the first problem found, or nil when the query is
// valid.
func (q NormalizedQuery) Validate() error {
	if q.Signal == "" {
		return ErrEmptySignal
	}
	if q.TimeRange.Start.IsZero() || q.TimeRange.End.IsZero() || !q.TimeRange.End.After(q.TimeRange.Start) {
		return ErrInvalidTimeRange
	}
	for _, m := range q.Matchers {
		if m.Label == "" || !m.Op.valid() {
			return ErrInvalidMatcher
		}
	}
	if !q.Aggregation.valid() {
		return ErrInvalidAggregation
	}
	return nil
}
