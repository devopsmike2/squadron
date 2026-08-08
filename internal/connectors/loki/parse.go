// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package loki

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
)

// lokiResponse is the envelope Loki's query / query_range endpoints
// return. Result is left raw so it can be decoded into the shape that
// matches ResultType.
type lokiResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// lokiStream is one `streams` entry: labels plus [ns-timestamp, line]
// value pairs (Loki may append per-line metadata as a third element,
// which we ignore).
type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

// lokiMetric is one `matrix` / `vector` entry. Matrix populates Values
// (a series); vector populates Value (a single point). Each point is
// [unix-seconds-float, "string-value"].
type lokiMetric struct {
	Metric map[string]string   `json:"metric"`
	Values [][]json.RawMessage `json:"values"`
	Value  []json.RawMessage   `json:"value"`
}

// parseResponse decodes a Loki response body into a NormalizedResult,
// dispatching on resultType: streams -> logs, matrix -> series, vector ->
// series (or a ScalarResult for a single-element vector).
func parseResponse(body []byte, q connectors.NormalizedQuery) (connectors.NormalizedResult, error) {
	var env lokiResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: decode response envelope: %w", err)
	}
	if env.Status != "" && env.Status != "success" {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: query status %q", env.Status)
	}

	switch env.Data.ResultType {
	case "streams":
		return parseStreams(env.Data.Result, q)
	case "matrix":
		return parseMatrix(env.Data.Result)
	case "vector":
		return parseVector(env.Data.Result)
	case "":
		return connectors.NormalizedResult{}, fmt.Errorf("loki: response missing resultType")
	default:
		return connectors.NormalizedResult{}, fmt.Errorf("loki: unsupported resultType %q", env.Data.ResultType)
	}
}

// parseStreams normalizes a `streams` result into log rows, newest-first
// as Loki returns them. It flags truncation when the row count hits the
// query limit.
func parseStreams(raw json.RawMessage, q connectors.NormalizedQuery) (connectors.NormalizedResult, error) {
	var streams []lokiStream
	if err := json.Unmarshal(raw, &streams); err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: decode streams: %w", err)
	}
	rows := make([]connectors.LogRow, 0, len(streams))
	for _, s := range streams {
		for _, v := range s.Values {
			if len(v) < 2 {
				continue
			}
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				return connectors.NormalizedResult{}, fmt.Errorf("loki: parse stream timestamp: %w", err)
			}
			rows = append(rows, connectors.LogRow{
				Timestamp: time.Unix(0, ns).UTC(),
				Line:      v[1],
				Labels:    s.Stream,
			})
		}
	}
	res := connectors.NewLogsResult(rows)
	res.Metadata = map[string]string{"resultType": "streams"}
	if q.Limit > 0 && len(rows) >= q.Limit {
		res.Warnings = append(res.Warnings, "results truncated at limit")
	}
	return res, nil
}

// parseMatrix normalizes a `matrix` result into metric series.
func parseMatrix(raw json.RawMessage) (connectors.NormalizedResult, error) {
	var metrics []lokiMetric
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: decode matrix: %w", err)
	}
	series := make([]connectors.MetricSeries, 0, len(metrics))
	for _, m := range metrics {
		samples := make([]connectors.Sample, 0, len(m.Values))
		for _, pair := range m.Values {
			s, err := parseSample(pair)
			if err != nil {
				return connectors.NormalizedResult{}, err
			}
			samples = append(samples, s)
		}
		series = append(series, connectors.MetricSeries{Labels: m.Metric, Samples: samples})
	}
	res := connectors.NewMetricsResult(series)
	res.Metadata = map[string]string{"resultType": "matrix"}
	return res, nil
}

// parseVector normalizes a `vector` result. A single-element vector
// becomes a ScalarResult (the SLO / burn-rate decision-context shape),
// so a Loki-derived scalar flows into that path without a model change.
// A multi-element vector becomes one single-sample series per element.
func parseVector(raw json.RawMessage) (connectors.NormalizedResult, error) {
	var metrics []lokiMetric
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("loki: decode vector: %w", err)
	}

	if len(metrics) == 1 && len(metrics[0].Value) == 2 {
		s, err := parseSample(metrics[0].Value)
		if err != nil {
			return connectors.NormalizedResult{}, err
		}
		// Kind is Generic: the connector reports the raw scalar and its
		// evaluation time; a consumer applies SLO / burn-rate semantics
		// (Objective, Threshold, Breached) — the connector does not invent
		// them.
		res := connectors.NewScalarResult(connectors.ScalarResult{
			Value:       s.Value,
			Kind:        connectors.ScalarGeneric,
			EvaluatedAt: s.Timestamp,
		})
		res.Metadata = map[string]string{"resultType": "vector"}
		return res, nil
	}

	series := make([]connectors.MetricSeries, 0, len(metrics))
	for _, m := range metrics {
		if len(m.Value) != 2 {
			continue
		}
		s, err := parseSample(m.Value)
		if err != nil {
			return connectors.NormalizedResult{}, err
		}
		series = append(series, connectors.MetricSeries{
			Labels:  m.Metric,
			Samples: []connectors.Sample{s},
		})
	}
	res := connectors.NewMetricsResult(series)
	res.Metadata = map[string]string{"resultType": "vector"}
	return res, nil
}

// parseSample decodes a [unix-seconds-float, "string-value"] Loki point
// into a connectors.Sample.
func parseSample(pair []json.RawMessage) (connectors.Sample, error) {
	if len(pair) != 2 {
		return connectors.Sample{}, fmt.Errorf("loki: malformed sample: want [ts, value], got %d elements", len(pair))
	}
	var tsSec float64
	if err := json.Unmarshal(pair[0], &tsSec); err != nil {
		return connectors.Sample{}, fmt.Errorf("loki: parse sample timestamp: %w", err)
	}
	var valStr string
	if err := json.Unmarshal(pair[1], &valStr); err != nil {
		return connectors.Sample{}, fmt.Errorf("loki: parse sample value: %w", err)
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return connectors.Sample{}, fmt.Errorf("loki: sample value %q is not numeric: %w", valStr, err)
	}
	sec := int64(tsSec)
	nsec := int64((tsSec - float64(sec)) * 1e9)
	return connectors.Sample{
		Timestamp: time.Unix(sec, nsec).UTC(),
		Value:     val,
	}, nil
}
