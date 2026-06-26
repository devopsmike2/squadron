// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// OCIMonitoringRateLimitTPS is 10 TPS per region; matches OCI's
// documented Monitoring API rate limit. Per slice 2 §12.
//
// Pinned by metrics_test.go::TestOCIMonitoringRateLimitTPS_Constant.
const OCIMonitoringRateLimitTPS = 10

// OCIFunctionsMetricNamespace is the OCI Monitoring namespace for
// Functions metrics. The summarizeMetricsData API requires the
// namespace + the per-metric MQL query as a tuple; the constant
// keeps the namespace single-sourced for the chunk-3 routing.
const OCIFunctionsMetricNamespace = "oci_faas"

// OCIFunctionsFunctionDurationMetric is the metric name for
// per-function execution duration. Slice 2 uses this as the proxy
// for cold-start latency when cold_start_count > 0. OCI doesn't
// expose an isolated cold-start latency metric, so the design doc's
// §3.4 detection joins function_duration P95 with the
// cold_start_count counter — when the counter is zero the
// duration's cold-start contribution is also zero, and the
// detection short-circuits.
const OCIFunctionsFunctionDurationMetric = "FunctionExecutionDuration"

// OCIFunctionsColdStartCountMetric is the counter Squadron uses to
// verify the function actually experienced cold starts in the
// window. When this counter is 0, slice 2 skips the detection (no
// cold starts = no signal). See cold_start.go::
// DetectColdStartRegression for the gate.
// AVAILABILITY WARNING: oci_faas has no cold-start counter metric (only
// FunctionInvocationCount / FunctionExecutionDuration / FunctionResponseCount /
// AllocatedProvisionedConcurrency). This name does not resolve, so the OCI
// cold-start gate is unsatisfiable — detection redesign deferred per
// docs/audit/detection-metric-availability.md.
const OCIFunctionsColdStartCountMetric = "cold_start_count"

// OCIFunctionsInvocationCountMetric is the OCI Monitoring counter
// for per-function invocation count. Sampling rate analysis slice 1
// chunk 1 (v0.89.122) uses this as the denominator for the
// observed_span_count / expected_invocation_count ratio per
// docs/proposals/sampling-rate-analysis-slice1.md §4.5.
//
// The MQL query mirrors the cold_start_count one but reads the
// invocation counter:
//
//	function_invocation_count[<window>]{resourceId = "<ocid>"}.sum()
//
// IAM stays unchanged: the existing "read metrics in compartment"
// permission covers function_invocation_count alongside
// function_duration + cold_start_count.
//
// Pinned to "function_invocation_count" by
// metrics_test.go::TestOCIFunctionsInvocationCountMetric_Constant.
const OCIFunctionsInvocationCountMetric = "FunctionInvocationCount"

// OCIFunctionsErrorResponseCountMetric is the Squadron-internal
// synthetic-suffix variant of OCIFunctionsInvocationCountMetric
// that the error-rate-correlation slice 1 chunk 1 (v0.89.127)
// routes through with a result = "error" dimension filter on the
// OCI Monitoring MQL query. The base metric name sent in the MQL
// expression is the suffix-stripped form
// (OCIFunctionsInvocationCountMetric); the "#error" suffix is
// consumed by the Squadron router and never appears on the wire.
//
// Why a synthetic-suffix constant rather than a new
// QueryAggregate parameter for dimension filtering: keeping the
// scanner.MetricQuerier interface stable across the cold-start /
// sampling-rate / error-rate arcs is a contract per the slice 1
// design doc §4. Mirrors the GCP CloudRunRequestCount5xx /
// CloudFunctionsExecutionCountError convention so the Squadron
// router signal looks the same across providers.
//
// MQL shapes:
//
//	without suffix → function_invocation_count[24h]{resourceId = "..."}.sum()
//	with #error suffix → function_invocation_count[24h]{resourceId = "...", result = "error"}.sum()
//
// IAM stays unchanged from slices 1 + 2: the existing
// "read metrics in compartment" permission covers all three OCI
// metric variants. Per-tenancy rate limiter stays UNCHANGED — the
// new metric query flows through the existing 10 TPS limiter.
//
// Pinned to "function_invocation_count#error" by
// metrics_test.go::TestOCIFunctionsErrorResponseCountMetric_Constant.
const OCIFunctionsErrorResponseCountMetric = "FunctionResponseCount"

