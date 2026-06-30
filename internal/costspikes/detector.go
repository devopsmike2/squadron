// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package costspikes implements the v0.29 automatic cost-spike
// alerting layer. It sits between the v0.27 pricing projector and
// the operator: every minute the detector pulls the current
// fleet $/month projection, compares it to a rolling baseline,
// and opens a CostSpikeEvent when the projection breaks above
// configurable warn/critical thresholds.
//
// Why not extend the existing alerts package? Existing alerts are
// operator-authored rules over Squadron QL queries — they expect
// the operator to write the predicate. Cost spikes need to be
// automatic and zero-config; the heuristic plus the pricing
// projection IS the predicate. Keeping it separate also lets the
// dashboard surface "open cost spikes" cleanly without polluting
// the rules list.
//
// The detector has no background queue and no goroutine fan-out.
// One tick = one ProjectFleet call + one storage read + zero or
// one storage write. It's cheap by construction.
package costspikes

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/devopsmike2/squadron/internal/insights"
	"github.com/devopsmike2/squadron/internal/pricing"
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// SpikeStore is the narrow slice of ApplicationStore the detector
// needs. Extracted so tests can fake it without standing up sqlite.
type SpikeStore interface {
	CreateCostSpikeEvent(ctx context.Context, e *storetypes.CostSpikeEvent) error
	UpdateCostSpikeEvent(ctx context.Context, e *storetypes.CostSpikeEvent) error
	LatestOpenCostSpike(ctx context.Context) (*storetypes.CostSpikeEvent, error)
}

// PricerProjector is the slice of pricing.Projector the detector
// uses. Decoupled so callers can pass a stub when pricing is off
// or under test.
type PricerProjector interface {
	Enabled() bool
	ProjectFleet(in pricing.FleetInput) pricing.FleetProjection
	MonthlyForBytesPerHour(bytesPerHour int64, signal string) float64
}

// InsightsQuerier is the slice of insights.Service the detector
// uses to attribute spikes — top agents + top attributes for the
// signal that's driving the spike.
type InsightsQuerier interface {
	FleetVolume(ctx context.Context, win insights.Window, signalFilter []insights.Signal) (*insights.FleetSummary, error)
	TopAgents(ctx context.Context, win insights.Window, limit int) ([]insights.AgentVolume, error)
	TopAttributes(ctx context.Context, win insights.Window, signal insights.Signal, limit int) ([]insights.AttributeVolume, error)
}

// Config tunes the detector. Defaults are conservative — we'd
// rather miss a small spike than wake someone up over a wobble in
// the projection.
type Config struct {
	// WarnPct is the fraction over baseline that fires a warn-level
	// spike. 0.25 = 25% over baseline.
	WarnPct float64
	// CriticalPct is the fraction over baseline that fires a
	// critical-level spike. Must be >= WarnPct.
	CriticalPct float64
	// BaselineSamples is how many of the most recent ticks (each
	// 1 minute apart by default) we average to form the baseline.
	// Excludes the current tick. 60 = "compare against the last
	// hour of projections."
	BaselineSamples int
	// MinBaselineUSD is the floor below which we don't fire — at
	// $5/month projected spend, a 50% spike is $2.50 and isn't
	// worth waking someone up over.
	MinBaselineUSD float64
	// Window is the insights window the detector consults for the
	// projection and for attribution. 1h matches the cost-insights
	// surface's default.
	Window insights.Window
}

// DefaultConfig is the production-suggested tuning. Tune via
// `cost_spike.*` in squadron.yaml.
func DefaultConfig() Config {
	return Config{
		WarnPct:         0.25,
		CriticalPct:     0.50,
		BaselineSamples: 60,
		MinBaselineUSD:  10.0,
		Window:          insights.Window1h,
	}
}

// Detector is the periodic anomaly detector. Stateful — keeps a
// rolling ring buffer of recent projections in memory so each
// tick only does O(1) baseline math.
type Detector struct {
	cfg      Config
	store    SpikeStore
	pricer   PricerProjector
	insights InsightsQuerier

	mu      sync.Mutex
	samples []float64 // ring of recent total monthly_usd projections

	// tickMu serializes whole Tick cycles. The detection state machine
	// reads the latest open spike and then conditionally creates one
	// (openOrEscalate) — a check-then-insert that is NOT atomic under
	// the fine-grained sample mutex alone. Without this, the 60s
	// background loop and an operator-triggered POST /tick can interleave,
	// both observe "no open spike", and both INSERT — leaving two
	// simultaneous open rows for one event, only one of which ever closes.
	// One tick is cheap by construction (one ProjectFleet + one read +
	// <=one write), so full serialization costs nothing.
	tickMu sync.Mutex
}

