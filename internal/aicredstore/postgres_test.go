// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aicredstore

import (
	"context"
	"database/sql"
	"os"
	"testing"

	// pgx stdlib driver, registered under the name "pgx" — the same
	// driver the application store opens Postgres with.
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
)

// newPGTestStore opens a real Postgres from TEST_POSTGRES_DSN (a services
// container in CI) and returns a ready store over a clean, empty credential
// table. Skips when the DSN is unset so local `go test ./...` stays green
// without a Postgres — mirrors internal/storage/applicationstore/postgres.
func newPGTestStore(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping Postgres integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pgx: %v", err)
	}
	store, err := NewPostgresStore(context.Background(), db, zap.NewNop())
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	// The store is a single app-global row; clear it so each test starts from
	// a known-empty state regardless of prior runs against the shared DB.
	if err := store.Clear(context.Background()); err != nil {
		t.Fatalf("Clear (setup): %v", err)
	}
	t.Cleanup(func() {
		_ = store.Clear(context.Background())
		_ = store.Close()
	})
	return store
}

// TestPostgres_SetGetRoundTrip proves the sealed ciphertext + nonce round-trip
// through BYTEA columns byte-for-byte, the provider persists, and Get reports
// ok=true after Set. This is the coverage the sqlite-only suite was missing.
func TestPostgres_SetGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newPGTestStore(t)
	key := newKey(t)

	const plaintext = "integration-test-fixture-value-not-a-key"
	sealed, nonce, err := key.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := store.Set(ctx, sealed, nonce, "anthropic"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	gotSealed, gotNonce, provider, ok, err := store.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: ok=false after Set")
	}
	if provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", provider)
	}
	opened, err := key.Open(gotSealed, gotNonce)
	if err != nil {
		t.Fatalf("Open(round-tripped bytes): %v", err)
	}
	if string(opened) != plaintext {
		t.Errorf("opened = %q, want %q", opened, plaintext)
	}
}

// TestPostgres_GetEmpty confirms Get on an unset store reports ok=false with a
// nil error (sql.ErrNoRows is swallowed by the shared genericStore).
func TestPostgres_GetEmpty(t *testing.T) {
	_, _, _, ok, err := newPGTestStore(t).Get(context.Background())
	if err != nil {
		t.Fatalf("Get on empty store: %v", err)
	}
	if ok {
		t.Fatal("Get on empty store: ok=true, want false")
	}
}

// TestPostgres_SetIsUpsert confirms the ON CONFLICT (id) upsert keeps the table
// single-row: the latest write wins for both the sealed bytes and the provider.
func TestPostgres_SetIsUpsert(t *testing.T) {
	ctx := context.Background()
	store := newPGTestStore(t)
	key := newKey(t)

	s1, n1, err := key.Seal([]byte("first"))
	if err != nil {
		t.Fatalf("Seal #1: %v", err)
	}
	if err := store.Set(ctx, s1, n1, "anthropic"); err != nil {
		t.Fatalf("Set #1: %v", err)
	}
	s2, n2, err := key.Seal([]byte("second"))
	if err != nil {
		t.Fatalf("Seal #2: %v", err)
	}
	if err := store.Set(ctx, s2, n2, "openai"); err != nil {
		t.Fatalf("Set #2: %v", err)
	}

	gotSealed, gotNonce, provider, ok, err := store.Get(ctx)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if provider != "openai" {
		t.Errorf("provider = %q, want openai (latest write wins)", provider)
	}
	opened, err := key.Open(gotSealed, gotNonce)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(opened) != "second" {
		t.Errorf("opened = %q, want second", opened)
	}
}

// TestPostgres_Clear confirms Clear removes the row (Get -> ok=false) and is
// idempotent — clearing an already-empty store is a no-op success.
func TestPostgres_Clear(t *testing.T) {
	ctx := context.Background()
	store := newPGTestStore(t)
	key := newKey(t)

	sealed, nonce, err := key.Seal([]byte("value"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := store.Set(ctx, sealed, nonce, "anthropic"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := store.Clear(ctx); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, _, _, ok, err := store.Get(ctx); err != nil || ok {
		t.Fatalf("Get after Clear: ok=%v err=%v, want ok=false", ok, err)
	}
	// Idempotent: clearing an already-empty store succeeds.
	if err := store.Clear(ctx); err != nil {
		t.Fatalf("Clear (idempotent): %v", err)
	}
}

// TestPostgres_WrongKeyOpenFails confirms bytes stored under one key cannot be
// opened with another (AES-GCM auth-tag mismatch), same as the sqlite suite.
func TestPostgres_WrongKeyOpenFails(t *testing.T) {
	ctx := context.Background()
	store := newPGTestStore(t)
	keyA := newKey(t)
	keyB := newKey(t)

	sealed, nonce, err := keyA.Seal([]byte("secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := store.Set(ctx, sealed, nonce, "anthropic"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	gotSealed, gotNonce, _, ok, err := store.Get(ctx)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if _, err := keyB.Open(gotSealed, gotNonce); err == nil {
		t.Fatal("Open with wrong key succeeded; want auth-tag mismatch error")
	}
}

// TestNewPostgresStore_NilDB confirms the constructor rejects a nil handle
// (mirrors the sqlite constructor guard).
func TestNewPostgresStore_NilDB(t *testing.T) {
	if _, err := NewPostgresStore(context.Background(), nil, zap.NewNop()); err == nil {
		t.Fatal("NewPostgresStore(nil): want error, got nil")
	}
}
