// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// TestListDiscoveryVerdicts_ReturnsMergedAndClosed — v0.89.36 (#655
// Stream 53, #531 slice 2 chunk 3). Seeds 2 pr_merged + 2
// pr_closed_not_merged audit rows in scope (varying timestamps), runs
// ListDiscoveryVerdicts, and asserts:
//   - all 4 rows surface,
//   - results are newest-first (timestamp DESC),
//   - State strings are set correctly per event_type,
//   - MergedBy reads from merged_by on merged rows and closed_by on
//     closed_not_merged rows.
func TestListDiscoveryVerdicts_ReturnsMergedAndClosed(t *testing.T) {
	s := NewStore()
	ctx := context.Background()
	const (
		connID = "conn-1"
		acctID = "123456789012"
		region = "us-east-1"
		repo   = "octo/widgets"
	)
	now := time.Now().UTC()
	seed := func(eventType string, age time.Duration, kind, url, actorKey, actorVal, tsKey string) {
		ts := now.Add(-age)
		ev := &types.AuditEvent{
			ID:         "audit-" + url,
			Timestamp:  ts,
			Actor:      "github_webhook",
			EventType:  eventType,
			TargetType: "iac_recommendation",
			TargetID:   connID,
			Payload: map[string]any{
				"repo_full_name":      repo,
				"pr_url":              url,
				"branch":              "squadron/rec/" + kind + "/" + acctID + "/" + region + "/x",
				"recommendation_kind": kind,
				"connection_id":       connID,
				"account_id":          acctID,
				"region":              region,
				actorKey:              actorVal,
				tsKey:                 ts.Format(time.RFC3339),
			},
		}
		if err := s.CreateAuditEvent(ctx, ev); err != nil {
			t.Fatalf("seed %s: %v", eventType, err)
		}
	}
	// The memory store walks audit_events in reverse insertion order
	// (the documented "newest-first" path) so we seed oldest-first
	// to land newest-first reads.
	seed("recommendation.pr_closed_not_merged", 4*time.Hour, "s3-access-logging",
		"https://github.com/octo/widgets/pull/4", "closed_by", "dave", "closed_at")
	seed("recommendation.pr_merged", 3*time.Hour, "lambda-otel-layer",
		"https://github.com/octo/widgets/pull/3", "merged_by", "carol", "merged_at")
	seed("recommendation.pr_closed_not_merged", 2*time.Hour, "eks-observability-addon",
		"https://github.com/octo/widgets/pull/2", "closed_by", "bob", "closed_at")
	seed("recommendation.pr_merged", 1*time.Hour, "rds-pi-em",
		"https://github.com/octo/widgets/pull/1", "merged_by", "alice", "merged_at")

	since := now.Add(-7 * 24 * time.Hour)
	rows, err := s.ListDiscoveryVerdicts(ctx, connID, acctID, region, since, 100)
	if err != nil {
		t.Fatalf("ListDiscoveryVerdicts: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}

	// Order is newest-first by timestamp.
	wantPRs := []string{
		"https://github.com/octo/widgets/pull/1", // 1h ago
		"https://github.com/octo/widgets/pull/2", // 2h
		"https://github.com/octo/widgets/pull/3", // 3h
		"https://github.com/octo/widgets/pull/4", // 4h
	}
	wantStates := []string{"merged", "closed_not_merged", "merged", "closed_not_merged"}
	wantActors := []string{"alice", "bob", "carol", "dave"}
	for i, r := range rows {
		if r.PRURL != wantPRs[i] {
			t.Errorf("row %d PRURL = %q, want %q", i, r.PRURL, wantPRs[i])
		}
		if r.State != wantStates[i] {
			t.Errorf("row %d State = %q, want %q", i, r.State, wantStates[i])
		}
		if r.MergedBy != wantActors[i] {
			t.Errorf("row %d MergedBy = %q, want %q", i, r.MergedBy, wantActors[i])
		}
	}

	// Out-of-scope tuples must yield no rows.
	rows, err = s.ListDiscoveryVerdicts(ctx, connID, "999999999999", region, since, 100)
	if err != nil {
		t.Fatalf("out-of-scope: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("out-of-scope rows = %d, want 0", len(rows))
	}
}
