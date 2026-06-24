// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Cold-start latency slice 2 chunk 1 (v0.89.118, #756 Stream 154) —
// GCP MetricQuerier implementation wrapping Cloud Monitoring V3
// timeSeries.list. Ships the Cloud Run request_latencies + Cloud
// Functions execution_times surfaces per design doc §3.1 + §3.2.
//
// SDK wiring posture: this chunk ships the
// scanner.MetricQuerier-satisfying QueryAggregate method against a
// narrow metricsClient interface that abstracts the underlying
// timeSeries.list call. The production-path SDK adapter
// (cloud.google.com/go/monitoring/apiv3/v2) is deferred to a
// follow-up chunk — same chunk-1 → chunk-2 split AWS did with
// slice 1. Tests use an in-memory fake satisfying metricsClient.
// The follow-up swaps the nil metricsClient on the production-path
// Scanner for a real SDK-backed adapter; QueryAggregate stays
// byte-identical.
//
// See docs/proposals/cold-start-latency-slice2.md §5 + §11.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// GCPCloudMonitoringRateLimitRPM pins the per-project rate limit the
// slice 2 substrate enforces for Cloud Monitoring timeSeries.list
// requests. Cloud Monitoring's documented per-project quota is 6000
// RPM (100 RPS); the slice 2 substrate sits well under that ceiling
// at 60 RPM (1 RPS) so multi-instance Squadron deployments scanning
// the same project stay below the throttle limit even with the
// per-page fan-out a typical fleet produces.
//
// Pinned to 60 by metrics_test.go::TestGCPCloudMonitoringRateLimitRPM_Constant.
// The runbook documents the choice — changes have to update the
// test (and the runbook).
//
// See docs/proposals/cold-start-latency-slice2.md §12 (threat
// model: per-cloud rate limit thresholds).
const GCPCloudMonitoringRateLimitRPM = 60

// CloudRunRequestLatenciesMetricType is the Cloud Monitoring metric
// type for Cloud Run request latency per design doc §3.1. Slice 2
// filters by response_code_class = "2xx" to get a cleaner baseline
// (error paths skew the latency distribution in a way that's not
// operator-actionable for the cold-start recommendation).
//
// Pinned to "run.googleapis.com/request_latencies" by
// metrics_test.go::TestCloudRunRequestLatenciesMetricType_Constant.
const CloudRunRequestLatenciesMetricType = "run.googleapis.com/request_latencies"

// CloudFunctionsExecutionTimesMetricType is the Cloud Monitoring
// metric type for Cloud Functions execution time per design doc
// §3.2. Slice 2 filters by status = "ok" — the cold-start latency
// signal is meaningful only against successful invocations; failed
// invocations exit early and produce execution_times values that
// don't reflect cold-start behavior.
//
// Pinned to "cloudfunctions.googleapis.com/function/execution_times"
// by metrics_test.go::TestCloudFunctionsExecutionTimesMetricType_Constant.
const CloudFunctionsExecutionTimesMetricType = "cloudfunctions.googleapis.com/function/execution_times"

// CloudRunRequestCountMetricType is the Cloud Monitoring metric type
// for Cloud Run request count per
// docs/proposals/sampling-rate-analysis-slice1.md §4.2. Sampling
// rate analysis slice 1 chunk 1 (v0.89.122) filters by
// response_code_class != "5xx" — the SDK's upstream sampling
// decision is what matters, not ingress success, so 5xx is the only
// class we exclude (4xx requests still reflect a real invocation
// the sampler observed).
//
// IAM unchanged from cold-start slice 2: the same
// monitoring.timeSeries.list permission covers all three Cloud
// Monitoring metrics the GCP MetricQuerier now routes.
//
// Pinned to "run.googleapis.com/request_count" by
// metrics_test.go::TestCloudRunRequestCountMetricType_Constant.
const CloudRunRequestCountMetricType = "run.googleapis.com/request_count"

