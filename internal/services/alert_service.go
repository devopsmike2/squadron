// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"time"
)

// AlertService defines operations for managing alert rules.
type AlertService interface {
	ListAlertRules(ctx context.Context) ([]*AlertRule, error)
	GetAlertRule(ctx context.Context, id string) (*AlertRule, error)
	CreateAlertRule(ctx context.Context, input AlertRuleInput) (*AlertRule, error)
	UpdateAlertRule(ctx context.Context, id string, input AlertRuleInput) (*AlertRule, error)
	DeleteAlertRule(ctx context.Context, id string) error
}

// AlertSeverity is the severity attached to a firing alert.
type AlertSeverity string

const (
	AlertSeverityInfo     AlertSeverity = "info"
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
)

// ThresholdOperator is the comparison applied between the query result and
// the configured threshold value.
type ThresholdOperator string

const (
	ThresholdGreater        ThresholdOperator = ">"
	ThresholdGreaterOrEqual ThresholdOperator = ">="
	ThresholdLess           ThresholdOperator = "<"
	ThresholdLessOrEqual    ThresholdOperator = "<="
	ThresholdEqual          ThresholdOperator = "=="
	ThresholdNotEqual       ThresholdOperator = "!="
)

// AlertRule is a periodically-evaluated Squadron QL query plus the threshold
// that determines when it fires.
type AlertRule struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	Query             string            `json:"query"`
	ThresholdOperator ThresholdOperator `json:"threshold_operator"`
	ThresholdValue    float64           `json:"threshold_value"`
	IntervalSeconds   int               `json:"interval_seconds"`
	Severity          AlertSeverity     `json:"severity"`
	Enabled           bool              `json:"enabled"`
	WebhookURL        string            `json:"webhook_url,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// AlertRuleInput captures user-editable fields for an alert rule.
type AlertRuleInput struct {
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	Query             string            `json:"query"`
	ThresholdOperator ThresholdOperator `json:"threshold_operator"`
	ThresholdValue    float64           `json:"threshold_value"`
	IntervalSeconds   int               `json:"interval_seconds"`
	Severity          AlertSeverity     `json:"severity"`
	Enabled           bool              `json:"enabled"`
	WebhookURL        string            `json:"webhook_url"`
}
