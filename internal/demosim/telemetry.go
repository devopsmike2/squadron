package demosim

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/otlp"
	tstypes "github.com/devopsmike2/squadron/internal/storage/telemetrystore/types"
)

// runTelemetry is the low-rate background loop. Each tick it emits a small,
// realistic batch of metrics/logs/traces for the next slice of online agents,
// cycling through the whole fleet over time so every agent accrues history and
// the charts keep moving while the operator explores. Cancelled via ctx.
func (s *Simulator) runTelemetry(ctx context.Context) {
	ticker := time.NewTicker(s.tickEvery)
	defer ticker.Stop()

	// Emit an immediate first batch so the UI isn't empty for a full tick.
	s.emitOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("demosim: telemetry loop stopped")
			return
		case <-ticker.C:
			s.emitOnce(ctx)
		}
	}
}

// emitOnce writes one tick's worth of telemetry for perTick online agents,
// advancing the round-robin cursor.
func (s *Simulator) emitOnce(ctx context.Context) {
	s.mu.Lock()
	agents := s.agents
	start := s.cursor
	if len(agents) == 0 {
		s.mu.Unlock()
		return
	}
	s.cursor = (s.cursor + s.perTick) % len(agents)
	s.mu.Unlock()

	now := time.Now().UTC()

	var sums []otlp.MetricSumData
	var gauges []otlp.MetricGaugeData
	var logs []otlp.LogData
	var traces []otlp.TraceData
	type meta struct {
		agentID string
		signal  string
		items   int64
		bytes   int64
	}
	var metas []meta

	for i := 0; i < s.perTick; i++ {
		a := agents[(start+i)%len(agents)]
		if !a.online {
			continue
		}
		aid := a.id.String()
		svc := "otelcol-" + a.role
		res := map[string]string{
			"service.name":           svc,
			"service.instance.id":    aid,
			"host.name":              a.name,
			"deployment.environment": "prod",
			"squadron.role":          a.role,
			"otel.exporter.endpoint": a.exporter,
		}

		// --- Metrics: two gauges + two counters ---
		mItems := 0
		gauges = append(gauges,
			gauge(res, svc, aid, a, "system.cpu.utilization", "1", 0.15+rand.Float64()*0.7, now),
			gauge(res, svc, aid, a, "system.memory.utilization", "1", 0.30+rand.Float64()*0.55, now),
		)
		sums = append(sums,
			sum(res, svc, aid, a, "otelcol_receiver_accepted_metric_points", "1", float64(500+rand.Intn(4000)), now),
			sum(res, svc, aid, a, "http.server.request.count", "1", float64(50+rand.Intn(900)), now),
		)
		mItems = 4
		metas = append(metas, meta{aid, "metrics", int64(mItems), int64(mItems) * 460})

		// --- Logs: a couple, mostly INFO ---
		nLogs := 2 + rand.Intn(2)
		lBytes := int64(0)
		for j := 0; j < nLogs; j++ {
			sev, num, body := "INFO", int32(9), "request completed"
			if rand.Float64() < 0.04 {
				sev, num, body = "ERROR", int32(17), "upstream request timed out after 30s"
			}
			logs = append(logs, otlp.LogData{
				Timestamp:          now,
				SeverityText:       sev,
				SeverityNumber:     num,
				ServiceName:        svc,
				Body:               body,
				ResourceAttributes: res,
				LogAttributes: map[string]string{
					"http.method": pick(rand.Intn(4), "GET", "POST", "PUT", "DELETE"),
					"http.route":  routeFor(a.role),
				},
				AgentID:   aid,
				GroupID:   a.groupID,
				GroupName: a.groupName,
			})
			lBytes += 340
		}
		metas = append(metas, meta{aid, "logs", int64(nLogs), lBytes})

		// --- Traces: a few spans. Each carries a unique high-cardinality
		// http.url attribute — the noisy attribute the cost-attribution and
		// recommendation engines will flag as a drop candidate. ---
		nSpans := 2 + rand.Intn(3)
		tBytes := int64(0)
		traceID := hexID(16)
		for j := 0; j < nSpans; j++ {
			status, msg := "STATUS_CODE_OK", ""
			if rand.Float64() < 0.05 {
				status, msg = "STATUS_CODE_ERROR", "handler returned 500"
			}
			traces = append(traces, otlp.TraceData{
				Timestamp:          now,
				TraceId:            traceID,
				SpanId:             hexID(8),
				SpanName:           routeFor(a.role),
				SpanKind:           2, // SERVER
				ServiceName:        svc,
				ResourceAttributes: res,
				SpanAttributes: map[string]string{
					"http.method":      pick(rand.Intn(4), "GET", "POST", "PUT", "DELETE"),
					"http.route":       routeFor(a.role),
					"http.status_code": pick(rand.Intn(5), "200", "200", "200", "404", "500"),
					// High-cardinality: unique per span → dominates trace bytes.
					"http.url":     fmt.Sprintf("https://%s.prod.internal%s?rid=%s&ts=%d", a.role, routeFor(a.role), hexID(8), now.UnixNano()),
					"http.user_id": fmt.Sprintf("u-%s", hexID(6)),
				},
				Duration:      int64(2_000_000 + rand.Intn(400_000_000)), // 2–400ms in ns
				StatusCode:    status,
				StatusMessage: msg,
				AgentID:       aid,
				GroupID:       a.groupID,
				GroupName:     a.groupName,
			})
			tBytes += 920 // traces are heavier, driven by the long http.url
		}
		metas = append(metas, meta{aid, "traces", int64(nSpans), tBytes})
	}

	if s.writer == nil {
		return
	}
	if len(sums) > 0 || len(gauges) > 0 {
		if err := s.writer.WriteMetrics(ctx, sums, gauges, nil); err != nil {
			s.logger.Warn("demosim: write metrics failed", zap.Error(err))
		}
	}
	if len(logs) > 0 {
		if err := s.writer.WriteLogs(ctx, logs); err != nil {
			s.logger.Warn("demosim: write logs failed", zap.Error(err))
		}
	}
	if len(traces) > 0 {
		if err := s.writer.WriteTraces(ctx, traces); err != nil {
			s.logger.Warn("demosim: write traces failed", zap.Error(err))
		}
	}
	for _, m := range metas {
		_ = s.writer.WriteBatchMeta(ctx, tstypes.BatchMeta{
			Timestamp:    now,
			AgentID:      m.agentID,
			SignalType:   m.signal,
			ItemCount:    m.items,
			DroppedCount: 0,
			PayloadBytes: m.bytes,
			Status:       "ok",
		})
	}
}

