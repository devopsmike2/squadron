// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// otlpsim is a synthetic OTLP/HTTP load generator for Squadron.
//
// It pushes OTLP metrics, logs, and traces at a controlled rate
// against a running Squadron instance's OTLP/HTTP receiver — real
// protobuf ExportRequests over the standard endpoints, the same
// bytes a production OpenTelemetry Collector would send. This is the
// v0.22.x follow-up deferred in docs/scale-testing.md: fleetsim
// stresses the OpAMP server path; otlpsim stresses the ingest path
// (receiver → worker pool → DuckDB writes → otlp_batches accounting).
//
// Usage:
//
//	otlpsim --rate=200 --duration=60s
//	otlpsim --agents=500 --rate=1000 --senders=16 --signal-mix=metrics:70,logs:20,traces:10
//	otlpsim --rate=2000 --ramp=30s --gzip
//
// Agent identity composes with fleetsim: both derive the same
// deterministic per-index UUID (UUIDv5 under the "fleetsim"
// namespace) and set it as service.instance.id, which the parser
// adopts verbatim as agent_id. Run fleetsim for a live OpAMP fleet
// and otlpsim for its telemetry and the data attributes to the same
// simulated agents.
//
// What gets measured (client side):
//   - Achieved batch rate vs requested.
//   - Per-request latency percentiles (p50/p95/p99/max).
//   - HTTP outcome split: 2xx OK / 503 backpressure (worker queue
//     full) / other errors. 503s are the server's designed
//     backpressure signal — count them, don't retry them.
//   - Items + bytes sent per signal, for cross-checking against the
//     server's otlp_batches accounting and /metrics counters.
//
// Server-side observation (queue depth, dead letters, DuckDB size,
// insights latency under burst) is the run harness's job — see
// docs/scale-testing.md.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

type config struct {
	target      string
	agents      int
	rate        int
	duration    time.Duration
	ramp        time.Duration
	senders     int
	itemsPerReq int
	signalMix   string
	labelPrefix string
	useGzip     bool
	verbose     bool
	mixMetrics  int
	mixLogs     int
	mixTraces   int
}

type signalKind int

const (
	sigMetrics signalKind = iota
	sigLogs
	sigTraces
)

func (s signalKind) path() string {
	switch s {
	case sigMetrics:
		return "/v1/metrics"
	case sigLogs:
		return "/v1/logs"
	default:
		return "/v1/traces"
	}
}

// stats is the shared progress counter all senders update. Writes
// use atomics so the hot path stays mutex-free; the latency recorder
// is the one guarded structure (append under lock, ~O(1)).
type stats struct {
	sent         atomic.Int64
	ok           atomic.Int64
	backpressure atomic.Int64 // HTTP 503 — worker queue full
	httpErr      atomic.Int64 // non-2xx, non-503 status
	netErr       atomic.Int64 // transport-level failure
	bytesSent    atomic.Int64
	itemsMetrics atomic.Int64
	itemsLogs    atomic.Int64
	itemsTraces  atomic.Int64

	mu        sync.Mutex
	latencies []time.Duration // microsecond-resolution per-request wall time
}

func (st *stats) recordLatency(d time.Duration) {
	st.mu.Lock()
	st.latencies = append(st.latencies, d)
	st.mu.Unlock()
}

// percentiles returns p50/p95/p99/max over a copy of the recorded
// latencies. Called from the status line (1Hz) and the final report;
// the copy keeps the lock hold time proportional to a memcpy.
func (st *stats) percentiles() (p50, p95, p99, max time.Duration) {
	st.mu.Lock()
	snap := make([]time.Duration, len(st.latencies))
	copy(snap, st.latencies)
	st.mu.Unlock()
	if len(snap) == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(snap, func(i, j int) bool { return snap[i] < snap[j] })
	idx := func(p float64) time.Duration {
		i := int(float64(len(snap)-1) * p)
		return snap[i]
	}
	return idx(0.50), idx(0.95), idx(0.99), snap[len(snap)-1]
}

// job is one send unit: which agent identity and which signal.
type job struct {
	agentIdx int
	kind     signalKind
	seq      int64
}

