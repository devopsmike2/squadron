// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// testStore opens a real Postgres from TEST_POSTGRES_DSN (a services container
// in CI) and truncates the tables it uses. Skips when the DSN is unset so local
// `go test ./...` stays green without a Postgres.
func testStore(t *testing.T) *Storage {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping Postgres integration test")
	}
	s, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.db.Exec("TRUNCATE groups"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func TestPostgres_GroupCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	g := &types.Group{
		ID:                "g1",
		Name:              "Prod",
		Labels:            map[string]string{"env": "prod", "tier": "1"},
		RequireApproval:   true,
		LearnFromVerdicts: true,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := s.CreateGroup(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetGroup(ctx, "g1")
	if err != nil || got == nil {
		t.Fatalf("get: err=%v got=%v", err, got)
	}
	if got.Name != "Prod" || got.Labels["env"] != "prod" || !got.RequireApproval {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	// Missing row must be (nil, nil), matching the sqlite backend.
	if miss, err := s.GetGroup(ctx, "nope"); err != nil || miss != nil {
		t.Fatalf("missing get should be (nil,nil), got %v %v", miss, err)
	}

	g.Name = "Prod-2"
	if err := s.UpdateGroup(ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ = s.GetGroup(ctx, "g1"); got.Name != "Prod-2" {
		t.Fatalf("update didn't persist: %+v", got)
	}

	if list, err := s.ListGroups(ctx); err != nil || len(list) != 1 {
		t.Fatalf("list: err=%v len=%d", err, len(list))
	}

	if err := s.DeleteGroup(ctx, "g1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Second delete must error (matches sqlite "group not found").
	if err := s.DeleteGroup(ctx, "g1"); err == nil {
		t.Fatal("second delete should error (group not found)")
	}
}

// TestOpen_EmptyDSN needs no database and always runs.
func TestOpen_EmptyDSN(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open with empty DSN must error")
	}
}
