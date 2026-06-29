// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package pipelinehealth

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// recordingReader captures the last query + args passed to QueryRaw so a
// test can assert the SQL shape and bind values without a real DuckDB.
type recordingReader struct {
	lastQuery string
	lastArgs  []interface{}
}

func (r *recordingReader) QueryRaw(_ context.Context, query string, args ...interface{}) ([]map[string]interface{}, error) {
	r.lastQuery = query
	r.lastArgs = args
	return nil, nil // no rows — FleetSummary still completes (all agents Unknown)
}

type noAgents struct{}

func (noAgents) AllAgentIDs(context.Context) ([]string, error) { return nil, nil }

// TestFleetSummary_TimeBoundsTheScan pins the perf fix: FleetSummary must
// bound its window-function input with `WHERE timestamp >= ?` and pass a
// cutoff at roughly now-fleetWindow. Without the bound the ROW_NUMBER()
// scans the entire (unbounded, GC-less) pipeline_health_samples table,
// which degraded the /pipeline-health/fleet endpoint to multi-second
// hangs on long-running fleets.
func TestFleetSummary_TimeBoundsTheScan(t *testing.T) {
	rr := &recordingReader{}
	s := NewService(rr, noAgents{}, zap.NewNop())

	if _, err := s.FleetSummary(context.Background()); err != nil {
		t.Fatalf("FleetSummary: %v", err)
	}

	if !strings.Contains(rr.lastQuery, "timestamp >= ?") {
		t.Fatalf("fleet query must time-bound the window scan; got:\n%s", rr.lastQuery)
	}
	if len(rr.lastArgs) != 1 {
		t.Fatalf("fleet query must pass exactly one cutoff arg; got %d", len(rr.lastArgs))
	}
	cutoff, ok := rr.lastArgs[0].(time.Time)
	if !ok {
		t.Fatalf("cutoff arg must be a time.Time; got %T", rr.lastArgs[0])
	}
	// Default window is 1h; the cutoff should sit ~1h in the past.
	age := time.Since(cutoff)
	if age < 50*time.Minute || age > 70*time.Minute {
		t.Errorf("cutoff age = %s, want ~1h (default fleetWindow)", age)
	}
}
