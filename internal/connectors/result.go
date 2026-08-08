// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"errors"
	"time"
)

// ResultKind discriminates which payload field of a NormalizedResult is
// populated. Exactly one of Series / Logs / Scalar is meaningful for a
// given result, selected by this field.
type ResultKind string

const (
	// ResultMetrics means Series is populated (a metric timeseries).
	ResultMetrics ResultKind = "metrics"
	// ResultLogs means Logs is populated (log rows).
	ResultLogs ResultKind = "logs"
	// ResultScalar means Scalar is populated (a single value — the
	// SLO / burn-rate decision-context shape).
	ResultScalar ResultKind = "scalar"
)

// Sample is one (timestamp, value) point in a metric series.
type Sample struct {
	Timestamp time.Time
	Value     float64
}

// MetricSeries is one labeled timeseries. Labels identify the series
// (e.g. {"service":"checkout","region":"us-east-1"}); Samples are the
// points, ordered oldest-first by the connector.
type MetricSeries struct {
	Labels  map[string]string
	Samples []Sample
}

// LogRow is one normalized log line. Labels carry the stream labels /
// indexed fields the backend attached to the line.
type LogRow struct {
	Timestamp time.Time
	Line      string
	Labels    map[string]string
}

// ScalarKind discriminates how a consumer should read a ScalarResult's
// Value.
type ScalarKind string

const (
	// ScalarGeneric is a plain number with no SLO semantics (e.g. a
	// request count, a p99 latency).
	ScalarGeneric ScalarKind = "generic"

	// ScalarSLO means Value is a service-level indicator — a ratio in
	// [0,1] (e.g. 0.9993 = 99.93% good) or remaining error budget,
	// depending on Unit.
	ScalarSLO ScalarKind = "slo"

	// ScalarBurnRate means Value is an error-budget burn rate — a
	// multiple of the sustainable burn (e.g. 14.4 for a fast-burn
	// window). This is the value a rollout auto-abort hook compares
	// against Threshold.
	ScalarBurnRate ScalarKind = "burn_rate"
)

// ScalarResult carries a single computed value and, crucially, the
// SLO / burn-rate metadata a rollout auto-abort decision needs.
//
// A primary consumer of the connector framework is surfacing SLO /
// health / burn-rate as READ-ONLY decision context for config changes
// and rollouts. The auto-abort hook itself is a LATER slice (ADR 0034
// does not wire it in S1), but the model must be able to represent the
// burn-rate scalar now so that hook does not force a model change. The
// connector computes Breached so every consumer agrees on the verdict
// rather than each re-deriving it from Value and Threshold.
type ScalarResult struct {
	// Value is the computed scalar. Its meaning is set by Kind and Unit
	// (a burn-rate multiple, an SLI ratio, a raw count, ...).
	Value float64

	// Unit describes Value: "ratio", "percent", "multiple", "seconds",
	// "count", etc. Empty when dimensionless and self-evident from Kind.
	Unit string

	// Kind discriminates how to read Value.
	Kind ScalarKind

	// Objective is the human name of the SLO this scalar evaluates
	// (e.g. "checkout-availability"). Empty for ScalarGeneric.
	Objective string

	// SLOTarget is the objective target as a ratio (0.999 = 99.9%).
	// Zero when not applicable.
	SLOTarget float64

	// Threshold is the value a rollout auto-abort hook compares Value
	// against — e.g. abort when a fast-burn rate exceeds 14.4. Zero
	// means "no threshold configured"; consumers must treat a zero
	// Threshold as "do not auto-abort on this scalar".
	Threshold float64

	// Breached reports whether Value crossed Threshold at evaluation
	// time, computed by the connector. The auto-abort hook (a later
	// slice) reads this directly instead of re-deriving the comparison.
	// Always false when Threshold is zero.
	Breached bool

	// Window is the evaluation window the value was measured over (e.g.
	// the 1h/5m multiwindow used for fast-burn alerts).
	Window TimeRange

	// EvaluatedAt is when the connector computed the value.
	EvaluatedAt time.Time
}

// NormalizedResult is the vendor-neutral result a connector returns from
// Query. Exactly one payload field is populated, selected by Kind:
// Series for ResultMetrics, Logs for ResultLogs, Scalar for
// ResultScalar.
type NormalizedResult struct {
	// Kind selects which payload field is meaningful.
	Kind ResultKind

	// Series holds the metric timeseries when Kind == ResultMetrics.
	Series []MetricSeries

	// Logs holds the log rows when Kind == ResultLogs.
	Logs []LogRow

	// Scalar holds the single value when Kind == ResultScalar.
	Scalar *ScalarResult

	// Metadata carries backend-reported, non-payload context (source
	// name, query id, truncation flag, ...). Never carries secrets.
	Metadata map[string]string

	// Warnings carries non-fatal notices from the backend or the
	// translation (e.g. "results truncated at limit", "step coerced").
	Warnings []string
}

// NewMetricsResult builds a ResultMetrics result from the given series.
func NewMetricsResult(series []MetricSeries) NormalizedResult {
	return NormalizedResult{Kind: ResultMetrics, Series: series}
}

// NewLogsResult builds a ResultLogs result from the given rows.
func NewLogsResult(rows []LogRow) NormalizedResult {
	return NormalizedResult{Kind: ResultLogs, Logs: rows}
}

// NewScalarResult builds a ResultScalar result from the given scalar.
func NewScalarResult(s ScalarResult) NormalizedResult {
	return NormalizedResult{Kind: ResultScalar, Scalar: &s}
}

// ErrResultKindMismatch is returned by Validate when the populated
// payload field does not match Kind (e.g. Kind == ResultScalar but
// Scalar is nil).
var ErrResultKindMismatch = errors.New("connectors: result payload does not match Kind")

// Validate checks that the populated payload field matches Kind. It does
// not inspect payload contents (an empty series set is a valid metrics
// result); it only enforces the discriminator invariant so consumers can
// switch on Kind without nil-checking every field.
func (r NormalizedResult) Validate() error {
	switch r.Kind {
	case ResultMetrics:
		if r.Logs != nil || r.Scalar != nil {
			return ErrResultKindMismatch
		}
	case ResultLogs:
		if r.Series != nil || r.Scalar != nil {
			return ErrResultKindMismatch
		}
	case ResultScalar:
		if r.Scalar == nil || r.Series != nil || r.Logs != nil {
			return ErrResultKindMismatch
		}
	default:
		return ErrResultKindMismatch
	}
	return nil
}