func main() {
	cfg := parseFlags()
	log.SetFlags(log.Ltime)

	st := &stats{latencies: make([]time.Duration, 0, 1<<16)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Pre-marshal per-agent resource blocks once — the per-request
	// work is then scope+datapoint construction and one proto.Marshal.
	agents := make([]*resourcepb.Resource, cfg.agents)
	for i := range agents {
		agents[i] = agentResource(i, cfg)
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        cfg.senders * 2,
			MaxIdleConnsPerHost: cfg.senders * 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	jobs := make(chan job, cfg.senders*4)
	var wg sync.WaitGroup
	for i := 0; i < cfg.senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sender(ctx, cfg, client, agents, jobs, st)
		}()
	}

	go statusLine(ctx, st, cfg)

	log.Printf("otlpsim: %d agents → %s, target %d req/s (ramp %s, duration %s, %d senders, mix m:%d/l:%d/t:%d)",
		cfg.agents, cfg.target, cfg.rate, cfg.ramp, cfg.duration, cfg.senders,
		cfg.mixMetrics, cfg.mixLogs, cfg.mixTraces)

	start := time.Now()
	dispatchDone := make(chan struct{})
	go func() {
		dispatch(ctx, cfg, jobs)
		close(dispatchDone)
	}()

	// Wait for: configured duration elapsing (dispatch returns),
	// or SIGINT/SIGTERM.
	select {
	case <-dispatchDone:
	case <-sigCh:
		log.Println("otlpsim: interrupted, draining in-flight requests…")
		cancel()
	}
	// Stop feeding senders and let in-flight requests finish.
	close(jobs)
	wg.Wait()
	cancel()

	elapsed := time.Since(start)
	report(st, cfg, elapsed)
}

