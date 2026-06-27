// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestListCrossScopeDiscoveryVerdicts_sqlite — cross-cloud citations
// (v0.89.247). Mirrors the memory-store test: a declined verdict on an AWS
// connection and a merged verdict on a GCP connection; each connection's
// cross-scope query returns only the other scope's verdict, origin-labeled.
func TestListCrossScopeDiscoveryVerdicts_sqlite(t *testing.T) {
	appStore, err := NewSQLiteStorage(makeTempDB(t), zap.NewNop())
	require.NoError(t, err)
	store, ok := appStore.(*Storage)
	require.True(t, ok, "expected *Storage")
	ctx := context.Background()
	now := time.Now().UTC()

	seed := func(eventType, connID, scopeKey, scopeVal, region, kind, url, actorKey, actorVal, tsKey string, age time.Duration) {
		ts := now.Add(-age)
		ev := &types.AuditEvent{
			ID:         "a-" + url,
			Timestamp:  ts,
			Actor:      "github_webhook",
			EventType:  eventType,
			TargetType: "iac_recommendation",
			TargetID:   connID,
			Payload: map[string]any{
				"pr_url":              url,
				"recommendation_kind": kind,
				"connection_id":       connID,
				scopeKey:              scopeVal,
				"region":              region,
				actorKey:              actorVal,
				tsKey:                 ts.Format(time.RFC3339),
			},
		}
		require.NoError(t, store.CreateAuditEvent(ctx, ev))
	}

	seed("recommendation.pr_closed_not_merged", "conn-aws", "account_id", "111111111111",
		"us-east-1", "metrics-volume-drop", "https://github.com/o/r/pull/1", "closed_by", "alice", "closed_at", 2*time.Hour)
	seed("recommendation.pr_merged", "conn-gcp", "project_id", "demo-proj",
		"us-central1", "gce-ops-agent", "https://github.com/o/r/pull/2", "merged_by", "bob", "merged_at", 1*time.Hour)

	since := now.Add(-7 * 24 * time.Hour)

	got, err := store.ListCrossScopeDiscoveryVerdicts(ctx, "conn-gcp", since, 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "gcp-perspective cross-scope")
	require.Equal(t, "aws", got[0].Provider)
	require.Equal(t, "111111111111", got[0].ScopeID)
	require.Equal(t, "metrics-volume-drop", got[0].RecommendationKind)
	require.Equal(t, "closed_not_merged", got[0].State)

	got2, err := store.ListCrossScopeDiscoveryVerdicts(ctx, "conn-aws", since, 10)
	require.NoError(t, err)
	require.Len(t, got2, 1, "aws-perspective cross-scope")
	require.Equal(t, "gcp", got2[0].Provider)
	require.Equal(t, "demo-proj", got2[0].ScopeID)
}
