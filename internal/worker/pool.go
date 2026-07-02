package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/otlp"
	"github.com/devopsmike2/squadron/internal/otlp/parser"
	"github.com/devopsmike2/squadron/internal/otlp/processor"
	"github.com/devopsmike2/squadron/internal/pipelinehealth"
	"github.com/devopsmike2/squadron/internal/services"
	telemetrytypes "github.com/devopsmike2/squadron/internal/storage/telemetrystore/types"
	"go.uber.org/zap"
)

// retryBackoffs is the schedule of delays between write attempts. The total
// budget is ~2.6s before an item is dead-lettered. Adjust here if storage is
// expected to recover from longer hiccups.
var retryBackoffs = []time.Duration{
	100 * time.Millisecond,
	500 * time.Millisecond,
	2 * time.Second,
}

// TelemetryWriter defines the interface for writing telemetry data
type TelemetryWriter interface {
	WriteTraces(ctx context.Context, traces []otlp.TraceData) error
	WriteMetrics(ctx context.Context, sums []otlp.MetricSumData, gauges []otlp.MetricGaugeData, histograms []otlp.MetricHistogramData) error
	WriteLogs(ctx context.Context, logs []otlp.LogData) error

	// WriteBatchMeta writes one row to otlp_batches for the inbound
	// ExportRequest. Best-effort accounting; see telemetrytypes.Writer
	// for the contract.
	WriteBatchMeta(ctx context.Context, meta telemetrytypes.BatchMeta) error

	// WritePipelineHealth writes collector self-metrics extracted
	// from the regular metrics ingest stream. Best-effort; see
	// telemetrytypes.Writer for the contract.
	WritePipelineHealth(ctx context.Context, samples []telemetrytypes.PipelineHealthSample) error
}

// WorkItemType represents the type of work item
type WorkItemType int

const (
	WorkItemTypeTraces WorkItemType = iota
	WorkItemTypeMetrics
	WorkItemTypeLogs
)

// WorkItem represents a single unit of work with raw OTLP bytes
type WorkItem struct {
	Type      WorkItemType
	RawData   []byte // Raw protobuf bytes
	Timestamp time.Time
}

// Pool represents a worker pool
type Pool struct {
	queue         chan WorkItem
	shutdown      chan struct{}
	wg            sync.WaitGroup
	writer        TelemetryWriter
	parser        *parser.OTLPParser
	enricher      *processor.Enricher
	logger        *zap.Logger
	metrics       *metrics.WorkerMetrics
	queueSize     int
	workerCount   int
	submitTimeout time.Duration
	// Byte-budget backpressure (v0.89 ingest finding 3). queueSize caps
	// request COUNT; maxQueueBytes caps the volatile ack'd-but-unwritten
	// backlog in DATA — the real memory bound, since a burst of large batches
	// can hold ~500k items under the count cap alone. Item counts aren't known
	// until the worker parses, so bytes (len(RawData)) are the only cheap
	// signal at ingest. maxQueueBytes <= 0 disables the byte bound (count cap
	// only). Set via SetMaxQueueBytes before Start.
	maxQueueBytes int64
	queuedBytes   atomic.Int64  // Σ len(RawData) of items currently in the queue
	bytesFreed    chan struct{} // buffered(1) wake for Submit waiters when a worker frees bytes
	// v0.36: passive OTLP discovery. Optional — nil disables the
	// discovery hook so the worker pool's hot path is unchanged for
	// installs that don't want it.
	discovery *discovery.Service
}

// SetDiscovery wires the v0.36 passive OTLP discovery service.
// nil disables it (and the worker pool's hot path is unchanged).
// Called from main.go after construction so the existing
// NewPool signature stays back-compat.
func (p *Pool) SetDiscovery(svc *discovery.Service) { p.discovery = svc }

// SetMaxQueueBytes sets the byte-budget bound on the volatile queue (v0.89
// ingest finding 3). n <= 0 disables it (request-count cap only). Call before
// Start; the existing NewPool signature stays back-compat (default: disabled),
// so cmd/all-in-one wires the operator-configured value and tests are unchanged.
func (p *Pool) SetMaxQueueBytes(n int64) { p.maxQueueBytes = n }