// dispatch paces job production at the requested rate. Every 100ms
// it releases the appropriate slice of the per-second budget, with a
// fractional accumulator so non-multiple-of-10 rates don't drift.
// During the ramp window the effective rate scales linearly 0→rate.
func dispatch(ctx context.Context, cfg config, jobs chan<- job) {
	const tick = 100 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()

	deadline := time.Time{}
	if cfg.duration > 0 {
		deadline = time.Now().Add(cfg.duration)
	}
	start := time.Now()
	var acc float64
	var seq int64
	agentIdx := 0

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if !deadline.IsZero() && now.After(deadline) {
				return
			}
			effRate := float64(cfg.rate)
			if cfg.ramp > 0 {
				since := now.Sub(start)
				if since < cfg.ramp {
					effRate *= float64(since) / float64(cfg.ramp)
				}
			}
			acc += effRate * tick.Seconds()
			n := int(acc)
			acc -= float64(n)
			for i := 0; i < n; i++ {
				seq++
				j := job{
					agentIdx: agentIdx % cfg.agents,
					kind:     pickSignal(cfg, seq),
					seq:      seq,
				}
				agentIdx++
				select {
				case jobs <- j:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// pickSignal deals signals out deterministically in proportion to the
// mix (per-100 sequence slots), so a run's signal split is exact
// rather than probabilistic — easier to cross-check against
// otlp_batches counts.
func pickSignal(cfg config, seq int64) signalKind {
	slot := int(seq % 100)
	switch {
	case slot < cfg.mixMetrics:
		return sigMetrics
	case slot < cfg.mixMetrics+cfg.mixLogs:
		return sigLogs
	default:
		return sigTraces
	}
}

// sender consumes jobs, builds the OTLP payload, POSTs it, and
// classifies the outcome. 503 is counted as backpressure and NOT
// retried — the point of the load test is to observe the server's
// designed shed behavior, not to mask it.
func sender(ctx context.Context, cfg config, client *http.Client, agents []*resourcepb.Resource, jobs <-chan job, st *stats) {
	for j := range jobs {
		if ctx.Err() != nil {
			return
		}
		body, items, err := buildPayload(cfg, agents[j.agentIdx], j)
		if err != nil {
			// Deterministic marshal failure — a bug, not load.
			log.Printf("otlpsim: payload build failed: %v", err)
			continue
		}

		payload := body
		if cfg.useGzip {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write(body)
			_ = zw.Close()
			payload = buf.Bytes()
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.target+j.kind.path(), bytes.NewReader(payload))
		if err != nil {
			st.netErr.Add(1)
			continue
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		if cfg.useGzip {
			req.Header.Set("Content-Encoding", "gzip")
		}

		st.sent.Add(1)
		st.bytesSent.Add(int64(len(payload)))
		reqStart := time.Now()
		resp, err := client.Do(req)
		lat := time.Since(reqStart)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			st.netErr.Add(1)
			if cfg.verbose {
				log.Printf("otlpsim: request failed: %v", err)
			}
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		st.recordLatency(lat)

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			st.ok.Add(1)
			switch j.kind {
			case sigMetrics:
				st.itemsMetrics.Add(int64(items))
			case sigLogs:
				st.itemsLogs.Add(int64(items))
			case sigTraces:
				st.itemsTraces.Add(int64(items))
			}
		case resp.StatusCode == http.StatusServiceUnavailable:
			st.backpressure.Add(1)
		default:
			st.httpErr.Add(1)
			if cfg.verbose {
				log.Printf("otlpsim: HTTP %d on %s", resp.StatusCode, j.kind.path())
			}
		}
	}
}

// buildPayload constructs a marshaled OTLP ExportRequest for the
// job's signal, returning the body and the item count it carries
// (data points / log records / spans — the unit otlp_batches counts).
func buildPayload(cfg config, res *resourcepb.Resource, j job) ([]byte, int, error) {
	now := uint64(time.Now().UnixNano())
	switch j.kind {
	case sigMetrics:
		msg := &colmetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{{
				Resource: res,
				ScopeMetrics: []*metricspb.ScopeMetrics{{
					Scope:   scope(),
					Metrics: syntheticMetrics(cfg.itemsPerReq, j.seq, now),
				}},
			}},
		}
		b, err := proto.Marshal(msg)
		return b, cfg.itemsPerReq, err
	case sigLogs:
		msg := &collogspb.ExportLogsServiceRequest{
			ResourceLogs: []*logspb.ResourceLogs{{
				Resource: res,
				ScopeLogs: []*logspb.ScopeLogs{{
					Scope:      scope(),
					LogRecords: syntheticLogs(cfg.itemsPerReq, j.seq, now),
				}},
			}},
		}
		b, err := proto.Marshal(msg)
		return b, cfg.itemsPerReq, err
	default:
		msg := &coltracepb.ExportTraceServiceRequest{
			ResourceSpans: []*tracepb.ResourceSpans{{
				Resource: res,
				ScopeSpans: []*tracepb.ScopeSpans{{
					Scope: scope(),
					Spans: syntheticSpans(cfg.itemsPerReq, j.seq, now),
				}},
			}},
		}
		b, err := proto.Marshal(msg)
		return b, cfg.itemsPerReq, err
	}
}

func scope() *commonpb.InstrumentationScope {
	return &commonpb.InstrumentationScope{Name: "otlpsim", Version: "1"}
}

// metricNames is a small stable vocabulary so the ingest exercises
// realistic name cardinality without exploding the attribute sampler.
var metricNames = []string{
	"synthetic.http.server.duration",
	"synthetic.cpu.utilization",
	"synthetic.memory.usage",
	"synthetic.queue.depth",
	"synthetic.request.count",
}

func syntheticMetrics(n int, seq int64, now uint64) []*metricspb.Metric {
	out := make([]*metricspb.Metric, 0, n)
	for i := 0; i < n; i++ {
		name := metricNames[i%len(metricNames)]
		dp := &metricspb.NumberDataPoint{
			TimeUnixNano: now,
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: float64((seq+int64(i))%1000) / 10.0},
			Attributes: []*commonpb.KeyValue{
				kv("otlpsim.shard", strconv.Itoa(i%8)),
				kv("http.method", []string{"GET", "POST", "PUT"}[i%3]),
			},
		}
		// Alternate sum/gauge so both write paths (metrics_sum,
		// metrics_gauge) take load.
		if i%2 == 0 {
			out = append(out, &metricspb.Metric{
				Name: name,
				Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
					AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
					IsMonotonic:            true,
					DataPoints:             []*metricspb.NumberDataPoint{dp},
				}},
			})
		} else {
			out = append(out, &metricspb.Metric{
				Name: name,
				Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
					DataPoints: []*metricspb.NumberDataPoint{dp},
				}},
			})
		}
	}
	return out
}

