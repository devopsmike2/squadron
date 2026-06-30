// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// AzureMonitorRateLimitRPH pins the per-subscription rate limit the
// slice 2 substrate enforces for Azure Monitor /metrics calls. The
// Azure Monitor documented quota is well above 12,000 requests per
// hour at the subscription scope; the substrate sits comfortably
// under that ceiling at 12K RPH (200 RPM, ~3.33 RPS) so multi-
// instance Squadron deployments scanning the same subscription stay
// below the shared throttle.
//
// Pinned by metrics_test.go::TestAzureMonitorRateLimitRPH_Constant.
//
// See docs/proposals/cold-start-latency-slice2.md §12 (threat model:
// Azure Monitor 12K RPH).
const AzureMonitorRateLimitRPH = 12000

// AzureMonitorMetricsAPIVersion pins the ARM API version the slice 2
// substrate uses for the microsoft.insights/metrics endpoint. The
// 2024-02-01 surface returns the timeseries[].data[] aggregation
// shape the substrate consumes; the filter= query parameter for
// dimension filtering (IsAfterColdStart eq 'true') is supported at
// this version.
//
// Pinned by metrics_test.go::TestAzureMonitorMetricsAPIVersion_Constant.
const AzureMonitorMetricsAPIVersion = "2024-02-01"

// AzureFunctionsExecutionDurationMetric is the Azure Monitor metric
// name for per-function execution duration. Slice 2 filters by
// IsAfterColdStart=true dimension to isolate cold-start invocations
// when the runtime version supports it (2023+ runtime versions emit
// the dimension; older runtimes fall back to unfiltered with an
// informational note per design doc §3.3).
//
// Pinned by metrics_test.go::TestAzureFunctionsExecutionDurationMetric_Constant.
// AVAILABILITY WARNING: Azure Monitor has no native per-function duration
// metric — duration requires Application Insights (requests/duration). This
// name returns empty, so Azure cold-start detection never fires. Deferred per
// docs/audit/detection-metric-availability.md.
const AzureFunctionsExecutionDurationMetric = "FunctionExecutionDuration"

// AzureFunctionsIsAfterColdStartDimension is the dimension name Azure
// Functions emits to distinguish cold-start vs warm invocations.
// Available in 2023+ runtime versions; older runtimes don't emit it
// — slice 2 detects absence (Azure Monitor returns 400 BadRequest
// with the dimension name in the error message) and retries
// unfiltered, signalling the fallback through the fellBack return
// value on queryAzureMetricWithFallback.
//
// Pinned by metrics_test.go::TestAzureFunctionsIsAfterColdStartDimension_Constant.
const AzureFunctionsIsAfterColdStartDimension = "IsAfterColdStart"

// AzureFunctionsInvocationsMetric is the Azure Monitor metric name
// for Azure Functions per-function invocation count. Sampling rate
// analysis slice 1 chunk 1 (v0.89.122) uses this as the denominator
// for the observed_span_count / expected_invocation_count ratio per
// docs/proposals/sampling-rate-analysis-slice1.md §4.4.
//
// Aggregation is "Total" (sum) rather than the "Maximum"
// approximation the FunctionExecutionDuration path uses — Azure
// Monitor natively supports Sum/Total on this counter. The
// QueryAggregate routing branches on the metric name to pick the
// appropriate aggregation; IAM stays unchanged (the existing Azure
// Reader role covers both metrics).
//
// Pinned to "FunctionInvocations" by
// metrics_test.go::TestAzureFunctionsInvocationsMetric_Constant.
// AVAILABILITY WARNING: the real Azure Monitor invocation metric is
// FunctionExecutionCount; "FunctionInvocations" does not exist. See
// docs/audit/detection-metric-availability.md (rename deferred with the Azure
// error/duration data-source decision).
const AzureFunctionsInvocationsMetric = "FunctionInvocations"

// AzureFunctionsErrorsMetric is the Azure Monitor metric name for
// per-function error count. Error rate correlation slice 1 chunk 1
// (v0.89.127) uses this as the numerator for the
// current_error_count / current_invocation_count ratio per
// docs/proposals/error-rate-correlation-slice1.md §4.4.
//
// Aggregation is "Total" (sum) — Azure Monitor natively supports
// Sum/Total on this counter. Mirrors the FunctionInvocations
// aggregation choice; the QueryAggregate routing branches on the
// metric name to pick the appropriate aggregation, and skips the
// IsAfterColdStart dimension filter (the error count wants ALL
// failed invocations, not just cold-start ones).
//
// IAM stays unchanged from cold-start slice 2 + sampling rate slice
// 1: the existing Azure Reader role covers all three metrics. The
// per-subscription rate limiter stays UNCHANGED — the new metric
// query flows through the existing 12K RPH limiter.
//
// Pinned to "FunctionErrors" by
// metrics_test.go::TestAzureFunctionsErrorsMetric_Constant.
// AVAILABILITY WARNING: Azure Monitor has no native per-function error metric —
// errors require Application Insights (requests/failed). This name returns
// empty. Deferred per docs/audit/detection-metric-availability.md.
const AzureFunctionsErrorsMetric = "FunctionErrors"

