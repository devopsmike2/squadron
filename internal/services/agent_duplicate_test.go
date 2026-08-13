package services

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// managedAgent builds an OpAMP-managed agent on the given host.
func managedAgent(name, host string) *Agent {
	return &Agent{
		ID:              uuid.New(),
		Name:            name,
		Labels:          map[string]string{"host.name": host},
		Version:         "0.100.0",
		Capabilities:    []string{"reports_status", "accepts_remote_config"},
		DiscoverySource: "opamp",
		CreatedAt:       time.Now(),
	}
}

// telemetryOnlyAgent builds a passive-OTLP telemetry-only agent on the given
// host (no capabilities, no version — the phantom signature from the incident).
func telemetryOnlyAgent(name, host string) *Agent {
	a := &Agent{
		ID:              uuid.New(),
		Name:            name,
		Labels:          map[string]string{},
		DiscoverySource: "otlp",
		CreatedAt:       time.Now(),
	}
	if host != "" {
		a.Labels["host.name"] = host
	}
	return a
}

func TestDetectDuplicates_FlagsTelemetryOnlySharingHost(t *testing.T) {
	managed := managedAgent("collector-1", "web-01")
	phantom := telemetryOnlyAgent("web-01", "web-01")

	detectDuplicates([]*Agent{managed, phantom})

	require.NotNil(t, phantom.SuspectedDuplicateOf, "telemetry-only agent sharing host.name should be flagged")
	assert.Equal(t, managed.ID.String(), phantom.SuspectedDuplicateOf.OfAgentID)
	assert.Equal(t, "collector-1", phantom.SuspectedDuplicateOf.OfAgentName)
	assert.Contains(t, phantom.SuspectedDuplicateOf.Reason, "web-01")
	assert.Nil(t, managed.SuspectedDuplicateOf, "the managed agent is never flagged as the duplicate")
}

func TestDetectDuplicates_DifferentHostNotFlagged(t *testing.T) {
	managed := managedAgent("collector-1", "web-01")
	phantom := telemetryOnlyAgent("web-02", "web-02")

	detectDuplicates([]*Agent{managed, phantom})

	assert.Nil(t, phantom.SuspectedDuplicateOf, "different host.name must not be flagged")
}

func TestDetectDuplicates_EmptyHostNotFlagged(t *testing.T) {
	managed := managedAgent("collector-1", "web-01")
	// Telemetry-only agent with NO host.name label (unknown host identity).
	phantom := telemetryOnlyAgent("mystery", "")

	detectDuplicates([]*Agent{managed, phantom})

	assert.Nil(t, phantom.SuspectedDuplicateOf, "empty/unknown host.name must never be flagged")
}

func TestDetectDuplicates_BothManagedNeverFlagged(t *testing.T) {
	a := managedAgent("collector-1", "web-01")
	b := managedAgent("collector-2", "web-01")

	detectDuplicates([]*Agent{a, b})

	assert.Nil(t, a.SuspectedDuplicateOf, "two managed agents on the same host are never flagged")
	assert.Nil(t, b.SuspectedDuplicateOf, "two managed agents on the same host are never flagged")
}

func TestDetectDuplicates_TelemetryOnlyWithVersionNotFlagged(t *testing.T) {
	managed := managedAgent("collector-1", "web-01")
	// Same host, but this OTLP agent reports a version — not the phantom
	// signature, so the conservative detector leaves it alone.
	withVersion := telemetryOnlyAgent("web-01", "web-01")
	withVersion.Version = "0.99.0"

	detectDuplicates([]*Agent{managed, withVersion})

	assert.Nil(t, withVersion.SuspectedDuplicateOf)
}

func TestDetectDuplicates_TwoTelemetryOnlyNoManagedNotFlagged(t *testing.T) {
	a := telemetryOnlyAgent("web-01", "web-01")
	b := telemetryOnlyAgent("web-01", "web-01")

	detectDuplicates([]*Agent{a, b})

	assert.Nil(t, a.SuspectedDuplicateOf, "no managed anchor means nothing to flag against")
	assert.Nil(t, b.SuspectedDuplicateOf)
}

func TestDetectDuplicates_DismissedNotFlagged(t *testing.T) {
	managed := managedAgent("collector-1", "web-01")
	phantom := telemetryOnlyAgent("web-01", "web-01")
	phantom.Labels[LabelDuplicateDismissed] = "true"

	detectDuplicates([]*Agent{managed, phantom})

	assert.Nil(t, phantom.SuspectedDuplicateOf, "a dismissed agent stays unflagged")
}