// New builds a detector. Caller is responsible for calling Tick
// on whatever cadence makes sense (typically once per minute via
// a goroutine in main.go).
func New(cfg Config, store SpikeStore, pricer PricerProjector, ins InsightsQuerier) *Detector {
	if cfg.WarnPct <= 0 {
		cfg.WarnPct = 0.25
	}
	if cfg.CriticalPct < cfg.WarnPct {
		cfg.CriticalPct = cfg.WarnPct * 2
	}
	if cfg.BaselineSamples <= 0 {
		cfg.BaselineSamples = 60
	}
	if cfg.MinBaselineUSD <= 0 {
		cfg.MinBaselineUSD = 10.0
	}
	if cfg.Window == "" {
		cfg.Window = insights.Window1h
	}
	return &Detector{
		cfg:      cfg,
		store:    store,
		pricer:   pricer,
		insights: ins,
		samples:  make([]float64, 0, cfg.BaselineSamples+1),
	}
}

// Tick is one detection cycle: pull the current projection, push
// it into the rolling window, compare to baseline, and write any
// state transitions to the store. Safe to call concurrently — tickMu
// serializes whole cycles so the open-spike check-then-insert stays
// atomic (the background loop and an operator POST /tick can race).
func (d *Detector) Tick(ctx context.Context) error {
	if d == nil || d.pricer == nil || !d.pricer.Enabled() {
		return nil
	}
	d.tickMu.Lock()
	defer d.tickMu.Unlock()
	current, currentBySignal, err := d.currentProjection(ctx)
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	d.recordSample(current)

	baseline, ok := d.baseline()
	if !ok {
		return nil // not enough history yet
	}
	if baseline < d.cfg.MinBaselineUSD {
		return d.closeAnyOpenSpike(ctx, current, "below_min_baseline")
	}

	pct := (current - baseline) / baseline
	switch {
	case pct >= d.cfg.CriticalPct:
		return d.openOrEscalate(ctx, "critical", baseline, current, pct, currentBySignal)
	case pct >= d.cfg.WarnPct:
		return d.openOrEscalate(ctx, "warn", baseline, current, pct, currentBySignal)
	default:
		return d.closeAnyOpenSpike(ctx, current, "below_threshold")
	}
}

// currentProjection asks insights for the fleet summary, hands it
// to the pricer, and returns the fleet-wide monthly USD plus the
// per-signal projection map. We don't need the per-destination
// breakdown here; the attribution path does that on fire.
func (d *Detector) currentProjection(ctx context.Context) (float64, map[insights.Signal]float64, error) {
	if d.insights == nil {
		return 0, nil, fmt.Errorf("insights service required")
	}
	summary, err := d.insights.FleetVolume(ctx, d.cfg.Window, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("fleet volume: %w", err)
	}
	winSec, _ := d.cfg.Window.AsDuration()
	windowSeconds := int64(winSec.Seconds())
	if windowSeconds <= 0 {
		windowSeconds = 3600
	}
	bySignal := map[pricing.Signal]int64{}
	for _, b := range summary.BySignal {
		// Normalize from bytes-in-window to bytes/hour.
		bySignal[pricing.Signal(b.Signal)] = int64(float64(b.Bytes) * 3600.0 / float64(windowSeconds))
	}
	proj := d.pricer.ProjectFleet(pricing.FleetInput{SignalBytesPerHour: bySignal})
	out := map[insights.Signal]float64{}
	for sig, usd := range proj.BySignal {
		out[insights.Signal(sig)] = usd
	}
	return proj.MonthlyUSD, out, nil
}

// recordSample pushes a sample into the ring buffer, evicting the
// oldest entry when full. We keep BaselineSamples+1 because the
// most recent entry (the current tick) is excluded from baseline.
func (d *Detector) recordSample(monthly float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.samples = append(d.samples, monthly)
	max := d.cfg.BaselineSamples + 1
	if len(d.samples) > max {
		d.samples = d.samples[len(d.samples)-max:]
	}
}

// baseline returns the trimmed-mean of the buffered samples
// excluding the most recent. Returns ok=false when we don't have
// enough history yet (boot warm-up).
func (d *Detector) baseline() (float64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.samples) < 5 {
		return 0, false
	}
	// Exclude the most recent — that's the value we're comparing
	// against. The rest is the baseline pool.
	pool := make([]float64, len(d.samples)-1)
	copy(pool, d.samples[:len(d.samples)-1])
	sort.Float64s(pool)
	// Trim top + bottom 10% to be robust against single anomalous
	// readings (a brief blip shouldn't permanently raise the
	// baseline and cause us to miss the next spike).
	trim := len(pool) / 10
	pool = pool[trim : len(pool)-trim]
	if len(pool) == 0 {
		return 0, false
	}
	var sum float64
	for _, v := range pool {
		sum += v
	}
	return sum / float64(len(pool)), true
}

// dominantSignal returns the signal whose share of the projected
// monthly cost is highest. Used to scope attribution.
func dominantSignal(bySignal map[insights.Signal]float64) insights.Signal {
	var best insights.Signal
	var bestUSD float64
	for sig, usd := range bySignal {
		if usd > bestUSD {
			best = sig
			bestUSD = usd
		}
	}
	return best
}