// Application Insights standard metric names (#153 enterprise-gate).
// Unlike the Azure Monitor Functions metrics above — which do not
// carry per-function duration or error signals — these live on the
// Application Insights component resource (microsoft.insights/components)
// and are queryable through the same Azure Monitor /metrics path with
// the same SP token, provided the resource ARN passed to QueryAggregate
// is the Application Insights component (not the Function App). They
// carry the real cold-start latency + error signals when the operator
// has the paid Application Insights add-on enabled.
const (
	// AppInsightsRequestDurationMetric is the App Insights server
	// request duration (ms); the cold-start regression detector reads
	// its P95 when the commercial gate is on.
	AppInsightsRequestDurationMetric = "requests/duration"
	// AppInsightsRequestsCountMetric is the App Insights total server
	// request count — the error-rate denominator (all requests).
	AppInsightsRequestsCountMetric = "requests/count"
	// AppInsightsRequestsFailedMetric is the App Insights failed server
	// request count — the error-rate numerator.
	AppInsightsRequestsFailedMetric = "requests/failed"
)

// coldStartDurationMetric returns the duration metric name the
// cold-start detector should query: the inert Functions metric in OSS
// (never fires) or the Application Insights request-duration metric
// when the commercial gate is on (#153).
func (s *Scanner) coldStartDurationMetric() string {
	if s.commercialDetectors {
		return AppInsightsRequestDurationMetric
	}
	return AzureFunctionsExecutionDurationMetric
}

// errorTotalMetric / errorFailedMetric return the (denominator,
// numerator) metric names the error-rate detector should query —
// inert Functions metrics in OSS, Application Insights request
// count/failed when the commercial gate is on (#153).
func (s *Scanner) errorTotalMetric() string {
	if s.commercialDetectors {
		return AppInsightsRequestsCountMetric
	}
	return AzureFunctionsInvocationsMetric
}

func (s *Scanner) errorFailedMetric() string {
	if s.commercialDetectors {
		return AppInsightsRequestsFailedMetric
	}
	return AzureFunctionsErrorsMetric
}

// ServiceBusDeadletteredMessagesMetric is the Azure Monitor metric
// name for Service Bus dead-lettered message count. Poison-rate
// substrate slice 4 chunk 3a (v0.89.179) reads this on the
// Microsoft.ServiceBus/namespaces resource to compute the real
// poison-message rate that slice 3 shipped as a §3.3 honest-framing
// absent sentinel.
//
// DeadletteredMessages is a GAUGE (current count of dead-lettered
// messages), not an arrival counter like the AWS SQS / GCP Cloud
// Tasks metrics. So the poison RATE is derived as the positive delta
// (max - min) over the window — the net dead-letter accumulation,
// the Azure analog of the slice-3 design's "DeadletteredMessages
// delta via Azure Monitor metrics."
//
// Chunk 3a queries this at the NAMESPACE resource (aggregated across
// all queues/topics in the namespace) — closing §3.3
// substrate-metric-dependence. PER-QUEUE attribution (the §3.2
// scanner-coverage-gap: the metric's EntityName dimension requires
// walking the per-queue sub-resource the namespace-level scanner
// does not enumerate) is deferred to chunk 3b. The same
// microsoft.insights metrics read the cold-start substrate already
// uses covers this — no new IAM.
//
// Pinned by servicebus_poison_rate_substrate_test.go::
// TestServiceBusDeadletteredMessagesMetric_Constant.
const ServiceBusDeadletteredMessagesMetric = "DeadletteredMessages"

// azureMonitorMetricsAPIBase is the Azure Resource Manager path
// segment under which microsoft.insights/metrics is exposed against
// a Microsoft.Web/sites resource. The full URL is:
//
//	{armEndpoint}/{resourceARN}/providers/microsoft.insights/metrics?...
//
// The resourceARN is the load-bearing ARM resource id (the full
// /subscriptions/.../sites/{name} path); the metrics endpoint nests
// underneath it as a sub-resource via the providers/microsoft.insights
// indirection.
const azureMonitorMetricsAPIBase = "providers/microsoft.insights/metrics"

// azureMonitorMetricsInterval pins the timeseries interval (PT5M =
// 5 minutes). Mirrors the AWS substrate's
// cloudWatchMetricPeriodSeconds=300 — the cross-window math (24h
// current vs 168h baseline) lines up at the same 5-minute
// granularity so per-cloud baseline comparisons remain apples-to-
// apples.
const azureMonitorMetricsInterval = "PT5M"

// azureMonitorAggregationForP95 is the Azure Monitor aggregation
// parameter the slice 2 substrate uses when the caller asks for
// StatisticP95. Azure Monitor does NOT natively support percentile
// aggregations on most metrics — FunctionExecutionDuration is in
// the "no percentile" bucket. The substrate maps P95 to
// "Maximum" as the closest approximation: cold-starts are the
// long-tail values that pull the maximum up, so a 5-minute Maximum
// across the window is the closest signal the operator can get to
// "the worst cold-start the function experienced" without per-
// execution trace correlation (a slice 3 candidate).
//
// Documented in code comments here and in the chunk 5 operator
// runbook (deferral). The approximation is acknowledged honestly
// — the cold-start detection branch reasons about "max 5-minute
// aggregate" rather than claiming a true P95.
const azureMonitorAggregationForP95 = "Maximum"

// azureMonitorUnitMilliseconds is the canonical Azure Monitor unit
// string for duration metrics. FunctionExecutionDuration is reported
// in milliseconds; the substrate preserves the unit on the
// AggregateMetricResult.Unit field so downstream consumers can
// normalize against the absolute floor.
const azureMonitorUnitMilliseconds = "Milliseconds"

