// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/otlp/parser"
	"go.uber.org/zap"
)

// TestBuildPayload_AgentAttribution is the load-bearing test: the
// payloads otlpsim generates must attribute to the SAME agent_id the
// server's parser resolves, and that ID must match fleetsim's
// deterministic UUID for the same index. If this breaks, a stress
// run silently produces otlp_batches rows under the wrong identities
// and every insights cross-check is garbage.
func TestBuildPayload_AgentAttribution(t *testing.T) {
	cfg := config{itemsPerReq: 10, labelPrefix: "otlpsim-test", agents: 3}
	p := parser.NewOTLPParser(zap.NewNop())

	for idx := 0; idx < 3; idx++ {
		res := agentResource(idx, cfg)
		wantID := deterministicUUID(idx).String()

		// Metrics
		body, items, err := buildPayload(cfg, res, job{agentIdx: idx, kind: sigMetrics, seq: 1})
		if err != nil {
			t.Fatalf("metrics payload: %v", err)
		}
		if items != cfg.itemsPerReq {
			t.Fatalf("metrics items = %d, want %d", items, cfg.itemsPerReq)
		}
		sums, gauges, _, err := p.ParseMetrics(body)
		if err != nil {
			t.Fatalf("server parser rejected metrics payload: %v", err)
		}
		if len(sums)+len(gauges) != cfg.itemsPerReq {
			t.Fatalf("parsed %d+%d metric points, want %d", len(sums), len(gauges), cfg.itemsPerReq)
		}
		for _, m := range sums {
			if m.AgentID != wantID {
				t.Fatalf("sum AgentID = %q, want %q", m.AgentID, wantID)
			}
		}
		for _, m := range gauges {
			if m.AgentID != wantID {
				t.Fatalf("gauge AgentID = %q, want %q", m.AgentID, wantID)
			}
		}

		// Logs
		body, _, err = buildPayload(cfg, res, job{agentIdx: idx, kind: sigLogs, seq: 2})
		if err != nil {
			t.Fatalf("logs payload: %v", err)
		}
		logs, err := p.ParseLogs(body)
		if err != nil {
			t.Fatalf("server parser rejected logs payload: %v", err)
		}
		if len(logs) != cfg.itemsPerReq {
			t.Fatalf("parsed %d logs, want %d", len(logs), cfg.itemsPerReq)
		}
		if logs[0].AgentID != wantID {
			t.Fatalf("log AgentID = %q, want %q", logs[0].AgentID, wantID)
		}

		// Traces
		body, _, err = buildPayload(cfg, res, job{agentIdx: idx, kind: sigTraces, seq: 3})
		if err != nil {
			t.Fatalf("traces payload: %v", err)
		}
		traces, err := p.ParseTraces(body)
		if err != nil {
			t.Fatalf("server parser rejected traces payload: %v", err)
		}
		if len(traces) != cfg.itemsPerReq {
			t.Fatalf("parsed %d spans, want %d", len(traces), cfg.itemsPerReq)
		}
		if traces[0].AgentID != wantID {
			t.Fatalf("trace AgentID = %q, want %q", traces[0].AgentID, wantID)
		}
	}
}

func TestParseMix(t *testing.T) {
	cases := []struct {
		in      string
		m, l, t int
		wantErr bool
	}{
		{in: "metrics:70,logs:20,traces:10", m: 70, l: 20, t: 10},
		{in: "traces:100", m: 0, l: 0, t: 100},
		{in: "metrics:50,logs:50", m: 50, l: 50, t: 0},
		{in: "metrics:50,logs:20", wantErr: true},           // sums to 70
		{in: "metrics:70,logs:20,traces:20", wantErr: true}, // sums to 110
		{in: "spans:100", wantErr: true},                    // unknown signal
		{in: "metrics", wantErr: true},                      // no colon
		{in: "metrics:x", wantErr: true},                    // non-numeric
	}
	for _, c := range cases {
		m, l, tr, err := parseMix(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseMix(%q): want error, got m:%d l:%d t:%d", c.in, m, l, tr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMix(%q): unexpected error %v", c.in, err)
			continue
		}
		if m != c.m || l != c.l || tr != c.t {
			t.Errorf("parseMix(%q) = %d/%d/%d, want %d/%d/%d", c.in, m, l, tr, c.m, c.l, c.t)
		}
	}
}

// TestPickSignal_ExactSplit verifies the deal-out is exactly
// proportional per 100 sequence numbers — the property the run
// report's cross-check against otlp_batches relies on.
func TestPickSignal_ExactSplit(t *testing.T) {
	cfg := config{mixMetrics: 70, mixLogs: 20, mixTraces: 10}
	counts := map[signalKind]int{}
	for seq := int64(0); seq < 1000; seq++ {
		counts[pickSignal(cfg, seq)]++
	}
	if counts[sigMetrics] != 700 || counts[sigLogs] != 200 || counts[sigTraces] != 100 {
		t.Fatalf("split = m:%d l:%d t:%d, want 700/200/100",
			counts[sigMetrics], counts[sigLogs], counts[sigTraces])
	}
}

func TestPercentiles_Empty(t *testing.T) {
	st := &stats{}
	p50, p95, p99, max := st.percentiles()
	if p50 != 0 || p95 != 0 || p99 != 0 || max != 0 {
		t.Fatalf("empty percentiles should be zero, got %v %v %v %v", p50, p95, p99, max)
	}
	st.recordLatency(5 * time.Millisecond)
	p50, _, _, max = st.percentiles()
	if p50 != 5*time.Millisecond || max != 5*time.Millisecond {
		t.Fatalf("single-sample percentiles wrong: p50=%v max=%v", p50, max)
	}
}
