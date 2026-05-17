// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package recommendations

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/insights"
)

// fakeInsights is a tiny stand-in for *insights.Service. The
// engine only depends on the three methods below, so a struct of
// canned answers is enough — no DuckDB, no fixtures, no flake.
type fakeInsights struct {
	fleet    *insights.FleetSummary
	agents   []insights.AgentVolume
	attrs    map[insights.Signal][]insights.AttributeVolume
	attrsErr error
}

func (f *fakeInsights) FleetVolume(_ context.Context, win insights.Window, _ []insights.Signal) (*insights.FleetSummary, error) {
	if f.fleet == nil {
		return &insights.FleetSummary{Window: win}, nil
	}
	return f.fleet, nil
}

func (f *fakeInsights) TopAgents(_ context.Context, _ insights.Window, limit int) ([]insights.AgentVolume, error) {
	if len(f.agents) <= limit {
		return f.agents, nil
	}
	return f.agents[:limit], nil
}

func (f *fakeInsights) TopAttributes(_ context.Context, _ insights.Window, sig insights.Signal, _ int) ([]insights.AttributeVolume, error) {
	if f.attrsErr != nil {
		return nil, f.attrsErr
	}
	return f.attrs[sig], nil
}

// newEngine constructs the engine with a fake insights querier
// and no dismissals filtering — the default for most tests.
func newEngine(t *testing.T, fi *fakeInsights) *Engine {
	t.Helper()
	return NewEngine(fi, NoopDismissals(), nil, zap.NewNop())
}

// TestNoisyAttribute exercises the headline recipe. Any attribute
// crossing the 15% threshold should produce a Warn; >=30% should
// escalate to Critical. The snippet should mention the attribute
// key and the signal.
func TestNoisyAttribute(t *testing.T) {
	fi := &fakeInsights{
		fleet: &insights.FleetSummary{
			Window: insights.Window1h,
			BySignal: []insights.SignalVolume{
				{Signal: insights.SignalMetrics, Bytes: 1_000_000, ItemCount: 50_000},
			},
		},
		attrs: map[insights.Signal][]insights.AttributeVolume{
			insights.SignalMetrics: {
				{Key: "http.url", PctOfSignal: 0.42, Bytes: 420_000, Estimated: true},
				{Key: "service.name", PctOfSignal: 0.20, Bytes: 200_000, Estimated: true},
				{Key: "k8s.pod.uid", PctOfSignal: 0.05, Bytes: 50_000, Estimated: true}, // below threshold
			},
		},
	}
	recs, err := newEngine(t, fi).Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	gotByKey := map[string]Recommendation{}
	for _, r := range recs {
		if r.Category == CategoryNoisyAttribute {
			// The Title is "Drop attribute %q from %s" — extract.
			gotByKey[extractAttributeKey(r.Title)] = r
		}
	}
	if _, ok := gotByKey["http.url"]; !ok {
		t.Fatalf("expected http.url recommendation, got: %v", titles(recs))
	}
	if _, ok := gotByKey["service.name"]; !ok {
		t.Fatalf("expected service.name recommendation, got: %v", titles(recs))
	}
	if _, ok := gotByKey["k8s.pod.uid"]; ok {
		t.Errorf("k8s.pod.uid was below threshold but surfaced anyway")
	}

	// Severity escalation: http.url at 42% → critical, service.name at 20% → warn.
	if gotByKey["http.url"].Severity != SeverityCritical {
		t.Errorf("http.url should be critical, got %s", gotByKey["http.url"].Severity)
	}
	if gotByKey["service.name"].Severity != SeverityWarn {
		t.Errorf("service.name should be warn, got %s", gotByKey["service.name"].Severity)
	}

	// Snippet should mention both the key and the signal name.
	snippet := gotByKey["http.url"].Snippet
	if !strings.Contains(snippet, "http.url") {
		t.Errorf("snippet missing attribute key: %s", snippet)
	}
	if !strings.Contains(snippet, "metrics") {
		t.Errorf("snippet missing signal name: %s", snippet)
	}
	if !strings.Contains(snippet, "attributes/drop_") {
		t.Errorf("snippet missing processor block: %s", snippet)
	}

	// Estimated savings should be the byte share of the signal total,
	// not blindly trusting the sampled AttributeVolume.Bytes (which
	// in real life may have been extrapolated separately).
	if got := gotByKey["http.url"].EstSavingsBytes; got != 420_000 {
		t.Errorf("http.url savings: got %d, want 420000", got)
	}
}

