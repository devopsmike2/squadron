// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// TestApplyLoggingEnv pins the env-override precedence: SQUADRON_LOG_* wins over
// the unprefixed LOG_* the shipped docker-compose sets, and an unset/empty env
// leaves the yaml (or default) value untouched.
func TestApplyLoggingEnv(t *testing.T) {
	t.Run("unset env leaves yaml value", func(t *testing.T) {
		t.Setenv("SQUADRON_LOG_LEVEL", "")
		t.Setenv("LOG_LEVEL", "")
		t.Setenv("SQUADRON_LOG_FORMAT", "")
		t.Setenv("LOG_FORMAT", "")
		c := &LoggingConfig{Level: "info", Format: "json"}
		applyLoggingEnv(c)
		if c.Level != "info" || c.Format != "json" {
			t.Fatalf("expected yaml values preserved, got %+v", c)
		}
	})

	t.Run("unprefixed LOG_* is honored (compose parity)", func(t *testing.T) {
		t.Setenv("SQUADRON_LOG_LEVEL", "")
		t.Setenv("LOG_LEVEL", "debug")
		t.Setenv("SQUADRON_LOG_FORMAT", "")
		t.Setenv("LOG_FORMAT", "console")
		c := &LoggingConfig{Level: "info", Format: "json"}
		applyLoggingEnv(c)
		if c.Level != "debug" || c.Format != "console" {
			t.Fatalf("LOG_* not honored: got %+v", c)
		}
	})

	t.Run("SQUADRON_LOG_* wins over LOG_*", func(t *testing.T) {
		t.Setenv("SQUADRON_LOG_LEVEL", "warn")
		t.Setenv("LOG_LEVEL", "debug")
		t.Setenv("SQUADRON_LOG_FORMAT", "json")
		t.Setenv("LOG_FORMAT", "console")
		c := &LoggingConfig{Level: "info", Format: "console"}
		applyLoggingEnv(c)
		if c.Level != "warn" || c.Format != "json" {
			t.Fatalf("SQUADRON_LOG_* did not win: got %+v", c)
		}
	})
}