// azureMonitorFallbackUnitSuffix is the marker the substrate appends
// to the Unit string when the IsAfterColdStart dimension filter
// fell back to unfiltered. The detection branch reads this suffix
// to populate the snapshot's UsedFallback bool — keeping the signal
// in-band on the AggregateMetricResult avoids growing the
// MetricQuerier interface's return shape just for the Azure-only
// fallback case. See queryAzureMetricWithFallback godoc.
const azureMonitorFallbackUnitSuffix = " (fallback)"

// QueryAggregate implements scanner.MetricQuerier for Azure via the
// Azure Monitor REST API at api-version=2024-02-01. Slice 2 chunk 2
// (v0.89.118) wires the real Azure Monitor call, the per-
// subscription rate limiter, the IsAfterColdStart dimension filter
// with fallback for older runtimes, and the empty-result-set
// semantics the MetricQuerier interface contract specifies.
//
// Routing per design doc §3.3 + §5:
//
//   - metricName == AzureFunctionsExecutionDurationMetric → real
//     Azure Monitor call against the supplied resourceARN's metrics
//     sub-resource, filtered by IsAfterColdStart=true. On a 400
//     BadRequest naming the dimension, the call retries unfiltered
//     and tags the Unit with " (fallback)" so the detection branch
//     can record the operator-visible note.
//   - Any other metricName → slice 2 supports
//     FunctionExecutionDuration only; returns an empty
//     AggregateMetricResult with SampleCount=0 and no error. The
//     chunk-2 detection branch is the only caller in slice 2, and
//     it always asks for FunctionExecutionDuration; the empty-result
//     branch keeps the interface contract honest for future slices
//     that may probe additional metric names speculatively.
//   - accessToken empty (the Scanner was constructed without the
//     chunk-2 wiring — historical scanner-struct paths in tests
//     that build Scanners directly without calling WithAccessToken)
//     → returns scanner.ErrMetricNotImplemented, mirroring the
//     chunk-1 skeleton's surface so callers can errors.Is-detect
//     the unwired path.
//
// Empty datapoint handling: when Azure Monitor returns
// value[0].timeseries=[] or all timeseries[].data[] are empty,
// the function returns Value=0, SampleCount=0, no error. Callers
// MUST check SampleCount before reading Value when distinguishing
// "value is genuinely 0" from "no datapoints existed".
//
// Rate limiter: a Wait call against the per-Scanner metricsLimiter
// precedes every Azure Monitor call, capping the per-subscription
// RPH at AzureMonitorRateLimitRPH.
//
// See docs/proposals/cold-start-latency-slice2.md §3.3, §11.
func (s *Scanner) QueryAggregate(
	ctx context.Context,
	resourceARN string,
	metricName string,
	window time.Duration,
	stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	if s.accessToken == "" {
		// Surfaces the chunk-1 skeleton sentinel so callers that
		// haven't wired the access token (validation-only Scanners,
		// partially-constructed test fixtures) observe the same
		// shape as v0.89.113.
		return scanner.AggregateMetricResult{
			ResourceARN: resourceARN,
			MetricName:  metricName,
			Window:      window,
			Statistic:   stat,
		}, scanner.ErrMetricNotImplemented
	}

	// Sampling rate slice 1 chunk 1 (v0.89.122): FunctionInvocations
	// is the second supported Azure Functions metric. The
	// QueryAggregate routing branches on the metric name to pick
	// the correct Azure Monitor aggregation parameter — Total
	// (sum) for the counter vs. Maximum for the duration metric —
	// and skips the IsAfterColdStart dimension filter (the
	// invocation count denominator wants ALL invocations, not just
	// cold-start ones).
	if metricName == AzureFunctionsInvocationsMetric || metricName == AppInsightsRequestsCountMetric {
		// #153: the App Insights requests/count denominator shares the
		// SUM-rollup, no-dimension-filter shape with FunctionInvocations,
		// so it reuses the same counter-total helper.
		return s.queryAzureFunctionCounterTotal(ctx, resourceARN, metricName, window, stat)
	}

	// Error rate correlation slice 1 chunk 1 (v0.89.127):
	// FunctionErrors is the third supported Azure Functions metric.
	// Same Azure-side shape as FunctionInvocations (aggregation =
	// "Total", no IsAfterColdStart filter — the error count wants
	// ALL failed invocations, not just cold-start ones), so the
	// underlying queryAzureFunctionCounterTotal helper is reused.
	if metricName == AzureFunctionsErrorsMetric || metricName == AppInsightsRequestsFailedMetric {
		// #153: the App Insights requests/failed numerator shares the
		// SUM-rollup shape with FunctionErrors.
		return s.queryAzureFunctionCounterTotal(ctx, resourceARN, metricName, window, stat)
	}

	// Poison-rate substrate slice 4 chunk 3a (v0.89.179): Service Bus
	// DeadletteredMessages on the namespace resource. Routes into a
	// dedicated delta helper — the metric is a gauge, so the rate is
	// max(Maximum) - min(Minimum) over the window rather than a SUM or
	// MAX rollup. See queryServiceBusDeadletterDelta.
	if metricName == ServiceBusDeadletteredMessagesMetric {
		return s.queryServiceBusDeadletterDelta(ctx, resourceARN, window, stat)
	}

	if metricName != AzureFunctionsExecutionDurationMetric && metricName != AppInsightsRequestDurationMetric {
		// Slice 2 substrate scope: FunctionExecutionDuration +
		// FunctionInvocations + FunctionErrors, plus the #153
		// App Insights requests/duration (which shares the
		// Maximum-aggregation duration query shape). Other names short-
		// circuit to an empty result with no error so the
		// interface contract distinguishes "metric not supported
		// in slice 1" (empty result) from "API call failed"
		// (non-nil error).
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
	aggregation := mapMetricStatisticToAzureAggregation(stat)

	// Filter on IsAfterColdStart=true first; on a 400 naming the
	// dimension, fall back to unfiltered and signal via fellBack.
	coldStartFilter := fmt.Sprintf("%s eq 'true'", AzureFunctionsIsAfterColdStartDimension)

	result, fellBack, err := s.queryAzureMetricWithFallback(
		ctx, resourceARN, metricName, startTime, endTime, aggregation, coldStartFilter,
	)
	if err != nil {
		return scanner.AggregateMetricResult{}, err
	}

	// Echo caller's input on the result so downstream consumers
	// (the cold-start detection branch) see the same shape as the
	// AWS substrate.
	result.ResourceARN = resourceARN
	result.MetricName = metricName
	result.Window = window
	result.Statistic = stat
	result.ObservedAt = endTime

	if fellBack {
		// The function runtime doesn't emit IsAfterColdStart
		// dimension. Fell back to unfiltered query. The detection
		// branch will record an informational note via the
		// snapshot detail — the fellBack signal flows through the
		// Unit field's " (fallback)" suffix.
		result.Unit = result.Unit + azureMonitorFallbackUnitSuffix
	}

	return result, nil
}

// queryAzureFunctionCounterTotal is the shared Azure Monitor
// counter aggregation path. v0.89.127 (error rate slice 1 chunk 1)
// generalizes the original sampling-rate-only
// queryAzureFunctionInvocations helper so both FunctionInvocations
// and FunctionErrors flow through the same code with the metric
// name as a parameter.
//
// Reuses the per-subscription rate limiter + the
// doAzureMonitorMetricsCall helper but uses aggregation="Total"
// rather than "Maximum" and does NOT apply the IsAfterColdStart
// dimension filter (the counter denominators want every
// invocation/error in the window, cold-start or warm). The
// cross-period rollup also switches from MAX (latency) to SUM
// (counter).
//
// Empty datapoint handling matches the duration path: zero events
// in the window returns Value=0 / SampleCount=0 with no error.
// The chunk-2 detection branches gate on the per-metric absolute
// floor (>= 1000 invocations / >= 50 errors per design doc §3) so
// the detection skips on empty responses.
//
// See docs/proposals/error-rate-correlation-slice1.md §4.4 and
// docs/proposals/sampling-rate-analysis-slice1.md §4.4.
func (s *Scanner) queryAzureFunctionCounterTotal(
	ctx context.Context,
	resourceARN, metricName string,
	window time.Duration,
	stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	if s.metricsLimiter != nil {
		if err := s.metricsLimiter.Wait(ctx); err != nil {
			return scanner.AggregateMetricResult{}, fmt.Errorf("rate limit: %w", err)
		}
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)

	// "Total" = sum across the timespan per Azure Monitor docs.
	// Different aggregation than mapMetricStatisticToAzureAggregation
	// returns (which always maps percentiles to "Maximum" for the
	// duration metric); for the counters, "Total" is the native
	// fit.
	const aggregation = "Total"

	out, callErr := s.doAzureMonitorMetricsCall(
		ctx, resourceARN, metricName,
		startTime, endTime, aggregation, "",
	)
	if callErr != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("azure monitor metrics: %w", callErr)
	}

	result := aggregateAzureTimeseriesSum(out, aggregation)
	result.ResourceARN = resourceARN
	result.MetricName = metricName
	result.Window = window
	result.Statistic = stat
	result.ObservedAt = endTime
	return result, nil
}

