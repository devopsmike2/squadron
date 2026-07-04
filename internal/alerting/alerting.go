// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package alerting evaluates Squadron QL alert rules on an interval and
// dispatches notifications when the threshold is crossed.
//
// Evaluation model: every rule has a Squadron QL query that returns a set of
// telemetry rows. The scalar value used for thresholding is the *count of
// returned rows*. Operators write filtering queries ("logs where severity =
// 'ERROR' over the last 5m"), and the rule fires when the count satisfies the
// threshold operator vs. the threshold value. This is conceptually equivalent
// to count_over_time in PromQL/LogQL.
//
// Firing state is held in memory only — restarting Squadron clears the
// firing-state map. That means a long-running drift will re-fire on restart
// (acceptable: the operator will get a fresh notification confirming the
// incident is still ongoing).
package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/alerts"
	"github.com/devopsmike2/squadron/internal/events"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/query"
	"github.com/devopsmike2/squadron/internal/services"
)

// tickInterval is how often the evaluator wakes up to scan rules. Rules with
// interval_seconds shorter than this won't fire faster than tickInterval; rules
// with longer intervals fire on the next tick after their per-rule cadence
// elapses.
const tickInterval = 5 * time.Second

// Evaluator periodically runs alert rules against telemetry storage and
// dispatches notifications when thresholds are crossed.
type Evaluator struct {
	alertService     services.AlertService
	telemetryService services.TelemetryQueryService
	auditService     services.AuditService // optional
	executor         *query.Executor
	httpClient       *http.Client
	logger           *zap.Logger
	metrics          *metrics.AlertMetrics
	broker           *events.Broker // optional; nil disables SSE event publishing
	tracer           *alerts.Tracer // optional; nil disables OTel evaluation spans

	mu       sync.Mutex
	firing   map[string]bool      // rule id -> currently firing?
	lastEval map[string]time.Time // rule id -> last evaluation time

	shutdown chan struct{}
	wg       sync.WaitGroup
}

// NewEvaluator wires up an evaluator. alertMetrics, broker, and audit are
// all optional — pass nil to disable that side effect.
func NewEvaluator(
	alertService services.AlertService,
	telemetryService services.TelemetryQueryService,
	alertMetrics *metrics.AlertMetrics,
	broker *events.Broker,
	audit services.AuditService,
	logger *zap.Logger,
) *Evaluator {
	return NewEvaluatorWithTracer(alertService, telemetryService, alertMetrics, broker, audit, nil, logger)
}

// NewEvaluatorWithTracer is the production constructor used when
// telemetry.enabled is true. Identical to NewEvaluator except for
// the tracer wiring. Separate constructor avoids a nil tracer
// parameter in every existing test caller.
func NewEvaluatorWithTracer(
	alertService services.AlertService,
	telemetryService services.TelemetryQueryService,
	alertMetrics *metrics.AlertMetrics,
	broker *events.Broker,
	audit services.AuditService,
	tracer *alerts.Tracer,
	logger *zap.Logger,
) *Evaluator {
	if alertMetrics == nil {
		alertMetrics = metrics.NewAlertMetrics(metrics.NullFactory)
	}
	return &Evaluator{
		alertService:     alertService,
		telemetryService: telemetryService,
		auditService:     audit,
		executor:         query.NewExecutor(telemetryService, logger),
		httpClient:       &http.Client{Timeout: 10 * time.Second},
		logger:           logger,
		metrics:          alertMetrics,
		broker:           broker,
		tracer:           tracer,
		firing:           make(map[string]bool),
		lastEval:         make(map[string]time.Time),
		shutdown:         make(chan struct{}),
	}
}

// Start begins the evaluation loop in a background goroutine.
func (e *Evaluator) Start() {
	e.wg.Add(1)
	go e.loop()
	e.logger.Info("alert evaluator started", zap.Duration("tick_interval", tickInterval))
}

// Stop signals shutdown and waits for the evaluation goroutine to finish.
func (e *Evaluator) Stop(timeout time.Duration) error {
	close(e.shutdown)
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		e.logger.Info("alert evaluator stopped")
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("alert evaluator shutdown timeout exceeded")
	}
}

func (e *Evaluator) loop() {
	defer e.wg.Done()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.tick()
		case <-e.shutdown:
			return
		}
	}
}

