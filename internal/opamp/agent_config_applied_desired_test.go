// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/devopsmike2/squadron/internal/services"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/memory"
)

// driftCompactIntent is a supervised collector's compact stored config: ${ENV}
// unresolved, no collector defaults enumerated, no opamp extension — the shape
// StoreConfigForAgent persists and drift compares against (ConfigIntent.Hash).
const driftCompactIntent = `receivers:
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

// driftExpandedEffective is what the same supervised agent reports as effective:
// ${ENV} resolved, collector defaults enumerated, supervisor opamp extension
// present. Meaningful surface identical to driftCompactIntent, but it never
// hash-matches the compact intent — the Southern 300vd false-positive shape.
const driftExpandedEffective = `receivers:
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

// newDriftTestService builds a memory-backed AgentService with one supervised
// agent already created, returning the service and the agent's fleet id.
func newDriftTestService(t *testing.T) (services.AgentService, uuid.UUID) {
	t.Helper()
	svc := services.NewAgentService(memory.NewStore(), nil, nil, nil, zap.NewNop())
	id := uuid.New()
	now := time.Now()
	if err := svc.CreateAgent(context.Background(), &services.Agent{
		ID: id, Name: "supervised", Status: services.AgentStatusOnline,
		Capabilities: []string{"accepts_remote_config"},
		LastSeen:     now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return svc, id
}

// TestAppliedConfigHashFromDesired covers the store-derived DELIVERED/APPLIED
// helper (ADR 0040 follow-up): it returns the confignorm hash of the assigned
// config exactly when the agent's reported wire hash matches the assigned
// config's wire hash — with NO in-memory staged remoteConfig required, unlike
// Agent.appliedConfigHash.
func TestAppliedConfigHashFromDesired(t *testing.T) {
	ctx := context.Background()

	t.Run("already-applied supervised agent, no staged remoteConfig -> applied", func(t *testing.T) {
		svc, id := newDriftTestService(t)
		if _, err := svc.StoreConfigForAgent(ctx, id, driftCompactIntent); err != nil {
			t.Fatalf("StoreConfigForAgent: %v", err)
		}
		// The exact production shape: reports the assigned config's wire hash,
		// remoteConfig NOT staged in memory (nil).
		agent := newReconcileAgent(id, wireConfigHash(driftCompactIntent))
		if agent.remoteConfig != nil {
			t.Fatal("precondition: remoteConfig must be nil for the already-applied case")
		}
		got, ok := appliedConfigHashFromDesired(ctx, svc, agent)
		if !ok {
			t.Fatal("expected ok=true: agent reports the assigned config's wire hash")
		}
		if want := confignorm.Hash(driftCompactIntent); got != want {
			t.Fatalf("hash = %q, want %q (confignorm hash of the assigned config)", got, want)
		}
	})

	t.Run("reports a different/older wire hash -> not applied", func(t *testing.T) {
		svc, id := newDriftTestService(t)
		if _, err := svc.StoreConfigForAgent(ctx, id, driftCompactIntent); err != nil {
			t.Fatalf("StoreConfigForAgent: %v", err)
		}
		agent := newReconcileAgent(id, wireConfigHash("some other config content"))
		if _, ok := appliedConfigHashFromDesired(ctx, svc, agent); ok {
			t.Fatal("expected ok=false: agent applied a different config than assigned")
		}
	})

	t.Run("no stored config -> not applied", func(t *testing.T) {
		svc, id := newDriftTestService(t)
		agent := newReconcileAgent(id, wireConfigHash(driftCompactIntent))
		if _, ok := appliedConfigHashFromDesired(ctx, svc, agent); ok {
			t.Fatal("expected ok=false: no assigned config to compare against")
		}
	})

	t.Run("empty reported hash -> not applied", func(t *testing.T) {
		svc, id := newDriftTestService(t)
		if _, err := svc.StoreConfigForAgent(ctx, id, driftCompactIntent); err != nil {
			t.Fatalf("StoreConfigForAgent: %v", err)
		}
		agent := newReconcileAgent(id, nil) // never reported RemoteConfigStatus
		if _, ok := appliedConfigHashFromDesired(ctx, svc, agent); ok {
			t.Fatal("expected ok=false: agent has not reported any delivered hash")
		}
	})

	t.Run("report-only agent (no accepts_remote_config) -> not applied", func(t *testing.T) {
		svc, id := newDriftTestService(t)
		// Report-only opamp agent: the accepts_remote_config bit is unset.
		agent := NewAgent(id, &mockConnection{})
		agent.Status = &protobufs.AgentToServer{
			Capabilities:       uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig),
			RemoteConfigStatus: &protobufs.RemoteConfigStatus{LastRemoteConfigHash: wireConfigHash(driftCompactIntent)},
		}
		if _, ok := appliedConfigHashFromDesired(ctx, svc, agent); ok {
			t.Fatal("expected ok=false: report-only agents are never on the delivered path")
		}
	})
}

// TestPersistAgentStampsDeliveredHashForAlreadyAppliedAgent is the end-to-end
// regression for the ADR 0040 follow-up (the post-control-plane-upgrade case): a
// supervised agent that applied its assigned config on a PRIOR build, then merely
// heartbeats after the upgrade — no fresh push, no in-memory staged remoteConfig —
// must flip from Drifted to Synced. Before the fix appliedConfigHash returned
// false (no staged remoteConfig) so delivered_config_hash stayed empty and the
// benign env/default/opamp expansion diff read as permanent drift.
func TestPersistAgentStampsDeliveredHashForAlreadyAppliedAgent(t *testing.T) {
	ctx := context.Background()
	svc, id := newDriftTestService(t)

	if _, err := svc.StoreConfigForAgent(ctx, id, driftCompactIntent); err != nil {
		t.Fatalf("StoreConfigForAgent: %v", err)
	}
	if err := svc.UpdateAgentEffectiveConfig(ctx, id, driftExpandedEffective); err != nil {
		t.Fatalf("UpdateAgentEffectiveConfig: %v", err)
	}

	// Precondition: with no delivered signal, the expansion diff reads as drift.
	pre, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent (pre): %v", err)
	}
	if pre.DriftStatus != services.ConfigDriftStatusDrifted {
		t.Fatalf("precondition: want %s, got %s", services.ConfigDriftStatusDrifted, pre.DriftStatus)
	}

	server := &Server{logger: zap.NewNop(), agentService: svc}

	// The agent reconnects and just heartbeats: reports the assigned config's wire
	// hash, carries no AgentDescription, and has no in-memory staged remoteConfig.
	agent := newReconcileAgent(id, wireConfigHash(driftCompactIntent))
	agent.Status.SequenceNum = 1
	if agent.remoteConfig != nil {
		t.Fatal("precondition: remoteConfig must be nil (already-applied, not re-staged)")
	}

	server.persistAgent(ctx, agent, &protobufs.AgentToServer{})

	got, err := svc.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent (post): %v", err)
	}
	if got.DriftStatus != services.ConfigDriftStatusSynced {
		t.Fatalf("want %s after heartbeat, got %s", services.ConfigDriftStatusSynced, got.DriftStatus)
	}
	if got.DriftDetails == nil {
		t.Fatal("expected drift details")
	}
	// Synced via the delivered path, NOT a content match: effective still differs.
	if got.DriftDetails.IntentHash == got.DriftDetails.EffectiveHash {
		t.Fatal("expected Synced via the delivered signal, not a content match")
	}
	if got.DriftDetails.DeliveredHash != got.DriftDetails.IntentHash {
		t.Fatalf("delivered hash %q should equal intent hash %q", got.DriftDetails.DeliveredHash, got.DriftDetails.IntentHash)
	}
}
