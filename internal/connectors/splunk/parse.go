// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package splunk

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/connectors"
)

// splunkResponse is the envelope a oneshot search with output_mode=json
// returns. Results are decoded as ordered field maps; each value is a JSON
// string for a normal field (Splunk serializes values as strings) or an array
// for a multi-value field.
type splunkResponse struct {
	Preview  bool                         `json:"preview"`
	Messages []splunkMessage              `json:"messages"`
	Fields   []struct{ Name string }      `json:"fields"`
	Results  []map[string]json.RawMessage `json:"results"`
}

// splunkMessage is one entry in the response's messages array. A FATAL or
// ERROR message means the search itself failed even though the HTTP status
// was 200.
type splunkMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// parseResponse decodes a Splunk oneshot search body into a NormalizedResult,
// dispatching on the query: a metric query (stats/timechart) parses to series
// or a scalar; a log query parses raw events to log rows.
func parseResponse(body []byte, q connectors.NormalizedQuery) (connectors.NormalizedResult, error) {
	var env splunkResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return connectors.NormalizedResult{}, fmt.Errorf("splunk: decode response envelope: %w", err)
	}
	for _, m := range env.Messages {
		if t := strings.ToUpper(m.Type); t == "FATAL" || t == "ERROR" {
			return connectors.NormalizedResult{}, fmt.Errorf("splunk: search error: %s", m.Text)
		}
	}

	if isMetricQuery(q) {
		return parseMetrics(env.Results, q)
	}
	return parseEvents(env.Results, q), nil
}

// parseEvents normalizes raw search results into log rows. _raw becomes the
// line, _time the timestamp, and every remaining non-internal field (one not
// prefixed with "_") becomes a label. It flags truncation when the row count
// hits the query limit.
func parseEvents(results []map[string]json.RawMessage, q connectors.NormalizedQuery) connectors.NormalizedResult {
	rows := make([]connectors.LogRow, 0, len(results))
	for _, r := range results {
		row := connectors.LogRow{Labels: map[string]string{}}
		if raw, ok := r["_raw"]; ok {
			if s, ok := rawToString(raw); ok {
				row.Line = s
			}
		}
		if t, ok := r["_time"]; ok {
			if s, ok := rawToString(t); ok {
				row.Timestamp = parseSplunkTime(s)
			}
		}
		for k, v := range r {
			if strings.HasPrefix(k, "_") {
				continue
			}
			if s, ok := rawToString(v); ok {
				row.Labels[k] = s
			}
		}
		rows = append(rows, row)
	}
	res := connectors.NewLogsResult(rows)
	res.Metadata = map[string]string{"source": "splunk"}
	if q.Limit > 0 && len(rows) >= q.Limit {
		res.Warnings = append(res.Warnings, "results truncated at limit")
	}
	return res
}

// parseMetrics normalizes a stats/timechart result. A timechart result (rows
// carry _time) becomes one series per value column across the rows. A stats
// result (no _time) with a single row, a single numeric column, and no
// group-by becomes a ScalarResult (the SLO / burn-rate decision-context
// shape); otherwise each row becomes a single-sample series labeled by its
// group-by values.
func parseMetrics(results []map[string]json.RawMessage, q connectors.NormalizedQuery) (connectors.NormalizedResult, error) {
	// timechart path: any row carrying _time means a stepped timeseries.
	for _, r := range results {
		if _, ok := r["_time"]; ok {
			return parseTimechart(results), nil
		}
	}
	return parseStats(results, q), nil
}

// parseTimechart builds one MetricSeries per non-internal value column,
// gathering a Sample per row from _time and the column value. Columns that
// never parse as a number are skipped.
func parseTimechart(results []map[string]json.RawMessage) connectors.NormalizedResult {
	cols := valueColumns(results, nil)
	byCol := make(map[string]*connectors.MetricSeries, len(cols))
	order := make([]string, 0, len(cols))
	for _, c := range cols {
		s := &connectors.MetricSeries{Labels: map[string]string{"metric": c}}
		byCol[c] = s
		order = append(order, c)
	}

	for _, r := range results {
		ts := time.Time{}
		if t, ok := r["_time"]; ok {
			if s, ok := rawToString(t); ok {
				ts = parseSplunkTime(s)
			}
		}
		for _, c := range cols {
			raw, ok := r[c]
			if !ok {
				continue
			}
			v, ok := rawToFloat(raw)
			if !ok {
				continue
			}
			byCol[c].Samples = append(byCol[c].Samples, connectors.Sample{Timestamp: ts, Value: v})
		}
	}

	series := make([]connectors.MetricSeries, 0, len(order))
	for _, c := range order {
		series = append(series, *byCol[c])
	}
	res := connectors.NewMetricsResult(series)
	res.Metadata = map[string]string{"source": "splunk", "result": "timechart"}
	return res
}

