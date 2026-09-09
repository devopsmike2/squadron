// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aicredstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
)

// newTestStore opens a file-backed SQLite store in a temp dir. A file
// (not ":memory:") so the store survives across the pool's connections.
func newTestStore(t *testing.T) Store {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "aicred.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	store, err := NewSQLiteStore(context.Background(), db, zap.NewNop())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newKey returns a fresh credstore.Key over 32 random bytes.
func newKey(t *testing.T) *credstore.Key {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	k, err := credstore.NewKey(raw)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k
}

func TestSQLite_SetGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := newKey(t)

	const plaintext = "unit-test-fixture-value-not-a-key"
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

func TestSQLite_GetEmpty(t *testing.T) {
	_, _, _, ok, err := newTestStore(t).Get(context.Background())
	if err != nil {
		t.Fatalf("Get on empty store: %v", err)
	}
	if ok {
		t.Fatal("Get on empty store: ok=true, want false")
	}
}

func TestSQLite_SetIsUpsert(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := newKey(t)

	s1, n1, _ := key.Seal([]byte("first"))
	if err := store.Set(ctx, s1, n1, "anthropic"); err != nil {
		t.Fatalf("Set #1: %v", err)
	}
	s2, n2, _ := key.Seal([]byte("second"))
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

func TestSQLite_Clear(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	key := newKey(t)

	sealed, nonce, _ := key.Seal([]byte("value"))
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

func TestSQLite_WrongKeyOpenFails(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	keyA := newKey(t)
	keyB := newKey(t)

	sealed, nonce, _ := keyA.Seal([]byte("secret"))
	if err := store.Set(ctx, sealed, nonce, "anthropic"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	gotSealed, gotNonce, _, ok, err := store.Get(ctx)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	// Opening with a different key must fail (AES-GCM auth-tag mismatch).
	if _, err := keyB.Open(gotSealed, gotNonce); err == nil {
		t.Fatal("Open with wrong key succeeded; want auth-tag mismatch error")
	}
}

func TestSQLite_SetValidation(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.Set(ctx, nil, []byte("n"), "anthropic"); err == nil {
		t.Error("Set with empty sealed: want error, got nil")
	}
	if err := store.Set(ctx, []byte("c"), nil, "anthropic"); err == nil {
		t.Error("Set with empty nonce: want error, got nil")
	}
}

func TestNewSQLiteStore_NilDB(t *testing.T) {
	if _, err := NewSQLiteStore(context.Background(), nil, zap.NewNop()); err == nil {
		t.Fatal("NewSQLiteStore(nil): want error, got nil")
	}
}
