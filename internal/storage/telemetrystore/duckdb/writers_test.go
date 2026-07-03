// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package duckdb

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/otlp"
	duckdb "github.com/marcboeker/go-duckdb"
	"go.uber.org/zap"
)

// These tests pin the Appender-based bulk writers (v0.89 ingest
// throughput rework) against a real DuckDB file: schema column order,
// JSON-column handling, empty-string→NULL flattening, and exact row
// counts. If AppendRow's column order ever drifts from schema.go, or
// a DuckDB/go-duckdb upgrade changes JSON/NULL binding, these fail.

func newTestStorage(t *testing.T) *Storage {
	t.Helper()
	s, err := NewStorage(filepath.Join(t.TempDir(), "telemetry.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestWriteTracesFromOTLP_Appender(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	traces := []otlp.TraceData{
		{
			Timestamp: now, AgentID: "agent-1", GroupID: "g1", GroupName: "group one",
			TraceId: "trace-a", SpanId: "span-1", ParentSpanId: "span-0",
			ServiceName: "svc", SpanName: "GET /x", Duration: 1234, StatusCode: "OK",
			ResourceAttributes: map[string]string{"host.name": "h1"},
			SpanAttributes:     map[string]string{"http.method": "GET", "note": "ünïcode✓"},
		},
		{
			Timestamp: now, AgentID: "agent-1", GroupID: "", GroupName: "",
			TraceId: "trace-b", SpanId: "span-2", ParentSpanId: "", // root span → NULL parent
			ServiceName: "svc", SpanName: "SELECT items", Duration: 42, StatusCode: "OK",
		},
	}
	if err := s.WriteTracesFromOTLP(ctx, traces); err != nil {
		t.Fatalf("WriteTracesFromOTLP: %v", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT trace_id, parent_span_id IS NULL, span_kind IS NULL, status_message IS NULL,
		       json_extract_string(span_attributes, '$."http.method"')
		FROM traces ORDER BY trace_id`)
	if err != nil {
		t.Fatalf("query traces: %v", err)
	}
	defer rows.Close()

	type got struct {
		traceID                       string
		parentNull, kindNull, msgNull bool
		httpMethod                    *string
	}
	var out []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.traceID, &g.parentNull, &g.kindNull, &g.msgNull, &g.httpMethod); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d trace rows, want 2", len(out))
	}
	// trace-a: has parent, JSON attrs queryable.
	if out[0].traceID != "trace-a" || out[0].parentNull {
		t.Fatalf("trace-a: parent_span_id should be set: %+v", out[0])
	}
	if out[0].httpMethod == nil || *out[0].httpMethod != "GET" {
		t.Fatalf("trace-a: span_attributes JSON not queryable, got %v", out[0].httpMethod)
	}
	// Both: unpopulated schema columns land NULL.
	for _, g := range out {
		if !g.kindNull || !g.msgNull {
			t.Fatalf("span_kind/status_message should be NULL: %+v", g)
		}
	}
	// trace-b: empty parent flattens to NULL (root-span semantics).
	if !out[1].parentNull {
		t.Fatalf("trace-b: empty ParentSpanId must store NULL, not ''")
	}
}

func TestWriteLogsFromOTLP_Appender(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	logs := []otlp.LogData{
		{
			Timestamp: now, AgentID: "agent-1", ServiceName: "svc",
			SeverityText: "INFO", SeverityNumber: 9, Body: "hello world",
			TraceId: "t1", SpanId: "s1",
			ResourceAttributes: map[string]string{"host.name": "h1"},
			LogAttributes:      map[string]string{"k": "v"},
		},
		{
			Timestamp: now, AgentID: "agent-1", ServiceName: "svc",
			SeverityText: "WARN", SeverityNumber: 13, Body: "no trace context",
			TraceId: "", SpanId: "", // → NULLs
		},
	}
	if err := s.WriteLogsFromOTLP(ctx, logs); err != nil {
		t.Fatalf("WriteLogsFromOTLP: %v", err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE trace_id IS NULL AND span_id IS NULL`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 1 {
		t.Fatalf("want exactly 1 log with NULL trace/span ids, got %d", n)
	}
	var sev int
	var body string
	if err := s.db.QueryRowContext(ctx,
		`SELECT severity_number, body FROM logs WHERE trace_id = 't1'`).Scan(&sev, &body); err != nil {
		t.Fatalf("query t1: %v", err)
	}
	if sev != 9 || body != "hello world" {
		t.Fatalf("log round-trip mismatch: sev=%d body=%q", sev, body)
	}
}

func TestWriteMetricsFromOTLP_Appender(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// 120 of each: crosses the old 50-row chunk boundary (histograms used to
	// commit per-50-row batch, which broke the retry-without-duplicates
	// contract) to prove counts stay exact, and exercises all three appender
	// tables.
	var sums []otlp.MetricSumData
	var gauges []otlp.MetricGaugeData
	var histograms []otlp.MetricHistogramData
	for i := 0; i < 120; i++ {
		sums = append(sums, otlp.MetricSumData{
			TimeUnix: now, AgentID: "agent-1", ServiceName: "svc",
			MetricName: "synthetic.request.count", Value: float64(i),
			Attributes: map[string]string{"shard": "a"},
		})
		gauges = append(gauges, otlp.MetricGaugeData{
			TimeUnix: now, AgentID: "agent-1", ServiceName: "svc",
			MetricName: "synthetic.queue.depth", Value: float64(i) / 2,
			Attributes: map[string]string{"shard": "b"},
		})
		histograms = append(histograms, otlp.MetricHistogramData{
			TimeUnix: now, AgentID: "agent-1", ServiceName: "svc",
			MetricName: "synthetic.latency", Count: uint64(i), Sum: float64(i) * 1.5,
			Min: 0, Max: float64(i),
			BucketCounts:   []uint64{uint64(i), uint64(i) + 1, uint64(i) + 2},
			ExplicitBounds: []float64{1, 5, 10},
			Attributes:     map[string]string{"shard": "c"},
		})
	}
	if err := s.WriteMetricsFromOTLP(ctx, sums, gauges, histograms); err != nil {
		t.Fatalf("WriteMetricsFromOTLP: %v", err)
	}

	for _, q := range []struct {
		table string
		want  int
	}{{"metrics_sum", 120}, {"metrics_gauge", 120}, {"metrics_histogram", 120}} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+q.table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.table, err)
		}
		if n != q.want {
			t.Fatalf("%s: got %d rows, want %d", q.table, n, q.want)
		}
	}

	// Values + JSON attrs survive the appender round-trip.
	var v float64
	var shard string
	if err := s.db.QueryRowContext(ctx, `
		SELECT value, json_extract_string(metric_attributes, '$.shard')
		FROM metrics_sum WHERE value = 119`).Scan(&v, &shard); err != nil {
		t.Fatalf("query sum: %v", err)
	}
	if v != 119 || shard != "a" {
		t.Fatalf("sum round-trip mismatch: v=%v shard=%q", v, shard)
	}

	// Histogram scalars + the BIGINT[]/DOUBLE[] LIST columns survive the
	// appender round-trip (the path the old prepared-statement code serialized
	// as array-literal strings; the Appender binds the slices natively).
	var hcount int64
	var hsum float64
	var b0, b1, b2 int64
	var eb0, eb1, eb2 float64
	if err := s.db.QueryRowContext(ctx, `
		SELECT count, sum,
		       bucket_counts[1], bucket_counts[2], bucket_counts[3],
		       explicit_bounds[1], explicit_bounds[2], explicit_bounds[3]
		FROM metrics_histogram WHERE count = 100`).
		Scan(&hcount, &hsum, &b0, &b1, &b2, &eb0, &eb1, &eb2); err != nil {
		t.Fatalf("query histogram: %v", err)
	}
	if hcount != 100 || hsum != 150 {
		t.Fatalf("histogram scalar round-trip mismatch: count=%d sum=%v", hcount, hsum)
	}
	if b0 != 100 || b1 != 101 || b2 != 102 {
		t.Fatalf("bucket_counts round-trip mismatch: [%d %d %d]", b0, b1, b2)
	}
	if eb0 != 1 || eb1 != 5 || eb2 != 10 {
		t.Fatalf("explicit_bounds round-trip mismatch: [%v %v %v]", eb0, eb1, eb2)
	}
}

