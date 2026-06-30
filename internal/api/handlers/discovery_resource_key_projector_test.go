// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/traceindex"
)

// TestResourceKeyProjector_PerKind verifies the projector resolves a
// (provider, kind, id) tuple to the same traceindex key the scan-side last-seen
// pass projects — for the three trace-joinable kinds — and returns (_, false)
// for unknown kinds, missing resources, and a nil store.
func TestResourceKeyProjector_PerKind(t *testing.T) {
	store := newFakeScanStore()
	now := time.Now().UTC()
	seedWHScan(t, store, "aws-1", "aws", "acc", now, `{
		"compute":   [{"resource_id":"i-0abc"}],
		"databases": [{"resource_id":"orders","engine":"aurora-postgresql"}],
		"clusters":  [{"resource_id":"arn:aws:eks:us-east-1:acc:cluster/prod","name":"prod"}]
	}`)

	p := NewDiscoveryScanResourceKeyProjector(store, zap.NewNop())
	ctx := context.Background()

	t.Run("compute", func(t *testing.T) {
		got, ok := p.ProjectKey(ctx, "aws", "compute", "i-0abc")
		want := traceindex.ProjectComputeKey("aws", "acc", "i-0abc")
		if !ok || got != want {
			t.Errorf("ProjectKey = (%q,%v), want (%q,true)", got, ok, want)
		}
	})

	t.Run("database normalizes engine", func(t *testing.T) {
		got, ok := p.ProjectKey(ctx, "aws", "database", "orders")
		// The key must use the OTel-normalized db.system, matching what the
		// receiver derives — not the raw "aurora-postgresql" engine string.
		want := traceindex.ProjectDatabaseKey("aws", "acc", normalizeDBSystem("aurora-postgresql"), "orders")
		if !ok || got != want {
			t.Errorf("ProjectKey = (%q,%v), want (%q,true)", got, ok, want)
		}
	})

	t.Run("cluster uses name", func(t *testing.T) {
		got, ok := p.ProjectKey(ctx, "aws", "cluster", "arn:aws:eks:us-east-1:acc:cluster/prod")
		want := traceindex.ProjectClusterKey("aws", "acc", "prod")
		if !ok || got != want {
			t.Errorf("ProjectKey = (%q,%v), want (%q,true)", got, ok, want)
		}
	})

	t.Run("plural kind accepted", func(t *testing.T) {
		if _, ok := p.ProjectKey(ctx, "aws", "clusters", "arn:aws:eks:us-east-1:acc:cluster/prod"); !ok {
			t.Error("plural 'clusters' should resolve")
		}
	})

	t.Run("resource not found", func(t *testing.T) {
		if k, ok := p.ProjectKey(ctx, "aws", "compute", "i-missing"); ok || k != "" {
			t.Errorf("missing resource = (%q,%v), want (\"\",false)", k, ok)
		}
	})

	t.Run("kind without trace-join key", func(t *testing.T) {
		if k, ok := p.ProjectKey(ctx, "aws", "object_store", "bkt"); ok || k != "" {
			t.Errorf("non-joinable kind = (%q,%v), want (\"\",false)", k, ok)
		}
	})
}

// TestResourceKeyProjector_LatestPerScope confirms the resource is resolved
// from the scan of the scope that actually contains it, using that scope's id.
func TestResourceKeyProjector_LatestPerScope(t *testing.T) {
	store := newFakeScanStore()
	now := time.Now().UTC()
	seedWHScan(t, store, "gcp-a", "gcp", "proj-a", now, `{"compute":[{"resource_id":"vm-a"}]}`)
	seedWHScan(t, store, "gcp-b", "gcp", "proj-b", now.Add(-time.Minute), `{"compute":[{"resource_id":"vm-b"}]}`)

	p := NewDiscoveryScanResourceKeyProjector(store, zap.NewNop())
	got, ok := p.ProjectKey(context.Background(), "gcp", "compute", "vm-b")
	want := traceindex.ProjectComputeKey("gcp", "proj-b", "vm-b")
	if !ok || got != want {
		t.Errorf("ProjectKey = (%q,%v), want (%q,true) — must use proj-b's scope", got, ok, want)
	}
}

// TestResourceKeyProjector_NilStore safe-degrades.
func TestResourceKeyProjector_NilStore(t *testing.T) {
	p := NewDiscoveryScanResourceKeyProjector(nil, zap.NewNop())
	if k, ok := p.ProjectKey(context.Background(), "aws", "compute", "i-0abc"); ok || k != "" {
		t.Errorf("nil store = (%q,%v), want (\"\",false)", k, ok)
	}
}