// ociMonitoringAPIVersion pins the OCI Monitoring
// summarizeMetricsData API path version. OCI versions live in the
// path; single-sourced for the same reason as the compute /
// identity / functions list constants.
const ociMonitoringAPIVersion = "20180401"

// ociMonitoringResolutionSeconds is the per-datapoint aggregation
// resolution the slice-2 substrate requests from OCI Monitoring.
// Mirrors the AWS CloudWatch 5-minute period — the cross-window
// math (24h current vs 168h baseline) lines up at the same
// granularity across both clouds.
const ociMonitoringResolutionSeconds = 300

// ociMetricUnitMs is the unit the substrate reports for both
// supported OCI metrics. function_duration is documented in
// milliseconds; cold_start_count is a counter (unitless) but the
// detection branch only consumes its sum and never reads the unit.
// Reporting "ms" uniformly keeps the AggregateMetricResult.Unit
// field consistent with the AWS substrate's "Milliseconds".
const ociMetricUnitMs = "ms"

// MonitoringClient is the minimal surface the OCI MetricQuerier
// needs from the OCI Monitoring SDK. Slice 2 chunk 3 consumes only
// summarizeMetricsData via a single POST; the rest of the OCI
// Monitoring API is intentionally outside the interface so the
// test fake stays a single-method shape.
//
// The signing flow re-uses the existing scanner_functions.go raw
// HTTP + RSA-signed-request pattern — the chunk-3 implementation
// wires a small POST helper (doSignedPOST) that signs both the
// request-target / date / host triple AND the content-length /
// content-type / x-content-sha256 triple per the OCI HTTP
// Signatures spec.
type MonitoringClient interface {
	SummarizeMetricsData(
		ctx context.Context,
		compartmentID string,
		namespace string,
		query string,
		startTime time.Time,
		endTime time.Time,
	) ([]ociMetricDataPoint, error)
}

// ociMetricDataPoint is the substrate's projection of one returned
// metric datapoint. OCI Monitoring's summarizeMetricsData returns
// a list of MetricData entries, each carrying an aggregatedDatapoints
// list of (timestamp, value) pairs. The chunk-3 substrate flattens
// across all MetricData entries (slice 2 queries one resourceId at
// a time, so the entries are always for the same resource) and
// returns the flattened (Timestamp, Value, SampleCount) tuples.
//
// SampleCount is synthesized at the substrate layer — OCI's API
// returns one value per datapoint (already pre-aggregated at the
// resolution) so the substrate counts the datapoint as one sample.
// The cold-start detection branch consumes this to drive the
// BaselineSampleCount gate; 50 samples over a 168h baseline at 5m
// resolution gives ~50/2016 ≈ 2.5% coverage, which the
// MinimumSamples constant treats as the floor for "trustworthy".
type ociMetricDataPoint struct {
	Timestamp   time.Time
	Value       float64
	SampleCount int
}

// ociSummarizeRequestBody is the JSON body shape OCI Monitoring's
// summarizeMetricsData accepts. The query field carries the MQL
// expression; namespace selects the metric family; startTime /
// endTime bound the window; resolution names the per-datapoint
// rollup granularity.
type ociSummarizeRequestBody struct {
	Query      string `json:"query"`
	Namespace  string `json:"namespace"`
	StartTime  string `json:"startTime"`
	EndTime    string `json:"endTime"`
	Resolution string `json:"resolution,omitempty"`
}

// ociSummarizeResponse is the response envelope from
// summarizeMetricsData. OCI returns a JSON object with an "items"
// list, each carrying its own aggregatedDatapoints list. Slice 2
// queries one resourceId at a time, so callers typically see a
// single item — but the substrate flattens across all items for
// resilience against unusual API responses.
type ociSummarizeResponse struct {
	Items []ociMetricDataItem `json:"items"`
}

// ociMetricDataItem is one MetricData entry in the response. Carries
// the metric name (echoes the query's metric), the resourceId (echoes
// the query's filter), and the list of (timestamp, value) pairs.
type ociMetricDataItem struct {
	Name                 string                   `json:"name"`
	Namespace            string                   `json:"namespace"`
	Resolution           string                   `json:"resolution,omitempty"`
	Dimensions           map[string]string        `json:"dimensions,omitempty"`
	AggregatedDatapoints []ociAggregatedDatapoint `json:"aggregatedDatapoints"`
}