// TestAppendRows_ErrorLeavesZeroRows pins the retry contract: if the
// fill function fails partway, nothing is flushed and the table is
// untouched, so writeWithRetry can re-run the whole batch without
// duplicating rows.
func TestAppendRows_ErrorLeavesZeroRows(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	traces := make([]otlp.TraceData, 10)
	for i := range traces {
		traces[i] = otlp.TraceData{
			Timestamp: now, AgentID: "agent-1", TraceId: "t", SpanId: "s",
			ServiceName: "svc", SpanName: "n", Duration: 1, StatusCode: "OK",
		}
	}

	errInjected := errors.New("injected fill failure")
	err := s.appendRows(ctx, "traces", func(ap *duckdb.Appender) error {
		// Append a few real rows first so the buffered-but-unflushed
		// path is exercised, then fail.
		for i := 0; i < 5; i++ {
			trace := &traces[i]
			if err := ap.AppendRow(
				trace.Timestamp, trace.AgentID, trace.GroupID, trace.GroupName,
				trace.TraceId, trace.SpanId, nullIfEmpty(trace.ParentSpanId),
				trace.ServiceName, trace.SpanName, nil, trace.Duration,
				trace.StatusCode, nil, "{}", "{}", nil, nil,
			); err != nil {
				return err
			}
		}
		return errInjected
	})
	if err == nil {
		t.Fatal("appendRows should propagate the fill error")
	}

	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM traces").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("failed append must leave zero rows, got %d", n)
	}
}

