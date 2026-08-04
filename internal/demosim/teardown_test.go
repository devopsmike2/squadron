// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package demosim

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/sqlite"
	appstore "github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestDisable_RemovesFleetByLabel_AfterRestart reproduces the OpenShift demo
// bug: the simulated fleet persists on the data volume, but the simulator's
// in-memory s.agents list is wiped on every pod restart. The old Disable()
// deleted from that in-memory list, so after a restart it found nothing to
// delete and orphaned the whole fleet (Remove could never clear it). The fix
// tears down by querying the store for demosim-labelled agents, so teardown is
// authoritative regardless of process memory. This test constructs a fresh
// simulator (empty memory) over a store that already holds a fleet — exactly
// the post-restart state — and asserts Disable clears the demosim rows while
// leaving unrelated agents untouched.
func TestDisable_RemovesFleetByLabel_AfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlite.NewSQLiteStorage(filepath.Join(dir, "demo.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()

	simGrp := simGroupDefs[0].id
	simGrpName := simGroupDefs[0].name
	if err := store.CreateGroup(ctx, &appstore.Group{ID: simGrp, Name: simGrpName, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create sim group: %v", err)
	}
	if err := store.CreateGroup(ctx, &appstore.Group{ID: "control-grp", Name: "Control", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create control group: %v", err)
	}

	mkAgent := func(name, groupID, groupName string, labels map[string]string) uuid.UUID {
		id := uuid.New()
		gid, gname := groupID, groupName
		if err := store.CreateAgent(ctx, &appstore.Agent{
			ID: id, Name: name, Labels: labels,
			Status: appstore.AgentStatusOnline, LastSeen: now,
			GroupID: &gid, GroupName: &gname, Version: "0.119.0",
			Capabilities:    []string{"AcceptsRemoteConfig"},
			EffectiveConfig: "receivers: {}", DiscoverySource: "opamp",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create agent %s: %v", name, err)
		}
		return id
	}

	// Two simulator-owned agents (fleet=demosim) + one unrelated control agent.
	mkAgent("web-001", simGrp, simGrpName, map[string]string{"fleet": demosimLabel, "role": "web"})
	mkAgent("web-002", simGrp, simGrpName, map[string]string{"fleet": demosimLabel, "role": "web"})
	controlID := mkAgent("real-collector-1", "control-grp", "Control", map[string]string{"env": "prod"})

	// Fresh simulator with an EMPTY in-memory list == the post-restart state.
	s := New(store, nil, zap.NewNop(), Options{})

	if err := s.Disable(ctx); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	agents, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list after disable: %v", err)
	}
	controlSurvived := false
	for _, a := range agents {
		if a.Labels["fleet"] == demosimLabel {
			t.Fatalf("demosim agent %q survived teardown (orphaned)", a.Name)
		}
		if a.ID == controlID {
			controlSurvived = true
		}
	}
	if !controlSurvived {
		t.Fatalf("control agent was deleted; teardown must only touch demosim-labelled rows")
	}
}
