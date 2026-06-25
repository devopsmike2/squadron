// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

// Cost-correlation opt-in slice 6 chunk 6 (v0.89.188, #830 Stream 227)
// — the operator-facing switch is OFF by default and bounded.

// --- default OFF: an omitted block means disabled, $0 spend ------

func TestCostCorrelation_DefaultOff(t *testing.T) {
	// A config with no cost_correlation block at all.
	var cfg Config
	err := yaml.Unmarshal([]byte("server:\n  http_port: 8080\n"), &cfg)
	assert.NoError(t, err)
	assert.False(t, cfg.CostCorrelation.Enabled, "cost correlation is OFF unless explicitly enabled")
}

// --- explicit enable + budget -----------------------------------

func TestCostCorrelation_ExplicitEnable(t *testing.T) {
	yamlSrc := "cost_correlation:\n  enabled: true\n  monthly_budget_usd: 5.0\n"
	var cfg Config
	assert.NoError(t, yaml.Unmarshal([]byte(yamlSrc), &cfg))
	assert.True(t, cfg.CostCorrelation.Enabled)
	assert.Equal(t, 5.0, cfg.CostCorrelation.EffectiveMonthlyBudgetUSD())
}

// --- budget defaults to $1 when unset / non-positive ------------

func TestCostCorrelation_BudgetDefaultsToOneDollar(t *testing.T) {
	assert.Equal(t, 1.0, CostCorrelationConfig{Enabled: true}.EffectiveMonthlyBudgetUSD(), "unset budget → $1 default")
	assert.Equal(t, 1.0, CostCorrelationConfig{Enabled: true, MonthlyBudgetUSD: 0}.EffectiveMonthlyBudgetUSD())
	assert.Equal(t, 1.0, CostCorrelationConfig{Enabled: true, MonthlyBudgetUSD: -2}.EffectiveMonthlyBudgetUSD(), "negative → safe $1 default, never unbounded")
}