// CloudFunctionsExecutionCountMetricType is the Cloud Monitoring
// metric type for Cloud Functions execution count per
// docs/proposals/sampling-rate-analysis-slice1.md §4.3. Filters by
// status = "ok" — failed invocations may exit before the SDK
// sampler fires, so a mixed-status denominator would skew the
// observed/expected ratio toward "Squadron sees no spans" for
// legitimate reasons.
//
// Pinned to "cloudfunctions.googleapis.com/function/execution_count"
// by metrics_test.go::TestCloudFunctionsExecutionCountMetricType_Constant.
const CloudFunctionsExecutionCountMetricType = "cloudfunctions.googleapis.com/function/execution_count"

// CloudRunRequestCount5xxMetricType is the Squadron-internal
// synthetic-suffix variant of CloudRunRequestCountMetricType that
// the error-rate-correlation slice 1 chunk 1 (v0.89.127) routes
// through with a response_code_class = "5xx" filter (the inverse of
// the sampling-rate path's response_code_class != "5xx"). The base
// metric.type sent to Cloud Monitoring is the suffix-stripped form
// (CloudRunRequestCountMetricType); the "#5xx" suffix is consumed
// by the Squadron router and never appears on the wire.
//
// Why a synthetic-suffix constant rather than a new
// QueryAggregate parameter for dimension filtering: keeping the
// scanner.MetricQuerier interface stable across the
// cold-start / sampling-rate / error-rate arcs is a contract per
// the slice 1 design doc §4 — the substrate now supports 5+ metric
// variants per cloud and a per-filter parameter would force every
// caller (and every existing test) to thread it through. Encoding
// the filter intent in the metric name string keeps the interface
// at four arguments and routes through the existing per-metric
// switch.
//
// See docs/proposals/error-rate-correlation-slice1.md §4.2.
//
// Pinned by metrics_test.go::TestCloudRunRequestCount5xxMetricType_Constant.
const CloudRunRequestCount5xxMetricType = "run.googleapis.com/request_count#5xx"

// CloudFunctionsExecutionCountErrorMetricType is the
// Squadron-internal synthetic-suffix variant of
// CloudFunctionsExecutionCountMetricType that the error-rate
// correlation slice 1 routes through with a status != "ok" filter
// (the inverse of the sampling-rate path's status = "ok"). Same
// suffix-stripping convention as
// CloudRunRequestCount5xxMetricType — the "#error" suffix is a
// Squadron router signal and never reaches Cloud Monitoring.
//
// See docs/proposals/error-rate-correlation-slice1.md §4.3.
//
// Pinned by metrics_test.go::TestCloudFunctionsExecutionCountErrorMetricType_Constant.
const CloudFunctionsExecutionCountErrorMetricType = "cloudfunctions.googleapis.com/function/execution_count#error"

// cloudMonitoringMetricUnit is the unit string the slice 2
// substrate stamps on the AggregateMetricResult.Unit field for
// Cloud Run / Cloud Functions latency metrics. Both surfaces emit
// values in milliseconds per Cloud Monitoring's documented unit
// for the request_latencies / execution_times metrics. The
// downstream cold-start detection branch compares the result
// against the 500ms absolute floor (ColdStartDetectionFloorMs)
// directly, so the unit is fixed at the package level rather than
// round-tripped per-call from the SDK response.
const cloudMonitoringMetricUnit = "ms"

// TimeSeriesPoint is the per-period aggregated value returned by
// the metricsClient adapter. Slice 2's MAX-of-P95s rollup reads
// Value; the cold-start detection branch reads the SampleCount sum
// to satisfy the baseline-minimum-samples gate. StartTime /
// EndTime carry the alignment period boundaries — slice 2 doesn't
// reason about per-point timestamps directly but the fields are
// surfaced for future-slice extensions.
type TimeSeriesPoint struct {
	// Value is the aggregated value (ms for latency surfaces) the
	// per-period alignment produced.
	Value float64

	// SampleCount is the number of raw datapoints the alignment
	// rolled up into this point.
	SampleCount int64

	// StartTime / EndTime are the alignment period boundaries.
	StartTime time.Time
	EndTime   time.Time
}

