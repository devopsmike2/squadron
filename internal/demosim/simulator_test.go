package demosim

import (
	"context"
	"strings"
	"testing"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// newTestSim builds a simulator over an in-memory store with a small fleet and
// no telemetry writer (so Enable just seeds the fleet — the loop is skipped).
func newTestSim(t *testing.T, agents int) (*Simulator, *memory.Store) {
	t.Helper()
	store := memory.NewStore()
	sim := New(store, nil, nil, Options{AgentCount: agents})
	return sim, store
}

func TestEnable_SeedsFleetAcrossGroups(t *testing.T) {
	ctx := context.Background()
	sim, store := newTestSim(t, 200)

	n, err := sim.Enable(ctx)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if n < 150 || n > 200 {
		t.Fatalf("expected ~200 agents (weighted truncation), got %d", n)
	}

	// Every simulator group exists.
	for _, g := range simGroupDefs {
		got, err := store.GetGroup(ctx, g.id)
		if err != nil || got == nil {
			t.Fatalf("group %s not created: err=%v", g.id, err)
		}
	}

	// Every agent belongs to a demo-fleet group.
	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != n {
		t.Fatalf("ListAgents returned %d, Enable reported %d", len(agents), n)
	}
	for _, a := range agents {
		if a.GroupID == nil || !strings.HasPrefix(*a.GroupID, demosimGroupPrefix) {
			t.Fatalf("agent %s not in a demo-fleet group (group=%v)", a.Name, a.GroupID)
		}
	}
}

// TestGroupConfigHashMatchesServiceFormula is the load-bearing correctness
// check: the group config's stored ConfigHash must equal the sha256 the
// application service computes over its Content, so agents whose effective
// config equals the group config read as "synced" (not spuriously drifted).
func TestGroupConfigHashMatchesServiceFormula(t *testing.T) {
	ctx := context.Background()
	sim, store := newTestSim(t, 100)
	if _, err := sim.Enable(ctx); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	for _, g := range simGroupDefs {
		cfg, err := store.GetLatestConfigForGroup(ctx, g.id)
		if err != nil || cfg == nil {
			t.Fatalf("no config for group %s: err=%v", g.id, err)
		}
		want := hashConfig(cfg.Content)
		if cfg.ConfigHash != want {
			t.Fatalf("group %s config hash mismatch: stored=%s want=%s", g.id, cfg.ConfigHash, want)
		}
		if want == "" {
			t.Fatalf("group %s produced empty config hash", g.id)
		}
	}
}

// TestDriftSpread verifies the fleet carries a realistic mix: a synced majority
// plus a non-zero drifted minority (effective config differing from the group
// baseline).
func TestDriftSpread(t *testing.T) {
	ctx := context.Background()
	sim, store := newTestSim(t, 300)
	if _, err := sim.Enable(ctx); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	agents, _ := store.ListAgents(ctx)

	var synced, drifted int
	for _, a := range agents {
		role := a.Labels["role"]
		if a.EffectiveConfig == baselineConfigFor(role) {
			synced++
		} else {
			drifted++
		}
	}
	if drifted == 0 {
		t.Fatalf("expected some drifted agents, got 0 (synced=%d)", synced)
	}
	if synced <= drifted {
		t.Fatalf("expected synced majority, got synced=%d drifted=%d", synced, drifted)
	}
}

func TestEnableIsIdempotent(t *testing.T) {
	ctx := context.Background()
	sim, store := newTestSim(t, 120)

	n1, err := sim.Enable(ctx)
	if err != nil {
		t.Fatalf("first Enable: %v", err)
	}
	n2, err := sim.Enable(ctx)
	if err != nil {
		t.Fatalf("second Enable: %v", err)
	}
	if n1 != n2 {
		t.Fatalf("Enable not idempotent: first=%d second=%d", n1, n2)
	}
	agents, _ := store.ListAgents(ctx)
	if len(agents) != n1 {
		t.Fatalf("re-enable duplicated agents: store=%d expected=%d", len(agents), n1)
	}
}

func TestDisableTearsDownFleet(t *testing.T) {
	ctx := context.Background()
	sim, store := newTestSim(t, 80)
	if _, err := sim.Enable(ctx); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := sim.Disable(ctx); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	agents, _ := store.ListAgents(ctx)
	for _, a := range agents {
		if a.GroupID != nil && strings.HasPrefix(*a.GroupID, demosimGroupPrefix) {
			t.Fatalf("demo agent %s survived Disable", a.Name)
		}
	}
	for _, g := range simGroupDefs {
		got, _ := store.GetGroup(ctx, g.id)
		if got != nil {
			t.Fatalf("group %s survived Disable", g.id)
		}
	}
}