// TestOutlierAgent confirms the 2x-median rule fires for the
// loudest agent and that small fleets are skipped.
func TestOutlierAgent(t *testing.T) {
	// 5 agents, one shouting 10× the median.
	fi := &fakeInsights{
		fleet: &insights.FleetSummary{
			Window: insights.Window1h,
			BySignal: []insights.SignalVolume{
				{Signal: insights.SignalMetrics, Bytes: 1_000_000},
			},
		},
		agents: []insights.AgentVolume{
			{AgentID: "agent-loud", TotalBytes: 1_000_000},
			{AgentID: "agent-quiet1", TotalBytes: 100_000},
			{AgentID: "agent-quiet2", TotalBytes: 110_000},
			{AgentID: "agent-quiet3", TotalBytes: 90_000},
			{AgentID: "agent-quiet4", TotalBytes: 100_000},
		},
	}
	recs, err := newEngine(t, fi).Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	outliers := filterByCategory(recs, CategoryOutlierAgent)
	if len(outliers) != 1 {
		t.Fatalf("expected exactly 1 outlier, got %d: %v", len(outliers), titles(outliers))
	}
	if outliers[0].AgentID != "agent-loud" {
		t.Errorf("wrong outlier: got %s", outliers[0].AgentID)
	}
	if outliers[0].Severity != SeverityCritical {
		t.Errorf("10× outlier should be critical, got %s", outliers[0].Severity)
	}
}

func TestOutlierAgent_TinyFleetSkipped(t *testing.T) {
	// Below the 4-agent floor — recipe should produce nothing.
	fi := &fakeInsights{
		fleet: &insights.FleetSummary{},
		agents: []insights.AgentVolume{
			{AgentID: "a1", TotalBytes: 100},
			{AgentID: "a2", TotalBytes: 100_000_000}, // would obviously be flagged in a bigger fleet
		},
	}
	recs, err := newEngine(t, fi).Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	if outliers := filterByCategory(recs, CategoryOutlierAgent); len(outliers) != 0 {
		t.Errorf("tiny fleet should skip outlier detection, got %v", titles(outliers))
	}
}

// TestDropHotspot verifies the drop-rate recipe and its severity
// escalation at the 5% threshold.
func TestDropHotspot(t *testing.T) {
	fi := &fakeInsights{
		fleet: &insights.FleetSummary{
			BySignal: []insights.SignalVolume{
				// 3% drop rate → warn
				{Signal: insights.SignalLogs, Bytes: 1000, ItemCount: 970, DroppedCount: 30},
				// 10% drop rate → critical
				{Signal: insights.SignalTraces, Bytes: 1000, ItemCount: 900, DroppedCount: 100},
				// No drops → skipped
				{Signal: insights.SignalMetrics, Bytes: 1000, ItemCount: 1000, DroppedCount: 0},
			},
		},
	}
	recs, err := newEngine(t, fi).Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	drops := filterByCategory(recs, CategoryDropHotspot)
	if len(drops) != 2 {
		t.Fatalf("expected 2 drop hotspots, got %d: %v", len(drops), titles(drops))
	}
	bySignal := map[insights.Signal]Recommendation{}
	for _, r := range drops {
		bySignal[r.Signal] = r
	}
	if bySignal[insights.SignalLogs].Severity != SeverityWarn {
		t.Errorf("logs 3%% should be warn, got %s", bySignal[insights.SignalLogs].Severity)
	}
	if bySignal[insights.SignalTraces].Severity != SeverityCritical {
		t.Errorf("traces 10%% should be critical, got %s", bySignal[insights.SignalTraces].Severity)
	}
	// Drops are reliability advice, not byte-savings advice.
	if bySignal[insights.SignalTraces].EstSavingsBytes != -1 {
		t.Errorf("drop hotspot savings should be -1, got %d", bySignal[insights.SignalTraces].EstSavingsBytes)
	}
}

