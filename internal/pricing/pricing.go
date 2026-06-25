// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package pricing is the v0.27+ dollar-projection layer. It maps
// the byte numbers from v0.24 insights and the savings estimates
// from v0.25 recommendations into $/month figures the operator
// can read directly.
//
// The pricing data is configuration, not measurement. Real
// per-byte costs vary by vendor, retention tier, contract, and
// region; Squadron ships sensible starting defaults but operators
// are expected to tune the per-destination rates to their own
// invoice. The Savings dashboard renders these as estimates, with
// the assumptions footer clearly visible.
//
// Design notes:
//
//   - Pure-Go, no I/O. Construct once at startup; consult per
//     request. Hot path is a small loop over destination rules.
//   - $/GB per signal per destination. Most backends differentiate
//     log/metric/trace pricing; we let operators express that.
//   - First-match-wins rule ordering, mirroring iptables semantics.
//     The default Rule (matches everything) sits at the tail.
//   - Currency is a free-form ISO code; we don't do FX conversions.
//     Operators set everything in their billing currency.
package pricing

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Defaults — these are conservative starter numbers across major
// OpenTelemetry destinations. They are NOT accurate enough to
// drive procurement decisions; they're a starting point operators
// tune against their actual invoice. The defaults bias high so
// "savings projected" doesn't overpromise.
//
// Per-GB ingest cost rough survey at time of writing:
//
//	Datadog logs   ~$0.10/GB (1-day retention, varies wildly)
//	Datadog APM    ~$5/M spans (depends on avg span size)
//	Honeycomb      ~$0.40/M events (~$1-2/GB at typical avg size)
//	New Relic      ~$0.30-0.50/GB telemetry
//	Grafana Cloud  ~$0.50/GB metrics, much cheaper logs
//	SigNoz Cloud   ~$0.30/GB telemetry
//	Self-hosted    effectively storage + egress (often <$0.10/GB)
//
// We default to $0.30/GB across all signals, with destination-
// specific overrides for the named backends. Operators override
// anything they care about.
const (
	DefaultPricePerGB    = 0.30
	DefaultCurrency      = "USD"
	defaultWindowsPerDay = 24 // hourly windows; we project from 1h volume
)

// Signal mirrors insights.Signal but is duplicated here to avoid
// the pricing package importing insights (which would create a
// cycle once recommendations consumes pricing). Tiny string enum.
type Signal string

const (
	SignalTraces  Signal = "traces"
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
)

// Rule is one row of the per-destination price book. The Match
// field is a substring tested against the destination_key built
// by the UI exporter-parser (e.g. "honeycomb:Honeycomb (api.honeycomb.io)").
// First-match-wins; a Rule with empty Match is the fallback and
// must be last.
//
// Per-signal rates default to PricePerGB when zero, so operators
// who don't care about per-signal differentiation can set one
// number and move on.
type Rule struct {
	// Match is a substring of destination_key (or destination_kind).
	// Empty match makes this the catch-all default rule.
	Match string `yaml:"match" json:"match"`
	// Label is operator-supplied for readability in the projection
	// response and the Savings UI. Optional; falls back to Match.
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	// PricePerGB is the base $/GB used for any signal where the
	// per-signal field is zero. Required.
	PricePerGB float64 `yaml:"price_per_gb" json:"price_per_gb"`
	// Per-signal overrides. Zero means "use PricePerGB".
	Traces  float64 `yaml:"traces,omitempty" json:"traces,omitempty"`
	Metrics float64 `yaml:"metrics,omitempty" json:"metrics,omitempty"`
	Logs    float64 `yaml:"logs,omitempty" json:"logs,omitempty"`
}

// Config is the pricing configuration as loaded from squadron.yaml
// (or supplied programmatically in tests). When Enabled is false,
// the projector returns zero $ for everything — the UI uses that
// to decide whether to render $ figures or fall back to bytes.
type Config struct {
	Enabled  bool   `yaml:"enabled"`
	Currency string `yaml:"currency,omitempty"`
	Rules    []Rule `yaml:"rules,omitempty"`
}