func TestDetectDuplicates_HostNameNormalized(t *testing.T) {
	managed := managedAgent("collector-1", "WEB-01")
	phantom := telemetryOnlyAgent("web-01", "web-01")

	detectDuplicates([]*Agent{managed, phantom})

	require.NotNil(t, phantom.SuspectedDuplicateOf, "case differences in host.name must still match")
	assert.Equal(t, managed.ID.String(), phantom.SuspectedDuplicateOf.OfAgentID)
}

func TestDetectDuplicates_MultipleManagedDeterministicAndAmbiguous(t *testing.T) {
	older := managedAgent("collector-old", "web-01")
	older.CreatedAt = time.Now().Add(-2 * time.Hour)
	newer := managedAgent("collector-new", "web-01")
	newer.CreatedAt = time.Now()
	phantom := telemetryOnlyAgent("web-01", "web-01")

	// Run twice with different input order; the pick must be stable (oldest).
	for _, order := range [][]*Agent{{older, newer, phantom}, {newer, phantom, older}} {
		older.SuspectedDuplicateOf = nil
		newer.SuspectedDuplicateOf = nil
		phantom.SuspectedDuplicateOf = nil
		detectDuplicates(order)
		require.NotNil(t, phantom.SuspectedDuplicateOf)
		assert.Equal(t, older.ID.String(), phantom.SuspectedDuplicateOf.OfAgentID,
			"deterministic pick should prefer the oldest equally-capable managed agent")
		assert.Contains(t, phantom.SuspectedDuplicateOf.Reason, "multiple managed agents")
	}
}

// --- service-layer surfacing (real impl over the in-memory store) ---

func TestAgentService_SurfacesSuspectedDuplicate(t *testing.T) {
	store := memory.NewStore()
	svc := NewAgentService(store, nil, nil, nil, zap.NewNop())
	ctx := context.Background()

	now := time.Now()
	managed := &Agent{
		ID:              uuid.New(),
		Name:            "collector-1",
		Labels:          map[string]string{"host.name": "web-01"},
		Status:          AgentStatusOnline,
		Version:         "0.100.0",
		Capabilities:    []string{"reports_status", "accepts_remote_config"},
		DiscoverySource: "opamp",
		LastSeen:        now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	phantom := &Agent{
		ID:              uuid.New(),
		Name:            "web-01",
		Labels:          map[string]string{"host.name": "web-01"},
		Status:          AgentStatusOnline,
		DiscoverySource: "otlp",
		LastSeen:        now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	require.NoError(t, svc.CreateAgent(ctx, managed))
	require.NoError(t, svc.CreateAgent(ctx, phantom))

	// ListAgents surfaces the flag on the phantom only.
	list, err := svc.ListAgents(ctx)
	require.NoError(t, err)
	byID := map[uuid.UUID]*Agent{}
	for _, a := range list {
		byID[a.ID] = a
	}
	require.NotNil(t, byID[phantom.ID].SuspectedDuplicateOf)
	assert.Equal(t, managed.ID.String(), byID[phantom.ID].SuspectedDuplicateOf.OfAgentID)
	assert.Nil(t, byID[managed.ID].SuspectedDuplicateOf)

	// GetAgent detail surfaces it too.
	gotPhantom, err := svc.GetAgent(ctx, phantom.ID)
	require.NoError(t, err)
	require.NotNil(t, gotPhantom.SuspectedDuplicateOf)
	assert.Equal(t, managed.ID.String(), gotPhantom.SuspectedDuplicateOf.OfAgentID)

	gotManaged, err := svc.GetAgent(ctx, managed.ID)
	require.NoError(t, err)
	assert.Nil(t, gotManaged.SuspectedDuplicateOf)

	// Dismiss via the reserved label (what the handler persists) clears it.
	gotPhantom.Labels[LabelDuplicateDismissed] = "true"
	require.NoError(t, svc.UpdateAgentRegistration(ctx, gotPhantom))

	after, err := svc.GetAgent(ctx, phantom.ID)
	require.NoError(t, err)
	assert.Nil(t, after.SuspectedDuplicateOf, "dismissing clears the flag")
}
