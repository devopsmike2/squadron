// Package demosim runs an in-process "live simulated production" environment
// for the Squadron demo. Unlike internal/demoseed (which upserts a handful of
// static rows), demosim stands up a realistic, continuously-updating fleet:
//
//   - Phase F: ~500 synthetic agents across several groups, with a realistic
//     spread of versions, online/offline status, and config drift. Each agent
//     is a real row in the application store, so the Fleet, Agents, Fleet Map,
//     and Groups surfaces populate exactly as they would for a real fleet.
//
//   - Phase T: a low-rate background loop that writes real OTLP metrics, logs,
//     and traces (plus per-batch volume accounting) into the telemetry store,
//     keyed to those same agents. This lights up the per-agent Logs / Metrics /
//     Traces tabs, Cost Insights, Savings, Data Flow, and the SquadronQL
//     explorer — all reading the same DuckDB rows a production ingest would
//     produce. The trace stream carries a deliberately noisy, high-cardinality
//     attribute so the cost-attribution and recommendation engines derive a
//     real "drop this attribute" finding.
//
// Everything is demo-scoped by reserved group ids (demosimGroupPrefix), so
// Disable can tear the fleet down without touching a user's real data. The
// simulator is owned by main.go (which holds both the application store and the
// telemetry writer) and driven by the one-click POST /api/v1/demo/enable
// endpoint. It is safe to Enable twice (idempotent) and safe to run with a nil
// telemetry writer (the fleet still seeds; only the telemetry loop is skipped).
package demosim

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	appstore "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	tstypes "github.com/devopsmike2/squadron/internal/storage/telemetrystore/types"
)

// demosimNamespace anchors deterministic per-agent UUIDs so re-enabling the
// demo reuses the same agent ids (idempotent) and the telemetry the loop emits
// keys to the same agent rows. Frozen — never change it.
var demosimNamespace = uuid.MustParse("b7d3f1a2-9c84-4e6f-a1b2-3c4d5e6f7081")

const (
	// demosimGroupPrefix scopes every group this simulator creates so Disable
	// can find and remove them without touching real or demoseed rows.
	demosimGroupPrefix = "demo-fleet-"
	// demosimLabel marks agents this simulator owns.
	demosimLabel = "demosim"
)

// group defines one demo fleet segment.
type simGroupDef struct {
	id       string
	name     string
	role     string
	weight   int    // relative share of the fleet
	version  string // dominant collector version for this segment
	exporter string // where this segment ships telemetry (for Data Flow)
}

// simGroupDefs is the fleet topology: a handful of believable production
// segments. Weights are relative; the simulator distributes agentCount across
// them proportionally.
var simGroupDefs = []simGroupDef{
	{demosimGroupPrefix + "web", "Web Tier (prod)", "web", 30, "0.119.0", "https://otel.datadoghq.com"},
	{demosimGroupPrefix + "api", "API Tier (prod)", "api", 25, "0.119.0", "https://api.honeycomb.io"},
	{demosimGroupPrefix + "workers", "Workers (prod)", "worker", 20, "0.111.0", "https://otel.datadoghq.com"},
	{demosimGroupPrefix + "data", "Data Plane (prod)", "data", 15, "0.119.0", "https://otlp-gateway.grafana.net"},
	{demosimGroupPrefix + "edge", "Edge (prod)", "edge", 10, "0.106.0", "https://api.honeycomb.io"},
}

// simAgent is the simulator's in-memory handle on one seeded agent, used by the
// telemetry loop to key the OTLP rows it writes.
type simAgent struct {
	id        uuid.UUID
	name      string
	groupID   string
	groupName string
	role      string
	exporter  string
	online    bool
}

// Simulator owns the demo fleet and its telemetry loop.
type Simulator struct {
	store  appstore.ApplicationStore
	writer tstypes.Writer // may be nil (no telemetry backend) — loop is skipped
	logger *zap.Logger

	agentCount int
	tickEvery  time.Duration
	perTick    int

	mu      sync.Mutex
	running bool
	seeded  bool
	cancel  context.CancelFunc
	agents  []simAgent
	cursor  int
}

// Options tunes the simulator. Zero values fall back to sensible defaults.
type Options struct {
	AgentCount int           // default 500
	TickEvery  time.Duration // default 8s
	PerTick    int           // agents to emit telemetry for each tick; default 60
}

