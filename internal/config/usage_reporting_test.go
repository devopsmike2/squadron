// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
	"time"
)

func TestUsageReportingConfig_Target(t *testing.T) {
	tests := []struct {
		name     string
		cfg      UsageReportingConfig
		wantOK   bool
		wantURL  string
		wantIval time.Duration
	}{
		{"default (off)", UsageReportingConfig{}, false, "", 0},
		{"enabled but no endpoint → off", UsageReportingConfig{Enabled: true}, false, "", 0},
		{"endpoint but not enabled → off", UsageReportingConfig{Endpoint: "https://x"}, false, "", 0},
		{"enabled + endpoint → default 24h", UsageReportingConfig{Enabled: true, Endpoint: "https://x"}, true, "https://x", 24 * time.Hour},
		{"custom interval", UsageReportingConfig{Enabled: true, Endpoint: "https://x", IntervalHours: 6}, true, "https://x", 6 * time.Hour},
		{"whitespace endpoint trimmed", UsageReportingConfig{Enabled: true, Endpoint: "  https://x  "}, true, "https://x", 24 * time.Hour},
		{"non-positive interval → 24h", UsageReportingConfig{Enabled: true, Endpoint: "https://x", IntervalHours: -1}, true, "https://x", 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url, ival, ok := tc.cfg.Target()
			if ok != tc.wantOK || url != tc.wantURL || ival != tc.wantIval {
				t.Fatalf("Target() = (%q, %v, %v), want (%q, %v, %v)", url, ival, ok, tc.wantURL, tc.wantIval, tc.wantOK)
			}
		})
	}
}

func TestApplyUsageEnv(t *testing.T) {
	t.Setenv("SQUADRON_USAGE_ENABLED", "true")
	t.Setenv("SQUADRON_USAGE_ENDPOINT", "https://usage.example/report")
	c := UsageReportingConfig{}
	applyUsageEnv(&c)
	if !c.Enabled || c.Endpoint != "https://usage.example/report" {
		t.Fatalf("env not applied: %+v", c)
	}

	// Unset env leaves yaml values intact.
	t.Setenv("SQUADRON_USAGE_ENABLED", "")
	t.Setenv("SQUADRON_USAGE_ENDPOINT", "")
	pre := UsageReportingConfig{Enabled: true, Endpoint: "https://from-yaml"}
	applyUsageEnv(&pre)
	if !pre.Enabled || pre.Endpoint != "https://from-yaml" {
		t.Fatalf("empty env should not clobber yaml: %+v", pre)
	}
}