// queryServiceBusDeadletterDelta computes the Service Bus poison-rate
// signal from the DeadletteredMessages gauge. Poison-rate substrate
// slice 4 chunk 3a (v0.89.179).
//
// DeadletteredMessages is a point-in-time count of dead-lettered
// messages, not an arrival counter. To express a per-hour RATE
// consistent with the AWS SQS / GCP Cloud Tasks chunks (messages
// entering the poison state per window), this queries both the
// Maximum and Minimum aggregations over the window in one Azure
// Monitor call (aggregation="Maximum,Minimum") and returns the
// positive delta max(Maximum) - min(Minimum), floored at 0 — the net
// dead-letter accumulation across the window.
//
// Semantics note (documented honestly in §3 of the design doc): the
// delta measures NET accumulation, not standing backlog. A namespace
// holding a constant 100 dead-lettered messages with no new arrivals
// reports rate 0 (no NEW poison this hour) — correct for a rate, and
// distinct from a depth signal a future slice could add separately.
//
// Empty timeseries → SampleCount=0 (the absent-sentinel signal the
// DetectServiceBusPoisonRate caller checks). A non-empty series with
// a flat gauge → delta 0 with SampleCount>0 (a real "zero new
// dead-letters" reading).
//
// See docs/proposals/poison-rate-substrate-slice4.md §3.
func (s *Scanner) queryServiceBusDeadletterDelta(
	ctx context.Context,
	resourceARN string,
	window time.Duration,
	stat scanner.MetricStatistic,
) (scanner.AggregateMetricResult, error) {
	if s.metricsLimiter != nil {
		if err := s.metricsLimiter.Wait(ctx); err != nil {
			return scanner.AggregateMetricResult{}, fmt.Errorf("rate limit: %w", err)
		}
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)

	out, callErr := s.doAzureMonitorMetricsCall(
		ctx, resourceARN, ServiceBusDeadletteredMessagesMetric,
		startTime, endTime, "Maximum,Minimum", "",
	)
	if callErr != nil {
		return scanner.AggregateMetricResult{}, fmt.Errorf("azure monitor metrics: %w", callErr)
	}

	result := scanner.AggregateMetricResult{
		ResourceARN: resourceARN,
		MetricName:  ServiceBusDeadletteredMessagesMetric,
		Window:      window,
		Statistic:   stat,
		ObservedAt:  endTime,
	}
	if out == nil || len(out.Value) == 0 {
		return result, nil
	}

	var maxVal, minVal float64
	haveMax, haveMin := false, false
	sampleCount := 0
	for _, ts := range out.Value[0].Timeseries {
		for _, dp := range ts.Data {
			if mx, ok := extractAggregateValue(dp, "Maximum"); ok {
				sampleCount++
				if !haveMax || mx > maxVal {
					maxVal, haveMax = mx, true
				}
			}
			if mn, ok := extractAggregateValue(dp, "Minimum"); ok {
				if !haveMin || mn < minVal {
					minVal, haveMin = mn, true
				}
			}
		}
	}
	if sampleCount == 0 {
		return result, nil
	}
	delta := maxVal - minVal
	if delta < 0 {
		delta = 0
	}
	result.Value = delta
	result.SampleCount = sampleCount
	return result, nil
}

