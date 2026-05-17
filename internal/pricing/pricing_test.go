// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package pricing

import (
	"math"
	"testing"
)

func mustNewProjector(t *testing.T, cfg Config) *Projector {
	t.Helper()
	p, err := NewProjector(cfg)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	return p
}

// TestDisabled — every projection method returns zero when off.
func TestDisabled(t *testing.T) {
	p := mustNewProjector(t, Config{Enabled: false})
	if p.Enabled() {
		t.Errorf("Enabled() should be false")
	}
	if got := p.MonthlyForBytes(1024*1024*1024, SignalMetrics, "datadog"); got != 0 {
		t.Errorf("disabled MonthlyForBytes: got %v want 0", got)
	}
	if got := p.MonthlyForRecommendation(1024*1024*1024, 3600, SignalLogs); got != 0 {
		t.Errorf("disabled MonthlyForRecommendation: got %v want 0", got)
	}
	fp := p.ProjectFleet(FleetInput{
		SignalBytesPerHour: map[Signal]int64{SignalMetrics: 1024 * 1024 * 1024},
	})
	if fp.MonthlyUSD != 0 || fp.Enabled {
		t.Errorf("disabled fleet projection: got monthly=%v enabled=%v", fp.MonthlyUSD, fp.Enabled)
	}
}

// TestMonthlyForBytes_BasicMath sanity-checks the 1h-to-monthly
// extrapolation. 1 GB/hour at $0.30/GB → ~$219/month (730h × $0.30).
func TestMonthlyForBytes_BasicMath(t *testing.T) {
	p := mustNewProjector(t, Config{
		Enabled: true,
		Rules: []Rule{
			{Match: "", PricePerGB: 0.30}, // catch-all only
		},
	})
	got := p.MonthlyForBytes(1024*1024*1024, SignalMetrics, "anything")
	want := 730.0 * 0.30 // hours per month × $/GB
	if math.Abs(got-want) > 0.01 {
		t.Errorf("1 GB/hr @ $0.30/GB: got $%.4f, want $%.4f", got, want)
	}
}

// TestMonthlyForBytesWindow normalizes from a non-hourly window.
func TestMonthlyForBytesWindow(t *testing.T) {
	p := mustNewProjector(t, Config{
		Enabled: true,
		Rules:   []Rule{{Match: "", PricePerGB: 0.50}},
	})
	// 24 GB in 24 hours = 1 GB/hour → 730 × $0.50 = $365/month.
	got := p.MonthlyForBytesWindow(24*1024*1024*1024, 86400, SignalMetrics, "")
	want := 730.0 * 0.50
	if math.Abs(got-want) > 0.01 {
		t.Errorf("24 GB/24h @ $0.50/GB: got $%.4f, want $%.4f", got, want)
	}
}

// TestDestinationMatching — first match wins; catch-all auto-appended.
func TestDestinationMatching(t *testing.T) {
	p := mustNewProjector(t, Config{
		Enabled: true,
		Rules: []Rule{
			{Match: "honeycomb", PricePerGB: 1.20, Label: "Honeycomb"},
			{Match: "datadog", PricePerGB: 0.10, Logs: 0.05, Traces: 1.50, Label: "Datadog"},
			// No explicit catch-all — NewProjector should add one.
		},
	})
	cases := []struct {
		name     string
		key      string
		signal   Signal
		bytes    int64
		wantRate float64
	}{
		{"honeycomb traces",
			"honeycomb:Honeycomb (api.honeycomb.io)", SignalTraces,
			1024 * 1024 * 1024, 1.20},
		{"datadog logs use log rate",
			"datadog:Datadog (dd.datadoghq.eu)", SignalLogs,
			1024 * 1024 * 1024, 0.05},
		{"datadog traces use trace rate",
			"datadog:Datadog", SignalTraces,
			1024 * 1024 * 1024, 1.50},
		{"datadog metrics fall back to base",
			"datadog:Datadog", SignalMetrics,
			1024 * 1024 * 1024, 0.10},
		{"unmatched falls back to catch-all default",
			"jaeger:Jaeger (jaeger:16686)", SignalLogs,
			1024 * 1024 * 1024, DefaultPricePerGB},
		{"case insensitive match",
			"HONEYCOMB:Honeycomb", SignalMetrics,
			1024 * 1024 * 1024, 1.20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.MonthlyForBytes(tc.bytes, tc.signal, tc.key)
			want := 730.0 * tc.wantRate
			if math.Abs(got-want) > 0.01 {
				t.Errorf("got $%.4f, want $%.4f (rate $%.2f/GB)", got, want, tc.wantRate)
			}
		})
	}
}

// TestNewProjector_RejectsNegativePrice.
func TestNewProjector_RejectsNegativePrice(t *testing.T) {
	_, err := NewProjector(Config{
		Enabled: true,
		Rules:   []Rule{{Match: "bad", PricePerGB: -1}},
	})
	if err == nil {
		t.Errorf("expected error for negative price; got nil")
	}
}

// TestNewProjector_AutoAppendsCatchAll.
func TestNewProjector_AutoAppendsCatchAll(t *testing.T) {
	p := mustNewProjector(t, Config{
		Enabled: true,
		Rules:   []Rule{{Match: "honeycomb", PricePerGB: 1.20}},
	})
	rules := p.Rules()
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules (named + catch-all), got %d", len(rules))
	}
	if rules[1].Match != "" {
		t.Errorf("last rule should be catch-all (empty Match), got %q", rules[1].Match)
	}
	if rules[1].PricePerGB != DefaultPricePerGB {
		t.Errorf("catch-all should use DefaultPricePerGB=%.2f, got %.2f",
			DefaultPricePerGB, rules[1].PricePerGB)
	}
}

