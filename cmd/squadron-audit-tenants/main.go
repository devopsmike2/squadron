// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Command squadron-audit-tenants is the ADR 0043 operator PREFLIGHT: a
// read-only scan an operator runs BEFORE arming strict tenant scoping on a
// Postgres (or SQLite) application store.
//
// Strict tenant scoping (the enterprise seam) fail-closes cross-tenant store
// access. If a store already holds more than one tenant's rows in a table that
// was assumed single-tenant ("commingled" data), arming strict would abruptly
// hide rows a caller legitimately expected — so the operator must reconcile
// first. This tool reports, per tenant-bearing table:
//
//   - total rows,
//   - unstamped rows (NULL/empty tenant_id — legacy rows the no-blind-backfill
//     ADR 0043 migration deliberately left as-is), and
//   - the DISTINCT tenant count (> 1 ⇒ COMMINGLED),
//
// plus a small sample of unstamped ids. It MUTATES NOTHING and reads across all
// tenants under a system context.
//
// Exit codes: 0 = safe to arm strict (no commingling); 2 = commingling detected
// (reconcile before arming); 1 = operational error. So it doubles as a CI/rollout
// gate:  squadron-audit-tenants --config squadron.yaml && arm-strict.sh
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/config"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"

	"go.uber.org/zap"
)

func main() {
	configPath := flag.String("config", "", "path to the Squadron config file (same file the server uses)")
	asJSON := flag.Bool("json", false, "emit the report as JSON instead of a table")
	flag.Parse()

	if err := run(*configPath, *asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "squadron-audit-tenants: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string, asJSON bool) error {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config %q: %w", configPath, err)
	}

	factory, err := applicationstore.NewFactoryFromAppConfig(cfg)
	if err != nil {
		return fmt.Errorf("build application-store factory: %w", err)
	}
	if err := factory.Initialize(zap.NewNop()); err != nil {
		return fmt.Errorf("initialize application store: %w", err)
	}
	defer func() { _ = factory.Close() }()

	store, err := factory.CreateApplicationStore()
	if err != nil {
		return fmt.Errorf("open application store: %w", err)
	}

	auditor, ok := store.(types.TenantComminglingAuditor)
	if !ok {
		return fmt.Errorf("the %q application-store backend does not support the tenant-commingling audit "+
			"(only the sqlite and postgres SQL backends do)", factory.GetStorageType())
	}

	// System context: the audit must see across ALL tenants, so it deliberately
	// runs with no tenant predicate.
	ctx := identity.WithSystemContext(context.Background())
	rep, err := auditor.AuditTenantCommingling(ctx)
	if err != nil {
		return fmt.Errorf("run tenant-commingling audit: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		printTable(rep)
	}

	if rep.AnyCommingled {
		fmt.Fprintln(os.Stderr, "\nRESULT: COMMINGLED DATA DETECTED — do NOT arm strict tenant scoping until the flagged tables are reconciled.")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "\nRESULT: no commingling detected — safe to arm strict tenant scoping.")
	return nil
}

func printTable(rep *types.TenantComminglingReport) {
	fmt.Printf("Tenant-commingling audit — backend=%s generated_at=%s\n\n", rep.Backend, rep.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "TABLE\tTOTAL\tUNSTAMPED\tDISTINCT_TENANTS\tCOMMINGLED\tSAMPLE_UNSTAMPED_IDS")
	for _, t := range rep.Tables {
		commingled := ""
		if t.Commingled {
			commingled = "YES"
		}
		sample := ""
		for i, id := range t.SampleUnstampedIDs {
			if i > 0 {
				sample += ","
			}
			sample += id
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\n",
			t.Table, t.TotalRows, t.UnstampedRows, t.DistinctTenants, commingled, sample)
	}
	_ = w.Flush()
}
