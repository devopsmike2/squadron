// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package costspikes

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/insights"
	"github.com/devopsmike2/squadron/internal/pricing"
	storetypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// lockedSpikeStore is a concurrency-safe SpikeStore that atomically
// models the latest-open state, so a concurrency test exercises the
// DETECTOR's serialization (tickMu) rather than a race in the fake.
type lockedSpikeStore struct {
	mu      sync.Mutex
	open    *storetypes.CostSpikeEvent
	created int
}

func (s *lockedSpikeStore) CreateCostSpikeEvent(_ context.Context, e *storetypes.CostSpikeEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	cp := *e
	s.open = &cp
	return nil
}
func (s *lockedSpikeStore) UpdateCostSpikeEvent(_ context.Context, e *storetypes.CostSpikeEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *e
	if cp.EndedAt != nil {
		s.open = nil
	} else {
		s.open = &cp
	}
	return nil
}
func (s *lockedSpikeStore) LatestOpenCostSpike(_ context.Context) (*storetypes.CostSpikeEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		return nil, nil
	}
	cp := *s.open
	return &cp, nil
}

// TestDetector_ConcurrentTicks_SingleOpenSpike pins the duplicate-open
// race fix: the 60s background loop and an operator POST /tick can call
// Tick concurrently; without serialization both observe "no open spike"
// and both INSERT. With tickMu, exactly one open row is created. Run with
// -race to also catch the underlying check-then-insert data race.
func TestDetector_ConcurrentTicks_SingleOpenSpike(t *testing.T) {
	store := &lockedSpikeStore{}
	// A large spike value keeps pct far above threshold even as ticks
	// append samples, so the spike never closes-and-reopens mid-test.
	pricer := &fakePricer{enabled: true, monthly: 10000}
	det := New(DefaultConfig(), store, pricer, &fakeInsights{})
	warmUp(det, 100, 30)

	const n = 25
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = det.Tick(context.Background())
		}()
	}
	wg.Wait()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.created != 1 {
		t.Fatalf("concurrent ticks created %d open spikes, want exactly 1 (duplicate-open race)", store.created)
	}
}

// fakeStore is an in-memory SpikeStore that records every write.
// Sufficient for testing the detector's state-machine.
type fakeStore struct {
	open    *storetypes.CostSpikeEvent
	all     []*storetypes.CostSpikeEvent
	created int
	updated int
	openErr error
}

func (f *fakeStore) CreateCostSpikeEvent(_ context.Context, e *storetypes.CostSpikeEvent) error {
	f.created++
	cp := *e
	f.open = &cp
	f.all = append(f.all, &cp)
	return nil
}
func (f *fakeStore) UpdateCostSpikeEvent(_ context.Context, e *storetypes.CostSpikeEvent) error {
	f.updated++
	cp := *e
	if cp.EndedAt != nil {
		f.open = nil // closed
	} else {
		f.open = &cp
	}
	// Replace the most recent matching entry. Cheap.
	for i := len(f.all) - 1; i >= 0; i-- {
		if f.all[i].ID == e.ID {
			f.all[i] = &cp
			break
		}
	}
	return nil
}
func (f *fakeStore) LatestOpenCostSpike(_ context.Context) (*storetypes.CostSpikeEvent, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	if f.open == nil {
		return nil, nil
	}
	cp := *f.open
	return &cp, nil
}

// fakePricer returns a configurable monthly projection. The detector
// only consumes Enabled, ProjectFleet, and MonthlyForBytesPerHour;
// the rest is just there to satisfy the interface.
type fakePricer struct {
	enabled bool
	monthly float64
	signal  pricing.Signal
}

func (p *fakePricer) Enabled() bool { return p.enabled }
func (p *fakePricer) ProjectFleet(_ pricing.FleetInput) pricing.FleetProjection {
	out := pricing.FleetProjection{
		Enabled:    p.enabled,
		Currency:   "USD",
		MonthlyUSD: p.monthly,
		BySignal:   map[pricing.Signal]float64{},
	}
	if p.signal != "" {
		out.BySignal[p.signal] = p.monthly
	} else {
		out.BySignal["metrics"] = p.monthly
	}
	return out
}
func (p *fakePricer) MonthlyForBytesPerHour(_ int64, _ string) float64 { return 0 }

// fakeInsights returns canned FleetSummary + TopAgents/TopAttributes.
// The detector only needs FleetVolume to compute byte rates for the
// projector — for tests we hand the projector a stub so the byte
// math doesn't matter here.
type fakeInsights struct{}

func (f *fakeInsights) FleetVolume(_ context.Context, _ insights.Window, _ []insights.Signal) (*insights.FleetSummary, error) {
	return &insights.FleetSummary{
		Totals: insights.SignalVolume{Bytes: 1000},
		BySignal: []insights.SignalVolume{
			{Signal: "metrics", Bytes: 1000},
		},
	}, nil
}
func (f *fakeInsights) TopAgents(_ context.Context, _ insights.Window, _ int) ([]insights.AgentVolume, error) {
	return []insights.AgentVolume{
		{AgentID: "a1", AgentName: "edge-1", TotalBytes: 800},
		{AgentID: "a2", AgentName: "edge-2", TotalBytes: 200},
	}, nil
}
func (f *fakeInsights) TopAttributes(_ context.Context, _ insights.Window, _ insights.Signal, _ int) ([]insights.AttributeVolume, error) {
	return []insights.AttributeVolume{
		{Key: "http.user_agent", Bytes: 500},
		{Key: "k8s.pod.uid", Bytes: 300},
	}, nil
}

// warmUp pushes N steady-state samples so baseline() has enough
// history. Without this the detector silently skips evaluation.
func warmUp(d *Detector, val float64, n int) {
	for i := 0; i < n; i++ {
		d.recordSample(val)
	}
}

