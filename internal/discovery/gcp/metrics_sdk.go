// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// metrics_sdk.go — the production Cloud Monitoring V3 adapter that
// satisfies the metricsClient interface (metrics.go), wiring
// QueryAggregate's narrow QueryTimeSeries contract onto the
// google.golang.org/api/monitoring/v3 timeSeries.list REST call.
//
// This is the chunk-2 SDK adapter the metrics.go header deferred ("The
// production-path SDK adapter ... is deferred to a follow-up chunk").
// It is activated only behind config.ServerlessMetricDetection.Enabled
// (option 2, #300); the default-off path never constructs it, so a
// stock scan issues zero Cloud Monitoring reads.
//
// ── LIVE-VERIFICATION STATUS ─────────────────────────────────────────
// ✅ Live-verified against a real Cloud Monitoring backend (v0.89.335,
// 2026-06-30). Using ADC, the production path (buildOAuthHTTPClient with
// the monitoring.read scope → buildMonitoringClient → QueryTimeSeries)
// was run against real timeSeries.list data round-tripped through Cloud
// Monitoring: the request authenticated + returned 200, the real
// response JSON parsed, the TypedValue DoubleValue extraction was
// correct (returned the exact written value), the cross-period rollup
// behaved, and the SampleCount proxy + interval parsing populated as
// designed. See metrics_live_test.go.
//
// SampleCount proxy (still worth understanding): Cloud Monitoring
// returns a SCALAR double for a percentile-aligned
// (ALIGN_PERCENTILE_95) distribution metric, so the underlying
// per-bucket sample count is not carried on the point. We report
// SampleCount = 1 per populated alignment period (each non-empty
// 5-minute bucket = one sample). The cold-start baseline-minimum-samples
// gate (ColdStartBaselineMinimumSamples = 50) thus requires >= 50
// populated 5-minute buckets across the 168h baseline window — a coarse
// but monotonic "enough baseline data" proxy.
//
// One sub-path remains canned-only (low risk): the Int64Value
// (ALIGN_DELTA count) decode and the end-to-end against a deployed Cloud
// Run service's request_latencies distribution. Both exercise the same
// adapter code that the live run confirmed (the count path differs only
// in which TypedValue pointer is non-nil), so they're a data-shape
// confirmation rather than new code coverage. metrics_live_test.go can
// run them against a real Cloud Run service when one is available.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	monitoring "google.golang.org/api/monitoring/v3"
	"google.golang.org/api/option"
)

// cloudMonitoringAlignmentPeriodSeconds is the per-period alignment the
// adapter requests. QueryAggregate's rollup (MAX for latency, SUM for
// count) runs across the returned per-period points, so the period sets
// the bucket granularity. 300s (5m) matches the QueryAggregate header's
// documented "one point per alignment period (5m)" assumption and keeps
// even a 168h window's point count (~2016) within Cloud Monitoring's
// per-request limits (with pagination handled below).
const cloudMonitoringAlignmentPeriodSeconds = 300

// cloudMonitoringClient is the production metricsClient: a thin adapter
// over the Cloud Monitoring V3 REST Service. Stateless beyond the
// service handle; safe for the single-goroutine-per-scan use the
// detection passes make of it.
type cloudMonitoringClient struct {
	svc *monitoring.Service
}