// NewPool creates a new worker pool with configurable workers.
//
// If workerMetrics is nil, a no-op metrics struct is wired up so the pool can
// run unmetered (useful in tests). Production callers should pass real metrics
// from the shared Prometheus registry.
func NewPool(queueSize, workerCount int, submitTimeout time.Duration, writer TelemetryWriter, agentService services.AgentService, workerMetrics *metrics.WorkerMetrics, logger *zap.Logger) *Pool {
	if workerMetrics == nil {
		workerMetrics = metrics.NewWorkerMetrics(metrics.NullFactory)
	}
	return &Pool{
		queue:         make(chan WorkItem, queueSize),
		shutdown:      make(chan struct{}),
		bytesFreed:    make(chan struct{}, 1),
		writer:        writer,
		parser:        parser.NewOTLPParser(logger),
		enricher:      processor.NewEnricher(agentService, logger),
		logger:        logger,
		metrics:       workerMetrics,
		queueSize:     queueSize,
		workerCount:   workerCount,
		submitTimeout: submitTimeout,
	}
}

// Start starts the worker pool
func (p *Pool) Start() {
	p.logger.Info("Starting worker pool", zap.Int("workers", p.workerCount), zap.Int("queue_size", p.queueSize), zap.Duration("submit_timeout", p.submitTimeout))
	for i := 0; i < p.workerCount; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
}

// Stop gracefully stops the worker pool
func (p *Pool) Stop(timeout time.Duration) error {
	p.logger.Info("Stopping worker pool", zap.Duration("timeout", timeout))

	// Signal shutdown
	close(p.shutdown)

	// Wait for worker to finish with timeout
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Info("Worker pool stopped gracefully")
		return nil
	case <-time.After(timeout):
		p.logger.Warn("Worker pool shutdown timeout", zap.Int("remaining_items", len(p.queue)))
		return fmt.Errorf("shutdown timeout exceeded")
	}
}

// Submit submits a work item to the queue, applying both the request-count cap
// (the buffered channel) and — when enabled — the byte budget (maxQueueBytes),
// under a single submit_timeout deadline. Over-budget or a full channel blocks
// for up to submit_timeout then returns an error (the receiver maps that to a
// 503). A single payload larger than the whole byte budget is rejected
// immediately (it could never fit).
func (p *Pool) Submit(item WorkItem) error {
	sz := int64(len(item.RawData))

	if p.maxQueueBytes > 0 && sz > p.maxQueueBytes {
		return fmt.Errorf("payload %d bytes exceeds max_queue_bytes %d", sz, p.maxQueueBytes)
	}

	timer := time.NewTimer(p.submitTimeout)
	defer timer.Stop()

	// 1) Reserve the byte budget (if enabled), waiting for workers to free
	//    space. CAS keeps concurrent submitters from over-reserving.
	if p.maxQueueBytes > 0 {
		for {
			cur := p.queuedBytes.Load()
			if cur+sz <= p.maxQueueBytes {
				if p.queuedBytes.CompareAndSwap(cur, cur+sz) {
					break // reserved
				}
				continue // lost the race; re-read and retry immediately
			}
			select {
			case <-timer.C:
				return fmt.Errorf("queue full (byte budget %d), submit timeout", p.maxQueueBytes)
			case <-p.bytesFreed:
				// a worker freed bytes — re-check the budget
			case <-p.shutdown:
				return fmt.Errorf("worker pool shutting down")
			}
		}
	} else {
		p.queuedBytes.Add(sz) // keep the gauge honest even when the bound is off
	}

	// 2) Enqueue under the same deadline (channel count cap).
	select {
	case p.queue <- item:
		p.metrics.QueueDepth.Update(int64(len(p.queue)))
		p.metrics.QueueBytes.Update(p.queuedBytes.Load())
		return nil
	case <-timer.C:
		p.releaseBytes(sz) // roll back the reservation
		return fmt.Errorf("queue full, submit timeout")
	case <-p.shutdown:
		p.releaseBytes(sz)
		return fmt.Errorf("worker pool shutting down")
	}
}

// releaseBytes returns sz bytes to the budget (on worker dequeue/drain, or when
// a Submit rolls back a failed enqueue) and wakes one waiting submitter.
func (p *Pool) releaseBytes(sz int64) {
	p.queuedBytes.Add(-sz)
	p.metrics.QueueBytes.Update(p.queuedBytes.Load())
	// Non-blocking wake: buffered(1) coalesces bursts; a waiter re-checks the
	// budget after each wake, so a coalesced signal never strands it.
	select {
	case p.bytesFreed <- struct{}{}:
	default:
	}
}

// QueueDepth returns the current queue depth (request count)
func (p *Pool) QueueDepth() int {
	return len(p.queue)
}