// TestEmptySignal: any agent with non-trivial volume that's missing
// a signal others are emitting should be flagged.
func TestEmptySignal(t *testing.T) {
	fi := &fakeInsights{
		fleet: &insights.FleetSummary{
			AgentCount: 3,
			BySignal: []insights.SignalVolume{
				{Signal: insights.SignalMetrics, Bytes: 1_000_000},
				{Signal: insights.SignalLogs, Bytes: 500_000},
			},
		},
		agents: []insights.AgentVolume{
			{
				AgentID: "metrics-only", TotalBytes: 100_000,
				BySignal: []insights.SignalVolume{{Signal: insights.SignalMetrics, Bytes: 100_000}},
			},
			{
				AgentID: "both", TotalBytes: 200_000,
				BySignal: []insights.SignalVolume{
					{Signal: insights.SignalMetrics, Bytes: 150_000},
					{Signal: insights.SignalLogs, Bytes: 50_000},
				},
			},
			{
				AgentID: "tiny-metrics-only", TotalBytes: 500, // below 10k floor
				BySignal: []insights.SignalVolume{{Signal: insights.SignalMetrics, Bytes: 500}},
			},
		},
	}
	recs, err := newEngine(t, fi).Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	empties := filterByCategory(recs, CategoryEmptySignal)
	if len(empties) != 1 {
		t.Fatalf("expected 1 empty-signal rec (metrics-only missing logs), got %d: %v",
			len(empties), titles(empties))
	}
	if empties[0].AgentID != "metrics-only" {
		t.Errorf("wrong empty-signal agent: %s", empties[0].AgentID)
	}
	if empties[0].Signal != insights.SignalLogs {
		t.Errorf("wrong missing signal: %s", empties[0].Signal)
	}
}

// TestDismissals: a recommendation present in one run should be
// suppressed on the next when the dismissals store says so.
func TestDismissals(t *testing.T) {
	fi := &fakeInsights{
		fleet: &insights.FleetSummary{
			BySignal: []insights.SignalVolume{{Signal: insights.SignalMetrics, Bytes: 1_000_000}},
		},
		attrs: map[insights.Signal][]insights.AttributeVolume{
			insights.SignalMetrics: {{Key: "http.url", PctOfSignal: 0.42}},
		},
	}
	dismissed := map[string]bool{}
	dismissStore := fakeDismissals{m: dismissed}

	eng := NewEngine(fi, dismissStore, nil, zap.NewNop())
	recs, err := eng.Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		t.Fatal("expected at least 1 recommendation before dismissal")
	}
	target := recs[0].ID

	// Dismiss it and re-evaluate — but bypass the cache, which would
	// otherwise return the pre-dismissal snapshot.
	dismissed[target] = true
	eng.mu.Lock()
	eng.cache = nil
	eng.mu.Unlock()

	recs2, err := eng.Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs2 {
		if r.ID == target {
			t.Errorf("dismissed recommendation %s still present", target)
		}
	}
}

// TestStableIDs: the same inputs across two engines should produce
// the same recommendation IDs. This is the property dismissals rely
// on, so it gets its own test.
func TestStableIDs(t *testing.T) {
	mk := func() *Engine {
		fi := &fakeInsights{
			fleet: &insights.FleetSummary{
				BySignal: []insights.SignalVolume{{Signal: insights.SignalMetrics, Bytes: 1_000_000}},
			},
			attrs: map[insights.Signal][]insights.AttributeVolume{
				insights.SignalMetrics: {{Key: "http.url", PctOfSignal: 0.42}},
			},
		}
		return newEngine(t, fi)
	}
	r1, err := mk().Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mk().Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1) == 0 || len(r2) == 0 || r1[0].ID != r2[0].ID {
		t.Errorf("expected stable IDs across runs; got %q vs %q",
			firstID(r1), firstID(r2))
	}
}