// QueryTimeSeries implements metricsClient against timeSeries.list. The
// caller (QueryAggregate) supplies a fully-formed Cloud Monitoring
// filter, the interval, and the per-series aligner string; this adapter
// adds the alignment period, pages through every result, and flattens
// each point of every returned series into a TimeSeriesPoint. Returning
// a flat slice matches QueryAggregate's rollup, which iterates all
// points regardless of which series they came from.
func (c *cloudMonitoringClient) QueryTimeSeries(
	ctx context.Context,
	projectName string,
	filter string,
	startTime, endTime time.Time,
	stat string,
) ([]TimeSeriesPoint, error) {
	call := c.svc.Projects.TimeSeries.List(projectName).
		Filter(filter).
		IntervalStartTime(startTime.UTC().Format(time.RFC3339)).
		IntervalEndTime(endTime.UTC().Format(time.RFC3339)).
		AggregationAlignmentPeriod(fmt.Sprintf("%ds", cloudMonitoringAlignmentPeriodSeconds)).
		AggregationPerSeriesAligner(stat).
		View("FULL").
		Context(ctx)

	var points []TimeSeriesPoint
	err := call.Pages(ctx, func(resp *monitoring.ListTimeSeriesResponse) error {
		for _, series := range resp.TimeSeries {
			for _, p := range series.Points {
				if p == nil {
					continue
				}
				tsp := TimeSeriesPoint{
					Value:       typedValueToFloat(p.Value),
					SampleCount: pointSampleCount(p.Value),
				}
				if p.Interval != nil {
					tsp.StartTime = parseRFC3339(p.Interval.StartTime)
					tsp.EndTime = parseRFC3339(p.Interval.EndTime)
				}
				points = append(points, tsp)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: cloud monitoring timeSeries.list: %w", err)
	}
	return points, nil
}

// typedValueToFloat extracts the numeric value from a Cloud Monitoring
// TypedValue across the field shapes the slice-2 aligners produce:
// DoubleValue for percentile-aligned latency, Int64Value for
// ALIGN_DELTA counts, and DistributionValue.Mean as a last resort if an
// unaligned distribution ever reaches here. The generated client uses
// pointer fields, so a nil pointer cleanly means "not this shape".
func typedValueToFloat(tv *monitoring.TypedValue) float64 {
	if tv == nil {
		return 0
	}
	switch {
	case tv.DoubleValue != nil:
		return *tv.DoubleValue
	case tv.Int64Value != nil:
		return float64(*tv.Int64Value)
	case tv.DistributionValue != nil:
		return tv.DistributionValue.Mean
	default:
		return 0
	}
}

// pointSampleCount reports the per-point sample count. When the point
// carried a distribution (e.g. an unaligned distribution surface), its
// true Count is used. For the scalar percentile / delta points the
// aligners actually produce, the underlying count is not on the point,
// so each populated period counts as one sample (see the SampleCount
// proxy note in the file header).
func pointSampleCount(tv *monitoring.TypedValue) int64 {
	if tv != nil && tv.DistributionValue != nil && tv.DistributionValue.Count > 0 {
		return tv.DistributionValue.Count
	}
	return 1
}

// parseRFC3339 best-effort parses a Cloud Monitoring interval boundary.
// The StartTime/EndTime fields are surfaced for future-slice use and are
// not load-bearing for the slice-2 rollup, so a parse failure yields the
// zero time rather than an error.
func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// buildMonitoringClient constructs the production Cloud Monitoring
// adapter. Mirrors the buildRunClient test-injection pattern exactly:
// when s.httpClient is set (tests), it builds the service against that
// transport + endpoint with authentication disabled; otherwise it builds
// against the scan-time oauth client. Called from Scan only when
// s.metricDetection is on.
func (s *Scanner) buildMonitoringClient(ctx context.Context, oauthClient *http.Client) (metricsClient, error) {
	var (
		svc *monitoring.Service
		err error
	)
	if s.httpClient != nil {
		opts := []option.ClientOption{
			option.WithHTTPClient(s.httpClient),
			option.WithoutAuthentication(),
		}
		if s.endpoint != "" {
			opts = append(opts, option.WithEndpoint(s.endpoint))
		}
		svc, err = monitoring.NewService(ctx, opts...)
	} else {
		svc, err = monitoring.NewService(ctx, option.WithHTTPClient(oauthClient))
	}
	if err != nil {
		return nil, fmt.Errorf("gcp: build cloud monitoring client: %w", err)
	}
	return &cloudMonitoringClient{svc: svc}, nil
}

// WithServerlessMetricDetection flips the native-metric serverless
// detection gate (config.ServerlessMetricDetection.Enabled; option 2,
// #300). When on, Scan builds the Cloud Monitoring adapter from the
// scan-time oauth client and wires it before the cold-start + error-rate
// passes. Default off keeps metricsClient nil — the passes no-op and a
// scan issues zero Cloud Monitoring reads. Returns the Scanner so the
// factory's constructor chain composes.
func (s *Scanner) WithServerlessMetricDetection(on bool) *Scanner {
	s.metricDetection = on
	return s
}
