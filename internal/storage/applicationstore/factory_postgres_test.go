// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package applicationstore

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestFactory_Postgres_ValidDSN_PingsThroughFactory is the (a) regression: with
// type: postgres and a VALID DSN, the meta-factory selects the Postgres backend
// and returns a working store that Pings. Gated on a real Postgres via
// TEST_POSTGRES_DSN (a services container in CI); skips locally when unset so
// `go test ./...` stays green without a database, matching the postgres package
// suite's convention.
func TestFactory_Postgres_ValidDSN_PingsThroughFactory(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping Postgres factory integration test")
	}

	f, err := NewFactory(FactoryConfig{Type: postgresStorageType, DSN: dsn})
	if err != nil {
		t.Fatalf("NewFactory(postgres, valid dsn): unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if got := f.GetStorageType(); got != postgresStorageType {
		t.Fatalf("GetStorageType = %q, want %q", got, postgresStorageType)
	}

	if err := f.Initialize(zap.NewNop()); err != nil {
		t.Fatalf("Initialize(postgres): unexpected error: %v", err)
	}

	store, err := f.CreateApplicationStore()
	if err != nil {
		t.Fatalf("CreateApplicationStore(postgres): unexpected error: %v", err)
	}
	if store == nil {
		t.Fatal("CreateApplicationStore(postgres): got nil store")
	}

	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on Postgres store returned by the factory: %v", err)
	}
}

// TestFactory_Postgres_MissingDSN_NoFallback is the first half of (b): selecting
// postgres WITHOUT a DSN must fail with a clear, postgres-specific error at
// construction time and must NOT silently fall back to SQLite (or any other
// backend). No store is returned at all.
func TestFactory_Postgres_MissingDSN_NoFallback(t *testing.T) {
	f, err := NewFactory(FactoryConfig{Type: postgresStorageType, DSN: ""})
	if err == nil {
		t.Fatalf("NewFactory(postgres, empty dsn): expected an error, got nil (factory=%v)", f)
	}
	if f != nil {
		t.Fatalf("NewFactory(postgres, empty dsn): expected nil factory on error, got %v", f)
	}
	// The error must name the DSN requirement — not a generic unknown-type
	// error — so an operator immediately knows what to set.
	if !strings.Contains(err.Error(), "dsn") {
		t.Fatalf("error should mention the missing dsn, got: %v", err)
	}
	// And it must NOT have quietly resolved to sqlite/memory.
	for _, fallback := range []string{sqliteStorageType, memoryStorageType} {
		if strings.Contains(err.Error(), fallback) {
			t.Fatalf("error should not reference a fallback backend %q (no silent fallback), got: %v", fallback, err)
		}
	}
}

// TestFactory_Postgres_InvalidDSN_NoFallback is the second half of (b): a
// syntactically-usable but UNREACHABLE DSN must surface a specific Postgres
// connection error from Initialize (the Ping fails) and must NOT fall back to a
// working SQLite store. Runs offline — it points at a closed local port, so no
// TEST_POSTGRES_DSN or network is required.
func TestFactory_Postgres_InvalidDSN_NoFallback(t *testing.T) {
	// 127.0.0.1:1 is a reserved/closed port → connection refused, fast.
	// connect_timeout bounds the failure even on hosts that black-hole it.
	const badDSN = "postgres://squadron:wrong@127.0.0.1:1/squadron?sslmode=disable&connect_timeout=2"

	f, err := NewFactory(FactoryConfig{Type: postgresStorageType, DSN: badDSN})
	if err != nil {
		t.Fatalf("NewFactory(postgres, non-empty dsn): unexpected construction error: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	// The type must have been resolved to postgres — never coerced elsewhere.
	if got := f.GetStorageType(); got != postgresStorageType {
		t.Fatalf("GetStorageType = %q, want %q", got, postgresStorageType)
	}

	err = f.Initialize(zap.NewNop())
	if err == nil {
		t.Fatal("Initialize(postgres, unreachable dsn): expected a connection error, got nil")
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("Initialize error should be a specific postgres error, got: %v", err)
	}
}

// TestEditionsContract_OSS_SupportsSqliteAndPostgres is (c): the OSS editions
// contract. Both sqlite and postgres are OSS-selectable backends (ADR 0033: the
// backend + DSN seam stays in OSS; only HA operability is Enterprise-reserved).
// This locks that in against a regression that would drop postgres from
// AllStorageTypes or accidentally gate it behind an edition.
func TestEditionsContract_OSS_SupportsSqliteAndPostgres(t *testing.T) {
	for _, typ := range []string{sqliteStorageType, postgresStorageType} {
		if !IsStorageTypeSupported(typ) {
			t.Errorf("OSS must support storage type %q (IsStorageTypeSupported=false)", typ)
		}
		found := false
		for _, s := range AllStorageTypes {
			if s == typ {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("OSS AllStorageTypes must include %q; got %v", typ, AllStorageTypes)
		}
	}

	// Sanity: an unknown type is still rejected (not swallowed as a default).
	if IsStorageTypeSupported("cassandra") {
		t.Error("IsStorageTypeSupported should reject an unknown backend")
	}
	if _, err := NewFactory(FactoryConfig{Type: "cassandra"}); err == nil {
		t.Error("NewFactory should reject an unknown backend type")
	}
}