// TestDefaultConfig is exercised end-to-end — common destination
// keys map to expected price tiers.
func TestDefaultConfig(t *testing.T) {
	p, err := NewProjector(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		key    string
		signal Signal
		// Just check the rate is in a sane range to catch ordering bugs.
		minRate, maxRate float64
	}{
		{"datadog traces are expensive", "datadog:Datadog", SignalTraces, 1.0, 2.0},
		{"datadog logs are cheap", "datadog:Datadog", SignalLogs, 0.05, 0.20},
		{"honeycomb base", "honeycomb:Honeycomb", SignalMetrics, 1.0, 1.5},
		{"grafana logs are cheap", "grafana:Grafana Cloud", SignalLogs, 0.05, 0.20},
		{"unmatched destination uses default", "kafka:Kafka", SignalMetrics, 0.25, 0.40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			perMonth := p.MonthlyForBytes(1024*1024*1024, tc.signal, tc.key) // 1 GB/h
			ratePerGB := perMonth / 730.0
			if ratePerGB < tc.minRate || ratePerGB > tc.maxRate {
				t.Errorf("%s: rate $%.2f/GB outside expected [$%.2f, $%.2f]",
					tc.name, ratePerGB, tc.minRate, tc.maxRate)
			}
		})
	}
}

// TestProjectFleet — full top-line projection rolls per-signal and
// per-destination correctly and sorts destinations by spend desc.
func TestProjectFleet(t *testing.T) {
	p := mustNewProjector(t, Config{
		Enabled: true,
		Rules: []Rule{
			{Match: "datadog", PricePerGB: 0.20, Logs: 0.10, Traces: 1.50, Label: "Datadog"},
			{Match: "honeycomb", PricePerGB: 1.20, Label: "Honeycomb"},
			{Match: "", PricePerGB: 0.30, Label: "Other"},
		},
	})
	in := FleetInput{
		SignalBytesPerHour: map[Signal]int64{
			SignalMetrics: 1024 * 1024 * 1024, // 1 GB/h
			SignalLogs:    512 * 1024 * 1024,  // 0.5 GB/h
		},
		Destinations: []DestinationInput{
			{
				Key: "honeycomb:Honeycomb", Label: "Honeycomb",
				BytesPerHour: 1024 * 1024 * 1024,
			},
			{
				Key: "datadog:Datadog", Label: "Datadog",
				BytesPerHour: 512 * 1024 * 1024,
				BySignal: map[Signal]int64{
					SignalLogs:   256 * 1024 * 1024, // half logs (cheap)
					SignalTraces: 256 * 1024 * 1024, // half traces (expensive)
				},
			},
		},
	}
	out := p.ProjectFleet(in)
	if !out.Enabled {
		t.Fatalf("expected enabled=true in projection")
	}
	if out.Currency != DefaultCurrency {
		t.Errorf("currency: got %q want %q", out.Currency, DefaultCurrency)
	}
	if len(out.ByDestination) != 2 {
		t.Fatalf("expected 2 destinations, got %d", len(out.ByDestination))
	}
	// Honeycomb at 1 GB/h × $1.20 × 730h ≈ $876/month — should be #1.
	// Datadog at 0.25 GB/h × ($0.10 logs + $1.50 traces) × 730h ≈ $292/month.
	if out.ByDestination[0].DestinationKey != "honeycomb:Honeycomb" {
		t.Errorf("top destination should be honeycomb; got %q",
			out.ByDestination[0].DestinationKey)
	}
	if out.ByDestination[1].DestinationKey != "datadog:Datadog" {
		t.Errorf("second destination should be datadog; got %q",
			out.ByDestination[1].DestinationKey)
	}
	if out.MonthlyUSD <= 0 {
		t.Errorf("MonthlyUSD should be > 0; got %.2f", out.MonthlyUSD)
	}
	if len(out.Assumptions) == 0 {
		t.Errorf("Assumptions should be populated for the dashboard footer")
	}
}

// TestMonthlyForRecommendation maps a v0.25 byte estimate to dollars.
func TestMonthlyForRecommendation(t *testing.T) {
	p := mustNewProjector(t, DefaultConfig())
	// 100 MB/hour of metrics saved → ~30 days × ... at $0.30/GB default
	got := p.MonthlyForRecommendation(100*1024*1024, 3600, SignalMetrics)
	want := 730.0 * (100.0 / 1024.0) * 0.30 // GB/month × $/GB
	if math.Abs(got-want) > 0.05 {
		t.Errorf("got $%.4f want $%.4f", got, want)
	}

	// Negative bytes (drop-hotspot encoding) → 0.
	if got := p.MonthlyForRecommendation(-1, 3600, SignalMetrics); got != 0 {
		t.Errorf("negative bytes should yield 0; got $%.4f", got)
	}
	// Zero window → 0.
	if got := p.MonthlyForRecommendation(1024*1024, 0, SignalMetrics); got != 0 {
		t.Errorf("zero window should yield 0; got $%.4f", got)
	}
}
