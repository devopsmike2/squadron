// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func validInput() AlertRuleInput {
	return AlertRuleInput{
		Name:              "high errors",
		Description:       "many errors over 5m",
		Query:             `logs{severity="ERROR"}`,
		ThresholdOperator: ThresholdGreater,
		ThresholdValue:    100,
		IntervalSeconds:   60,
		Severity:          AlertSeverityWarning,
		Enabled:           true,
		WebhookURL:        "https://example.com/hook",
	}
}

func TestAlertService_CreateGetListUpdateDelete(t *testing.T) {
	svc := NewAlertService(memory.NewStore(), zap.NewNop())
	ctx := context.Background()

	created, err := svc.CreateAlertRule(ctx, validInput())
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID, "id should be populated")
	assert.False(t, created.CreatedAt.IsZero(), "created_at should be populated")
	assert.Equal(t, created.CreatedAt, created.UpdatedAt, "created_at and updated_at should match on create")

	got, err := svc.GetAlertRule(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, created.Name, got.Name)
	assert.Equal(t, created.Query, got.Query)
	assert.Equal(t, created.ThresholdOperator, got.ThresholdOperator)

	rules, err := svc.ListAlertRules(ctx)
	require.NoError(t, err)
	assert.Len(t, rules, 1)

	// Update
	input := validInput()
	input.Name = "updated"
	input.ThresholdValue = 200
	updated, err := svc.UpdateAlertRule(ctx, created.ID, input)
	require.NoError(t, err)
	assert.Equal(t, "updated", updated.Name)
	assert.Equal(t, float64(200), updated.ThresholdValue)
	assert.True(t, updated.UpdatedAt.After(updated.CreatedAt) || updated.UpdatedAt.Equal(updated.CreatedAt),
		"updated_at should be >= created_at")
	assert.Equal(t, created.CreatedAt, updated.CreatedAt, "created_at must not change on update")

	// Delete
	require.NoError(t, svc.DeleteAlertRule(ctx, created.ID))
	got, err = svc.GetAlertRule(ctx, created.ID)
	require.NoError(t, err)
	assert.Nil(t, got, "rule should be gone after delete")
}

func TestAlertService_ValidationRejectsBadInput(t *testing.T) {
	svc := NewAlertService(memory.NewStore(), zap.NewNop())
	ctx := context.Background()

	cases := []struct {
		name    string
		mutate  func(*AlertRuleInput)
		errSub  string
	}{
		{"empty name", func(i *AlertRuleInput) { i.Name = "" }, "name is required"},
		{"empty query", func(i *AlertRuleInput) { i.Query = "" }, "query is required"},
		{"bad operator", func(i *AlertRuleInput) { i.ThresholdOperator = "approx" }, "invalid threshold_operator"},
		{"zero interval", func(i *AlertRuleInput) { i.IntervalSeconds = 0 }, "interval_seconds must be positive"},
		{"negative interval", func(i *AlertRuleInput) { i.IntervalSeconds = -5 }, "interval_seconds must be positive"},
		{"bad severity", func(i *AlertRuleInput) { i.Severity = "panic" }, "invalid severity"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := validInput()
			tc.mutate(&input)
			_, err := svc.CreateAlertRule(ctx, input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSub)
		})
	}
}

func TestAlertService_UpdateNonexistentReturnsError(t *testing.T) {
	svc := NewAlertService(memory.NewStore(), zap.NewNop())
	_, err := svc.UpdateAlertRule(context.Background(), "nope", validInput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