// ociAggregatedDatapoint is one (timestamp, value) pair. OCI emits
// the timestamp as an RFC3339 string and the value as a JSON
// number. The substrate parses the timestamp into time.Time before
// returning.
type ociAggregatedDatapoint struct {
	Timestamp string  `json:"timestamp"`
	Value     float64 `json:"value"`
}

// QueryAggregate implements scanner.MetricQuerier for OCI via OCI
// Monitoring summarizeMetricsData. Slice 2 chunk 3 (v0.89.118)
// wires the real signed-POST call, the per-tenancy rate limiter,
// and the empty-result-set semantics the MetricQuerier interface
// contract specifies.
//
// Routing per design doc §3.4 + §5:
//
//   - metricName == OCIFunctionsFunctionDurationMetric → real OCI
//     Monitoring call with the MQL query
//     "function_duration[<window>]{resourceId = \"<ocid>\"}.percentile(95)".
//   - metricName == OCIFunctionsColdStartCountMetric → real OCI
//     Monitoring call with the MQL query
//     "cold_start_count[<window>]{resourceId = \"<ocid>\"}.sum()".
//   - Any other metricName → returns an empty
//     AggregateMetricResult with SampleCount=0 and no error
//     (matches the AWS chunk-2 contract for unsupported names).
//   - Monitoring client not wired → returns
//     scanner.ErrMetricNotImplemented, mirroring the chunk-1
//     skeleton's surface so callers can errors.Is-detect the
//     unwired path.
//
// Empty datapoint handling: when the OCI API returns an empty
// items list (or items with empty aggregatedDatapoints), the
// function returns Value=0, SampleCount=0, no error. Callers MUST
// check SampleCount before reading Value when distinguishing
// "value is genuinely 0" from "no datapoints existed".
//
// Rate limiter: a Wait call against the per-Scanner
// monitoringLimiter precedes every OCI call, capping the
// per-tenancy TPS at OCIMonitoringRateLimitTPS.
//
// See docs/proposals/cold-start-latency-slice2.md §3.4, §11.
func (s *Scanner) QueryAggregate(
	ctx context.Context,
	resourceARN string,
	metricName string,
	window time.Duration,
	stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	if !s.monitoringClientReady() {
		return scanner.AggregateMetricResult{
			ResourceARN: resourceARN,
			MetricName:  metricName,
			Window:      window,
			Statistic:   stat,
		}, scanner.ErrMetricNotImplemented
	}

	switch metricName {
	case OCIFunctionsFunctionDurationMetric,
		OCIFunctionsColdStartCountMetric,
		OCIFunctionsInvocationCountMetric,
		OCIFunctionsErrorResponseCountMetric:
		// Supported — fall through to the real call. Sampling rate
		// slice 1 chunk 1 (v0.89.122) adds the InvocationCount
		// entry; error rate slice 1 chunk 1 (v0.89.127) adds the
		// InvocationCountError suffix variant.
	default:
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

	// Parse resource ARN to extract compartment + function OCID.
	// OCI Functions snapshots from scanner_functions.go carry the
	// full function OCID (ocid1.fnfunc.oc1.<region>.<unique>) as
	// the ResourceARN; the compartment OCID is required separately
	// by the Monitoring API. We accept either form for resilience:
	// a bare OCID is treated as the function OCID and the
	// compartment falls back to the scanner's TenancyOCID
	// (slice-2-substrate behaviour — slice 3 may join against the
	// Functions snapshot table for the real compartment).
	compartmentID, functionOCID, err := parseOCIFunctionARN(resourceARN, s.TenancyOCID)
	if err != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("parse function ARN: %w", err)
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)

	// Resolve the metric name + dimension filter the MQL query
	// expression carries. The synthetic-suffix variants
	// (OCIFunctionsErrorResponseCountMetric) strip the
	// "#error" suffix to recover the base metric name and add a
	// result = "error" tag to the resource filter. The base
	// (non-suffix) variants pass through verbatim.
	baseMetric, _ := splitOCIMetricSuffix(metricName)

	var query string
	switch metricName {
	case OCIFunctionsFunctionDurationMetric:
		query = fmt.Sprintf(
			"%s[%s]{resourceId = %q}.percentile(0.95)",
			baseMetric, ociWindowQuery(window), functionOCID,
		)
	case OCIFunctionsColdStartCountMetric, OCIFunctionsInvocationCountMetric:
		// Both counter metrics use .sum() — the per-period
		// datapoints already carry the per-resolution count, and
		// the substrate's cross-period rollup below also SUMs.
		query = fmt.Sprintf(
			"%s[%s]{resourceId = %q}.sum()",
			baseMetric, ociWindowQuery(window), functionOCID,
		)
	case OCIFunctionsErrorResponseCountMetric:
		// Error-response count. OCI's FunctionResponseCount metric
		// counts requests that returned an error response (error code
		// + 429 throttles), so it IS the error numerator directly —
		// no result-tag filter needed (the prior code synthesised a
		// result = "error" tag on FunctionInvocationCount, which is
		// not a valid oci_faas dimension). .sum() rollup matches the
		// sampling-rate denominator path.
		query = fmt.Sprintf(
			"%s[%s]{resourceId = %q}.sum()",
			baseMetric, ociWindowQuery(window), functionOCID,
		)
	}

	points, err := s.monitoringClient.SummarizeMetricsData(
		ctx, compartmentID, OCIFunctionsMetricNamespace, query, startTime, endTime,
	)
	if err != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("summarize metrics: %w", err)
	}

	result := scanner.AggregateMetricResult{
		ResourceARN: resourceARN,
		MetricName:  metricName,
		Window:      window,
		Statistic:   stat,
		ObservedAt:  endTime,
	}
	if len(points) == 0 {
		// Empty response - return zero with no error per the
		// MetricQuerier contract. The cold-start detection branch
		// reads SampleCount=0 and skips the comparison.
		return result, nil
	}

	// Aggregate: function_duration uses MAX (worst-case 5-minute
	// P95 across the window — mirrors the CloudWatch substrate's
	// per-period rollup), cold_start_count uses SUM (total cold
	// starts across the window).
	val := 0.0
	totalSamples := 0
	switch metricName {
	case OCIFunctionsFunctionDurationMetric:
		for _, p := range points {
			if p.Value > val {
				val = p.Value
			}
			totalSamples += p.SampleCount
		}
	case OCIFunctionsColdStartCountMetric,
		OCIFunctionsInvocationCountMetric,
		OCIFunctionsErrorResponseCountMetric:
		// Counter rollup: SUM across periods = total events
		// across the window. Mirrors the cold_start_count path —
		// the invocation count uses the same MQL .sum() reduction
		// and the same per-period SUM aggregation. The error
		// suffix variant follows the same path because the OCI
		// API has already done the result="error" filter at the
		// MQL layer (see splitOCIMetricSuffix); the substrate
		// just sums what comes back.
		for _, p := range points {
			val += p.Value
			totalSamples += p.SampleCount
		}
	}

	result.Value = val
	result.SampleCount = totalSamples
	result.Unit = ociMetricUnitMs
	return result, nil
}