// TestAppendRowsMulti_ErrorLeavesZeroRows pins the CROSS-TABLE retry contract
// that WriteMetricsFromOTLP depends on: sums + gauges append successfully, then
// the histogram fill fails partway. Because all three tables share one
// transaction, the failure must roll back the already-buffered sum and gauge
// rows too — otherwise a worker retry of the whole metric batch would re-append
// them and double-count. Before this fix each table committed independently, so
// the sum and gauge rows would have survived the histogram failure.
func TestAppendRowsMulti_ErrorLeavesZeroRows(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	sums := []otlp.MetricSumData{{
		TimeUnix: now, AgentID: "agent-1", ServiceName: "svc",
		MetricName: "s.count", Value: 1,
	}}
	gauges := []otlp.MetricGaugeData{{
		TimeUnix: now, AgentID: "agent-1", ServiceName: "svc",
		MetricName: "g.depth", Value: 2,
	}}

	errInjected := errors.New("injected histogram fill failure")
	err := s.appendRowsMulti(ctx,
		tableAppend{table: "metrics_sum", fill: otlpSumsFill(sums)},
		tableAppend{table: "metrics_gauge", fill: otlpGaugesFill(gauges)},
		tableAppend{table: "metrics_histogram", fill: func(ap *duckdb.Appender) error {
			// Buffer a real row first, then fail — exercises the
			// buffered-but-must-roll-back path across all three tables.
			if err := ap.AppendRow(
				now, "agent-1", "", "", "svc", "h.latency", "",
				int64(1), 1.0, 0.0, 1.0,
				[]int64{1}, []float64{1}, "{}", "{}",
			); err != nil {
				return err
			}
			return errInjected
		}},
	)
	if !errors.Is(err, errInjected) {
		t.Fatalf("appendRowsMulti should propagate the injected error, got %v", err)
	}

	for _, table := range []string{"metrics_sum", "metrics_gauge", "metrics_histogram"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s: cross-table rollback must leave zero rows, got %d", table, n)
		}
	}
}
