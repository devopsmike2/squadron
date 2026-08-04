// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package applicationstore

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/config"
)

// TestConfigFrom_ThreadsDSN confirms the postgres connection string surfaces
// through the store factory config. This is the config seam for the pluggable
// Postgres/RDS backend (decisions/0033); the backend itself lands in later
// slices, but the DSN plumbing is here so it's a drop-in.
func TestConfigFrom_ThreadsDSN(t *testing.T) {
	c := &config.Config{}
	c.Storage.App.Type = "postgres"
	c.Storage.App.DSN = "postgres://u:p@h:5432/squadron?sslmode=require"

	fc := ConfigFrom(c)
	if fc.Type != "postgres" {
		t.Fatalf("type not threaded: got %q", fc.Type)
	}
	if fc.DSN != "postgres://u:p@h:5432/squadron?sslmode=require" {
		t.Fatalf("dsn not threaded: got %q", fc.DSN)
	}
}

// TestDefaultConfig_StaysSqlite locks the standard-deployment default: sqlite,
// zero external dependency. Postgres is opt-in, never the default (making it
// the default would force every OSS user to run a database server).
func TestDefaultConfig_StaysSqlite(t *testing.T) {
	if got := DefaultConfig().Type; got != "sqlite" {
		t.Fatalf("default app-store backend must stay sqlite, got %q", got)
	}
}
