package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// compactIntent is a supervised collector's compact stored config: ${ENV}
// unresolved, no collector defaults enumerated, no opamp extension.
const compactIntent = `receivers:
  otlp:
    protocols:
      grpc:
exporters:
  otlphttp:
    endpoint: ${ENDPOINT_URL}
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [otlphttp]`

// expandedEffective is what the same supervised agent reports as its effective
// config: ${ENV} resolved, collector defaults enumerated, and the supervisor-
// injected local opamp extension present. The meaningful surface
// (service.pipelines/receivers/exporters) is identical to compactIntent, but the
// content — and therefore the content hash — differs, so it never hash-matches
// the compact intent. This is the Southern 300vd false-positive shape.
const expandedEffective = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
exporters:
  otlphttp:
    endpoint: https://otel-cs.example:4318
    compression: gzip
extensions:
  opamp:
    server:
      ws:
        endpoint: ws://127.0.0.1:4320/v1/opamp
service:
  extensions: [opamp]
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [otlphttp]`

// TestSupervisedAgentDeliveredHashSynced is the core regression this fixes (the
// Southern 300vd case). A supervised agent has applied exactly the config
// Squadron assigned, but its reported effective config is env-/default-expanded
// and carries the supervisor-injected opamp extension, so it does NOT hash-match
// the compact stored intent. Before the fix the agent showed permanent drift;
// with the DELIVERED signal set (delivered hash == intent hash) it is Synced.
func TestSupervisedAgentDeliveredHashSynced(t *testing.T) {
	store := memory.NewStore()
	service := NewAgentService(store, nil, nil, nil, zap.NewNop())
	ctx := context.Background()

	agentID := uuid.New()
	now := time.Now()
	require.NoError(t, service.CreateAgent(ctx, &Agent{
		ID: agentID, Name: "supervised-300vd", Status: AgentStatusOnline,
		Capabilities: []string{"accepts_remote_config"},
		LastSeen:     now, CreatedAt: now, UpdatedAt: now,
	}))

	cfg, err := service.StoreConfigForAgent(ctx, agentID, compactIntent)
	require.NoError(t, err)
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, expandedEffective))

	// Before the delivered signal is known, the expansion diff reads as drift
	// (this is exactly the false positive being fixed).
	pre, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, ConfigDriftStatusDrifted, pre.DriftStatus)

	// The OpAMP server confirms the agent applied the assigned config.
	require.NoError(t, service.UpdateAgentDeliveredConfigHash(ctx, agentID, cfg.ConfigHash))

	got, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, got.DriftDetails)
	assert.Equal(t, ConfigDriftStatusSynced, got.DriftStatus, "supervised agent that applied the assigned config must be synced despite expansion")
	// Synced via the delivered path, NOT a content match: effective still differs.
	assert.NotEqual(t, got.DriftDetails.IntentHash, got.DriftDetails.EffectiveHash)
	assert.Equal(t, got.DriftDetails.IntentHash, got.DriftDetails.DeliveredHash)
}

// TestSupervisedAgentGenuineDivergenceDrifted verifies real divergence still
// surfaces for supervised agents: an agent that never applied the pushed config
// (empty delivered hash) and one whose delivered hash is stale (it applied a
// different/older config than the current intent) both read as Drifted.
func TestSupervisedAgentGenuineDivergenceDrifted(t *testing.T) {
	store := memory.NewStore()
	service := NewAgentService(store, nil, nil, nil, zap.NewNop())
	ctx := context.Background()

	agentID := uuid.New()
	now := time.Now()
	require.NoError(t, service.CreateAgent(ctx, &Agent{
		ID: agentID, Name: "supervised-diverged", Status: AgentStatusOnline,
		Capabilities: []string{"accepts_remote_config"},
		LastSeen:     now, CreatedAt: now, UpdatedAt: now,
	}))
	_, err := service.StoreConfigForAgent(ctx, agentID, compactIntent)
	require.NoError(t, err)
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, expandedEffective))

	// Never applied the pushed config: delivered hash empty -> drifted.
	got, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, ConfigDriftStatusDrifted, got.DriftStatus)

	// Applied an OLDER/different config than the current intent: stale delivered
	// hash -> still drifted (it did not apply what Squadron currently assigns).
	require.NoError(t, service.UpdateAgentDeliveredConfigHash(ctx, agentID, confignorm.Hash("some other config content")))
	got, err = service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, ConfigDriftStatusDrifted, got.DriftStatus)
}

// TestReportOnlyAgentDriftUnchanged verifies report-only agents (no
// accepts_remote_config) keep the effective-vs-intent content comparison — the
// DELIVERED path is gated on the supervised capability. Even a delivered hash
// that equals the intent must NOT flip a report-only agent to synced when its
// effective config differs from intent.
func TestReportOnlyAgentDriftUnchanged(t *testing.T) {
	store := memory.NewStore()
	service := NewAgentService(store, nil, nil, nil, zap.NewNop())
	ctx := context.Background()

	agentID := uuid.New()
	now := time.Now()
	require.NoError(t, service.CreateAgent(ctx, &Agent{
		ID: agentID, Name: "report-only", Status: AgentStatusOnline,
		Capabilities: []string{"reports_remote_config"}, // NOT accepts_remote_config
		LastSeen:     now, CreatedAt: now, UpdatedAt: now,
	}))

	// A report-only agent can't StoreConfigForAgent (no accepts capability), so
	// seed the intent directly in the store.
	intentHash := confignorm.Hash(compactIntent)
	require.NoError(t, service.CreateConfig(ctx, &Config{
		ID: uuid.New().String(), AgentID: &agentID,
		ConfigHash: intentHash, Content: compactIntent, Version: 1, CreatedAt: now,
	}))

	// Exact content match -> synced (unchanged behavior).
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, compactIntent))
	got, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, ConfigDriftStatusSynced, got.DriftStatus)

	// Effective differs -> drifted, even with a (spurious) delivered hash equal
	// to the intent: the delivered path must stay gated to supervised agents.
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, expandedEffective))
	require.NoError(t, service.UpdateAgentDeliveredConfigHash(ctx, agentID, intentHash))
	got, err = service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, ConfigDriftStatusDrifted, got.DriftStatus)
	// DeliveredHash is not surfaced for report-only agents.
	require.NotNil(t, got.DriftDetails)
	assert.Empty(t, got.DriftDetails.DeliveredHash)
}

// TestSupervisedDeliveredHashTransitionCounters verifies UpdateAgentDeliveredConfigHash
// fires the drift transition when a supervised agent reconciles (drifted ->
// synced) purely via the delivered signal — the effective config never changes.
func TestSupervisedDeliveredHashTransitionCounters(t *testing.T) {
	store := memory.NewStore()
	driftMetrics := newRecordingDriftMetrics()
	service := NewAgentService(store, driftMetrics, nil, nil, zap.NewNop())
	ctx := context.Background()

	agentID := uuid.New()
	now := time.Now()
	require.NoError(t, service.CreateAgent(ctx, &Agent{
		ID: agentID, Name: "supervised", Status: AgentStatusOnline,
		Capabilities: []string{"accepts_remote_config"},
		LastSeen:     now, CreatedAt: now, UpdatedAt: now,
	}))
	cfg, err := service.StoreConfigForAgent(ctx, agentID, compactIntent)
	require.NoError(t, err)
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, expandedEffective)) // drifted (expansion, no delivered hash)

	toSynced := driftMetrics.TransitionsToSynced.(*recordingCounter)
	before := toSynced.Value()

	// Apply the assigned config -> drifted -> synced, effective unchanged.
	require.NoError(t, service.UpdateAgentDeliveredConfigHash(ctx, agentID, cfg.ConfigHash))
	assert.Equal(t, before+1, toSynced.Value(), "reconcile via delivered signal should record a to-synced transition")
}

// recordingCounter is a metrics.Counter that simply counts increments — used
// to assert that a transition counter was fired.
type recordingCounter struct{ n int64 }

func (c *recordingCounter) Inc(v int64)  { atomic.AddInt64(&c.n, v) }
func (c *recordingCounter) Value() int64 { return atomic.LoadInt64(&c.n) }

// recordingGauge is a metrics.Gauge that remembers the last value set on it.
type recordingGauge struct{ v int64 }

func (g *recordingGauge) Update(v int64) { atomic.StoreInt64(&g.v, v) }
func (g *recordingGauge) Value() int64   { return atomic.LoadInt64(&g.v) }

// newRecordingDriftMetrics builds a DriftMetrics with every field backed by a
// recording impl. Tests read values back through the typed fields.
func newRecordingDriftMetrics() *metrics.DriftMetrics {
	return &metrics.DriftMetrics{
		FleetAgentsTotal:     &recordingGauge{},
		FleetSynced:          &recordingGauge{},
		FleetDrifted:         &recordingGauge{},
		FleetNoIntent:        &recordingGauge{},
		FleetNoEffective:     &recordingGauge{},
		FleetUnknown:         &recordingGauge{},
		TransitionsToDrifted: &recordingCounter{},
		TransitionsToSynced:  &recordingCounter{},
	}
}

func TestAgentServiceConfigDriftDetection(t *testing.T) {
	store := memory.NewStore()
	logger := zap.NewNop()
	service := NewAgentService(store, nil, nil, nil, logger)

	agentID := uuid.New()
	now := time.Now()
	baseConfig := "receivers:\n  otlp:\n    protocols:\n      grpc:"

	agent := &Agent{
		ID:           agentID,
		Name:         "test-agent",
		Status:       AgentStatusOnline,
		Labels:       map[string]string{},
		Capabilities: []string{"accepts_remote_config"},
		LastSeen:     now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	ctx := context.Background()
	require.NoError(t, service.CreateAgent(ctx, agent))

	_, err := service.StoreConfigForAgent(ctx, agentID, baseConfig)
	require.NoError(t, err)

	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, baseConfig))

	syncedAgent, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, syncedAgent)
	assert.Equal(t, ConfigDriftStatusSynced, syncedAgent.DriftStatus)
	require.NotNil(t, syncedAgent.ConfigIntent)
	require.NotNil(t, syncedAgent.DriftDetails)
	assert.Equal(t, syncedAgent.DriftDetails.IntentHash, syncedAgent.DriftDetails.EffectiveHash)

	driftedConfig := baseConfig + "\n  http: {}"
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, driftedConfig))

	driftedAgent, err := service.GetAgent(ctx, agentID)
	require.NoError(t, err)
	require.NotNil(t, driftedAgent)
	assert.Equal(t, ConfigDriftStatusDrifted, driftedAgent.DriftStatus)
	require.NotNil(t, driftedAgent.DriftDetails)
	assert.NotEmpty(t, driftedAgent.DriftDetails.Diff)
}

// TestDriftTransitionCounters verifies that UpdateAgentEffectiveConfig fires
// the correct transition counter when an agent's drift status changes.
//
// Scenario:
//  1. Agent created, intent stored, effective set to match -> synced.
//  2. Effective changed to a different config -> drifted (TransitionsToDrifted increments).
//  3. Effective changed back to match intent -> synced (TransitionsToSynced increments).
func TestDriftTransitionCounters(t *testing.T) {
	store := memory.NewStore()
	logger := zap.NewNop()
	driftMetrics := newRecordingDriftMetrics()
	service := NewAgentService(store, driftMetrics, nil, nil, logger)

	agentID := uuid.New()
	now := time.Now()
	baseConfig := "receivers:\n  otlp:\n    protocols:\n      grpc:"

	agent := &Agent{
		ID:           agentID,
		Name:         "test-agent",
		Status:       AgentStatusOnline,
		Labels:       map[string]string{},
		Capabilities: []string{"accepts_remote_config"},
		LastSeen:     now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	ctx := context.Background()
	require.NoError(t, service.CreateAgent(ctx, agent))
	_, err := service.StoreConfigForAgent(ctx, agentID, baseConfig)
	require.NoError(t, err)

	// 1) no_intent -> synced (we count this as TransitionsToSynced because the
	// status was no_effective before and is now synced).
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, baseConfig))
	toDrifted := driftMetrics.TransitionsToDrifted.(*recordingCounter)
	toSynced := driftMetrics.TransitionsToSynced.(*recordingCounter)
	assert.Equal(t, int64(0), toDrifted.Value(), "no drift transition expected on first sync")
	assert.Equal(t, int64(1), toSynced.Value(), "first sync should record a to-synced transition")

	// 2) synced -> drifted
	driftedConfig := baseConfig + "\n  http: {}"
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, driftedConfig))
	assert.Equal(t, int64(1), toDrifted.Value(), "drift should record a to-drifted transition")
	assert.Equal(t, int64(1), toSynced.Value(), "to-synced should be unchanged")

	// 3) drifted -> synced (recovery)
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, agentID, baseConfig))
	assert.Equal(t, int64(1), toDrifted.Value(), "to-drifted unchanged on recovery")
	assert.Equal(t, int64(2), toSynced.Value(), "recovery should record a second to-synced transition")
}

// TestFleetDriftGaugesViaListAgents verifies that ListAgents refreshes the
// per-status fleet gauges based on the current set of agents.
func TestFleetDriftGaugesViaListAgents(t *testing.T) {
	store := memory.NewStore()
	logger := zap.NewNop()
	driftMetrics := newRecordingDriftMetrics()
	service := NewAgentService(store, driftMetrics, nil, nil, logger)
	ctx := context.Background()

	now := time.Now()
	cfg := "receivers:\n  otlp:\n    protocols:\n      grpc:"

	// Three agents in three different states:
	//   syncedAgent  -> intent matches effective => synced
	//   driftedAgent -> intent set, effective differs => drifted
	//   bareAgent    -> no intent, no effective => no_intent
	syncedID, driftedID, bareID := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{syncedID, driftedID, bareID} {
		require.NoError(t, service.CreateAgent(ctx, &Agent{
			ID: id, Name: id.String(), Status: AgentStatusOnline,
			Capabilities: []string{"accepts_remote_config"},
			LastSeen:     now, CreatedAt: now, UpdatedAt: now,
		}))
	}

	_, err := service.StoreConfigForAgent(ctx, syncedID, cfg)
	require.NoError(t, err)
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, syncedID, cfg))

	_, err = service.StoreConfigForAgent(ctx, driftedID, cfg)
	require.NoError(t, err)
	require.NoError(t, service.UpdateAgentEffectiveConfig(ctx, driftedID, cfg+"\n  http: {}"))

	// Trigger gauge refresh.
	_, err = service.ListAgents(ctx)
	require.NoError(t, err)

	g := func(field metrics.Gauge) int64 { return field.(*recordingGauge).Value() }
	assert.Equal(t, int64(3), g(driftMetrics.FleetAgentsTotal), "total agents")
	assert.Equal(t, int64(1), g(driftMetrics.FleetSynced), "synced count")
	assert.Equal(t, int64(1), g(driftMetrics.FleetDrifted), "drifted count")
	assert.Equal(t, int64(1), g(driftMetrics.FleetNoIntent), "no-intent count")
	assert.Equal(t, int64(0), g(driftMetrics.FleetNoEffective), "no-effective count")
	assert.Equal(t, int64(0), g(driftMetrics.FleetUnknown), "unknown count")
}