// parseStats builds series (or a single scalar) from a non-timechart stats
// result. Value columns are the numeric columns not named in GroupBy; group
// columns carry the series labels.
func parseStats(results []map[string]json.RawMessage, q connectors.NormalizedQuery) connectors.NormalizedResult {
	valueCols := valueColumns(results, q.GroupBy)

	// Single scalar: one row, one value column, no group-by. This is the
	// SLO / burn-rate decision-context shape.
	if len(q.GroupBy) == 0 && len(results) == 1 && len(valueCols) == 1 {
		if v, ok := rawToFloat(results[0][valueCols[0]]); ok {
			res := connectors.NewScalarResult(connectors.ScalarResult{
				Value: v,
				// Kind is Generic: the connector reports the raw scalar and its
				// evaluation window; a consumer applies SLO / burn-rate
				// semantics (Objective, Threshold, Breached) rather than the
				// connector inventing them.
				Kind:        connectors.ScalarGeneric,
				Window:      q.TimeRange,
				EvaluatedAt: q.TimeRange.End,
			})
			res.Metadata = map[string]string{"source": "splunk", "result": "stats", "column": valueCols[0]}
			return res
		}
	}

	series := make([]connectors.MetricSeries, 0, len(results))
	for _, r := range results {
		labels := map[string]string{}
		for _, g := range q.GroupBy {
			if raw, ok := r[g]; ok {
				if s, ok := rawToString(raw); ok {
					labels[g] = s
				}
			}
		}
		for _, c := range valueCols {
			v, ok := rawToFloat(r[c])
			if !ok {
				continue
			}
			ls := map[string]string{"metric": c}
			for k, val := range labels {
				ls[k] = val
			}
			series = append(series, connectors.MetricSeries{
				Labels:  ls,
				Samples: []connectors.Sample{{Timestamp: q.TimeRange.End, Value: v}},
			})
		}
	}
	res := connectors.NewMetricsResult(series)
	res.Metadata = map[string]string{"source": "splunk", "result": "stats"}
	return res
}

// valueColumns returns the sorted set of numeric columns across the result
// rows, excluding internal (_-prefixed) fields and any column named in
// exclude (the group-by fields). A column is included if it parses as a float
// in at least one row.
func valueColumns(results []map[string]json.RawMessage, exclude []string) []string {
	skip := map[string]bool{}
	for _, e := range exclude {
		skip[e] = true
	}
	seen := map[string]bool{}
	var cols []string
	for _, r := range results {
		for k, v := range r {
			if strings.HasPrefix(k, "_") || skip[k] || seen[k] {
				continue
			}
			if _, ok := rawToFloat(v); ok {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	sort.Strings(cols)
	return cols
}

// rawToString decodes a Splunk result value (a JSON string, number, or
// multi-value array) into a string. Arrays are joined with commas.
func rawToString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return strconv.FormatFloat(f, 'g', -1, 64), true
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return strings.Join(arr, ","), true
	}
	return "", false
}

// rawToFloat decodes a Splunk result value into a float. Splunk serializes
// numeric stats output as JSON strings, so this trims and ParseFloats the
// string form.
func rawToFloat(raw json.RawMessage) (float64, bool) {
	s, ok := rawToString(raw)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// parseSplunkTime parses a Splunk _time value, which is an ISO-8601 string
// (e.g. "2023-11-14T22:13:20.000+00:00") in JSON output mode, falling back to
// an epoch-seconds float. Returns the zero time when neither form parses.
func parseSplunkTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec := int64(f)
		nsec := int64((f - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC()
	}
	return time.Time{}
}
