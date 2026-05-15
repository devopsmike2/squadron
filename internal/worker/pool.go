package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/otlp"
	"github.com/devopsmike2/squadron/internal/otlp/parser"
	"github.com/devopsmike2/squadron/internal/otlp/processor"
	"github.com/devopsmike2/squadron/internal/services"
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
}

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

// Submit submits a work item to the queue
func (p *Pool) Submit(item WorkItem) error {
	select {
	case p.queue <- item:
		return nil
	case <-time.After(p.submitTimeout):
		return fmt.Errorf("queue full, submit timeout")
	}
}

// QueueDepth returns the current queue depth
func (p *Pool) QueueDepth() int {
	return len(p.queue)
}

// worker is the main worker goroutine
func (p *Pool) worker(id int) {
	defer p.wg.Done()

	p.logger.Info("Worker started", zap.Int("worker_id", id))

	for {
		select {
		case item := <-p.queue:
			p.processItem(item)
		case <-p.shutdown:
			// Drain remaining items
			p.logger.Info("Draining remaining queue items", zap.Int("count", len(p.queue)))
			for {
				select {
				case item := <-p.queue:
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
func (p *Pool) processItem(item WorkItem) {
	start := time.Now()
	ctx := context.Background()

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
		p.logger.Debug("Processed logs",
			zap.Int("count", len(logs)),
			zap.Duration("duration", time.Since(start)),
			zap.Error(writeErr))
	}
}
