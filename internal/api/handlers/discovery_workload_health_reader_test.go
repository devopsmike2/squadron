// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

func seedWHScan(t *testing.T, store *fakeScanStore, id, provider, scope string, started time.Time, resultJSON string) {
	t.Helper()
	if err := store.SaveDiscoveryScan(context.Background(), &types.ScanRecord{
		ScanID:     id,
		Provider:   provider,
		ScopeID:    scope,
		StartedAt:  started,
		ResultJSON: resultJSON,
	}); err != nil {
		t.Fatalf("seed scan %s: %v", id, err)
	}
}

// TestPersistedScanWorkloadHealthReader_CountsAndDedup verifies the reader rolls
// up the three serverless diagnostics from the latest scan per scope, dedups
// older scans of the same scope, aggregates across scopes, and applies UNION
// semantics for any_issue_count.
func TestPersistedScanWorkloadHealthReader_CountsAndDedup(t *testing.T) {
	store := newFakeScanStore()
	now := time.Now().UTC()

	// Scope proj-a: an OLDER scan (must be ignored) + the LATEST scan with
	// 4 serverless rows — 2 cold-start, 0 sampling, 2 error-rate, with one row
	// firing BOTH (so any_issue = 3 of 4, not 4).
	seedWHScan(t, store, "gcp-a-old", "gcp", "proj-a", now.Add(-time.Hour),
		`{"serverless":[{"cold_start_exceeds_threshold":true}]}`)
	seedWHScan(t, store, "gcp-a-new", "gcp", "proj-a", now,
		`{"serverless":[
			{"cold_start_exceeds_threshold":true,"sampling_exceeds_floor":false,"error_rate_exceeds_threshold":false},
			{"cold_start_exceeds_threshold":false,"error_rate_exceeds_threshold":true},
			{"cold_start_exceeds_threshold":true,"error_rate_exceeds_threshold":true},
			{}
		]}`)
	// Scope proj-b: latest scan with 1 sampling-only row.
	seedWHScan(t, store, "gcp-b", "gcp", "proj-b", now,
		`{"serverless":[{"sampling_exceeds_floor":true}]}`)

	reader := NewPersistedScanWorkloadHealthReader(store, zap.NewNop())
	got := reader.WorkloadHealthCounts(context.Background())

	gcp := got["gcp"]
	// proj-a latest (4 rows) + proj-b (1 row) = 5; old proj-a scan ignored.
	if gcp.ServerlessResourceCount != 5 {
		t.Errorf("ServerlessResourceCount = %d, want 5 (dedup: old proj-a scan ignored)", gcp.ServerlessResourceCount)
	}
	if gcp.ColdStartExceededCount != 2 {
		t.Errorf("ColdStartExceededCount = %d, want 2", gcp.ColdStartExceededCount)
	}
	if gcp.SamplingTooAggressiveCount != 1 {
		t.Errorf("SamplingTooAggressiveCount = %d, want 1", gcp.SamplingTooAggressiveCount)
	}
	if gcp.ErrorRateSpikeCount != 2 {
		t.Errorf("ErrorRateSpikeCount = %d, want 2", gcp.ErrorRateSpikeCount)
	}
	// UNION: proj-a rows 1,2,3 fire (row3 fires two but counts once) + proj-b
	// row = 4 resources with at least one issue.
	if gcp.AnyIssueCount != 4 {
		t.Errorf("AnyIssueCount = %d, want 4 (union)", gcp.AnyIssueCount)
	}

	// Providers with no scans are present + zero (the handler/panel relies on
	// every provider key existing).
	for _, p := range []string{"aws", "azure", "oci"} {
		if c := got[p]; c.ServerlessResourceCount != 0 || c.AnyIssueCount != 0 {
			t.Errorf("%s: want zero counts, got %+v", p, c)
		}
	}
}

// TestPersistedScanWorkloadHealthReader_MissingFlagsNotFiring confirms the
// honest posture: a serverless row whose error-rate / sampling flags are absent
// from the scan JSON (e.g. a pre-marshal-parity AWS scan, or the fleet-wide
// dormant sampling axis) reads as not-firing rather than fabricated.
func TestPersistedScanWorkloadHealthReader_AWSPartial(t *testing.T) {
	store := newFakeScanStore()
	seedWHScan(t, store, "aws-1", "aws", "123456789012", time.Now().UTC(),
		`{"serverless":[{"cold_start_exceeds_threshold":true},{"cold_start_exceeds_threshold":false}]}`)

	reader := NewPersistedScanWorkloadHealthReader(store, zap.NewNop())
	aws := reader.WorkloadHealthCounts(context.Background())["aws"]

	if aws.ServerlessResourceCount != 2 {
		t.Errorf("ServerlessResourceCount = %d, want 2", aws.ServerlessResourceCount)
	}
	if aws.ColdStartExceededCount != 1 {
		t.Errorf("ColdStartExceededCount = %d, want 1", aws.ColdStartExceededCount)
	}
	if aws.ErrorRateSpikeCount != 0 || aws.SamplingTooAggressiveCount != 0 {
		t.Errorf("error-rate/sampling should be 0 (dropped from AWS wire row), got err=%d samp=%d",
			aws.ErrorRateSpikeCount, aws.SamplingTooAggressiveCount)
	}
	if aws.AnyIssueCount != 1 {
		t.Errorf("AnyIssueCount = %d, want 1", aws.AnyIssueCount)
	}
}

// TestPersistedScanWorkloadHealthReader_NilStore safe-degrades to empty counts.
func TestPersistedScanWorkloadHealthReader_NilStore(t *testing.T) {
	reader := NewPersistedScanWorkloadHealthReader(nil, zap.NewNop())
	got := reader.WorkloadHealthCounts(context.Background())
	for _, p := range workloadHealthProviders {
		if c := got[p]; c.ServerlessResourceCount != 0 {
			t.Errorf("%s: nil store should yield zero, got %+v", p, c)
		}
	}
}
