// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

// audit_events retention is OPT-IN and OFF by default — the audit log is the
// append-only compliance/evidence store, so it grows unbounded unless an
// operator explicitly configures a window. These pin that contract.

// Default: an omitted audit_retention block means disabled / unbounded.
func TestAuditRetention_DefaultOff(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte("server:\n  http_port: 8080\n"), &cfg)
	assert.NoError(t, err)
	assert.False(t, cfg.AuditRetention.Enabled, "audit retention is OFF unless explicitly enabled")
	_, active := cfg.AuditRetention.RetentionWindow()
	assert.False(t, active, "no audit pruning without an explicit window")
}

// Explicit enable + positive window → active with the configured duration.
func TestAuditRetention_ExplicitEnable(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte("audit_retention:\n  enabled: true\n  retention_days: 365\n"), &cfg)
	assert.NoError(t, err)
	window, active := cfg.AuditRetention.RetentionWindow()
	assert.True(t, active, "enabled + positive retention_days must activate pruning")
	assert.Equal(t, 365*24*time.Hour, window)
}

// Misconfiguration guard: enabled but non-positive retention_days must NOT
// activate — otherwise a zero/negative window would delete the entire log.
func TestAuditRetention_EnabledZeroDaysIsInactive(t *testing.T) {
	for _, days := range []int{0, -30} {
		cfg := AuditRetentionConfig{Enabled: true, RetentionDays: days}
		_, active := cfg.RetentionWindow()
		assert.Falsef(t, active, "enabled with retention_days=%d must be inactive (never wipe the whole log)", days)
	}
}

// Disabled with a window set is still inactive — the switch, not the number,
// is what turns pruning on.
func TestAuditRetention_DisabledWithWindowIsInactive(t *testing.T) {
	cfg := AuditRetentionConfig{Enabled: false, RetentionDays: 365}
	_, active := cfg.RetentionWindow()
	assert.False(t, active, "retention_days without enabled:true must not prune")
}