// QueuedBytes returns the current volatile queued payload bytes (Σ len(RawData)
// of items awaiting write) — the byte-budget's live view.
func (p *Pool) QueuedBytes() int64 {
	return p.queuedBytes.Load()
}

// worker is the main worker goroutine
func (p *Pool) worker(id int) {
	defer p.wg.Done()

	p.logger.Info("Worker started", zap.Int("worker_id", id))

	for {
		select {
		case item := <-p.queue:
			p.releaseBytes(int64(len(item.RawData))) // item left the queue — free its budget + wake a waiter
			p.metrics.QueueDepth.Update(int64(len(p.queue)))
			p.processItem(item)
		case <-p.shutdown:
			// Drain remaining items
			p.logger.Info("Draining remaining queue items", zap.Int("count", len(p.queue)))
			for {
				select {
				case item := <-p.queue:
					p.releaseBytes(int64(len(item.RawData)))
					p.processItem(item)
				default:
					p.logger.Info("Worker stopped", zap.Int("worker_id", id))
					return
				}
			}
		}
	}
}

// writeWithRetry calls writeFn, retrying on error with the configured backoff
// schedule. If all retries are exhausted, the item is dead-lettered: the
// signal-specific dead-letter counter is incremented and the failure is logged.
// Shutdown short-circuits any in-flight backoff so we don't block Stop.
func (p *Pool) writeWithRetry(ctx context.Context, signal string, retries, deadLetters metrics.Counter, writeFn func(context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt <= len(retryBackoffs); attempt++ {
		if err := writeFn(ctx); err == nil {
			if attempt > 0 {
				p.logger.Info("storage write succeeded after retry",
					zap.String("signal", signal),
					zap.Int("attempts", attempt+1))
			}
			return nil
		} else {
			lastErr = err
		}

		if attempt == len(retryBackoffs) {
			break // budget exhausted, fall through to dead-letter
		}

		retries.Inc(1)
		p.logger.Warn("storage write failed, retrying",
			zap.String("signal", signal),
			zap.Int("attempt", attempt+1),
			zap.Duration("backoff", retryBackoffs[attempt]),
			zap.Error(lastErr))

		select {
		case <-time.After(retryBackoffs[attempt]):
		case <-p.shutdown:
			return errors.Join(lastErr, fmt.Errorf("shutdown during retry"))
		}
	}

	deadLetters.Inc(1)
	p.logger.Error("dead-lettering item after exhausting retries",
		zap.String("signal", signal),
		zap.Int("attempts", len(retryBackoffs)+1),
		zap.Error(lastErr))
	return lastErr
}

// processItem processes a single work item. Parse failures are logged and the
// item is dropped (no retry — a parse error is deterministic and won't recover).
// Write failures get retried and, on final failure, dead-lettered with a metric.
//
// After the actual telemetry write completes, the worker writes a batch meta
// row to otlp_batches. That row drives the v0.24+ Telemetry Volume Insights
// surface. The accounting is best-effort: an error here is logged but does
// not propagate, since the real telemetry has already been written.
func (p *Pool) processItem(item WorkItem) {
	start := time.Now()
	ctx := context.Background()
	totalBytes := int64(len(item.RawData))

	switch item.Type {
	case WorkItemTypeTraces:
		traces, err := p.parser.ParseTraces(item.RawData)
		if err != nil {
			p.logger.Error("Failed to parse traces", zap.Error(err))
			return
		}
		p.enricher.EnrichTraces(ctx, traces)

		writeErr := p.writeWithRetry(ctx, "traces", p.metrics.TraceWriteRetries, p.metrics.TraceDeadLetters, func(c context.Context) error {
			return p.writer.WriteTraces(c, traces)
		})
		p.recordBatchMeta(ctx, "traces", traces, totalBytes, writeErr)
		// v0.36 discovery: register agents we see in incoming
		// telemetry even if they never opened an OpAMP session.
		// Only attempt on successful write — a failed batch isn't
		// evidence the agent exists.
		if writeErr == nil && p.discovery != nil {
			p.discoverFromTraces(ctx, traces)
		}
		p.logger.Debug("Processed traces",
			zap.Int("count", len(traces)),
			zap.Duration("duration", time.Since(start)),
			zap.Error(writeErr))

	case WorkItemTypeMetrics:
		sums, gauges, histograms, err := p.parser.ParseMetrics(item.RawData)
		if err != nil {
			p.logger.Error("Failed to parse metrics", zap.Error(err))
			return
		}
		p.enricher.EnrichMetrics(ctx, sums, gauges, histograms)

		writeErr := p.writeWithRetry(ctx, "metrics", p.metrics.MetricWriteRetries, p.metrics.MetricDeadLetters, func(c context.Context) error {
			return p.writer.WriteMetrics(c, sums, gauges, histograms)
		})
		// Metrics batch accounting unions across all metric subtypes —
		// a single ExportRequest can mix sums, gauges, and histograms.
		// We emit one BatchMeta per agent_id seen across all three.
		p.recordMetricsBatchMeta(ctx, sums, gauges, histograms, totalBytes, writeErr)

		// v0.36 discovery on the metrics path.
		if writeErr == nil && p.discovery != nil {
			p.discoverFromMetrics(ctx, sums, gauges, histograms)
		}

		// Pipeline-health extraction (v0.31+): if the batch contains
		// otelcol_* self-metrics, fork them into the dedicated
		// pipeline_health_samples table so the dashboard can query
		// without re-scanning the wide metrics tables. The regular
		// write above still includes these data points in
		// metrics_sum / metrics_gauge for SquadronQL power users.
		if pipelinehealth.HasAny(sums, gauges) {
			samples := pipelinehealth.Extract(sums, gauges)
			if len(samples) > 0 {
				if err := p.writer.WritePipelineHealth(ctx, samples); err != nil {
					p.logger.Warn("pipeline-health write failed (non-fatal)",
						zap.Int("samples", len(samples)),
						zap.Error(err))
				}
			}
		}

		p.logger.Debug("Processed metrics",
			zap.Int("sums", len(sums)),
			zap.Int("gauges", len(gauges)),
			zap.Int("histograms", len(histograms)),
			zap.Duration("duration", time.Since(start)),
			zap.Error(writeErr))

	case WorkItemTypeLogs:
		logs, err := p.parser.ParseLogs(item.RawData)
		if err != nil {
			p.logger.Error("Failed to parse logs", zap.Error(err))
			return
		}
		p.enricher.EnrichLogs(ctx, logs)

		writeErr := p.writeWithRetry(ctx, "logs", p.metrics.LogWriteRetries, p.metrics.LogDeadLetters, func(c context.Context) error {
			return p.writer.WriteLogs(c, logs)
		})
		p.recordBatchMeta(ctx, "logs", logs, totalBytes, writeErr)
		if writeErr == nil && p.discovery != nil {
			p.discoverFromLogs(ctx, logs)
		}
		p.logger.Debug("Processed logs",
			zap.Int("count", len(logs)),
			zap.Duration("duration", time.Since(start)),
			zap.Error(writeErr))
	}
}

// recordBatchMeta groups items by agent_id and emits one BatchMeta
// row per agent for the batch. Bytes are attributed proportionally:
// agentSlice / totalItems × totalBytes. Approximate but acceptable
// because mixed-agent batches are uncommon (one collector typically
// produces one agent_id per ExportRequest).
//
// writeErr drives the status field: "ok" on success, "dropped" on
// terminal write failure (the items got dead-lettered).
func (p *Pool) recordBatchMeta(ctx context.Context, signal string, items interface{}, totalBytes int64, writeErr error) {
	counts := countByAgent(items)
	if len(counts) == 0 {
		return
	}
	totalItems := int64(0)
	for _, c := range counts {
		totalItems += c
	}
	status := "ok"
	if writeErr != nil {
		status = "dropped"
	}
	now := time.Now().UTC()
	for agentID, count := range counts {
		bytes := int64(0)
		if totalItems > 0 {
			bytes = totalBytes * count / totalItems
		}
		dropped := int64(0)
		if writeErr != nil {
			dropped = count
		}
		_ = p.writer.WriteBatchMeta(ctx, telemetrytypes.BatchMeta{
			Timestamp:    now,
			AgentID:      agentID,
			SignalType:   signal,
			ItemCount:    count,
			DroppedCount: dropped,
			PayloadBytes: bytes,
			Status:       status,
		})
	}
}

// recordMetricsBatchMeta is the metrics variant: a single batch
// can contain sums, gauges, AND histograms, so we union the agent
// counts across all three before emitting per-agent rows.
func (p *Pool) recordMetricsBatchMeta(
	ctx context.Context,
	sums []otlp.MetricSumData,
	gauges []otlp.MetricGaugeData,
	histograms []otlp.MetricHistogramData,
	totalBytes int64,
	writeErr error,
) {
	counts := map[string]int64{}
	for _, m := range sums {
		counts[m.AgentID]++
	}
	for _, m := range gauges {
		counts[m.AgentID]++
	}
	for _, m := range histograms {
		counts[m.AgentID]++
	}
	if len(counts) == 0 {
		return
	}
	totalItems := int64(0)
	for _, c := range counts {
		totalItems += c
	}
	status := "ok"
	if writeErr != nil {
		status = "dropped"
	}
	now := time.Now().UTC()
	for agentID, count := range counts {
		bytes := int64(0)
		if totalItems > 0 {
			bytes = totalBytes * count / totalItems
		}
		dropped := int64(0)
		if writeErr != nil {
			dropped = count
		}
		_ = p.writer.WriteBatchMeta(ctx, telemetrytypes.BatchMeta{
			Timestamp:    now,
			AgentID:      agentID,
			SignalType:   "metrics",
			ItemCount:    count,
			DroppedCount: dropped,
			PayloadBytes: bytes,
			Status:       status,
		})
	}
}

// discoverFrom{Traces,Metrics,Logs} extract one observation per
// unique agent_id seen in the batch and feed it to the v0.36
// discovery service. The service's own LRU dedup means we don't
// need to deduplicate per-batch — the worst case is a Map allocation
// + a noop lookup, both cheap. Hostname is taken from the
// host.name resource attribute, falling back to service.name.

func (p *Pool) discoverFromTraces(ctx context.Context, traces []otlp.TraceData) {
	seen := map[string]struct{}{}
	for i := range traces {
		t := &traces[i]
		if t.AgentID == "" {
			continue
		}
		if _, dup := seen[t.AgentID]; dup {
			continue
		}
		seen[t.AgentID] = struct{}{}
		p.discovery.RegisterIfUnknown(ctx, obsFromResource(t.AgentID, t.ResourceAttributes))
	}
}

func (p *Pool) discoverFromMetrics(
	ctx context.Context,
	sums []otlp.MetricSumData,
	gauges []otlp.MetricGaugeData,
	histograms []otlp.MetricHistogramData,
) {
	seen := map[string]struct{}{}
	for i := range sums {
		m := &sums[i]
		if m.AgentID == "" {
			continue
		}
		if _, dup := seen[m.AgentID]; dup {
			continue
		}
		seen[m.AgentID] = struct{}{}
		p.discovery.RegisterIfUnknown(ctx, obsFromResource(m.AgentID, m.ResourceAttributes))
	}
	for i := range gauges {
		m := &gauges[i]
		if m.AgentID == "" {
			continue
		}
		if _, dup := seen[m.AgentID]; dup {
			continue
		}
		seen[m.AgentID] = struct{}{}
		p.discovery.RegisterIfUnknown(ctx, obsFromResource(m.AgentID, m.ResourceAttributes))
	}
	for i := range histograms {
		m := &histograms[i]
		if m.AgentID == "" {
			continue
		}
		if _, dup := seen[m.AgentID]; dup {
			continue
		}
		seen[m.AgentID] = struct{}{}
		p.discovery.RegisterIfUnknown(ctx, obsFromResource(m.AgentID, m.ResourceAttributes))
	}
}

func (p *Pool) discoverFromLogs(ctx context.Context, logs []otlp.LogData) {
	seen := map[string]struct{}{}
	for i := range logs {
		l := &logs[i]
		if l.AgentID == "" {
			continue
		}
		if _, dup := seen[l.AgentID]; dup {
			continue
		}
		seen[l.AgentID] = struct{}{}
		p.discovery.RegisterIfUnknown(ctx, obsFromResource(l.AgentID, l.ResourceAttributes))
	}
}

// obsFromResource builds a discovery.Observation from the standard
// OTel resource attributes. service.name + host.name are the two
// the collector emits by default; everything else is gravy.
func obsFromResource(agentID string, resource map[string]string) discovery.Observation {
	return discovery.Observation{
		AgentID:     agentID,
		Hostname:    resource["host.name"],
		ServiceName: resource["service.name"],
		Version:     resource["service.version"],
		OS:          resource["os.type"],
	}
}

// countByAgent walks the slice via type switch and tallies AgentID
// occurrences. Cheaper than reflection; the four switch arms cover
// every signal type we currently support.
func countByAgent(items interface{}) map[string]int64 {
	counts := map[string]int64{}
	switch v := items.(type) {
	case []otlp.TraceData:
		for _, t := range v {
			counts[t.AgentID]++
		}
	case []otlp.LogData:
		for _, l := range v {
			counts[l.AgentID]++
		}
	}
	return counts
}
