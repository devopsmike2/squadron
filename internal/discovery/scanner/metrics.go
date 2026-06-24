// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package scanner

import (
	"context"
	"errors"
	"time"
)

// MetricStatistic is the aggregation type for a metric query against a
// per-cloud metrics surface. Slice 1 of the cold-start latency analysis
// arc (docs/proposals/cold-start-latency-slice1.md, v0.89.113) ships
// only the P95 statistic — that's the SRE-standard compromise for the
// cold-start latency outlier rule per §3.2 of the design doc. The other
// values are defined here so that future slices (sampling-rate
// analysis, error-rate correlation) can extend the substrate without a
// breaking change to the wire shape.
//
// The constant string values are stable across releases — the
// MetricQuerier interface uses the typed value as the API contract.
// See metrics_test.go::TestMetricStatistic_StringValues_Stable for the
// pin.
type MetricStatistic string

const (
	// StatisticP95 is the 95th-percentile aggregation. Slice 1's
	// cold-start latency detection uses this exclusively per design
	// doc §3.2 — P99 is too noisy at typical Lambda throughputs,
	// P50 misses the operator-facing long tail, P95 is the SRE
	// compromise.
	StatisticP95 MetricStatistic = "p95"

	// StatisticP99 is the 99th-percentile aggregation. Reserved for
	// future slices; not used by the slice 1 cold-start rule.
	StatisticP99 MetricStatistic = "p99"

	// StatisticAverage is the arithmetic-mean aggregation. Reserved
	// for future slices; some metric kinds (e.g. CPU utilization)
	// are more naturally averaged than percentiled.
	StatisticAverage MetricStatistic = "average"

	// StatisticSum is the sum aggregation. Reserved for future
	// slices; the sampling-rate analysis substrate may use it for
	// span-count rollups.
	StatisticSum MetricStatistic = "sum"
)

// AggregateMetricResult is the return shape from a
// MetricQuerier.QueryAggregate call. Slice 1 fields are stable; future
// slices may add per-cloud detail (e.g. a Datapoints []time.Time field
// for the sampling-rate substrate's bin-by-bin reasoning) but the
// existing fields are guaranteed not to be renamed or removed.
//
// Empty-result semantics: a metric query against a resource that has
// emitted no datapoints in the requested window returns Value=0,
// SampleCount=0, no error. The MetricQuerier interface godoc names this
// contract; callers MUST check SampleCount before reading Value when
// distinguishing "value is genuinely 0" from "no datapoints exist".
//
// See docs/proposals/cold-start-latency-slice1.md §5 (scanner
// contract).
type AggregateMetricResult struct {
	// ResourceARN echoes the caller's input — the provider-native
	// fully-qualified resource identifier the query was issued
	// against. Denormalized here so a downstream consumer that
	// receives only the result doesn't have to track the request
	// context separately.
	ResourceARN string

	// MetricName echoes the caller's input — the per-cloud metric
	// identifier (e.g. "InitDuration" for AWS Lambda cold-start).
	MetricName string

	// Window is the duration the aggregation covers. Echoes the
	// caller's input. The substrate is agnostic to the exact period
	// the per-cloud implementation uses internally (CloudWatch's
	// 5-minute period vs. GCP Cloud Monitoring's 1-minute alignment)
	// — the Window field carries the operator-visible time span.
	Window time.Duration

	// Statistic echoes the caller's input MetricStatistic enum.
	Statistic MetricStatistic

	// Value is the aggregated value in the metric's native unit.
	// Cold-start latency arrives in milliseconds; sampling rate (a
	// future slice) would arrive as a unitless fraction. Always 0
	// when SampleCount is 0 — see type godoc on empty-result
	// semantics.
	Value float64

	// Unit is the metric's native unit string. AWS CloudWatch
	// returns "Milliseconds" for Lambda InitDuration; GCP returns
	// "ms" for Cloud Run / Cloud Functions request_latencies. The
	// substrate preserves the per-cloud unit verbatim; the
	// downstream consumer (the cold-start detection branch in chunk
	// 2) normalizes when comparing against the absolute floor.
	Unit string

	// SampleCount is the number of underlying datapoints the
	// aggregation was computed over. CloudWatch's 5-minute period
	// across a 24h window yields up to 288 samples; the real
	// CloudWatch response carries this as the SampleCount field per
	// datapoint, summed across the response. Zero is a valid value
	// — see type godoc on empty-result semantics.
	SampleCount int

	// ObservedAt is the timestamp the implementation considers the
	// aggregation's reference time. Slice 1 ships
	// time.Now().UTC() at the call site — chunk 2 will refine this
	// when the CloudWatch GetMetricStatistics wiring lands and the
	// per-datapoint timestamps become available. Round-tripped
	// through the storage layer's observed_at column.
	ObservedAt time.Time
}

// MetricQuerier is the interface per-cloud scanners implement to expose
// aggregate metric values for resources. The substrate is deliberately
// narrow — slice 1 (v0.89.113) ships one method, the AWS implementation
// is the only one with a concrete body (and even that body is a
// skeleton stub returning ErrMetricNotImplemented; chunk 2 wires the
// actual CloudWatch GetMetricStatistics call).
//
// The interface stays stable across the chunk 1 → 4 progression so that
// chunk 3 (proposer + UI) and chunk 4 (runbook) can be written against
// the v0.89.113 shape without waiting on chunk 2 to ship.
//
// Implementations MUST:
//
//   - Authenticate via the existing per-cloud scanner credentials. The
//     MetricQuerier is intentionally a method on the per-cloud Scanner
//     struct so the credential chain is shared with the existing
//     discovery walk.
//
//   - Handle empty result sets — a resource that has emitted no
//     datapoints in the requested window returns Value=0, SampleCount=0,
//     no error. The interface contract distinguishes "no datapoints" from
//     "API error" because the cold-start detection rule (chunk 2) skips
//     resources with insufficient samples without flagging an error.
//
//   - Respect per-cloud rate limits. CloudWatch GetMetricStatistics is
//     rate-limited per AWS account at ~50 RPS; chunk 2 ships a 10 RPS
//     limiter (the AWSCloudWatchRateLimitRPS constant in
//     internal/discovery/aws/metrics.go).
//
// See docs/proposals/cold-start-latency-slice1.md §5.
type MetricQuerier interface {
	// QueryAggregate returns the aggregated metric value for the
	// supplied resource over the supplied window. The metricName is
	// the per-cloud native identifier (e.g. "InitDuration" for AWS
	// Lambda); the substrate does not normalize across clouds —
	// callers know which cloud they're querying because they hold a
	// concrete per-cloud MetricQuerier.
	QueryAggregate(
		ctx context.Context,
		resourceARN string,
		metricName string,
		window time.Duration,
		stat MetricStatistic,
	) (AggregateMetricResult, error)
}

// ErrMetricNotImplemented is the sentinel error returned by per-cloud
// MetricQuerier skeletons that haven't shipped the concrete
// implementation yet. Used by chunk 1 of the cold-start latency arc
// (v0.89.113) so the interface compiles and downstream chunks (3, 4)
// can be written against the stable interface; chunks 2 (AWS),
// slice 2 (GCP / Azure / OCI) replace the skeleton with real
// implementations.
//
// Comparable via errors.Is — callers test the sentinel rather than
// string-matching the error message. See
// metrics_test.go::TestErrMetricNotImplemented_SentinelComparable.
var ErrMetricNotImplemented = errors.New("metric query not implemented for this provider in slice 1")