// DefaultConfig returns the conservative starter price book —
// per-destination rules for the major backends plus a $0.30/GB
// catch-all. Operators are expected to tune these against their
// actual invoice; the defaults bias high to avoid overpromising
// savings.
func DefaultConfig() Config {
	return Config{
		Enabled:  true,
		Currency: DefaultCurrency,
		Rules: []Rule{
			{
				Match:      "datadog",
				Label:      "Datadog",
				PricePerGB: 0.40, // logs ~$0.10/GB, but spans/metrics significantly more
				Traces:     1.50, // APM is the expensive one
				Logs:       0.10,
				Metrics:    0.50,
			},
			{
				Match:      "honeycomb",
				Label:      "Honeycomb",
				PricePerGB: 1.20, // event-based pricing rolls up to ~$1-2/GB
			},
			{
				Match:      "newrelic",
				Label:      "New Relic",
				PricePerGB: 0.40,
			},
			{
				Match:      "signoz",
				Label:      "SigNoz",
				PricePerGB: 0.30,
			},
			{
				Match:      "grafana",
				Label:      "Grafana Cloud",
				PricePerGB: 0.50,
				Logs:       0.10, // Loki ingest is cheap
			},
			{
				Match:      "splunk",
				Label:      "Splunk",
				PricePerGB: 0.80,
			},
			// Catch-all — always last. Empty Match matches everything
			// the named rules above didn't catch.
			{
				Match:      "",
				Label:      "Other",
				PricePerGB: DefaultPricePerGB,
			},
		},
	}
}

// Projector wraps a Config with the projection math. Construct
// once at startup and share across requests; the struct is
// effectively immutable after construction.
type Projector struct {
	cfg Config
}

// NewProjector validates the config and returns a Projector. The
// catch-all rule (empty Match) is auto-appended if missing — the
// alternative is silent zero-cost projection for unmatched
// destinations, which is worse than a slightly-too-conservative
// number.
func NewProjector(cfg Config) (*Projector, error) {
	if cfg.Currency == "" {
		cfg.Currency = DefaultCurrency
	}
	hasCatchAll := false
	for _, r := range cfg.Rules {
		if r.Match == "" {
			hasCatchAll = true
		}
		if r.PricePerGB < 0 {
			return nil, fmt.Errorf("pricing rule %q: price_per_gb must be non-negative", r.Match)
		}
	}
	if !hasCatchAll {
		cfg.Rules = append(cfg.Rules, Rule{
			Match:      "",
			Label:      "Other",
			PricePerGB: DefaultPricePerGB,
		})
	}
	return &Projector{cfg: cfg}, nil
}

// Enabled reports whether pricing projections are active. When
// false, every projection method returns zero $.
func (p *Projector) Enabled() bool { return p != nil && p.cfg.Enabled }

// Currency returns the configured currency code (e.g. "USD").
func (p *Projector) Currency() string {
	if p == nil {
		return DefaultCurrency
	}
	return p.cfg.Currency
}

// Rules returns the configured rules for display in the Savings
// dashboard's pricing-assumptions footer. Returns a copy to keep
// the internal state immutable.
func (p *Projector) Rules() []Rule {
	out := make([]Rule, len(p.cfg.Rules))
	copy(out, p.cfg.Rules)
	return out
}

// ----------------------------------------------------------------
// Projection math
// ----------------------------------------------------------------

// MonthlyForBytes projects $/month given a number of bytes
// observed in a 1-hour window. We assume linear extrapolation —
// good enough for v0.27 "ballpark monthly bill" framing. Real
// usage seasonality is a v0.27.x concern.
//
// signal selects the per-signal rate; destinationKey selects the
// matching rule (first-match-wins; empty key uses the catch-all).
// Returns 0 when the projector is disabled.
func (p *Projector) MonthlyForBytes(bytesPerHour int64, signal Signal, destinationKey string) float64 {
	if !p.Enabled() || bytesPerHour <= 0 {
		return 0
	}
	rule := p.matchRule(destinationKey)
	pricePerGB := signalPrice(rule, signal)
	// 1 GB = 1024^3 bytes. Project hourly → monthly.
	const bytesPerGB = float64(1024 * 1024 * 1024)
	const hoursPerMonth = 730.0 // 365.25 * 24 / 12
	gbPerMonth := float64(bytesPerHour) * hoursPerMonth / bytesPerGB
	return gbPerMonth * pricePerGB
}