// metricsClient is the minimal surface the GCP MetricQuerier needs
// from the Cloud Monitoring V3 SDK. The production adapter
// translates QueryTimeSeries into a ListTimeSeriesRequest against
// monitoring/apiv3/v2.MetricClient. The filter is the fully-formed
// Cloud Monitoring filter expression — QueryAggregate builds it
// per-metric before calling, which keeps the test fake able to
// assert on the filter string. projectName is the
// "projects/{project}" form; stat is the per-series-aligner enum
// string (ALIGN_PERCENTILE_95 etc.).
type metricsClient interface {
	QueryTimeSeries(
		ctx context.Context,
		projectName string,
		filter string,
		startTime, endTime time.Time,
		stat string,
	) ([]TimeSeriesPoint, error)
}

// QueryAggregate implements scanner.MetricQuerier for GCP via Cloud
// Monitoring V3 timeSeries.list. Slice 2 chunk 1 (v0.89.118) ships
// the interface + a fake-backed test path; the real SDK adapter
// lands in a follow-up chunk. Until the follow-up lands the
// production-path Scanner has a nil metricsClient and
// QueryAggregate returns scanner.ErrMetricNotImplemented.
//
// Routing per design doc §3.1 + §3.2 + §5:
//
//   - CloudRunRequestLatenciesMetricType + ARN kind "services" →
//     request_latencies filter scoped to service_name with
//     response_code_class = "2xx".
//   - CloudFunctionsExecutionTimesMetricType + ARN kind
//     "functions" → execution_times filter scoped to function_name
//     with status = "ok".
//   - Metric/kind mismatch (e.g. request_latencies on a Cloud
//     Function ARN) → empty result, no error. The detection branch
//     never asks for the wrong pair, but the substrate contract
//     distinguishes "wrong combination" from "query failed".
//   - Any other metricName → empty result, no error (slice 2
//     scope: the two metrics above).
//   - metricsClient nil → scanner.ErrMetricNotImplemented.
//
// Empty datapoint handling (§11 test 2): empty timeSeries response
// → Value=0, SampleCount=0, no error. Callers MUST check
// SampleCount before reading Value.
//
// Rate limiter (§11 test 4): a Wait call against the per-Scanner
// metricsLimiter precedes every Cloud Monitoring call, capping the
// per-project RPM at GCPCloudMonitoringRateLimitRPM.
//
// Aggregation: Cloud Monitoring returns one point per alignment
// period (5m); MAX across all points in the window gives the
// worst-case 5-minute P95 — mirrors the slice 1 CloudWatch rollup
// for cross-cloud comparison honesty.
//
// See docs/proposals/cold-start-latency-slice2.md §3.1, §3.2, §5,
// §11.
func (s *Scanner) QueryAggregate(
	ctx context.Context,
	resourceARN string,
	metricName string,
	window time.Duration,
	stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	if s.metricsClient == nil {
		// Surfaces the chunk-1 skeleton sentinel so callers that
		// haven't wired the Cloud Monitoring client (validation-
		// only Scanners, partially-constructed test fixtures)
		// observe the same shape as the v0.89.113 substrate. The
		// follow-up SDK chunk replaces nil with a real adapter and
		// this branch becomes inert in production.
		return scanner.AggregateMetricResult{
			ResourceARN: resourceARN,
			MetricName:  metricName,
			Window:      window,
			Statistic:   stat,
		}, scanner.ErrMetricNotImplemented
	}

	project, kind, name, err := parseGCPResourceName(resourceARN)
	if err != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("parse resource name: %w", err)
	}

	var filter string
	// isCountMetric flips the per-period rollup from MAX (latency
	// surfaces) to SUM (count surfaces). Sampling rate slice 1
	// chunk 1 (v0.89.122) adds the two count metrics for Cloud Run
	// + Cloud Functions; the latency metrics keep their MAX
	// rollup so cross-cloud comparisons stay honest.
	isCountMetric := false
	switch metricName {
	case CloudRunRequestLatenciesMetricType:
		if kind != "services" {
			// Cloud Run latency metric on a non-Cloud-Run ARN —
			// substrate scope mismatch. Empty result, no error.
			return scanner.AggregateMetricResult{
				ResourceARN: resourceARN,
				MetricName:  metricName,
				Window:      window,
				Statistic:   stat,
			}, nil
		}
		filter = fmt.Sprintf(
			`metric.type = %q AND resource.labels.service_name = %q AND metric.labels.response_code_class = "2xx"`,
			CloudRunRequestLatenciesMetricType, name)
	case CloudFunctionsExecutionTimesMetricType:
		if kind != "functions" {
			// Cloud Functions latency metric on a non-Cloud-Function
			// ARN — substrate scope mismatch. Empty result, no error.
			return scanner.AggregateMetricResult{
				ResourceARN: resourceARN,
				MetricName:  metricName,
				Window:      window,
				Statistic:   stat,
			}, nil
		}
		filter = fmt.Sprintf(
			`metric.type = %q AND resource.labels.function_name = %q AND metric.labels.status = "ok"`,
			CloudFunctionsExecutionTimesMetricType, name)
	case CloudRunRequestCountMetricType:
		// Sampling rate slice 1 (v0.89.122) §4.2. Filter excludes
		// 5xx responses so the denominator reflects the sampler's
		// upstream decision (4xx requests are still invocations
		// the sampler observed).
		if kind != "services" {
			return scanner.AggregateMetricResult{
				ResourceARN: resourceARN,
				MetricName:  metricName,
				Window:      window,
				Statistic:   stat,
			}, nil
		}
		filter = fmt.Sprintf(
			`metric.type = %q AND resource.labels.service_name = %q AND metric.labels.response_code_class != "5xx"`,
			CloudRunRequestCountMetricType, name)
		isCountMetric = true
	case CloudFunctionsExecutionCountMetricType:
		// Sampling rate slice 1 (v0.89.122) §4.3. Filter restricts
		// to status="ok" — failed invocations may exit before the
		// SDK sampler fires, skewing the denominator.
		if kind != "functions" {
			return scanner.AggregateMetricResult{
				ResourceARN: resourceARN,
				MetricName:  metricName,
				Window:      window,
				Statistic:   stat,
			}, nil
		}
		filter = fmt.Sprintf(
			`metric.type = %q AND resource.labels.function_name = %q AND metric.labels.status = "ok"`,
			CloudFunctionsExecutionCountMetricType, name)
		isCountMetric = true
	case CloudRunRequestCount5xxMetricType:
		// Error rate correlation slice 1 (v0.89.127) §4.2. The
		// Squadron-internal "#5xx" suffix variant of
		// CloudRunRequestCountMetricType — the base metric.type
		// going on the wire is the suffix-stripped form; the
		// dimension filter inverts the sampling-rate path (5xx
		// only rather than != 5xx). The detection branch uses
		// this metric as the error-rate numerator; the existing
		// CloudRunRequestCountMetricType (with the != "5xx"
		// filter) reused as the denominator. Same MetricQuerier
		// interface; only the metric name string changes — no
		// new parameter required.
		if kind != "services" {
			return scanner.AggregateMetricResult{
				ResourceARN: resourceARN,
				MetricName:  metricName,
				Window:      window,
				Statistic:   stat,
			}, nil
		}
		filter = fmt.Sprintf(
			`metric.type = %q AND resource.labels.service_name = %q AND metric.labels.response_code_class = "5xx"`,
			CloudRunRequestCountMetricType, name)
		isCountMetric = true
	case CloudFunctionsExecutionCountErrorMetricType:
		// Error rate correlation slice 1 (v0.89.127) §4.3.
		// Squadron-internal "#error" suffix variant of
		// CloudFunctionsExecutionCountMetricType — sibling of the
		// sampling-rate path's status = "ok" filter (status != "ok"
		// catches every non-success status the Cloud Functions
		// runtime emits: error, timeout, crash, etc.). Same
		// suffix-stripping convention as the Cloud Run #5xx
		// variant.
		if kind != "functions" {
			return scanner.AggregateMetricResult{
				ResourceARN: resourceARN,
				MetricName:  metricName,
				Window:      window,
				Statistic:   stat,
			}, nil
		}
		filter = fmt.Sprintf(
			`metric.type = %q AND resource.labels.function_name = %q AND metric.labels.status != "ok"`,
			CloudFunctionsExecutionCountMetricType, name)
		isCountMetric = true
	default:
		// Slice 2 substrate scope: Cloud Run + Cloud Functions
		// only. Other names short-circuit to empty so the interface
		// contract distinguishes "metric not supported in slice 2"
		// (empty result) from "API call failed" (non-nil error).
		return scanner.AggregateMetricResult{
			ResourceARN: resourceARN,
			MetricName:  metricName,
			Window:      window,
			Statistic:   stat,
		}, nil
	}

	if s.metricsLimiter != nil {
		if err := s.metricsLimiter.Wait(ctx); err != nil {
			return scanner.AggregateMetricResult{}, fmt.Errorf("rate limit: %w", err)
		}
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)
	statStr := mapStatToGCP(stat)
	if isCountMetric {
		// Count metrics aggregate via ALIGN_DELTA at the per-period
		// alignment so each TimeSeriesPoint.Value carries the
		// per-period invocation count — the cross-period rollup
		// below then SUMs across periods to get total invocations.
		// Mirrors the AWS Statistics=["Sum"] choice for Lambda
		// Invocations.
		statStr = "ALIGN_DELTA"
	}

	points, err := s.metricsClient.QueryTimeSeries(
		ctx,
		fmt.Sprintf("projects/%s", project),
		filter, startTime, endTime, statStr,
	)
	if err != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("query time series: %w", err)
	}

	result := scanner.AggregateMetricResult{
		ResourceARN: resourceARN,
		MetricName:  metricName,
		Window:      window,
		Statistic:   stat,
		ObservedAt:  endTime,
	}
	if len(points) == 0 {
		// Acceptance test 2 — empty timeSeries response is a real
		// "no datapoints" signal, not an error. Value stays 0,
		// SampleCount stays 0; the cold-start detection branch sees
		// SampleCount=0 and skips the comparison per the
		// ColdStartDetectionResult.ShouldFireRecommendation
		// contract.
		return result, nil
	}

	// Per-period rollup. Latency surfaces use MAX (mirrors the
	// CloudWatch rollup so cross-cloud comparisons stay honest);
	// count surfaces use SUM (total invocations across the window
	// = sum of per-period deltas).
	val := 0.0
	var totalSamples int64
	if isCountMetric {
		for _, p := range points {
			val += p.Value
			totalSamples += p.SampleCount
		}
	} else {
		for _, p := range points {
			if p.Value > val {
				val = p.Value
			}
			totalSamples += p.SampleCount
		}
	}
	result.Value = val
	result.SampleCount = int(totalSamples)
	// Latency surfaces report "ms"; count surfaces are unitless
	// (the chunk-2 detection branch treats the count value as
	// dimensionless when computing the observed/expected ratio).
	if isCountMetric {
		result.Unit = ""
	} else {
		result.Unit = cloudMonitoringMetricUnit
	}
	return result, nil
}