func syntheticLogs(n int, seq int64, now uint64) []*logspb.LogRecord {
	out := make([]*logspb.LogRecord, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &logspb.LogRecord{
			TimeUnixNano:   now,
			SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
			SeverityText:   "INFO",
			Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{
				StringValue: fmt.Sprintf("otlpsim synthetic log line seq=%d idx=%d — request handled in %dms", seq, i, (seq+int64(i))%250),
			}},
			Attributes: []*commonpb.KeyValue{
				kv("otlpsim.shard", strconv.Itoa(i%8)),
			},
		})
	}
	return out
}

func syntheticSpans(n int, seq int64, now uint64) []*tracepb.Span {
	out := make([]*tracepb.Span, 0, n)
	// Deterministic IDs derived from seq: reproducible runs, no
	// deprecated global-rand use, and unique per batch/span.
	traceID := make([]byte, 16)
	binary.BigEndian.PutUint64(traceID[:8], uint64(seq))
	binary.BigEndian.PutUint64(traceID[8:], ^uint64(seq))
	for i := 0; i < n; i++ {
		spanID := make([]byte, 8)
		binary.BigEndian.PutUint64(spanID, uint64(seq)<<16|uint64(i)+1)
		durNs := uint64((seq+int64(i))%200+1) * uint64(time.Millisecond)
		out = append(out, &tracepb.Span{
			TraceId:           traceID,
			SpanId:            spanID,
			Name:              []string{"GET /api/items", "SELECT items", "cache.lookup"}[i%3],
			Kind:              tracepb.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: now - durNs,
			EndTimeUnixNano:   now,
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
			Attributes: []*commonpb.KeyValue{
				kv("otlpsim.shard", strconv.Itoa(i%8)),
			},
		})
	}
	return out
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   k,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}},
	}
}

// agentResource builds the per-agent OTLP resource block. The
// service.instance.id is the SAME deterministic UUID fleetsim
// assigns to agent index i (UUIDv5, "fleetsim" namespace), so
// telemetry attributes to fleetsim's simulated agents when the two
// generators run together.
func agentResource(idx int, cfg config) *resourcepb.Resource {
	pid := os.Getpid()
	hostname, _ := os.Hostname()
	return &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			kv("service.name", "synthetic-collector"),
			kv("service.version", "0.119.0"),
			kv("service.instance.id", deterministicUUID(idx).String()),
			kv("host.name", fmt.Sprintf("%s-sim-%d-%d", hostname, pid, idx)),
			kv("simulated.fleet", cfg.labelPrefix),
		},
	}
}

// deterministicUUID matches cmd/fleetsim exactly: same index → same
// UUID across both tools and across runs.
func deterministicUUID(idx int) uuid.UUID {
	ns := uuid.NewSHA1(uuid.NameSpaceDNS, []byte("fleetsim"))
	return uuid.NewSHA1(ns, fmt.Appendf(nil, "agent-%d", idx))
}

func statusLine(ctx context.Context, st *stats, cfg config) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	var lastSent int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sent := st.sent.Load()
			p50, _, p99, _ := st.percentiles()
			fmt.Fprintf(os.Stderr,
				"\rsent:%d (%d/s)  ok:%d  503:%d  err:%d  p50:%s p99:%s   ",
				sent, sent-lastSent,
				st.ok.Load(), st.backpressure.Load(),
				st.httpErr.Load()+st.netErr.Load(),
				p50.Round(time.Millisecond), p99.Round(time.Millisecond),
			)
			lastSent = sent
		}
	}
}

