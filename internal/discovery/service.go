// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package discovery is the v0.36+ passive-OTLP-discovery surface.
//
// When a collector sends telemetry to Squadron's OTLP receiver but
// never opened an OpAMP connection, Squadron used to drop the
// data's agent identity on the floor — the metrics/traces/logs
// landed in DuckDB carrying an agent_id, but there was no row in
// the agents application-store to surface that agent in the UI.
//
// This package fills the gap. The worker pool calls
// RegisterIfUnknown for each unique agent_id seen in a parsed OTLP
// batch. If we haven't seen the ID recently (LRU dedup), we
// upsert a placeholder agent record with DiscoverySource="otlp"
// so the operator can see it in the agents list.
//
// Telemetry-only agents are observable but NOT manageable: there's
// no OpAMP socket to push config to. The UI surfaces this
// distinction (different badge color, no "Restart" affordance) so
// operators know the next step is "bring this under management"
// rather than "push a config update."
package discovery

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	apptypes "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// DefaultDedupWindow throttles re-upserts. We want to register a
// newly-discovered agent immediately, but we don't want every OTLP
// batch (collectors send every 10s by default) to take a write
// lock on the agents table. 5 minutes is plenty — last_seen is
// updated by the regular OpAMP path for managed agents, and for
// telemetry-only agents the worker pool calls UpdateLastSeen on
// every batch independently of this dedup window.
const DefaultDedupWindow = 5 * time.Minute

// Store is the application-store subset the discovery service
// needs. Declared as a local interface for fake-based tests.
type Store interface {
	GetAgent(ctx context.Context, id uuid.UUID) (*apptypes.Agent, error)
	CreateAgent(ctx context.Context, agent *apptypes.Agent) error
	UpdateAgentLastSeen(ctx context.Context, id uuid.UUID, lastSeen time.Time) error
}

// Observation is one piece of evidence the worker pool collected
// while parsing an OTLP batch. We attempt to populate as many
// fields as the resource attributes provide. Anything missing
// falls back to safe defaults at upsert time.
type Observation struct {
	AgentID     string            // service.instance.id from the OTLP resource
	Hostname    string            // host.name resource attribute, or empty
	ServiceName string            // service.name, or empty
	Version     string            // service.version, or empty
	OS          string            // os.type, or empty
	Labels      map[string]string // any additional resource attributes worth keeping
}

// Service is the public surface. Construct once, share across the
// worker pool's goroutines. All methods are safe for concurrent use.
type Service struct {
	store     Store
	logger    *zap.Logger
	dedup     time.Duration

	mu       sync.Mutex
	lastSeen map[string]time.Time // dedup window cache
}

// NewService constructs a discovery service. Pass DefaultDedupWindow
// for the second argument unless you have a specific reason to
// tune; callers typically don't.
func NewService(store Store, dedup time.Duration, logger *zap.Logger) *Service {
	if dedup <= 0 {
		dedup = DefaultDedupWindow
	}
	return &Service{
		store:    store,
		logger:   logger,
		dedup:    dedup,
		lastSeen: map[string]time.Time{},
	}
}

// RegisterIfUnknown is the hot-path entry. The worker pool calls
// this once per (agent_id, batch) it processes. We short-circuit on
// the dedup window for ids we've seen recently; otherwise we check
// the store and either upsert a placeholder agent (new
// telemetry-only discovery) or refresh last_seen (existing agent).
//
// Never blocks the caller meaningfully: store errors are logged
// and swallowed because this is a discovery best-effort, not a
// hard prerequisite of telemetry ingestion.
func (s *Service) RegisterIfUnknown(ctx context.Context, obs Observation) {
	if obs.AgentID == "" {
		return
	}
	parsed, err := uuid.Parse(obs.AgentID)
	if err != nil {
		// Some collectors set service.instance.id to a non-UUID
		// string. Skip silently — the agents table requires UUIDs.
		return
	}
	now := time.Now().UTC()

	// Fast-path dedup. The lock here is uncontended in steady
	// state — only the rare cache-miss takes the slow path below.
	s.mu.Lock()
	if last, ok := s.lastSeen[obs.AgentID]; ok && now.Sub(last) < s.dedup {
		s.mu.Unlock()
		return
	}
	s.lastSeen[obs.AgentID] = now
	s.mu.Unlock()

	// Slow path: check the store. The dedup window above bounds
	// how often we take this path — at default settings, once per
	// agent per 5 minutes.
	existing, err := s.store.GetAgent(ctx, parsed)
	if err != nil {
		s.logger.Warn("discovery: get agent failed (non-fatal)",
			zap.String("agent_id", obs.AgentID), zap.Error(err))
		return
	}
	if existing != nil {
		// Agent already known. Just bump last_seen so OpAMP-managed
		// agents that ALSO send via OTLP get accurate freshness.
		// (For pure telemetry-only agents this is the only path
		// that updates last_seen at all.)
		if err := s.store.UpdateAgentLastSeen(ctx, parsed, now); err != nil {
			s.logger.Debug("discovery: bump last_seen failed",
				zap.String("agent_id", obs.AgentID), zap.Error(err))
		}
		return
	}

	// Brand-new agent. Create with discovery_source="otlp" and
	// status="online" — we've literally just received telemetry
	// from it, so it's clearly running.
	name := obs.Hostname
	if name == "" {
		name = obs.ServiceName
	}
	if name == "" {
		name = obs.AgentID
	}
	agent := &apptypes.Agent{
		ID:              parsed,
		Name:            name,
		Labels:          obs.Labels,
		Status:          apptypes.AgentStatusOnline,
		LastSeen:        now,
		Version:         obs.Version,
		DiscoverySource: "otlp",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.store.CreateAgent(ctx, agent); err != nil {
		// A race with a concurrent CreateAgent (same UUID, both
		// going through GetAgent → CreateAgent at the same moment)
		// is the most likely cause — both threads see no existing
		// row, both try to insert, one loses on the PK. Treat that
		// as benign; the other path won.
		s.logger.Debug("discovery: create agent failed (likely a race)",
			zap.String("agent_id", obs.AgentID), zap.Error(err))
		return
	}
	s.logger.Info("discovered telemetry-only agent",
		zap.String("agent_id", obs.AgentID),
		zap.String("name", name),
		zap.String("hostname", obs.Hostname))
}

// ResetDedup clears the dedup cache. Exposed for tests; production
// callers don't need this.
func (s *Service) ResetDedup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeen = map[string]time.Time{}
}