// MonthlyForBytesWindow is a convenience for callers who have a
// volume number scoped to a different window than 1 hour. windowSeconds
// is the window the bytes accumulated over (e.g. 86400 for 24h
// volume). Internally normalizes to hourly before the per-month
// extrapolation so the math is the same.
func (p *Projector) MonthlyForBytesWindow(bytesInWindow int64, windowSeconds int64, signal Signal, destinationKey string) float64 {
	if !p.Enabled() || bytesInWindow <= 0 || windowSeconds <= 0 {
		return 0
	}
	hourly := float64(bytesInWindow) * 3600.0 / float64(windowSeconds)
	return p.MonthlyForBytes(int64(hourly), signal, destinationKey)
}

// DestinationProjection is the per-destination row the Savings
// dashboard renders.
type DestinationProjection struct {
	DestinationKey string  `json:"destination_key"`
	Label          string  `json:"label"`
	MonthlyUSD     float64 `json:"monthly_usd"`
	RuleMatched    string  `json:"rule_matched"` // which Rule.Match caught it, for debugging
	RuleLabel      string  `json:"rule_label,omitempty"`
}

// FleetProjection is the top-line $/month answer for the whole
// fleet, broken down by destination + by signal.
type FleetProjection struct {
	Enabled       bool                    `json:"enabled"`
	Currency      string                  `json:"currency"`
	MonthlyUSD    float64                 `json:"monthly_usd"`
	BySignal      map[Signal]float64      `json:"by_signal,omitempty"`
	ByDestination []DestinationProjection `json:"by_destination,omitempty"`
	Assumptions   []Rule                  `json:"assumptions,omitempty"`
}

// FleetInput is the shape the projector consumes for fleet-wide
// estimates. Built by the handler from the existing insights
// FleetSummary + destination breakdown. Kept here (not pulled
// from insights) so internal/pricing has no upward deps.
type FleetInput struct {
	// SignalBytesPerHour is bytes/hour observed per signal across
	// the fleet. Handler computes from FleetSummary by normalizing
	// the window.
	SignalBytesPerHour map[Signal]int64
	// Destinations is the per-destination byte share observed in
	// the same window. Handler builds this from the v0.24
	// DestinationBreakdown.
	Destinations []DestinationInput
}

// DestinationInput pairs a destination key + label with its
// observed bytes/hour. Handler may pass per-signal bytes when
// available; we project either way.
type DestinationInput struct {
	Key          string
	Label        string
	BytesPerHour int64
	// BySignal is optional. When provided, each signal's bytes are
	// priced individually for this destination. When nil, we
	// assume the destination's bytes follow the fleet-wide signal
	// mix.
	BySignal map[Signal]int64
}

