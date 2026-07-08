// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package duckdb

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/telemetrystore/types"
	"go.uber.org/zap"
)

// TestCleanupOldData_SweepsOTLPBatches pins the v0.89 fix: otlp_batches
// (the v0.24 volume-insights accounting table) must be included in the
// retention sweep. Before the fix it grew unbounded — one row per agent
// per ExportRequest, never deleted — the same pathology
// pipeline_health_samples had. If someone removes the table from
// CleanupOldData's list, this fails.
func TestCleanupOldData_SweepsOTLPBatches(t *testing.T) {
	s, err := NewStorage(filepath.Join(t.TempDir(), "telemetry.db"), "", zap.NewNop())
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	write := func(ts time.Time, agent string) {
		t.Helper()
		if err := s.WriteBatchMeta(ctx, types.BatchMeta{
			Timestamp:    ts,
			AgentID:      agent,
			SignalType:   "metrics",
			ItemCount:    50,
			PayloadBytes: 1024,
			Status:       "ok",
		}); err != nil {
			t.Fatalf("WriteBatchMeta: %v", err)
		}
	}

	write(now.Add(-48*time.Hour), "old-agent")   // outside retention
	write(now.Add(-30*time.Minute), "new-agent") // inside retention

	if err := s.CleanupOldData(ctx, 24*time.Hour); err != nil {
		t.Fatalf("CleanupOldData: %v", err)
	}

	rows, err := s.db.QueryContext(ctx, "SELECT agent_id FROM otlp_batches")
	if err != nil {
		t.Fatalf("query otlp_batches: %v", err)
	}
	defer rows.Close()

	var agents []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan: %v", err)
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(agents) != 1 || agents[0] != "new-agent" {
		t.Fatalf("after cleanup want only [new-agent], got %v", agents)
	}
}