// AzureServiceBusEntityNameDimension is the Azure Monitor dimension
// name used to split Service Bus metrics per entity (queue / topic).
// Poison-rate substrate slice 4 chunk 3b (v0.89.180) filters with
// "EntityName eq '*'" to retrieve one timeseries per queue in a
// single call — closing the §3.2 scanner-coverage-gap via the metric
// dimension the gap named, with no separate ARM queue enumeration.
const AzureServiceBusEntityNameDimension = "EntityName"

// queryServiceBusDeadletterPerEntity returns each Service Bus entity's
// net dead-letter accumulation (max(Maximum) - min(Minimum) delta,
// floored at 0) over the window, keyed by entity (queue / topic)
// name. Poison-rate substrate slice 4 chunk 3b (v0.89.180).
//
// Issues ONE Azure Monitor call with $filter="EntityName eq '*'" and
// aggregation="Maximum,Minimum"; Azure returns one timeseries per
// entity, each tagged via metadatavalues with its entityname. This
// closes §3.2 (per-queue attribution) using the EntityName dimension
// directly — no per-queue ARM walk.
//
// Entities with no parsable name, or no datapoints, are skipped. An
// empty map means Azure returned no per-entity series (the caller
// falls back to the namespace-level chunk-3a reading).
//
// See docs/proposals/poison-rate-substrate-slice4.md §3.
func (s *Scanner) queryServiceBusDeadletterPerEntity(
	ctx context.Context,
	resourceARN string,
	window time.Duration,
) (map[string]int, error) {
	if s.metricsLimiter != nil {
		if err := s.metricsLimiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limit: %w", err)
		}
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-window)
	filter := fmt.Sprintf("%s eq '*'", AzureServiceBusEntityNameDimension)

	out, callErr := s.doAzureMonitorMetricsCall(
		ctx, resourceARN, ServiceBusDeadletteredMessagesMetric,
		startTime, endTime, "Maximum,Minimum", filter,
	)
	if callErr != nil {
		return nil, fmt.Errorf("azure monitor metrics: %w", callErr)
	}

	perEntity := make(map[string]int)
	if out == nil || len(out.Value) == 0 {
		return perEntity, nil
	}
	for _, ts := range out.Value[0].Timeseries {
		entity := entityNameFromMetadata(ts.MetadataValues)
		if entity == "" {
			continue
		}
		var maxVal, minVal float64
		haveMax, haveMin := false, false
		samples := 0
		for _, dp := range ts.Data {
			if mx, ok := extractAggregateValue(dp, "Maximum"); ok {
				samples++
				if !haveMax || mx > maxVal {
					maxVal, haveMax = mx, true
				}
			}
			if mn, ok := extractAggregateValue(dp, "Minimum"); ok {
				if !haveMin || mn < minVal {
					minVal, haveMin = mn, true
				}
			}
		}
		if samples == 0 {
			continue
		}
		delta := maxVal - minVal
		if delta < 0 {
			delta = 0
		}
		perEntity[entity] = int(delta)
	}
	return perEntity, nil
}

// entityNameFromMetadata extracts the Service Bus entity (queue /
// topic) name from a split-by-EntityName timeseries' metadatavalues.
// Matches the dimension name case-insensitively (Azure lowercases it
// to "entityname" in metadata). Returns "" when no entity dimension
// is present.
func entityNameFromMetadata(mvs []armMetricsMetadataValue) string {
	for _, mv := range mvs {
		if strings.EqualFold(mv.Name.Value, AzureServiceBusEntityNameDimension) {
			return mv.Value
		}
	}
	return ""
}

