// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Command squadron-demo-seed seeds a fresh Squadron install with a
// realistic demo scenario so the engineer copilot loop becomes
// visible in seconds, without standing up a real OTel fleet.
//
// What gets seeded:
//
//   - A demo group "demo-web-prod" with require_approval=true so the
//     viewer sees the approval gate.
//   - A baseline collector config for the group with a high
//     cardinality knob set (hashing.rounds=12) that the AI proposer
//     has something to point at.
//   - A synthetic agent in the group so the proposer's "top
//     contributing agents" attribution has a target.
//   - A cost spike event (+312% above baseline) on the demo group.
//     The proposer polls cost spikes on its tick interval, so within
//     30 seconds of seeding the operator sees an AI drafted rollout
//     appear under /rollouts.
//
// Idempotent: running twice is safe. --force resets the demo state
// (deletes the spike acknowledgement so the proposer reconsiders).
//
// Usage:
//
//	squadron-demo-seed --db ~/.squadron/data/squadron.db
//
// or, from the repo:
//
//	make demo-seed
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

const (
	demoGroupID   = "demo-web-prod"
	demoGroupName = "Demo Web Prod"
	demoConfigID  = "cfg-demo-web-prod-baseline"

	// baselineYAML is the config the demo group starts on. The
	// hashing.rounds=12 knob is the high cardinality lever the
	// proposer will likely propose to drop. Kept short so the diff
	// view in the rollout approval drawer reads at a glance.
	baselineYAML = `receivers:
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
)

func main() {
	var (
		dbPath  string
		dbType  string
		force   bool
		verbose bool
	)
	flag.StringVar(&dbPath, "db", "", "Path to the Squadron application store DB (sqlite file). Default: ./data/squadron.db.")
	flag.StringVar(&dbType, "type", "sqlite", "Storage type (sqlite or memory).")
	flag.BoolVar(&force, "force", false, "Reseed the cost spike even if the demo group already exists.")
	flag.BoolVar(&verbose, "v", false, "Verbose log output.")
	flag.Parse()

	if dbPath == "" && dbType == "sqlite" {
		dbPath = "./data/squadron.db"
	}

	zapCfg := zap.NewProductionConfig()
	if !verbose {
		zapCfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	}
	logger, err := zapCfg.Build()
	if err != nil {
		exit("build logger: %v", err)
	}
	defer func() { _ = logger.Sync() }()

	factory, err := applicationstore.NewFactory(applicationstore.FactoryConfig{
		Type: dbType,
		Path: dbPath,
	})
	if err != nil {
		exit("open application store factory: %v", err)
	}
	if err := factory.Initialize(logger); err != nil {
		exit("initialize factory: %v", err)
	}
	defer func() { _ = factory.Close() }()

	store, err := factory.CreateApplicationStore()
	if err != nil {
		exit("create application store: %v", err)
	}

	ctx := context.Background()

	if err := seedGroup(ctx, store, force); err != nil {
		exit("seed group: %v", err)
	}
	if err := seedConfig(ctx, store); err != nil {
		exit("seed config: %v", err)
	}
	agentID, err := seedAgent(ctx, store)
	if err != nil {
		exit("seed agent: %v", err)
	}
	spikeID, err := seedCostSpike(ctx, store, agentID, force)
	if err != nil {
		exit("seed cost spike: %v", err)
	}

	fmt.Println("squadron demo seed: complete.")
	fmt.Printf("  group:        %s (%s)\n", demoGroupName, demoGroupID)
	fmt.Printf("  config:       %s\n", demoConfigID)
	fmt.Printf("  agent:        %s\n", agentID)
	fmt.Printf("  cost spike:   %s\n", spikeID)
	fmt.Println()
	fmt.Println("Next: open the Squadron UI and watch /rollouts. Within 30 seconds")
	fmt.Println("an AI proposed rollout should appear in pending_approval.")
}

func seedGroup(ctx context.Context, store types.ApplicationStore, force bool) error {
	existing, err := store.GetGroup(ctx, demoGroupID)
	if err != nil {
		return err
	}
	if existing != nil {
		if !force {
			fmt.Fprintln(os.Stderr, "demo group already exists; skipping (use --force to reseed the cost spike anyway).")
		}
		return nil
	}
	now := time.Now().UTC()
	return store.CreateGroup(ctx, &types.Group{
		ID:   demoGroupID,
		Name: demoGroupName,
		Labels: map[string]string{
			"env":  "demo",
			"tier": "prod",
		},
		RequireApproval: true,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
}

func seedConfig(ctx context.Context, store types.ApplicationStore) error {
	groupID := demoGroupID
	existing, err := store.GetConfig(ctx, demoConfigID)
	if err == nil && existing != nil {
		return nil
	}
	return store.CreateConfig(ctx, &types.Config{
		ID:         demoConfigID,
		Name:       "demo-baseline",
		GroupID:    &groupID,
		ConfigHash: "demo-baseline-v1",
		Content:    baselineYAML,
		Version:    1,
		CreatedAt:  time.Now().UTC(),
	})
}

func seedAgent(ctx context.Context, store types.ApplicationStore) (uuid.UUID, error) {
	// Idempotent: look for the canonical demo agent by name before
	// creating. ListAgents is the simple option since the demo set
	// is tiny.
	agents, err := store.ListAgents(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	for _, a := range agents {
		if a.Name == "demo-web-canary-1" {
			return a.ID, nil
		}
	}

	id := uuid.New()
	groupID := demoGroupID
	now := time.Now().UTC()
	err = store.CreateAgent(ctx, &types.Agent{
		ID:              id,
		Name:            "demo-web-canary-1",
		Labels:          map[string]string{"role": "web", "tier": "canary"},
		Status:          types.AgentStatusOnline,
		LastSeen:        now,
		GroupID:         &groupID,
		Version:         "0.106.0",
		Capabilities:    []string{"AcceptsRemoteConfig", "ReportsEffectiveConfig"},
		EffectiveConfig: baselineYAML,
		DiscoverySource: "opamp",
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	return id, err
}

func seedCostSpike(ctx context.Context, store types.ApplicationStore, agentID uuid.UUID, force bool) (string, error) {
	// Look for an existing open spike attributed to the demo agent.
	// If one is there and not forcing, leave it alone (the proposer
	// will still pick it up).
	existing, err := store.ListCostSpikeEvents(ctx, types.CostSpikeFilter{
		Status: "open",
		Limit:  10,
	})
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
		"group_id":       demoGroupID,
		"note":           "demo seed",
	})
	now := time.Now().UTC()
	spike := &types.CostSpikeEvent{
		ID:                   "spike-demo-" + now.Format("20060102-150405"),
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

func exit(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "squadron-demo-seed: "+fmt.Sprintf(format, args...))
	os.Exit(1)
}
