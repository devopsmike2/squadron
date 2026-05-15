// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// AlertServiceImpl implements AlertService over an ApplicationStore.
type AlertServiceImpl struct {
	appStore applicationstore.ApplicationStore
	logger   *zap.Logger
}

// NewAlertService creates a new AlertService.
func NewAlertService(appStore applicationstore.ApplicationStore, logger *zap.Logger) AlertService {
	return &AlertServiceImpl{appStore: appStore, logger: logger}
}

func (s *AlertServiceImpl) ListAlertRules(ctx context.Context) ([]*AlertRule, error) {
	stored, err := s.appStore.ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*AlertRule, len(stored))
	for i, r := range stored {
		out[i] = toServiceAlertRule(r)
	}
	return out, nil
}

func (s *AlertServiceImpl) GetAlertRule(ctx context.Context, id string) (*AlertRule, error) {
	stored, err := s.appStore.GetAlertRule(ctx, id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, nil
	}
	return toServiceAlertRule(stored), nil
}

func (s *AlertServiceImpl) CreateAlertRule(ctx context.Context, input AlertRuleInput) (*AlertRule, error) {
	if err := validateAlertInput(input); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	rule := &AlertRule{
		ID:                uuid.New().String(),
		Name:              input.Name,
		Description:       input.Description,
		Query:             input.Query,
		ThresholdOperator: input.ThresholdOperator,
		ThresholdValue:    input.ThresholdValue,
		IntervalSeconds:   input.IntervalSeconds,
		Severity:          input.Severity,
		Enabled:           input.Enabled,
		WebhookURL:        input.WebhookURL,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.appStore.CreateAlertRule(ctx, toStorageAlertRule(rule)); err != nil {
		return nil, fmt.Errorf("failed to create alert rule: %w", err)
	}
	s.logger.Info("created alert rule",
		zap.String("rule_id", rule.ID),
		zap.String("name", rule.Name))
	return rule, nil
}

func (s *AlertServiceImpl) UpdateAlertRule(ctx context.Context, id string, input AlertRuleInput) (*AlertRule, error) {
	if err := validateAlertInput(input); err != nil {
		return nil, err
	}

	existing, err := s.appStore.GetAlertRule(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("alert rule not found: %s", id)
	}

	now := time.Now().UTC()
	updated := &AlertRule{
		ID:                id,
		Name:              input.Name,
		Description:       input.Description,
		Query:             input.Query,
		ThresholdOperator: input.ThresholdOperator,
		ThresholdValue:    input.ThresholdValue,
		IntervalSeconds:   input.IntervalSeconds,
		Severity:          input.Severity,
		Enabled:           input.Enabled,
		WebhookURL:        input.WebhookURL,
		CreatedAt:         existing.CreatedAt,
		UpdatedAt:         now,
	}
	if err := s.appStore.UpdateAlertRule(ctx, toStorageAlertRule(updated)); err != nil {
		return nil, fmt.Errorf("failed to update alert rule: %w", err)
	}
	s.logger.Info("updated alert rule", zap.String("rule_id", id))
	return updated, nil
}

func (s *AlertServiceImpl) DeleteAlertRule(ctx context.Context, id string) error {
	if err := s.appStore.DeleteAlertRule(ctx, id); err != nil {
		return err
	}
	s.logger.Info("deleted alert rule", zap.String("rule_id", id))
	return nil
}

// validateAlertInput rejects inputs that the evaluator can't safely run.
// Caller-supplied fields are validated; computed fields (id, timestamps)
// are not.
func validateAlertInput(input AlertRuleInput) error {
	if input.Name == "" {
		return fmt.Errorf("name is required")
	}
	if input.Query == "" {
		return fmt.Errorf("query is required")
	}
	switch input.ThresholdOperator {
	case ThresholdGreater, ThresholdGreaterOrEqual,
		ThresholdLess, ThresholdLessOrEqual,
		ThresholdEqual, ThresholdNotEqual:
	default:
		return fmt.Errorf("invalid threshold_operator %q", input.ThresholdOperator)
	}
	if input.IntervalSeconds <= 0 {
		return fmt.Errorf("interval_seconds must be positive")
	}
	switch input.Severity {
	case AlertSeverityInfo, AlertSeverityWarning, AlertSeverityCritical:
	default:
		return fmt.Errorf("invalid severity %q", input.Severity)
	}
	return nil
}

func toStorageAlertRule(r *AlertRule) *applicationstore.AlertRule {
	return &applicationstore.AlertRule{
		ID:                r.ID,
		Name:              r.Name,
		Description:       r.Description,
		Query:             r.Query,
		ThresholdOperator: applicationstore.ThresholdOperator(r.ThresholdOperator),
		ThresholdValue:    r.ThresholdValue,
		IntervalSeconds:   r.IntervalSeconds,
		Severity:          applicationstore.AlertSeverity(r.Severity),
		Enabled:           r.Enabled,
		WebhookURL:        r.WebhookURL,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}

func toServiceAlertRule(r *applicationstore.AlertRule) *AlertRule {
	return &AlertRule{
		ID:                r.ID,
		Name:              r.Name,
		Description:       r.Description,
		Query:             r.Query,
		ThresholdOperator: ThresholdOperator(r.ThresholdOperator),
		ThresholdValue:    r.ThresholdValue,
		IntervalSeconds:   r.IntervalSeconds,
		Severity:          AlertSeverity(r.Severity),
		Enabled:           r.Enabled,
		WebhookURL:        r.WebhookURL,
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
}