// monitoringClientReady returns true when the Scanner has been
// wired with a MonitoringClient. The nil-tolerant gate keeps the
// validation-only Scanner constructions (which don't need to make
// Monitoring API calls) producing the chunk-1 skeleton's
// ErrMetricNotImplemented sentinel.
func (s *Scanner) monitoringClientReady() bool {
	return s.monitoringClient != nil
}

// parseOCIFunctionARN extracts the (compartmentID, functionOCID)
// pair from a Function ARN. OCI Functions snapshots carry the
// full function OCID as the ResourceARN; the compartment is not
// embedded in the OCID itself, so the caller's TenancyOCID is
// used as the fallback. A subsequent slice can join against the
// Functions snapshot row's CompartmentID field once that data
// is wired through.
//
// Accepts either:
//
//   - A bare function OCID (ocid1.fnfunc.oc1...) → compartment
//     = fallbackCompartment, function = the OCID.
//   - A "compartment|function" tuple-encoded ARN (the chunk-4
//     detection branch may pre-pack this) → compartment / function
//     split on the | character.
//
// Returns an error when the ARN is empty or not OCID-shaped.
func parseOCIFunctionARN(arn, fallbackCompartment string) (string, string, error) {
	if arn == "" {
		return "", "", errors.New("empty resource ARN")
	}
	if strings.Contains(arn, "|") {
		parts := strings.SplitN(arn, "|", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("invalid pipe-encoded ARN: %q", arn)
		}
		return parts[0], parts[1], nil
	}
	if !strings.HasPrefix(arn, "ocid1.") {
		return "", "", fmt.Errorf("not an OCI OCID: %q", arn)
	}
	return fallbackCompartment, arn, nil
}