// New constructs a simulator. writer may be nil.
func New(store appstore.ApplicationStore, writer tstypes.Writer, logger *zap.Logger, opts Options) *Simulator {
	if logger == nil {
		logger = zap.NewNop()
	}
	if opts.AgentCount <= 0 {
		opts.AgentCount = 500
	}
	if opts.TickEvery <= 0 {
		opts.TickEvery = 8 * time.Second
	}
	if opts.PerTick <= 0 {
		opts.PerTick = 60
	}
	return &Simulator{
		store:      store,
		writer:     writer,
		logger:     logger,
		agentCount: opts.AgentCount,
		tickEvery:  opts.TickEvery,
		perTick:    opts.PerTick,
	}
}

// Enable seeds the fleet (once) and starts the background telemetry loop.
// Idempotent: a second call while running is a no-op. Returns after seeding so
// the caller's HTTP response reflects a provisioned fleet; the telemetry loop
// runs on its own long-lived context (not the request context).
func (s *Simulator) Enable(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.store == nil {
		return 0, fmt.Errorf("demosim: application store is nil")
	}

	if !s.seeded {
		agents, err := s.seedFleet(ctx)
		if err != nil {
			return 0, err
		}
		s.agents = agents
		s.seeded = true
	}

	if s.running {
		return len(s.agents), nil
	}

	if s.writer != nil {
		loopCtx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		s.running = true
		go s.runTelemetry(loopCtx)
	}

	s.logger.Info("demosim: demo environment enabled",
		zap.Int("agents", len(s.agents)),
		zap.Bool("telemetry_loop", s.writer != nil),
	)
	return len(s.agents), nil
}

// Disable stops the telemetry loop and removes the seeded fleet. Telemetry rows
// already written are left to the telemetry store's retention GC. Best-effort:
// a missing row is not an error.
func (s *Simulator) Disable(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.running = false

	// Remove agents, then groups.
	for _, a := range s.agents {
		if err := s.store.DeleteAgent(ctx, a.id); err != nil {
			s.logger.Warn("demosim: delete agent failed", zap.String("agent", a.name), zap.Error(err))
		}
	}
	for _, g := range simGroupDefs {
		if err := s.store.DeleteGroup(ctx, g.id); err != nil {
			s.logger.Warn("demosim: delete group failed", zap.String("group", g.id), zap.Error(err))
		}
	}
	s.agents = nil
	s.seeded = false
	s.logger.Info("demosim: demo environment disabled")
	return nil
}

// Stop cancels the telemetry loop without tearing down the fleet. For server
// shutdown.
func (s *Simulator) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.running = false
}

