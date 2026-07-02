// Package demoseed seeds a Squadron install with a realistic demo scenario so
// the flagship feature loops (fleet + config, cost-spike -> AI proposal ->
// rollout, incident) become visible in seconds without standing up a real OTel
// fleet or connecting a cloud account. It is the single source of truth for the
// demo scenario, shared by the squadron-demo-seed CLI and the in-app
// "Enable demo data" endpoint (POST /api/v1/demo/enable).
//
// Everything it creates is demo-scoped by reserved ids/names (demo-web-prod,
// cfg-demo-web-prod-baseline, demo-web-canary-1, spike-demo-*), so Remove can
// find and tear it down without touching a user's real fleet.
package demoseed

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Reserved demo identities. Callers (and Remove) key off these.
const (
	GroupID   = "demo-web-prod"
	GroupName = "Demo Web Prod"
	ConfigID  = "cfg-demo-web-prod-baseline"
	AgentName = "demo-web-canary-1"
	// SpikeIDPrefix prefixes the demo cost-spike id (a timestamp follows) so
	// Remove can match demo spikes without deleting a real one.
	SpikeIDPrefix = "spike-demo-"
)

// BaselineYAML is the config the demo group starts on. The hashing.rounds=12
// knob is the high-cardinality lever the AI proposer will likely propose to
// drop; kept short so the rollout approval diff reads at a glance.
const BaselineYAML = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
        # demo knob the AI proposer will likely pin to 6 to absorb
        # the new ML attribution workload without dropping signal
        hashing:
          rounds: 12

processors:
  batch:
    timeout: 5s
    send_batch_size: 8192

exporters:
  otlphttp/cloud:
    endpoint: https://otel.example.com
    headers:
      authorization: Bearer ${env:OTEL_TOKEN}

service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/cloud]
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/cloud]
`

// Summary reports what a Seed created (or found already present).
type Summary struct {
	GroupID string `json:"group_id"`
	Config  string `json:"config_id"`
	AgentID string `json:"agent_id"`
	SpikeID string `json:"spike_id"`
}

// Seed provisions the demo scenario. Idempotent: running twice is safe. When
// force is true it reseeds the cost spike even if the demo group already exists.
func Seed(ctx context.Context, store types.ApplicationStore, force bool) (Summary, error) {
	if err := seedGroup(ctx, store, force); err != nil {
		return Summary{}, err
	}
	if err := seedConfig(ctx, store); err != nil {
		return Summary{}, err
	}
	agentID, err := seedAgent(ctx, store)
	if err != nil {
		return Summary{}, err
	}
	spikeID, err := seedCostSpike(ctx, store, agentID, force)
	if err != nil {
		return Summary{}, err
	}
	return Summary{
		GroupID: GroupID,
		Config:  ConfigID,
		AgentID: agentID.String(),
		SpikeID: spikeID,
	}, nil
}

// Remove tears down the demo-scoped rows: the demo agent + group are deleted,
// and any open demo cost spikes are closed (ended + acknowledged) so they drop
// off the open list. The config has no store-level delete, so it's left in
// place (orphaned once its group is gone — harmless and invisible). Best-effort:
// a missing row is not an error.
func Remove(ctx context.Context, store types.ApplicationStore) error {
	// Delete the demo agent (found by its reserved name).
	agents, err := store.ListAgents(ctx)
	if err == nil {
		for _, a := range agents {
			if a.Name == AgentName {
				if derr := store.DeleteAgent(ctx, a.ID); derr != nil {
					return derr
				}
			}
		}
	}

	// Close any open demo cost spikes.
	spikes, err := store.ListCostSpikeEvents(ctx, types.CostSpikeFilter{Status: "open", Limit: 50})
	if err == nil {
		now := time.Now().UTC()
		for _, s := range spikes {
			if len(s.ID) < len(SpikeIDPrefix) || s.ID[:len(SpikeIDPrefix)] != SpikeIDPrefix {
				continue
			}
			s.EndedAt = &now
			if s.AcknowledgedAt == nil {
				s.AcknowledgedAt = &now
				s.AcknowledgedBy = "demo-remove"
			}
			if uerr := store.UpdateCostSpikeEvent(ctx, s); uerr != nil {
				return uerr
			}
		}
	}

	// Delete the demo group last (agents referenced it).
	if derr := store.DeleteGroup(ctx, GroupID); derr != nil {
		return derr
	}
	return nil
}

func seedGroup(ctx context.Context, store types.ApplicationStore, force bool) error {
	existing, err := store.GetGroup(ctx, GroupID)
	if err != nil {
		return err
	}
	if existing != nil {
		// Already present; nothing to (re)create. force only affects the spike.
		_ = force
		return nil
	}
	now := time.Now().UTC()
	return store.CreateGroup(ctx, &types.Group{
		ID:              GroupID,
		Name:            GroupName,
		Labels:          map[string]string{"env": "demo", "tier": "prod"},
		RequireApproval: true,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
}

func seedConfig(ctx context.Context, store types.ApplicationStore) error {
	groupID := GroupID
	existing, err := store.GetConfig(ctx, ConfigID)
	if err == nil && existing != nil {
		return nil
	}
	return store.CreateConfig(ctx, &types.Config{
		ID:         ConfigID,
		Name:       "demo-baseline",
		GroupID:    &groupID,
		ConfigHash: "demo-baseline-v1",
		Content:    BaselineYAML,
		Version:    1,
		CreatedAt:  time.Now().UTC(),
	})
}

func seedAgent(ctx context.Context, store types.ApplicationStore) (uuid.UUID, error) {
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	for _, a := range agents {
		if a.Name == AgentName {
			return a.ID, nil
		}
	}
	id := uuid.New()
	groupID := GroupID
	now := time.Now().UTC()
	err = store.CreateAgent(ctx, &types.Agent{
		ID:              id,
		Name:            AgentName,
		Labels:          map[string]string{"role": "web", "tier": "canary"},
		Status:          types.AgentStatusOnline,
		LastSeen:        now,
		GroupID:         &groupID,
		Version:         "0.106.0",
		Capabilities:    []string{"AcceptsRemoteConfig", "ReportsEffectiveConfig"},
		EffectiveConfig: BaselineYAML,
		DiscoverySource: "opamp",
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	return id, err
}

func seedCostSpike(ctx context.Context, store types.ApplicationStore, agentID uuid.UUID, force bool) (string, error) {
	existing, err := store.ListCostSpikeEvents(ctx, types.CostSpikeFilter{Status: "open", Limit: 10})
	if err == nil {
		for _, s := range existing {
			if s.Severity == "critical" && s.PeakPctAboveBaseline >= 300 {
				if !force {
					return s.ID, nil
				}
				break
			}
		}
	}

	attrib, _ := json.Marshal(map[string]any{
		"top_agents":     []string{agentID.String()},
		"top_attributes": []string{"container.id", "k8s.pod.uid"},
		"group_id":       GroupID,
		"note":           "demo seed",
	})
	now := time.Now().UTC()
	spike := &types.CostSpikeEvent{
		ID:                   SpikeIDPrefix + now.Format("20060102-150405"),
		StartedAt:            now,
		Severity:             "critical",
		Signal:               "metrics",
		BaselineMonthlyUSD:   400,
		PeakMonthlyUSD:       1648,
		PeakPctAboveBaseline: 312,
		AttributionJSON:      string(attrib),
	}
	if err := store.CreateCostSpikeEvent(ctx, spike); err != nil {
		return "", err
	}
	return spike.ID, nil
}