// tick lists current rules, evaluates the ones whose interval has elapsed.
// Rule listing errors are logged and the loop continues on the next tick.
func (e *Evaluator) tick() {
	// ADR 0011: the evaluator fires EVERY tenant's alert rules each tick, so
	// it runs on a system (all-tenant) context. Inert in OSS; the enterprise
	// scoped store reads a system context as "apply no tenant predicate" so no
	// tenant's alerts silently stop firing.
	ctx, cancel := context.WithTimeout(identity.WithSystemContext(context.Background()), 30*time.Second)
	defer cancel()

	rules, err := e.alertService.ListAlertRules(ctx)
	if err != nil {
		e.logger.Error("failed to list alert rules", zap.Error(err))
		return
	}

	now := time.Now()
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		e.mu.Lock()
		last := e.lastEval[rule.ID]
		e.mu.Unlock()
		if !last.IsZero() && now.Sub(last) < time.Duration(rule.IntervalSeconds)*time.Second {
			continue
		}
		e.evaluateRule(ctx, rule, now)
		e.mu.Lock()
		e.lastEval[rule.ID] = now
		e.mu.Unlock()
	}

	// Refresh the currently-firing gauge.
	e.mu.Lock()
	var firing int64
	for _, isFiring := range e.firing {
		if isFiring {
			firing++
		}
	}
	e.mu.Unlock()
	e.metrics.CurrentlyFiring.Update(firing)
}

// evaluateRule runs a single rule's query, applies the threshold, and
// dispatches firing/resolution notifications if the state changed.
func (e *Evaluator) evaluateRule(ctx context.Context, rule *services.AlertRule, now time.Time) {
	e.metrics.EvaluationsTotal.Inc(1)

	// Wrap the whole evaluation cycle in a span. A nil tracer
	// returns a nil-safe Evaluation so the rest of the function
	// chains method calls without conditional checks.
	eval := e.tracer.BeginEvaluation(ctx, alerts.Rule{
		ID:                rule.ID,
		Name:              rule.Name,
		Query:             rule.Query,
		ThresholdOperator: string(rule.ThresholdOperator),
		ThresholdValue:    rule.ThresholdValue,
		Severity:          string(rule.Severity),
	})
	defer eval.End()

	value, err := e.queryScalar(ctx, rule.Query)
	if err != nil {
		e.metrics.EvaluationErrors.Inc(1)
		eval.RecordQueryError(err)
		e.logger.Warn("alert query failed",
			zap.String("rule_id", rule.ID),
			zap.String("name", rule.Name),
			zap.Error(err))
		return
	}

	shouldFire := compareThreshold(value, rule.ThresholdOperator, rule.ThresholdValue)
	eval.SetObservedValue(value, shouldFire)

	e.mu.Lock()
	wasFiring := e.firing[rule.ID]
	e.firing[rule.ID] = shouldFire
	e.mu.Unlock()

	switch {
	case shouldFire && !wasFiring:
		e.recordFiring(rule.Severity)
		e.dispatch(ctx, rule, value, "firing", now, eval)
		e.publishEvent(events.AlertFired, rule, value, now)
		e.recordAudit(ctx, rule, value, services.AuditEventAlertFired, "fired")
	case !shouldFire && wasFiring:
		e.recordResolved(rule.Severity)
		e.dispatch(ctx, rule, value, "resolved", now, eval)
		e.publishEvent(events.AlertResolved, rule, value, now)
		e.recordAudit(ctx, rule, value, services.AuditEventAlertResolved, "resolved")
	}
}

// recordAudit logs a durable audit entry for a state transition. Pulled
// out of evaluateRule so the call sites stay one-line.
func (e *Evaluator) recordAudit(ctx context.Context, rule *services.AlertRule, value float64, eventType, action string) {
	if e.auditService == nil {
		return
	}
	_ = e.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  eventType,
		TargetType: services.AuditTargetRule,
		TargetID:   rule.ID,
		Action:     action,
		Payload: map[string]any{
			"rule_name": rule.Name,
			"severity":  string(rule.Severity),
			"value":     value,
		},
	})
}

// publishEvent emits a domain event to the broker if one is wired in.
// Separated so the firing/resolved branches stay short and obvious.
func (e *Evaluator) publishEvent(t events.Type, rule *services.AlertRule, value float64, at time.Time) {
	if e.broker == nil {
		return
	}
	e.broker.Publish(events.Event{
		Type: t,
		At:   at,
		Data: map[string]any{
			"rule_id":   rule.ID,
			"rule_name": rule.Name,
			"severity":  string(rule.Severity),
			"value":     value,
		},
	})
}

// queryScalar parses and executes a Squadron QL query, returning the row
// count as the scalar value. Row count is the alert evaluation scalar — see
// the package docs.
func (e *Evaluator) queryScalar(ctx context.Context, raw string) (float64, error) {
	parsed, err := query.NewParser(raw).Parse()
	if err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}

	now := time.Now()
	start := now.Add(-5 * time.Minute)
	execCtx := &query.ExecutionContext{
		// Use a 5-minute default window. Operators can write queries with
		// explicit time windows; this is only the fallback.
		StartTime: &start,
		EndTime:   &now,
		Limit:     10000,
	}
	results, _, err := e.executor.Execute(ctx, parsed, execCtx)
	if err != nil {
		return 0, fmt.Errorf("execute: %w", err)
	}
	return float64(len(results)), nil
}

