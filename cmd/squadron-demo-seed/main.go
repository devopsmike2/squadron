// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Command squadron-demo-seed seeds a fresh Squadron install with a realistic
// demo scenario so the engineer copilot loop becomes visible in seconds,
// without standing up a real OTel fleet. The scenario itself lives in
// internal/demoseed (shared with the in-app "Enable demo data" endpoint):
//
//   - A demo group "demo-web-prod" with require_approval=true (approval gate).
//   - A baseline collector config with a high-cardinality knob for the AI
//     proposer to point at.
//   - A synthetic agent in the group (proposer attribution target).
//   - A cost spike event (+312% above baseline). The proposer polls cost spikes
//     on its tick interval, so within ~30s an AI-drafted rollout appears under
//     /rollouts.
//
// Idempotent: running twice is safe. --force resets the demo state (reseeds the
// cost spike).
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
	"flag"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/demoseed"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
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

	summary, err := demoseed.Seed(context.Background(), store, force)
	if err != nil {
		exit("seed demo: %v", err)
	}

	fmt.Println("squadron demo seed: complete.")
	fmt.Printf("  group:        %s (%s)\n", demoseed.GroupName, summary.GroupID)
	fmt.Printf("  config:       %s\n", summary.Config)
	fmt.Printf("  agent:        %s\n", summary.AgentID)
	fmt.Printf("  cost spike:   %s\n", summary.SpikeID)
	fmt.Println()
	fmt.Println("Next: open the Squadron UI and watch /rollouts. Within 30 seconds")
	fmt.Println("an AI proposed rollout should appear in pending_approval.")
}

func exit(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "squadron-demo-seed: "+fmt.Sprintf(format, args...))
	os.Exit(1)
}