// aggregateAzureTimeseriesSum rolls up the value[0].timeseries[].data[]
// datapoints into a single AggregateMetricResult via SUM rather
// than MAX. Used by the sampling-rate-slice-1 invocation-count
// path — the per-5-minute Total values add up to the total
// invocations across the window, which is the denominator the
// detection branch compares against the observed_span_count from
// traceindex.
//
// Counters are unitless on the sampling-rate side; this helper
// leaves Unit empty rather than stamping the Azure-reported "Count"
// so the cross-cloud detection branch can treat the value
// dimensionlessly.
func aggregateAzureTimeseriesSum(out *armMetricsResponse, aggregation string) scanner.AggregateMetricResult {
	result := scanner.AggregateMetricResult{}
	if out == nil || len(out.Value) == 0 {
		return result
	}
	totalVal := 0.0
	sampleCount := 0
	for _, ts := range out.Value[0].Timeseries {
		for _, dp := range ts.Data {
			v, ok := extractAggregateValue(dp, aggregation)
			if !ok {
				continue
			}
			sampleCount++
			totalVal += v
		}
	}
	result.Value = totalVal
	result.SampleCount = sampleCount
	return result
}

// queryAzureMetricWithFallback issues the metric query with the
// supplied dimension filter first; if Azure Monitor responds with
// 400 BadRequest indicating the dimension does not exist on the
// resource, retries without the filter and returns fellBack=true.
//
// Azure Monitor returns a 400 with error code "BadRequest" (or
// "InvalidParameter") when an invalid dimension is named in the
// filter. The substrate matches on either the code OR the message
// referencing the dimension name — both signals are reliable across
// the api-version=2024-02-01 surface, and using both keeps the
// fallback resilient when Azure tweaks one branch.
//
// Returns a populated AggregateMetricResult with Value, Unit, and
// SampleCount set from the timeseries response. The caller is
// responsible for echoing ResourceARN, MetricName, Window,
// Statistic, ObservedAt onto the result — those are caller-context
// fields, not response-derived ones.
func (s *Scanner) queryAzureMetricWithFallback(
	ctx context.Context,
	resourceARN, metricName string,
	startTime, endTime time.Time,
	aggregation string,
	dimensionFilter string,
) (scanner.AggregateMetricResult, bool, error) {
	// First attempt with the filter.
	out, callErr := s.doAzureMonitorMetricsCall(
		ctx, resourceARN, metricName, startTime, endTime, aggregation, dimensionFilter,
	)
	if callErr == nil {
		return aggregateAzureTimeseries(out, aggregation), false, nil
	}

	// Inspect the error: is it the dimension-not-found 400?
	if !isAzureDimensionNotFoundError(callErr, AzureFunctionsIsAfterColdStartDimension) {
		return scanner.AggregateMetricResult{}, false, fmt.Errorf("azure monitor metrics: %w", callErr)
	}

	// Second attempt without the dimension filter.
	out2, callErr2 := s.doAzureMonitorMetricsCall(
		ctx, resourceARN, metricName, startTime, endTime, aggregation, "",
	)
	if callErr2 != nil {
		return scanner.AggregateMetricResult{}, false, fmt.Errorf("azure monitor metrics (fallback): %w", callErr2)
	}
	return aggregateAzureTimeseries(out2, aggregation), true, nil
}

// doAzureMonitorMetricsCall performs the Azure Monitor REST call
// against the metrics sub-resource of the supplied ARM resource id.
// Returns the parsed armMetricsResponse on success or an
// *armCallError on any non-200 / transport failure. The empty
// dimensionFilter argument elides the &filter= query parameter so
// the same helper covers both the filtered and unfiltered attempt.
func (s *Scanner) doAzureMonitorMetricsCall(
	ctx context.Context,
	resourceARN, metricName string,
	startTime, endTime time.Time,
	aggregation string,
	dimensionFilter string,
) (*armMetricsResponse, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	// Compose the path: {endpoint}{resourceARN}/{azureMonitorMetricsAPIBase}.
	// resourceARN already begins with "/subscriptions/..." so the
	// endpoint is trimmed of its trailing slash and the ARN is
	// concatenated verbatim.
	base := strings.TrimRight(endpoint, "/") + ensureLeadingSlash(resourceARN)

	q := url.Values{}
	q.Set("api-version", AzureMonitorMetricsAPIVersion)
	q.Set("metricnames", metricName)
	q.Set("timespan", fmt.Sprintf("%s/%s",
		startTime.UTC().Format(time.RFC3339),
		endTime.UTC().Format(time.RFC3339)))
	q.Set("interval", azureMonitorMetricsInterval)
	q.Set("aggregation", aggregation)
	if dimensionFilter != "" {
		q.Set("$filter", dimensionFilter)
	}

	fullURL := fmt.Sprintf("%s/%s?%s", base, azureMonitorMetricsAPIBase, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, &armCallError{Wrapped: err}
	}
	req.Header.Set("Authorization", "Bearer "+s.accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, &armCallError{Wrapped: err, IsNetwork: true}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		var aerr armErrorResponse
		_ = json.Unmarshal(body, &aerr)
		return nil, &armCallError{
			StatusCode: resp.StatusCode,
			Code:       aerr.Error.Code,
			Message:    aerr.Error.Message,
			BodyHint:   truncate(string(body), 200),
			RetryAfter: resp.Header.Get("Retry-After"),
		}
	}

	var out armMetricsResponse
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, &armCallError{Wrapped: fmt.Errorf("metrics response parse: %w", jerr)}
	}
	return &out, nil
}