func compareThreshold(value float64, op services.ThresholdOperator, threshold float64) bool {
	switch op {
	case services.ThresholdGreater:
		return value > threshold
	case services.ThresholdGreaterOrEqual:
		return value >= threshold
	case services.ThresholdLess:
		return value < threshold
	case services.ThresholdLessOrEqual:
		return value <= threshold
	case services.ThresholdEqual:
		return value == threshold
	case services.ThresholdNotEqual:
		return value != threshold
	default:
		return false
	}
}

func (e *Evaluator) recordFiring(sev services.AlertSeverity) {
	switch sev {
	case services.AlertSeverityCritical:
		e.metrics.FiringsCritical.Inc(1)
	case services.AlertSeverityWarning:
		e.metrics.FiringsWarning.Inc(1)
	default:
		e.metrics.FiringsInfo.Inc(1)
	}
}

func (e *Evaluator) recordResolved(sev services.AlertSeverity) {
	switch sev {
	case services.AlertSeverityCritical:
		e.metrics.ResolvedCritical.Inc(1)
	case services.AlertSeverityWarning:
		e.metrics.ResolvedWarning.Inc(1)
	default:
		e.metrics.ResolvedInfo.Inc(1)
	}
}

// NotificationPayload is the JSON shape we POST to webhook channels and write
// to logs on state transitions. Stable so users can build automations against
// it.
type NotificationPayload struct {
	RuleID            string                     `json:"rule_id"`
	RuleName          string                     `json:"rule_name"`
	State             string                     `json:"state"` // "firing" or "resolved"
	Severity          services.AlertSeverity     `json:"severity"`
	Description       string                     `json:"description,omitempty"`
	Query             string                     `json:"query"`
	Value             float64                    `json:"value"`
	ThresholdOperator services.ThresholdOperator `json:"threshold_operator"`
	ThresholdValue    float64                    `json:"threshold_value"`
	At                time.Time                  `json:"at"`
}

// dispatch sends an alert notification through every channel configured on the
// rule. The log channel always runs; the webhook channel runs only if a URL
// is configured.
func (e *Evaluator) dispatch(ctx context.Context, rule *services.AlertRule, value float64, state string, at time.Time, eval *alerts.Evaluation) {
	payload := NotificationPayload{
		RuleID:            rule.ID,
		RuleName:          rule.Name,
		State:             state,
		Severity:          rule.Severity,
		Description:       rule.Description,
		Query:             rule.Query,
		Value:             value,
		ThresholdOperator: rule.ThresholdOperator,
		ThresholdValue:    rule.ThresholdValue,
		At:                at,
	}

	// Log channel — always on. Severity gates the log level so config drift
	// alerts vs. a noisy 'info' query don't look the same in journalctl.
	e.dispatchLog(payload)

	// Webhook channel — opt-in. The tracer records a
	// dispatched_to_webhook event on the evaluation span so an
	// operator can see in the trace which evaluations actually
	// triggered an external notification (vs. just changing state).
	if rule.WebhookURL != "" {
		e.dispatchWebhook(ctx, rule.WebhookURL, payload)
		eval.RecordWebhookDispatched(rule.WebhookURL, state)
	}
}

func (e *Evaluator) dispatchLog(p NotificationPayload) {
	e.metrics.DispatchLog.Inc(1)
	fields := []zap.Field{
		zap.String("rule_id", p.RuleID),
		zap.String("rule_name", p.RuleName),
		zap.String("state", p.State),
		zap.String("severity", string(p.Severity)),
		zap.Float64("value", p.Value),
		zap.String("threshold", fmt.Sprintf("%s %g", p.ThresholdOperator, p.ThresholdValue)),
	}
	switch {
	case p.State == "resolved":
		e.logger.Info("alert resolved", fields...)
	case p.Severity == services.AlertSeverityCritical:
		e.logger.Error("alert firing", fields...)
	case p.Severity == services.AlertSeverityWarning:
		e.logger.Warn("alert firing", fields...)
	default:
		e.logger.Info("alert firing", fields...)
	}
}

func (e *Evaluator) dispatchWebhook(ctx context.Context, url string, p NotificationPayload) {
	body, err := json.Marshal(p)
	if err != nil {
		e.metrics.DispatchWebhookErrors.Inc(1)
		e.logger.Warn("failed to marshal webhook payload", zap.Error(err))
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		e.metrics.DispatchWebhookErrors.Inc(1)
		e.logger.Warn("failed to build webhook request", zap.Error(err), zap.String("url", url))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Squadron/alerts")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		e.metrics.DispatchWebhookErrors.Inc(1)
		e.logger.Warn("webhook delivery failed", zap.Error(err), zap.String("url", url))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		e.metrics.DispatchWebhookErrors.Inc(1)
		e.logger.Warn("webhook returned non-2xx",
			zap.String("url", url),
			zap.Int("status", resp.StatusCode))
		return
	}
	e.metrics.DispatchWebhook.Inc(1)
}