// splitOCIMetricSuffix is the v0.89.127 (error rate slice 1 chunk 1)
// helper that decodes the Squadron-internal synthetic-suffix metric
// name convention into the (base metric name, MQL dimension filter)
// pair the QueryAggregate routing assembles into the final MQL
// expression.
//
// The convention follows the GCP per-cloud counterpart
// (CloudRunRequestCount5xxMetricType / CloudFunctionsExecutionCountError
// MetricType): a "#<tag>" suffix on the metric name is consumed by
// the Squadron router rather than passed on the wire, and the
// router translates the tag into a per-API dimension filter
// expression.
//
// Slice 1 ships exactly one suffix variant: "#error" on
// function_invocation_count. Additional suffix variants land in
// future slices through this same helper; keeping the helper as a
// table-driven switch lets us add new variants without growing the
// QueryAggregate switch.
//
// Returns ("function_invocation_count", `result = "error"`) for
// the OCIFunctionsErrorResponseCountMetric input; returns the
// input verbatim with an empty filter for any non-suffix metric.
func splitOCIMetricSuffix(metricName string) (baseMetric, dimensionFilter string) {
	// No synthetic-suffix variants remain: the error path now uses the
	// real FunctionResponseCount metric directly. Kept as a pass-through
	// seam for any future tag-filtered variants.
	return metricName, ""
}

// ociWindowQuery formats the MQL window suffix. OCI's MQL accepts
// a window in the form "<n>h" (hours), "<n>m" (minutes), or
// "<n>d" (days). The slice-2 substrate uses hour resolution
// uniformly — 24h current + 168h baseline — so the helper rounds
// up to the next hour for non-hour-aligned windows.
//
// Examples:
//
//	24*time.Hour          → "24h"
//	168*time.Hour         → "168h"
//	30*time.Minute        → "1h" (rounded up — MQL doesn't
//	                       accept sub-hour windows on the
//	                       summarizeMetricsData endpoint)
func ociWindowQuery(window time.Duration) string {
	hours := int(window / time.Hour)
	if window%time.Hour > 0 {
		hours++
	}
	if hours < 1 {
		hours = 1
	}
	return fmt.Sprintf("%dh", hours)
}

// WithMonitoringClient wires the MonitoringClient (or a test fake
// satisfying MonitoringClient) into the Scanner. v0.89.118.
// Returns the Scanner so the constructor chain composes:
//
//	s := NewScannerFromConnection(conn, key).WithMonitoringClient(mc)
//
// Nil clients are accepted — the QueryAggregate path treats a nil
// monitoringClient as the chunk-1 skeleton (returns
// scanner.ErrMetricNotImplemented), preserving the v0.89.113
// surface when callers explicitly want to opt out.
func (s *Scanner) WithMonitoringClient(mc MonitoringClient) *Scanner {
	s.monitoringClient = mc
	return s
}

// WithMonitoringRateLimiter overrides the per-Scanner monitoring
// rate limiter. v0.89.118. Reserved for tests that need to pin the
// limiter's burst to a specific value to deterministically time
// the 10-TPS pin (TestOCIRateLimiterCapsAt10TPS), or to disable
// it entirely (a nil limiter short-circuits the Wait call).
// Production never calls this — the constructors pre-arm the
// limiter at the substrate-default TPS.
func (s *Scanner) WithMonitoringRateLimiter(limiter *rate.Limiter) *Scanner {
	s.metricsLimiter = limiter
	return s
}

// WithColdStartStore wires the cold-start observation storage
// adapter into the Scanner. v0.89.118 — mirrors the AWS scanner's
// chunk-2 setter so the chunk-4 detection branch can persist
// observations through the shared interface.
func (s *Scanner) WithColdStartStore(store ColdStartStore) *Scanner {
	s.coldStartStore = store
	return s
}

// WithConnectionID overrides the connection identifier used to
// scope persisted cold-start observations. v0.89.118 — mirrors
// the AWS scanner's chunk-2 setter.
func (s *Scanner) WithConnectionID(id string) *Scanner {
	s.connectionID = id
	return s
}

// defaultMonitoringLimiter returns a fresh OCI Monitoring rate
// limiter at the substrate-default TPS. Exposed as a helper so
// the constructors single-source the burst=1 + rate=10 pair.
// burst=1 forces every request to acquire a token rather than
// coalescing into a burst window.
func defaultMonitoringLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Limit(OCIMonitoringRateLimitTPS), 1)
}

