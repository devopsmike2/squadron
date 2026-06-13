// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package silentagents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// fakeStore implements the Store interface with a controllable
// agent slice. The test mutates the slice between Tick() calls to
// simulate an agent going quiet and recovering.
type fakeStore struct {
	mu     sync.Mutex
	agents []*apptypes.Agent
}

func (f *fakeStore) setAgents(a []*apptypes.Agent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = a
}
func (f *fakeStore) ListAgents(_ context.Context) ([]*apptypes.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*apptypes.Agent, len(f.agents))
	copy(out, f.agents)
	return out, nil
}
func (f *fakeStore) ListExpectedAgents(_ context.Context, _ string) ([]*apptypes.ExpectedAgent, error) {
	return nil, nil
}

func TestWatcher_FiresOnHealthyToSilent(t *testing.T) {
	store := &fakeStore{}
	now := time.Now().UTC()
	agent := &apptypes.Agent{ID: uuid.New(), Name: "host01", LastSeen: now}
	store.setAgents([]*apptypes.Agent{agent})

	received := make(chan Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var evt Event
		_ = json.NewDecoder(r.Body).Decode(&evt)
		received <- evt
		w.WriteHeader(200)
	}))
	defer srv.Close()

	w := New(Config{
		SilenceThreshold: 1 * time.Second,
		PollInterval:     10 * time.Millisecond,
		WebhookURL:       srv.URL,
	}, store, zap.NewNop())

	// Tick 1: register healthy state — no event yet (no prior state
	// to transition from).
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	select {
	case e := <-received:
		t.Fatalf("unexpected event on first tick: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}

	// Now make the agent silent (last_seen far in the past).
	store.setAgents([]*apptypes.Agent{{
		ID:       agent.ID,
		Name:     "host01",
		LastSeen: now.Add(-1 * time.Hour),
	}})

	// Tick 2: should fire a "firing" event.
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	select {
	case e := <-received:
		if e.State != "firing" {
			t.Errorf("expected firing event, got state=%s", e.State)
		}
		if e.Hostname != "host01" {
			t.Errorf("expected hostname host01, got %s", e.Hostname)
		}
		if e.Kind != "silent_agent" {
			t.Errorf("expected kind silent_agent, got %s", e.Kind)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected firing event, none received")
	}

	// Now recover the agent.
	store.setAgents([]*apptypes.Agent{{
		ID: agent.ID, Name: "host01", LastSeen: time.Now().UTC(),
	}})

	// Tick 3: should fire a "resolved" event.
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick3: %v", err)
	}
	select {
	case e := <-received:
		if e.State != "resolved" {
			t.Errorf("expected resolved event, got state=%s", e.State)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected resolved event, none received")
	}
}

func TestWatcher_DoesNotFireOnInitiallySilent(t *testing.T) {
	// An agent that's already past the silence threshold at startup
	// must NOT fire on the first tick — otherwise a watcher restart
	// would generate a noisy burst of silent-agent events for every
	// agent that happened to be quiet.
	store := &fakeStore{}
	store.setAgents([]*apptypes.Agent{
		{ID: uuid.New(), Name: "host01", LastSeen: time.Now().Add(-1 * time.Hour)},
	})

	received := make(chan Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var evt Event
		_ = json.NewDecoder(r.Body).Decode(&evt)
		received <- evt
	}))
	defer srv.Close()

	w := New(Config{
		SilenceThreshold: 1 * time.Second,
		PollInterval:     10 * time.Millisecond,
		WebhookURL:       srv.URL,
	}, store, zap.NewNop())

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	select {
	case e := <-received:
		t.Fatalf("unexpected event for initially-silent agent: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatcher_NoTransitionNoEvent(t *testing.T) {
	store := &fakeStore{}
	store.setAgents([]*apptypes.Agent{
		{ID: uuid.New(), Name: "host01", LastSeen: time.Now()},
	})
	received := make(chan Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var evt Event
		_ = json.NewDecoder(r.Body).Decode(&evt)
		received <- evt
	}))
	defer srv.Close()
	w := New(Config{
		SilenceThreshold: 1 * time.Second,
		PollInterval:     10 * time.Millisecond,
		WebhookURL:       srv.URL,
	}, store, zap.NewNop())
	for i := 0; i < 3; i++ {
		if err := w.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	select {
	case e := <-received:
		t.Fatalf("unexpected event when no transition: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
}
