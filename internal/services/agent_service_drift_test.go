package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/metrics"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// recordingCounter is a metrics.Counter that simply counts increments — used
// to assert that a transition counter was fired.
type recordingCounter struct{ n int64 }

func (c *recordingCounter) Inc(v int64)   { atomic.AddInt64(&c.n, v) }
func (c *recordingCounter) Value() int64  { return atomic.LoadInt64(&c.n) }

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
//   1. Agent created, intent stored, effective set to match -> synced.
//   2. Effective changed to a different config -> drifted (TransitionsToDrifted increments).
//   3. Effective changed back to match intent -> synced (TransitionsToSynced increments).
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