func gauge(res map[string]string, svc, aid string, a simAgent, name, unit string, val float64, now time.Time) otlp.MetricGaugeData {
	return otlp.MetricGaugeData{
		ResourceAttributes: res,
		ServiceName:        svc,
		MetricName:         name,
		MetricUnit:         unit,
		Attributes:         map[string]string{"host": a.name},
		TimeUnix:           now,
		StartTimeUnix:      now,
		Value:              val,
		AgentID:            aid,
		GroupID:            a.groupID,
		GroupName:          a.groupName,
	}
}

func sum(res map[string]string, svc, aid string, a simAgent, name, unit string, val float64, now time.Time) otlp.MetricSumData {
	return otlp.MetricSumData{
		ResourceAttributes:     res,
		ServiceName:            svc,
		MetricName:             name,
		MetricUnit:             unit,
		Attributes:             map[string]string{"host": a.name},
		TimeUnix:               now,
		StartTimeUnix:          now,
		Value:                  val,
		IsMonotonic:            true,
		AggregationTemporality: 2, // CUMULATIVE
		AgentID:                aid,
		GroupID:                a.groupID,
		GroupName:              a.groupName,
	}
}

func routeFor(role string) string {
	switch role {
	case "web":
		return pick(rand.Intn(4), "/", "/checkout", "/product", "/cart")
	case "api":
		return pick(rand.Intn(4), "/v1/orders", "/v1/users", "/v1/payments", "/v1/search")
	case "worker":
		return pick(rand.Intn(3), "/jobs/process", "/jobs/retry", "/jobs/dispatch")
	case "data":
		return pick(rand.Intn(3), "/ingest", "/query", "/rollup")
	default:
		return pick(rand.Intn(3), "/edge/route", "/edge/cache", "/edge/auth")
	}
}

func pick(i int, opts ...string) string {
	if i < 0 || i >= len(opts) {
		i = 0
	}
	return opts[i]
}

// hexID returns a random lowercase hex id of n bytes (2n chars).
func hexID(n int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, n*2)
	for i := range b {
		b[i] = hexdigits[rand.Intn(16)]
	}
	return string(b)
}