// seedFleet creates the groups (each with a correctly-hashed baseline config so
// drift is controllable) and distributes agentCount agents across them with a
// realistic status/version/drift spread.
func (s *Simulator) seedFleet(ctx context.Context) ([]simAgent, error) {
	now := time.Now().UTC()
	rng := rand.New(rand.NewSource(42)) // deterministic spread

	// 1. Groups + their baseline configs.
	totalWeight := 0
	for _, g := range simGroupDefs {
		totalWeight += g.weight
	}
	for _, g := range simGroupDefs {
		if err := s.ensureGroup(ctx, g, now); err != nil {
			return nil, err
		}
		if err := s.ensureGroupConfig(ctx, g, now); err != nil {
			return nil, err
		}
	}

	// 2. Agents. Distribute across groups by weight.
	agents := make([]simAgent, 0, s.agentCount)
	idx := 0
	for _, g := range simGroupDefs {
		share := s.agentCount * g.weight / totalWeight
		baseline := baselineConfigFor(g.role)
		driftedCfg := baseline + "\n# local override: sampler.rate lowered by on-call\n"
		for i := 0; i < share; i++ {
			idx++
			name := fmt.Sprintf("%s-%03d", g.role, i+1)
			id := uuid.NewSHA1(demosimNamespace, []byte(name+"@"+g.id))

			// Status spread: ~5% offline, ~1% error, rest online.
			status := appstore.AgentStatusOnline
			online := true
			roll := rng.Float64()
			if roll < 0.05 {
				status = appstore.AgentStatusOffline
				online = false
			} else if roll < 0.06 {
				status = appstore.AgentStatusError
			}

			// Drift spread: ~12% drifted (effective differs from group config).
			effective := baseline
			if rng.Float64() < 0.12 {
				effective = driftedCfg
			}

			// Version spread: mostly the segment version, some laggards.
			version := g.version
			if rng.Float64() < 0.15 {
				version = "0.106.0"
			}

			gid := g.id
			gname := g.name
			lastSeen := now
			if !online {
				lastSeen = now.Add(-time.Duration(30+rng.Intn(240)) * time.Minute)
			}

			if err := s.store.CreateAgent(ctx, &appstore.Agent{
				ID:              id,
				Name:            name,
				Labels:          map[string]string{"role": g.role, "fleet": demosimLabel, "env": "prod"},
				Status:          status,
				LastSeen:        lastSeen,
				GroupID:         &gid,
				GroupName:       &gname,
				Version:         version,
				Capabilities:    []string{"AcceptsRemoteConfig", "ReportsEffectiveConfig"},
				EffectiveConfig: effective,
				DiscoverySource: "opamp",
				CreatedAt:       now.Add(-time.Duration(rng.Intn(30)) * 24 * time.Hour),
				UpdatedAt:       now,
			}); err != nil {
				// Idempotent re-enable: an existing agent id is fine.
				s.logger.Debug("demosim: create agent skipped", zap.String("agent", name), zap.Error(err))
			}

			agents = append(agents, simAgent{
				id:        id,
				name:      name,
				groupID:   gid,
				groupName: gname,
				role:      g.role,
				exporter:  g.exporter,
				online:    online,
			})
		}
	}

	s.logger.Info("demosim: fleet seeded", zap.Int("agents", len(agents)), zap.Int("groups", len(simGroupDefs)))
	return agents, nil
}

func (s *Simulator) ensureGroup(ctx context.Context, g simGroupDef, now time.Time) error {
	existing, err := s.store.GetGroup(ctx, g.id)
	if err == nil && existing != nil {
		return nil
	}
	return s.store.CreateGroup(ctx, &appstore.Group{
		ID:                g.id,
		Name:              g.name,
		Labels:            map[string]string{"env": "prod", "fleet": demosimLabel, "role": g.role},
		RequireApproval:   true,
		LearnFromVerdicts: true,
		CreatedAt:         now,
		UpdatedAt:         now,
	})
}

func (s *Simulator) ensureGroupConfig(ctx context.Context, g simGroupDef, now time.Time) error {
	cfgID := "cfg-" + g.id + "-baseline"
	existing, err := s.store.GetConfig(ctx, cfgID)
	if err == nil && existing != nil {
		return nil
	}
	content := baselineConfigFor(g.role)
	gid := g.id
	return s.store.CreateConfig(ctx, &appstore.Config{
		ID:         cfgID,
		Name:       g.role + "-baseline",
		GroupID:    &gid,
		ConfigHash: hashConfig(content), // real hash so synced agents match
		Content:    content,
		Version:    1,
		CreatedAt:  now,
	})
}

// hashConfig mirrors the application service's drift hash exactly
// (sha256 of TrimSpace + CRLF->LF normalized content) so agents whose
// effective_config equals a group's config read as "synced".
func hashConfig(content string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(content), "\r\n", "\n")
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("%x", sum)
}

// baselineConfigFor returns a role-appropriate collector config. Kept small and
// readable so the config diff / pipeline view is legible in the demo.
func baselineConfigFor(role string) string {
	exporter := "otlphttp/cloud"
	return fmt.Sprintf(`receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
  hostmetrics:
    collection_interval: 30s

processors:
  batch:
    timeout: 5s
    send_batch_size: 8192
  resource:
    attributes:
      - key: squadron.role
        value: %s
        action: upsert

exporters:
  %s:
    endpoint: https://otel.example.com
    headers:
      authorization: Bearer ${env:OTEL_TOKEN}

service:
  pipelines:
    metrics:
      receivers: [otlp, hostmetrics]
      processors: [batch, resource]
      exporters: [%s]
    logs:
      receivers: [otlp]
      processors: [batch, resource]
      exporters: [%s]
    traces:
      receivers: [otlp]
      processors: [batch, resource]
      exporters: [%s]
`, role, exporter, exporter, exporter, exporter)
}
