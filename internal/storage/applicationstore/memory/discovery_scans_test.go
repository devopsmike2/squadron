// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestDiscoveryScans_SaveListGet covers the slice-1 persistence contract:
// newest-first listing scoped by provider+scope, result_json omitted from the
// list but present on get, scope/provider filtering, and upsert.
func TestDiscoveryScans_SaveListGet(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	base := time.Now().UTC()

	mk := func(id, provider, scope string, age time.Duration) *types.ScanRecord {
		return &types.ScanRecord{
			ScanID:      id,
			Provider:    provider,
			ScopeID:     scope,
			Regions:     []string{"us-east-1"},
			StartedAt:   base.Add(-age),
			CompletedAt: base.Add(-age).Add(time.Minute),
			Summary:     map[string]int{"compute": 3},
			ResultJSON:  `{"scan_id":"` + id + `","compute":[]}`,
		}
	}
	for _, r := range []*types.ScanRecord{
		mk("scan-old", "aws", "111111111111", 3*time.Hour),
		mk("scan-new", "aws", "111111111111", 1*time.Hour),
		mk("scan-other-scope", "aws", "222222222222", 2*time.Hour),
		mk("scan-gcp", "gcp", "proj-x", 1*time.Hour),
	} {
		if err := s.SaveDiscoveryScan(ctx, r); err != nil {
			t.Fatalf("save %s: %v", r.ScanID, err)
		}
	}

	// List scoped to aws/111... — newest first, result_json omitted.
	list, err := s.ListDiscoveryScans(ctx, "aws", "111111111111", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 scans for the scope, got %d", len(list))
	}
	if list[0].ScanID != "scan-new" || list[1].ScanID != "scan-old" {
		t.Errorf("not newest-first: %s, %s", list[0].ScanID, list[1].ScanID)
	}
	if list[0].ResultJSON != "" {
		t.Errorf("list leaked result_json: %q", list[0].ResultJSON)
	}
	if list[0].Summary["compute"] != 3 {
		t.Errorf("summary not returned in list: %v", list[0].Summary)
	}

	// Blank scope lists all aws scans.
	all, _ := s.ListDiscoveryScans(ctx, "aws", "", 10)
	if len(all) != 3 {
		t.Errorf("expected 3 aws scans for blank scope, got %d", len(all))
	}

	// Get returns the full inventory.
	got, err := s.GetDiscoveryScan(ctx, "scan-new")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || got.ResultJSON == "" {
		t.Fatalf("get returned no result_json: %+v", got)
	}

	// Unknown id → (nil, nil).
	missing, err := s.GetDiscoveryScan(ctx, "nope")
	if err != nil || missing != nil {
		t.Errorf("expected (nil,nil) for unknown id, got (%v,%v)", missing, err)
	}

	// Upsert: re-save scan-new with a different summary.
	upd := mk("scan-new", "aws", "111111111111", 1*time.Hour)
	upd.Summary = map[string]int{"compute": 9}
	if err := s.SaveDiscoveryScan(ctx, upd); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	after, _ := s.ListDiscoveryScans(ctx, "aws", "111111111111", 10)
	if len(after) != 2 {
		t.Errorf("upsert created a duplicate row: %d", len(after))
	}
	if g, _ := s.GetDiscoveryScan(ctx, "scan-new"); g.Summary["compute"] != 9 {
		t.Errorf("upsert did not update summary: %v", g.Summary)
	}
}
