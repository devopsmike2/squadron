// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeStore counts CreateAgent + UpdateLastSeen calls.
type fakeStore struct {
	mu        sync.Mutex
	agents    map[uuid.UUID]*apptypes.Agent
	created   int
	bumped    int
	createErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{agents: map[uuid.UUID]*apptypes.Agent{}}
}
func (f *fakeStore) GetAgent(_ context.Context, id uuid.UUID) (*apptypes.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[id], nil
}
func (f *fakeStore) CreateAgent(_ context.Context, a *apptypes.Agent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	if _, ok := f.agents[a.ID]; ok {
		return nil // simulate "row already exists" race outcome silently
	}
	f.agents[a.ID] = a
	f.created++
	return nil
}
func (f *fakeStore) UpdateAgentLastSeen(_ context.Context, id uuid.UUID, t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[id]; ok {
		a.LastSeen = t
		f.bumped++
	}
	return nil
}

func TestRegisterIfUnknown_CreatesNewAgent(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, time.Hour, zap.NewNop())
	agentID := uuid.NewString()

	svc.RegisterIfUnknown(context.Background(), Observation{
		AgentID:     agentID,
		Hostname:    "GAXGPAP158UA",
		ServiceName: "otelcol-contrib",
		Version:     "v0.105.0",
	})

	if store.created != 1 {
		t.Fatalf("expected 1 create, got %d", store.created)
	}
	parsed, _ := uuid.Parse(agentID)
	got := store.agents[parsed]
	if got == nil {
		t.Fatal("agent was not stored")
	}
	if got.Name != "GAXGPAP158UA" {
		t.Errorf("name = %q, want hostname", got.Name)
	}
	if got.DiscoverySource != "otlp" {
		t.Errorf("DiscoverySource = %q, want otlp", got.DiscoverySource)
	}
	if got.Status != apptypes.AgentStatusOnline {
		t.Errorf("Status = %q, want online", got.Status)
	}
}

func TestRegisterIfUnknown_DedupesWithinWindow(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, time.Hour, zap.NewNop())
	agentID := uuid.NewString()
	obs := Observation{AgentID: agentID, Hostname: "host01"}

	// First call creates.
	svc.RegisterIfUnknown(context.Background(), obs)
	if store.created != 1 {
		t.Fatalf("first call: expected 1 create, got %d", store.created)
	}

	// Subsequent calls within the window should short-circuit
	// BEFORE hitting the store at all.
	for i := 0; i < 10; i++ {
		svc.RegisterIfUnknown(context.Background(), obs)
	}
	if store.created != 1 {
		t.Errorf("expected dedup, but created = %d", store.created)
	}
	if store.bumped != 0 {
		t.Errorf("dedup window should also short-circuit bumps; bumped = %d", store.bumped)
	}
}

func TestRegisterIfUnknown_BumpsExistingAgent(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, time.Hour, zap.NewNop())
	agentID := uuid.NewString()
	parsed, _ := uuid.Parse(agentID)
	// Pre-seed: an OpAMP-managed agent that ALSO sends OTLP.
	existing := &apptypes.Agent{
		ID:              parsed,
		Name:            "managed-host",
		Status:          apptypes.AgentStatusOnline,
		LastSeen:        time.Now().Add(-time.Hour),
		DiscoverySource: "opamp",
	}
	store.agents[parsed] = existing

	svc.RegisterIfUnknown(context.Background(), Observation{
		AgentID:  agentID,
		Hostname: "managed-host",
	})

	if store.created != 0 {
		t.Errorf("should not create — agent exists. created = %d", store.created)
	}
	if store.bumped != 1 {
		t.Errorf("should bump last_seen for existing agent; bumped = %d", store.bumped)
	}
	// Discovery source preserved (opamp, not overridden to otlp).
	if existing.DiscoverySource != "opamp" {
		t.Errorf("DiscoverySource overwritten: %q", existing.DiscoverySource)
	}
}

func TestRegisterIfUnknown_NonUUIDIDSkippedSilently(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, time.Hour, zap.NewNop())
	svc.RegisterIfUnknown(context.Background(), Observation{
		AgentID:  "not-a-uuid",
		Hostname: "weird-host",
	})
	if store.created != 0 {
		t.Errorf("expected non-UUID to be skipped, but created = %d", store.created)
	}
}

func TestRegisterIfUnknown_ConcurrentSafe(t *testing.T) {
	// 50 goroutines hammer the same agent ID. Net effect should be
	// exactly one create.
	store := newFakeStore()
	svc := NewService(store, time.Hour, zap.NewNop())
	agentID := uuid.NewString()
	obs := Observation{AgentID: agentID, Hostname: "concurrent-host"}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.RegisterIfUnknown(context.Background(), obs)
		}()
	}
	wg.Wait()
	if store.created != 1 {
		t.Errorf("expected exactly 1 create from 50 concurrent calls, got %d", store.created)
	}
}

func TestRegisterIfUnknown_FallbackName(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, time.Hour, zap.NewNop())
	agentID := uuid.NewString()

	// No hostname, only service name.
	svc.RegisterIfUnknown(context.Background(), Observation{
		AgentID:     agentID,
		ServiceName: "otelcol-contrib",
	})
	parsed, _ := uuid.Parse(agentID)
	if store.agents[parsed].Name != "otelcol-contrib" {
		t.Errorf("name fallback: %q", store.agents[parsed].Name)
	}
}