// signedMonitoringClient is the production MonitoringClient
// implementation. Wraps the OCI Monitoring summarizeMetricsData
// REST endpoint with RSA-signed-POST plumbing. Lives on the
// Scanner directly so the SigningKey + http.Client + region are
// shared with the existing Functions walk.
type signedMonitoringClient struct {
	scanner *Scanner
}

// SummarizeMetricsData implements MonitoringClient against the
// real OCI Monitoring API. Builds the POST body, signs the
// request per the OCI HTTP Signatures spec (POST adds
// content-length / content-type / x-content-sha256 to the
// signing string per the spec), dispatches it, and parses the
// response.
//
// Production callers obtain this implementation via
// NewSignedMonitoringClient(scanner). The slice-2 chunk-3
// substrate currently exports it for the production wiring
// path; tests substitute an in-memory fake satisfying
// MonitoringClient directly.
func (c *signedMonitoringClient) SummarizeMetricsData(
	ctx context.Context,
	compartmentID, namespace, query string,
	startTime, endTime time.Time,
) ([]ociMetricDataPoint, error) {
	sk, err := c.scanner.signingKey()
	if err != nil {
		return nil, fmt.Errorf("oci monitoring: signing key: %w", err)
	}

	body := ociSummarizeRequestBody{
		Query:      query,
		Namespace:  namespace,
		StartTime:  startTime.UTC().Format(time.RFC3339),
		EndTime:    endTime.UTC().Format(time.RFC3339),
		Resolution: fmt.Sprintf("%dm", ociMonitoringResolutionSeconds/60),
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("oci monitoring: marshal body: %w", err)
	}

	endpoint := c.scanner.monitoringEndpoint()
	u := fmt.Sprintf(
		"%s/%s/metricData/actions/summarizeMetricsData?compartmentId=%s",
		strings.TrimRight(endpoint, "/"),
		ociMonitoringAPIVersion,
		compartmentID,
	)

	respBody, err := c.scanner.doSignedPOST(ctx, sk, u, bodyBytes)
	if err != nil {
		return nil, err
	}

	var resp ociSummarizeResponse
	if jerr := json.Unmarshal(respBody, &resp); jerr != nil {
		return nil, fmt.Errorf("oci monitoring: parse response: %w", jerr)
	}

	var out []ociMetricDataPoint
	for _, item := range resp.Items {
		for _, dp := range item.AggregatedDatapoints {
			ts, _ := time.Parse(time.RFC3339, dp.Timestamp)
			out = append(out, ociMetricDataPoint{
				Timestamp:   ts,
				Value:       dp.Value,
				SampleCount: 1,
			})
		}
	}
	return out, nil
}

// NewSignedMonitoringClient returns a production MonitoringClient
// wrapping the supplied Scanner's signing material + http client.
// The chunk-3 wiring path constructs this once per Scanner and
// passes it through WithMonitoringClient; tests substitute a fake
// directly via WithMonitoringClient and never call this helper.
func NewSignedMonitoringClient(s *Scanner) MonitoringClient {
	return &signedMonitoringClient{scanner: s}
}

// monitoringEndpoint returns the OCI Monitoring API base URL.
// When ociEndpoint is set (tests), it's used directly. In
// production the per-region monitoring endpoint pattern is
// https://telemetry.<region>.oraclecloud.com.
func (s *Scanner) monitoringEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://telemetry.%s.oraclecloud.com", s.Region)
}

// doSignedPOST signs and dispatches a single POST request with the
// supplied JSON body, returning the response body on success or
// an error on any non-2xx status / transport error. Mirrors
// doSignedGET / doSignedGETWithPage but signs the POST-shaped
// headers per the OCI HTTP Signatures spec.
//
// The signing surface for POST adds three headers to the GET
// surface: content-length, content-type, and x-content-sha256.
// All three are computed from the body bytes; x-content-sha256
// is the base64-encoded SHA-256 of the body.
func (s *Scanner) doSignedPOST(ctx context.Context, sk *SigningKey, u string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("oci monitoring: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	if signErr := sk.SignPOSTRequest(req, body); signErr != nil {
		return nil, fmt.Errorf("oci monitoring: sign request: %w", signErr)
	}

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("oci monitoring: dispatch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := readAllLimited(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return respBody, nil
	}

	var oerr ociErrorBody
	_ = json.Unmarshal(respBody, &oerr)
	return nil, fmt.Errorf("oci monitoring: HTTP %d: %s", resp.StatusCode, truncate(oerr.Message, 200))
}