// mapStatToGCP converts the scanner.MetricStatistic enum into the
// Cloud Monitoring per-series-aligner string. Slice 2 ships
// StatisticP95 + StatisticP99; the StatisticAverage / StatisticSum
// values are reserved for future slices and currently fall through
// to ALIGN_PERCENTILE_95 (the slice 2 detection rule's statistic).
//
// Pinned by metrics_test.go::TestMapStatToGCP_Mapping.
func mapStatToGCP(stat scanner.MetricStatistic) string {
	switch stat {
	case scanner.StatisticP95:
		return "ALIGN_PERCENTILE_95"
	case scanner.StatisticP99:
		return "ALIGN_PERCENTILE_99"
	default:
		return "ALIGN_PERCENTILE_95"
	}
}

// parseGCPResourceName parses the GCP fully-qualified resource name
// shared across Cloud Run + Cloud Functions:
//
//	projects/{project}/locations/{loc}/{kind}/{name}
//
// where kind is "services" (Cloud Run) or "functions" (Cloud
// Functions). Returns the project id, kind, and name segments.
//
// Returns an error when the ARN doesn't match the expected shape —
// most commonly when the caller passed an unrelated resource ARN
// (a Lambda ARN, a Compute Engine self-link, a Cloud SQL connection
// name, etc.). The error message includes the offending ARN so the
// detection branch's log surface points at the specific row that
// misfired.
//
// Pinned by metrics_test.go::TestParseGCPResourceName_* variants.
func parseGCPResourceName(arn string) (project, kind, name string, err error) {
	parts := strings.Split(arn, "/")
	if len(parts) < 6 || parts[0] != "projects" || parts[2] != "locations" {
		return "", "", "", fmt.Errorf("not a GCP resource name: %q", arn)
	}
	if parts[1] == "" || parts[3] == "" || parts[4] == "" || parts[5] == "" {
		return "", "", "", fmt.Errorf("not a GCP resource name: %q", arn)
	}
	return parts[1], parts[4], parts[5], nil
}