// openOrEscalate handles both "new spike" and "existing spike got
// worse" paths. It also handles the warn → critical upgrade in
// place; we don't open a fresh row for severity bumps on the
// same incident.
func (d *Detector) openOrEscalate(
	ctx context.Context,
	severity string,
	baseline, current, pct float64,
	bySignal map[insights.Signal]float64,
) error {
	open, err := d.store.LatestOpenCostSpike(ctx)
	if err != nil {
		return fmt.Errorf("latest open: %w", err)
	}
	signal := dominantSignal(bySignal)
	if open == nil {
		// New spike. Compute attribution once at fire time and
		// freeze it on the row so the operator sees a stable
		// picture even hours later.
		attr := d.attribute(ctx, signal)
		ev := &storetypes.CostSpikeEvent{
			ID:                   newSpikeID(),
			StartedAt:            time.Now().UTC(),
			Severity:             severity,
			Signal:               string(signal),
			BaselineMonthlyUSD:   baseline,
			PeakMonthlyUSD:       current,
			PeakPctAboveBaseline: pct,
			AttributionJSON:      attr,
		}
		return d.store.CreateCostSpikeEvent(ctx, ev)
	}
	// Update peak + severity if either climbed. Keep the existing
	// baseline (the one captured at first fire) — that's the
	// honest "what was normal before the spike?" anchor.
	changed := false
	if current > open.PeakMonthlyUSD {
		open.PeakMonthlyUSD = current
		open.PeakPctAboveBaseline = pct
		changed = true
	}
	if severity == "critical" && open.Severity == "warn" {
		open.Severity = "critical"
		changed = true
	}
	if !changed {
		return nil
	}
	return d.store.UpdateCostSpikeEvent(ctx, open)
}

// closeAnyOpenSpike marks any in-flight spike as ended. Called
// when current drops back below the warn threshold OR baseline is
// below the floor (which we treat as "the fleet went quiet, end
// the alarm").
func (d *Detector) closeAnyOpenSpike(ctx context.Context, _ float64, _ string) error {
	open, err := d.store.LatestOpenCostSpike(ctx)
	if err != nil {
		return fmt.Errorf("latest open: %w", err)
	}
	if open == nil {
		return nil
	}
	now := time.Now().UTC()
	open.EndedAt = &now
	return d.store.UpdateCostSpikeEvent(ctx, open)
}

// attribute calls TopAgents + TopAttributes and serializes the
// top 3 of each into a compact JSON blob stored on the event.
// Empty string on any error — attribution is best-effort, we
// don't fail the alert on an attribution miss.
func (d *Detector) attribute(ctx context.Context, signal insights.Signal) string {
	if d.insights == nil {
		return ""
	}
	type agentRow struct {
		AgentID   string `json:"agent_id"`
		AgentName string `json:"agent_name,omitempty"`
		BytesPct  string `json:"bytes_pct,omitempty"`
	}
	type attrRow struct {
		Key      string `json:"key"`
		BytesPct string `json:"bytes_pct,omitempty"`
	}
	out := struct {
		TopAgents     []agentRow `json:"top_agents,omitempty"`
		TopAttributes []attrRow  `json:"top_attributes,omitempty"`
		Signal        string     `json:"signal,omitempty"`
	}{Signal: string(signal)}

	if agents, err := d.insights.TopAgents(ctx, d.cfg.Window, 5); err == nil {
		var totalBytes int64
		for _, a := range agents {
			totalBytes += a.TotalBytes
		}
		for i, a := range agents {
			if i >= 3 {
				break
			}
			pct := ""
			if totalBytes > 0 {
				pct = fmt.Sprintf("%.0f%%", 100.0*float64(a.TotalBytes)/float64(totalBytes))
			}
			out.TopAgents = append(out.TopAgents, agentRow{
				AgentID:   a.AgentID,
				AgentName: a.AgentName,
				BytesPct:  pct,
			})
		}
	}

	if signal != "" {
		if attrs, err := d.insights.TopAttributes(ctx, d.cfg.Window, signal, 10); err == nil {
			var totalBytes int64
			for _, a := range attrs {
				totalBytes += a.Bytes
			}
			for i, a := range attrs {
				if i >= 3 {
					break
				}
				pct := ""
				if totalBytes > 0 {
					pct = fmt.Sprintf("%.0f%%", 100.0*float64(a.Bytes)/float64(totalBytes))
				}
				out.TopAttributes = append(out.TopAttributes, attrRow{
					Key:      a.Key,
					BytesPct: pct,
				})
			}
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// newSpikeID returns a short random ID. We don't need uuid-grade
// uniqueness here — spikes are scoped to one Squadron instance
// and the cardinality is tiny.
func newSpikeID() string {
	return fmt.Sprintf("cs_%d_%x", time.Now().UnixNano(), randomTag())
}

func randomTag() uint32 {
	// time.Now() at nanosecond resolution + a derived value is
	// unique enough; we avoid pulling crypto/rand here to keep
	// this package dependency-light.
	t := uint64(time.Now().UnixNano())
	return uint32(math.Float64bits(float64(t)) >> 32)
}