// TestSorting: severity wins, then estimated savings.
func TestSorting(t *testing.T) {
	fi := &fakeInsights{
		fleet: &insights.FleetSummary{
			BySignal: []insights.SignalVolume{
				{Signal: insights.SignalMetrics, Bytes: 1_000_000, ItemCount: 100_000, DroppedCount: 8000}, // critical drop (8%)
			},
		},
		attrs: map[insights.Signal][]insights.AttributeVolume{
			insights.SignalMetrics: {
				{Key: "small-noisy", PctOfSignal: 0.18, Bytes: 180_000}, // warn
				{Key: "big-critical", PctOfSignal: 0.40, Bytes: 400_000}, // critical
			},
		},
	}
	recs, err := newEngine(t, fi).Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	// First two should both be critical, ordered by savings desc.
	// drop hotspot has est=-1 so it's ranked last among criticals;
	// big-critical (400k) should come first.
	if len(recs) < 2 {
		t.Fatalf("not enough recs: %v", titles(recs))
	}
	if recs[0].Severity != SeverityCritical {
		t.Errorf("first rec not critical: %s", recs[0].Severity)
	}
	if recs[0].Category != CategoryNoisyAttribute || !strings.Contains(recs[0].Title, "big-critical") {
		t.Errorf("expected big-critical noisy-attribute first; got %s/%s",
			recs[0].Category, recs[0].Title)
	}
}

// TestAccumulateNoErrorOnEmptySignal: zero-byte signals shouldn't
// trigger a TopAttributes call that might 400 the API.
func TestNoQueryOnZeroByteSignal(t *testing.T) {
	called := false
	fi := &fakeInsights{
		fleet: &insights.FleetSummary{
			BySignal: []insights.SignalVolume{
				{Signal: insights.SignalMetrics, Bytes: 0},
			},
		},
		attrs: map[insights.Signal][]insights.AttributeVolume{},
	}
	// Wrap to track calls
	eng := NewEngine(&callTracker{inner: fi, called: &called}, NoopDismissals(), nil, zap.NewNop())
	_, err := eng.Evaluate(context.Background(), insights.Window1h)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Errorf("TopAttributes called for zero-byte signal")
	}
}

// ----------------------------------------------------------------
// Test helpers
// ----------------------------------------------------------------

type fakeDismissals struct{ m map[string]bool }

func (f fakeDismissals) IsDismissed(_ context.Context, id string) (bool, error) {
	return f.m[id], nil
}

type callTracker struct {
	inner  InsightsQuerier
	called *bool
}

func (c *callTracker) FleetVolume(ctx context.Context, w insights.Window, s []insights.Signal) (*insights.FleetSummary, error) {
	return c.inner.FleetVolume(ctx, w, s)
}
func (c *callTracker) TopAgents(ctx context.Context, w insights.Window, l int) ([]insights.AgentVolume, error) {
	return c.inner.TopAgents(ctx, w, l)
}
func (c *callTracker) TopAttributes(ctx context.Context, w insights.Window, sig insights.Signal, l int) ([]insights.AttributeVolume, error) {
	*c.called = true
	return c.inner.TopAttributes(ctx, w, sig, l)
}

func filterByCategory(recs []Recommendation, cat Category) []Recommendation {
	out := make([]Recommendation, 0, len(recs))
	for _, r := range recs {
		if r.Category == cat {
			out = append(out, r)
		}
	}
	return out
}

func titles(recs []Recommendation) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = fmt.Sprintf("[%s/%s] %s", r.Severity, r.Category, r.Title)
	}
	return out
}

func firstID(recs []Recommendation) string {
	if len(recs) == 0 {
		return "(empty)"
	}
	return recs[0].ID
}

// extractAttributeKey pulls the quoted attribute key out of a
// "Drop attribute %q from %s" title. Cheap regex-free parse.
func extractAttributeKey(title string) string {
	start := strings.Index(title, `"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(title[start+1:], `"`)
	if end < 0 {
		return ""
	}
	return title[start+1 : start+1+end]
}