// report prints the machine-checkable final block. The items-per-
// signal counts are the numbers to reconcile against the server's
// otlp_batches table (SUM(item_count) GROUP BY signal_type).
func report(st *stats, cfg config, elapsed time.Duration) {
	p50, p95, p99, max := st.percentiles()
	sent := st.sent.Load()
	achieved := float64(sent) / elapsed.Seconds()
	fmt.Printf("\n--- otlpsim report ---\n")
	fmt.Printf("elapsed:        %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("requests sent:  %d (%.1f/s achieved, %d/s requested)\n", sent, achieved, cfg.rate)
	fmt.Printf("ok:             %d\n", st.ok.Load())
	fmt.Printf("503 backpressure: %d\n", st.backpressure.Load())
	fmt.Printf("http errors:    %d\n", st.httpErr.Load())
	fmt.Printf("net errors:     %d\n", st.netErr.Load())
	fmt.Printf("bytes sent:     %d (%.2f MiB)\n", st.bytesSent.Load(), float64(st.bytesSent.Load())/(1<<20))
	fmt.Printf("items ok — metrics:%d logs:%d traces:%d\n",
		st.itemsMetrics.Load(), st.itemsLogs.Load(), st.itemsTraces.Load())
	fmt.Printf("latency p50:%s p95:%s p99:%s max:%s\n",
		p50.Round(time.Microsecond), p95.Round(time.Microsecond),
		p99.Round(time.Microsecond), max.Round(time.Microsecond))
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.target, "target", "http://localhost:4318",
		"OTLP/HTTP receiver base URL (endpoints /v1/{metrics,logs,traces} are appended)")
	flag.IntVar(&cfg.agents, "agents", 100,
		"Number of distinct simulated agent identities (fleetsim-compatible UUIDs)")
	flag.IntVar(&cfg.rate, "rate", 200,
		"Target aggregate request rate (ExportRequests per second)")
	flag.DurationVar(&cfg.duration, "duration", 60*time.Second,
		"How long to run (0 = until SIGINT)")
	flag.DurationVar(&cfg.ramp, "ramp", 10*time.Second,
		"Linearly ramp the rate from 0 to --rate over this window (0 = full rate immediately)")
	flag.IntVar(&cfg.senders, "senders", 8,
		"Concurrent HTTP sender workers")
	flag.IntVar(&cfg.itemsPerReq, "items-per-batch", 50,
		"Data points / log records / spans per ExportRequest")
	flag.StringVar(&cfg.signalMix, "signal-mix", "metrics:70,logs:20,traces:10",
		"Signal split per 100 requests, e.g. metrics:70,logs:20,traces:10 (must sum to 100)")
	flag.StringVar(&cfg.labelPrefix, "label-prefix", "otlpsim",
		"value of the 'simulated.fleet' resource attribute")
	flag.BoolVar(&cfg.useGzip, "gzip", false,
		"gzip-compress request bodies (Content-Encoding: gzip)")
	flag.BoolVar(&cfg.verbose, "v", false,
		"Verbose per-request error logging")
	flag.Parse()

	if cfg.agents <= 0 {
		fatalUsage("--agents must be > 0")
	}
	if cfg.rate <= 0 {
		fatalUsage("--rate must be > 0")
	}
	if cfg.senders <= 0 {
		fatalUsage("--senders must be > 0")
	}
	if cfg.itemsPerReq <= 0 {
		fatalUsage("--items-per-batch must be > 0")
	}

	m, l, t, err := parseMix(cfg.signalMix)
	if err != nil {
		fatalUsage(err.Error())
	}
	cfg.mixMetrics, cfg.mixLogs, cfg.mixTraces = m, l, t
	return cfg
}

// parseMix parses "metrics:70,logs:20,traces:10" into three ints
// summing to 100. Order-insensitive; omitted signals default to 0.
func parseMix(s string) (metrics, logs, traces int, err error) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, found := strings.Cut(part, ":")
		if !found {
			return 0, 0, 0, fmt.Errorf("bad --signal-mix segment %q (want name:pct)", part)
		}
		pct, convErr := strconv.Atoi(strings.TrimSpace(v))
		if convErr != nil || pct < 0 {
			return 0, 0, 0, fmt.Errorf("bad --signal-mix percentage in %q", part)
		}
		switch strings.TrimSpace(k) {
		case "metrics":
			metrics = pct
		case "logs":
			logs = pct
		case "traces":
			traces = pct
		default:
			return 0, 0, 0, fmt.Errorf("unknown signal %q in --signal-mix", k)
		}
	}
	if metrics+logs+traces != 100 {
		return 0, 0, 0, fmt.Errorf("--signal-mix must sum to 100 (got %d)", metrics+logs+traces)
	}
	return metrics, logs, traces, nil
}

func fatalUsage(msg string) {
	fmt.Fprintln(os.Stderr, "otlpsim: "+msg)
	os.Exit(2)
}
