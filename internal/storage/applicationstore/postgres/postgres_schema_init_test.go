// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPostgres_ConcurrentInitSchemaNoRace is a regression test for the schema-init
// race fixed by the pg_advisory_xact_lock in initSchema.
//
// Root cause: `CREATE TABLE IF NOT EXISTS` is NOT concurrency-safe. When two
// processes initialize the SAME database at once — in CI, the `postgres` and
// `applicationstore` test packages both Open() the shared TEST_POSTGRES_DSN
// database concurrently — the loser fails with
// `duplicate key value violates unique constraint "pg_type_typname_nsp_index"`
// (SQLSTATE 23505) on the new table's companion pg_type row. Before the fix this
// test fails intermittently; after it, every Open() succeeds.
//
// To reproduce the from-scratch catalog race deterministically WITHOUT disturbing
// the shared `public` schema that other test packages use concurrently, the test
// creates a throwaway schema and points a search_path-scoped DSN at it, so all N
// concurrent initSchema runs create their tables into that isolated namespace.
func TestPostgres_ConcurrentInitSchemaNoRace(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping Postgres integration test")
	}
	ctx := context.Background()

	// Control connection used only to create/drop the isolated schema.
	ctrl, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	defer ctrl.Close()
	if err := ctrl.PingContext(ctx); err != nil {
		t.Fatalf("ping control: %v", err)
	}

	schema := fmt.Sprintf("schema_init_race_%d", time.Now().UnixNano())
	if _, err := ctrl.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ctrl.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})

	// Scope Open()'s connections to the throwaway schema so initSchema creates its
	// tables there (isolated from `public`), reproducing the empty-catalog race.
	childDSN := withSearchPath(dsn, schema)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	stores := make([]*Storage, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together to maximize contention
			s, err := Open(ctx, childDSN)
			errs[i], stores[i] = err, s
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Open() #%d failed (schema-init race): %v", i, err)
		}
		if stores[i] != nil {
			t.Cleanup(func() { _ = stores[i].Close() })
		}
	}
	if t.Failed() {
		return
	}

	// initSchema must be idempotent: a further Open() against the now-populated
	// schema is a clean no-op (every statement is IF NOT EXISTS).
	s, err := Open(ctx, childDSN)
	if err != nil {
		t.Fatalf("idempotent re-Open failed: %v", err)
	}
	_ = s.Close()
}

// withSearchPath returns dsn with a search_path runtime parameter set to schema.
// pgx forwards unrecognized connection parameters as server runtime params, so
// this scopes the session's default schema. Handles both URL-style
// (postgres://...) and keyword/value DSNs.
func withSearchPath(dsn, schema string) string {
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + "search_path=" + schema
	}
	return dsn + " search_path=" + schema
}
