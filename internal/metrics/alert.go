// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

// AlertMetrics tracks the alerting subsystem. These give operators a way to
// alert on the alerter itself — e.g. evaluation errors climbing usually
// means a rule query is broken and the alerter is silently degraded.
type AlertMetrics struct {
	// Evaluation outcomes.
	EvaluationsTotal Counter `metric:"alert_evaluations_total"        tags:"component=alerting" help:"Total alert rule evaluations"`
	EvaluationErrors Counter `metric:"alert_evaluation_errors_total"  tags:"component=alerting" help:"Rule evaluations that failed (parse error, query failure, etc.)"`

	// Per-severity firing transition counters. Distinct metric names rather
	// than label-sliced because each field registers as its own Counter under
	// the metrics framework here.
	FiringsInfo     Counter `metric:"alert_firings_info_total"     tags:"component=alerting,severity=info"     help:"Info-severity alert transitions into firing state"`
	FiringsWarning  Counter `metric:"alert_firings_warning_total"  tags:"component=alerting,severity=warning"  help:"Warning-severity alert transitions into firing state"`
	FiringsCritical Counter `metric:"alert_firings_critical_total" tags:"component=alerting,severity=critical" help:"Critical-severity alert transitions into firing state"`

	ResolvedInfo     Counter `metric:"alert_resolutions_info_total"     tags:"component=alerting,severity=info"     help:"Info-severity alert transitions back to resolved"`
	ResolvedWarning  Counter `metric:"alert_resolutions_warning_total"  tags:"component=alerting,severity=warning"  help:"Warning-severity alert transitions back to resolved"`
	ResolvedCritical Counter `metric:"alert_resolutions_critical_total" tags:"component=alerting,severity=critical" help:"Critical-severity alert transitions back to resolved"`

	// Currently-firing gauge — alert-on-alerter view ("are we in an incident
	// right now?").
	CurrentlyFiring Gauge `metric:"alerts_firing" tags:"component=alerting" help:"Number of alert rules currently in firing state"`

	// Dispatch counters — per channel type.
	DispatchLog           Counter `metric:"alert_dispatch_log_total"           tags:"component=alerting,channel=log"     help:"Alert notifications written to the log channel"`
	DispatchWebhook       Counter `metric:"alert_dispatch_webhook_total"       tags:"component=alerting,channel=webhook" help:"Alert notifications sent to a webhook"`
	DispatchWebhookErrors Counter `metric:"alert_dispatch_webhook_errors_total" tags:"component=alerting,channel=webhook" help:"Webhook dispatch errors"`
}

// NewAlertMetrics creates and initializes alerting metrics.
func NewAlertMetrics(factory Factory) *AlertMetrics {
	m := &AlertMetrics{}
	MustInit(m, factory, nil)
	return m
}