// aggregateAzureTimeseries rolls up the value[0].timeseries[].data[]
// datapoints into a single AggregateMetricResult. Mirrors the AWS
// substrate's per-datapoint MAX rollup: the worst-case 5-minute
// aggregate across the window is the operator-visible signal the
// cold-start recommendation reasons over. Slice 3 may adopt a more
// sophisticated rollup once cross-cloud comparison work surfaces a
// preference.
//
// SampleCount on the Azure side is "count of timeseries data points
// that had a non-nil aggregate value" — Azure Monitor doesn't
// surface a per-bucket sample count the way CloudWatch does. The
// detection branch's BaselineMinimumSamples gate becomes "at least
// 50 5-minute buckets had data" which is the closest analog and
// keeps the cross-cloud threshold comparable.
func aggregateAzureTimeseries(out *armMetricsResponse, aggregation string) scanner.AggregateMetricResult {
	result := scanner.AggregateMetricResult{
		Unit: azureMonitorUnitMilliseconds,
	}
	if out == nil || len(out.Value) == 0 {
		return result
	}
	// Prefer the unit Azure Monitor explicitly reports if present.
	if out.Value[0].Unit != "" {
		result.Unit = out.Value[0].Unit
	}
	maxVal := 0.0
	sampleCount := 0
	for _, ts := range out.Value[0].Timeseries {
		for _, dp := range ts.Data {
			v, ok := extractAggregateValue(dp, aggregation)
			if !ok {
				continue
			}
			sampleCount++
			if v > maxVal {
				maxVal = v
			}
		}
	}
	result.Value = maxVal
	result.SampleCount = sampleCount
	return result
}

// extractAggregateValue picks the aggregate field on an Azure Monitor
// timeseries datapoint matching the requested aggregation. The
// Azure Monitor response carries one of {average, total, maximum,
// minimum, count} on each datapoint depending on which
// aggregation= parameter the request asked for. Returns (0, false)
// when neither the requested aggregation nor a sensible fallback
// is populated — the caller treats that datapoint as "no data" and
// skips it.
func extractAggregateValue(dp armMetricsDatapoint, aggregation string) (float64, bool) {
	switch aggregation {
	case "Maximum":
		if dp.Maximum != nil {
			return *dp.Maximum, true
		}
	case "Average":
		if dp.Average != nil {
			return *dp.Average, true
		}
	case "Total":
		if dp.Total != nil {
			return *dp.Total, true
		}
	case "Minimum":
		if dp.Minimum != nil {
			return *dp.Minimum, true
		}
	case "Count":
		if dp.Count != nil {
			return *dp.Count, true
		}
	}
	return 0, false
}

// isAzureDimensionNotFoundError detects the Azure Monitor 400
// response that signals the requested dimension does not exist on
// the resource. The two reliable signals at api-version=2024-02-01:
//
//   - HTTP 400 with error code "BadRequest" OR "InvalidParameter",
//   - The error message referencing the dimension name verbatim.
//
// Either is sufficient — using both keeps the detection resilient
// when Azure tweaks one branch.
func isAzureDimensionNotFoundError(err error, dimensionName string) bool {
	if err == nil {
		return false
	}
	ace, ok := err.(*armCallError)
	if !ok {
		return false
	}
	if ace.StatusCode != http.StatusBadRequest {
		return false
	}
	// Code match: BadRequest / InvalidParameter are the two codes
	// Azure Monitor surfaces for filter dimension errors.
	switch ace.Code {
	case "BadRequest", "InvalidParameter":
		// Pair with a dimension-name match in the message to
		// avoid false positives on other BadRequest conditions
		// (malformed timespan, unsupported aggregation, etc.).
		if dimensionName != "" && strings.Contains(ace.Message, dimensionName) {
			return true
		}
		// If the message is missing or doesn't name the dimension
		// explicitly, but the code is BadRequest AND the body
		// hint references the dimension, still flip the fallback.
		if dimensionName != "" && strings.Contains(ace.BodyHint, dimensionName) {
			return true
		}
	}
	return false
}

// mapMetricStatisticToAzureAggregation converts the
// scanner.MetricStatistic enum into the Azure Monitor aggregation=
// query parameter. Slice 2 maps StatisticP95 → "Maximum" because
// Azure Monitor does NOT natively support percentile aggregations
// on FunctionExecutionDuration. The approximation is documented in
// code comments here, in the azureMonitorAggregationForP95 godoc,
// and in the chunk 5 operator runbook (deferral).
//
// The other statistics map to their natural Azure counterparts so
// future slices that probe additional aggregations work without
// changing this helper.
func mapMetricStatisticToAzureAggregation(stat scanner.MetricStatistic) string {
	switch stat {
	case scanner.StatisticP95:
		return azureMonitorAggregationForP95
	case scanner.StatisticP99:
		// Same approximation reasoning as P95 — Azure Monitor
		// doesn't natively support P99 on FunctionExecutionDuration
		// either. Maximum is the closest signal.
		return azureMonitorAggregationForP95
	case scanner.StatisticAverage:
		return "Average"
	case scanner.StatisticSum:
		return "Total"
	default:
		return azureMonitorAggregationForP95
	}
}

// ensureLeadingSlash guards against an ARM resource id that doesn't
// begin with a leading slash — the URL composition concatenates the
// endpoint with the ARN directly, so a missing slash would produce a
// malformed URL. ARM ids always start with "/subscriptions/..." in
// production; this helper handles the rare case where a caller
// passes a denormalized id and keeps the metrics call from silently
// 404ing.
func ensureLeadingSlash(s string) string {
	if s == "" {
		return ""
	}
	if s[0] == '/' {
		return s
	}
	return "/" + s
}