// WithMetricsClient wires the Cloud Monitoring client adapter (or a
// test fake satisfying metricsClient) into the Scanner. v0.89.118.
// Returns the Scanner so the constructor chain composes — mirrors
// the AWS slice 1 chunk 2 WithCloudWatchClient setter pattern.
//
// Nil clients are accepted — the QueryAggregate path treats a nil
// metricsClient as the chunk-1 skeleton (returns
// scanner.ErrMetricNotImplemented), preserving the v0.89.113
// surface when callers explicitly want to opt out.
func (s *Scanner) WithMetricsClient(client metricsClient) *Scanner {
	s.metricsClient = client
	return s
}

// WithMetricsRateLimiter overrides the per-Scanner rate limiter.
// v0.89.118. Reserved for tests that need to pin the limiter's
// burst to a specific value to deterministically time the
// 60-RPM pin (TestGCPRateLimiterCapsAt60RPM), or to disable it
// entirely (a nil limiter short-circuits the Wait call).
// Production never calls this — the constructors pre-arm the
// limiter at the substrate-default 60 RPM.
func (s *Scanner) WithMetricsRateLimiter(limiter *rate.Limiter) *Scanner {
	s.metricsLimiter = limiter
	return s
}

// WithColdStartStore wires the cold-start observation storage
// adapter into the Scanner. v0.89.118. Mirrors the AWS slice 1
// chunk 2 setter pattern — production wires the real
// *sqlite.Storage; tests substitute an in-memory fake.
func (s *Scanner) WithColdStartStore(store ColdStartStore) *Scanner {
	s.coldStartStore = store
	return s
}

// WithConnectionID overrides the connection identifier used to
// scope persisted cold-start observations. v0.89.118. Production
// constructors carry the value through their CloudConnection
// argument; the validation-constructor path leaves it empty, so
// tests that want to exercise the persistence branch set it here
// explicitly.
func (s *Scanner) WithConnectionID(id string) *Scanner {
	s.connectionID = id
	return s
}

// preArmMetricsLimiter returns a rate.Limiter configured for the
// substrate-default GCPCloudMonitoringRateLimitRPM (60 RPM = 1
// request per second). Burst=1 forces every request to acquire a
// token rather than coalescing into a burst window — deterministic
// for the rate-limiter timing test. Used by both
// NewScannerForValidation and NewScannerFromConnection so the
// production path always carries the slice 2 substrate contract.
func preArmMetricsLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Every(time.Second), 1)
}