// ProjectFleet returns the top-line $/month projection for the
// whole fleet, plus per-signal and per-destination breakdowns.
// Empty input → zero projection (not an error); operators expect
// the Savings page to render gracefully on day one.
func (p *Projector) ProjectFleet(in FleetInput) FleetProjection {
	out := FleetProjection{
		Enabled:     p.Enabled(),
		Currency:    p.Currency(),
		BySignal:    map[Signal]float64{},
		Assumptions: p.Rules(),
	}
	if !p.Enabled() {
		return out
	}

	// Per-signal: priced at the fleet-wide weighted average rate
	// for that signal across all destinations. We compute by
	// summing per-destination signal-shares × their matched rule's
	// signal price. When the destination doesn't supply BySignal,
	// we fall back to the catch-all rate for the signal — this
	// gives a sensible answer when destination attribution is
	// imperfect (v0.24 estimates).
	defaultRule := p.matchRule("")

	for sig, bytesPerHour := range in.SignalBytesPerHour {
		// Start with the catch-all rate.
		if bytesPerHour <= 0 {
			continue
		}
		monthly := p.MonthlyForBytes(bytesPerHour, sig, "")
		out.BySignal[sig] = monthly
		out.MonthlyUSD += monthly
		_ = defaultRule
	}

	// Per-destination, with per-signal pricing when available.
	for _, dest := range in.Destinations {
		rule := p.matchRule(dest.Key)
		var monthly float64
		if dest.BySignal != nil {
			for sig, b := range dest.BySignal {
				monthly += p.MonthlyForBytes(b, sig, dest.Key)
			}
		} else {
			// Lump sum at the rule's base rate.
			monthly += p.MonthlyForBytes(dest.BytesPerHour, "", dest.Key)
		}
		out.ByDestination = append(out.ByDestination, DestinationProjection{
			DestinationKey: dest.Key,
			Label:          dest.Label,
			MonthlyUSD:     monthly,
			RuleMatched:    rule.Match,
			RuleLabel:      rule.Label,
		})
	}
	sort.Slice(out.ByDestination, func(i, j int) bool {
		return out.ByDestination[i].MonthlyUSD > out.ByDestination[j].MonthlyUSD
	})

	return out
}

// MonthlyForRecommendation is the per-card $ savings figure shown
// next to each Recommendation. Uses the signal from the rec when
// set, and the catch-all destination rate when destinationKey is
// empty (rec-level destination attribution is a v0.27.x concern).
//
// estSavingsBytes is the engine's per-window byte savings; windowSeconds
// is the window. Returns 0 when projector is disabled or when
// estSavingsBytes is negative (drop-hotspot category encodes
// "non-byte" savings as -1).
func (p *Projector) MonthlyForRecommendation(estSavingsBytes int64, windowSeconds int64, signal Signal) float64 {
	if !p.Enabled() || estSavingsBytes <= 0 || windowSeconds <= 0 {
		return 0
	}
	return p.MonthlyForBytesWindow(estSavingsBytes, windowSeconds, signal, "")
}

// ----------------------------------------------------------------
// Internals
// ----------------------------------------------------------------

// matchRule walks the rule list and returns the first rule whose
// Match is a substring of destinationKey. The catch-all (Match
// == "") is appended at construction time so this never returns
// the zero Rule.
func (p *Projector) matchRule(destinationKey string) Rule {
	key := strings.ToLower(destinationKey)
	for _, r := range p.cfg.Rules {
		if r.Match == "" {
			return r
		}
		if strings.Contains(key, strings.ToLower(r.Match)) {
			return r
		}
	}
	// Defensive — NewProjector guarantees a catch-all exists, but
	// callers might construct *Projector directly in tests.
	return Rule{PricePerGB: DefaultPricePerGB}
}

// signalPrice returns the per-signal rate for a rule, falling back
// to PricePerGB when the signal-specific field is zero.
func signalPrice(r Rule, signal Signal) float64 {
	switch signal {
	case SignalTraces:
		if r.Traces > 0 {
			return r.Traces
		}
	case SignalMetrics:
		if r.Metrics > 0 {
			return r.Metrics
		}
	case SignalLogs:
		if r.Logs > 0 {
			return r.Logs
		}
	}
	return r.PricePerGB
}

// ErrDisabled is returned by API handlers (not by Projector) when
// callers want a distinct error for "pricing not configured" so
// the UI can show the opt-in nudge. Exposed here so handlers
// don't have to define their own sentinel.
var ErrDisabled = errors.New("pricing service disabled")

// MonthlyForBytesPerHour is a small shim satisfying the
// recommendations.Pricer interface. The recommendations package
// doesn't import pricing directly (to keep the dep arrow pointing
// the other way); main.go passes *Projector wherever
// recommendations.Pricer is expected.
func (p *Projector) MonthlyForBytesPerHour(bytesPerHour int64, signal string) float64 {
	return p.MonthlyForBytes(bytesPerHour, Signal(signal), "")
}