func TestDetectorFiresOnSpike(t *testing.T) {
	store := &fakeStore{}
	pricer := &fakePricer{enabled: true, monthly: 100}
	det := New(DefaultConfig(), store, pricer, &fakeInsights{})

	// Warm baseline at $100/mo with 30 samples (plenty above the
	// 5-sample minimum and the trim threshold).
	warmUp(det, 100, 30)

	// Bump the projection to $180/mo (80% above baseline → critical).
	pricer.monthly = 180
	if err := det.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if store.created != 1 {
		t.Fatalf("want 1 created event, got %d", store.created)
	}
	if store.open == nil {
		t.Fatal("expected an open spike")
	}
	if store.open.Severity != "critical" {
		t.Errorf("severity = %q, want critical", store.open.Severity)
	}
	if store.open.PeakMonthlyUSD != 180 {
		t.Errorf("peak = %v, want 180", store.open.PeakMonthlyUSD)
	}
	if store.open.AttributionJSON == "" {
		t.Error("expected attribution_json populated")
	}
}

func TestDetectorWarnThenCriticalEscalates(t *testing.T) {
	store := &fakeStore{}
	pricer := &fakePricer{enabled: true, monthly: 100}
	det := New(DefaultConfig(), store, pricer, &fakeInsights{})
	warmUp(det, 100, 30)

	// 30% above baseline → warn.
	pricer.monthly = 130
	_ = det.Tick(context.Background())
	if store.created != 1 || store.open.Severity != "warn" {
		t.Fatalf("first tick: created=%d severity=%v", store.created, store.open.Severity)
	}
	// Climb to 60% above → critical. Should UPDATE in place.
	pricer.monthly = 160
	_ = det.Tick(context.Background())
	if store.created != 1 {
		t.Errorf("created should still be 1, got %d", store.created)
	}
	if store.open.Severity != "critical" {
		t.Errorf("severity should escalate to critical, got %q", store.open.Severity)
	}
	if store.updated < 1 {
		t.Errorf("expected an update for escalation, got %d", store.updated)
	}
}

func TestDetectorClosesWhenRecovered(t *testing.T) {
	store := &fakeStore{}
	pricer := &fakePricer{enabled: true, monthly: 100}
	det := New(DefaultConfig(), store, pricer, &fakeInsights{})
	warmUp(det, 100, 30)

	// Fire.
	pricer.monthly = 180
	_ = det.Tick(context.Background())
	if store.open == nil {
		t.Fatal("expected open spike")
	}

	// Recover.
	pricer.monthly = 100
	_ = det.Tick(context.Background())
	if store.open != nil {
		t.Fatalf("expected spike to close, still open: %+v", store.open)
	}
	// The closed event should still be in the audit trail.
	if len(store.all) != 1 {
		t.Errorf("expected 1 event in trail, got %d", len(store.all))
	}
	if store.all[0].EndedAt == nil {
		t.Error("ended_at should be set on closed spike")
	}
}

func TestDetectorSkipsBelowMinBaseline(t *testing.T) {
	store := &fakeStore{}
	pricer := &fakePricer{enabled: true, monthly: 1}
	cfg := DefaultConfig()
	det := New(cfg, store, pricer, &fakeInsights{})
	warmUp(det, 1, 30)

	// Even a 1000% spike from $1 → $11 stays below the
	// MinBaselineUSD floor of $10. Should not fire.
	pricer.monthly = 11
	_ = det.Tick(context.Background())
	if store.created != 0 {
		t.Errorf("expected no events below min baseline, got %d", store.created)
	}
}

func TestDetectorDisabledPricerIsNoOp(t *testing.T) {
	store := &fakeStore{}
	pricer := &fakePricer{enabled: false, monthly: 1000}
	det := New(DefaultConfig(), store, pricer, &fakeInsights{})
	if err := det.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if store.created != 0 {
		t.Errorf("disabled pricer should not write, created=%d", store.created)
	}
}

// TestDetectorBaselineTrimsSingleOutlierOnSmallPool pins the small-history
// robustness fix. With a pool in the range the gate admits (samples>=5 →
// pool>=4), a literal 10% trim rounds to zero, so a single anomalous reading
// would fully inflate the baseline mean — exactly what the trim exists to
// prevent. The fix forces one off each end, so the single highest and lowest
// samples are excluded and the baseline stays at the steady value.
func TestDetectorBaselineTrimsSingleOutlierOnSmallPool(t *testing.T) {
	det := New(DefaultConfig(), &fakeStore{}, &fakePricer{enabled: true}, &fakeInsights{})
	// baseline() pools all but the most-recent sample. These six give a
	// 5-element pool {100,100,100,100,5000}: without the fix the mean is
	// (400+5000)/5 = 1080; with it, the low 100 and the 5000 blip are trimmed,
	// leaving {100,100,100} → 100.
	for _, v := range []float64{100, 100, 100, 100, 5000, 100} {
		det.recordSample(v)
	}
	got, ok := det.baseline()
	if !ok {
		t.Fatal("baseline should be available with 6 samples")
	}
	if got != 100 {
		t.Fatalf("baseline = %v, want 100 (single outlier must be trimmed on a small pool)", got)
	}
}

// Belt-and-suspenders: verify the ID generator returns distinct
// values across rapid back-to-back calls. The detector relies on
// this for uniqueness within a single second.
func TestNewSpikeIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := newSpikeID()
		if seen[id] {
			t.Fatalf("duplicate id at iteration %d: %s", i, id)
		}
		seen[id] = true
		time.Sleep(1 * time.Microsecond)
	}
}