// WithAccessToken wires an OAuth2 bearer token onto the Scanner so
// QueryAggregate (and the cold-start detection branch) can issue
// Azure Monitor calls outside of a Scan() lifecycle. v0.89.118.
// Tests and the chunk-4 per-resource cold-start endpoint set this
// directly with a pre-acquired token; the Scan path acquires its
// own token internally and never persists it on the Scanner.
//
// Returns the Scanner so the setter chain composes:
//
//	s := (&Scanner{...}).WithAccessToken("...").WithMetricsLimiter(...)
//
// Empty tokens are accepted — the QueryAggregate path treats an
// empty accessToken as the chunk-1 skeleton (returns
// scanner.ErrMetricNotImplemented), preserving the v0.89.113
// surface when callers explicitly want to opt out.
func (s *Scanner) WithAccessToken(token string) *Scanner {
	s.accessToken = token
	return s
}

// WithMetricsLimiter overrides the per-Scanner Azure Monitor rate
// limiter. v0.89.118. Reserved for tests that need to pin the
// limiter's burst to a specific value to deterministically time the
// 12K-RPH pin (TestAzureRateLimiterCapsAt12000RPH), or to disable
// it entirely (a nil limiter short-circuits the Wait call). Production
// never calls this — the chunk-4 wiring pre-arms the limiter at the
// substrate-default rate.
// WithCommercialDetectors flips the add-on-dependent regression
// detectors on (#153 enterprise-gate). Default false (OSS). When true
// the Functions cold-start + error detectors request the Application
// Insights metric names (requests/duration, requests/count,
// requests/failed) instead of the inert Azure Monitor Functions metric
// names. The caller must also pass the Application Insights component
// resource ARN to the detectors and wire an observation store for the
// detection branch to run.
func (s *Scanner) WithCommercialDetectors(on bool) *Scanner {
	s.commercialDetectors = on
	return s
}

func (s *Scanner) WithMetricsLimiter(limiter *rate.Limiter) *Scanner {
	s.metricsLimiter = limiter
	return s
}

// armMetricsResponse is the JSON shape returned by the Azure Monitor
// metrics endpoint at api-version=2024-02-01. The response wraps
// the queried metrics under a value[] array — slice 2 queries one
// metric at a time so value[0] is always the load-bearing entry.
type armMetricsResponse struct {
	Value []armMetricsValue `json:"value"`
}

// armMetricsValue is one metric's worth of timeseries data inside
// armMetricsResponse. The Unit field is the human-friendly Azure
// Monitor unit string ("Milliseconds", "Count", "Percent", etc.);
// slice 2 preserves it on the AggregateMetricResult so downstream
// consumers can normalize against the absolute floor.
type armMetricsValue struct {
	Unit       string                 `json:"unit,omitempty"`
	Timeseries []armMetricsTimeseries `json:"timeseries,omitempty"`
}

// armMetricsTimeseries is one dimension-combination's worth of
// datapoints. When the query filters by a dimension (e.g.
// IsAfterColdStart=true), Azure Monitor still returns the wrapping
// shape — usually a single timeseries entry whose metadatavalues[]
// carries the dimension value. Slice 2 sums across timeseries[].data
// so multi-dimension responses fold cleanly into the substrate's
// scalar aggregate.
type armMetricsTimeseries struct {
	Data []armMetricsDatapoint `json:"data,omitempty"`

	// MetadataValues carries the dimension values when the query
	// splits by a dimension (e.g. $filter="EntityName eq '*'" returns
	// one timeseries per Service Bus entity, each tagged with its
	// entityname here). Poison-rate substrate slice 4 chunk 3b
	// (v0.89.180) reads this to attribute per-queue dead-letter rates.
	// Additive — existing single-series parsing ignores it.
	MetadataValues []armMetricsMetadataValue `json:"metadatavalues,omitempty"`
}

// armMetricsMetadataValue is one dimension name/value pair on a
// split-by-dimension timeseries. For Service Bus DeadletteredMessages
// split by EntityName, Name.Value is "entityname" and Value is the
// queue / topic name.
type armMetricsMetadataValue struct {
	Name  armMetricsMetadataName `json:"name"`
	Value string                 `json:"value"`
}

// armMetricsMetadataName is the inner localized-name envelope Azure
// Monitor wraps each metadata dimension name in.
type armMetricsMetadataName struct {
	Value string `json:"value"`
}

// armMetricsDatapoint is one bucket inside a timeseries[].data[]
// array. The aggregate values are pointer-typed so the substrate
// can distinguish "this aggregation wasn't requested" (nil) from
// "the aggregation was requested and the value is zero" (non-nil
// 0.0). TimeStamp is parsed verbatim — the substrate doesn't reason
// about per-bucket timestamps today (slice 3 may, for sampling-rate
// analysis).
type armMetricsDatapoint struct {
	TimeStamp string   `json:"timeStamp,omitempty"`
	Average   *float64 `json:"average,omitempty"`
	Total     *float64 `json:"total,omitempty"`
	Maximum   *float64 `json:"maximum,omitempty"`
	Minimum   *float64 `json:"minimum,omitempty"`
	Count     *float64 `json:"count,omitempty"`
}
