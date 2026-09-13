// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Command squadron-cutover is the operator PREFLIGHT for flipping Squadron's
// grace-first security postures to enforce. Squadron ships four enforcement
// dimensions grace-first (accept-with-warning until a deliberate cutover):
// API authentication (ADR 0045), OpAMP channel auth (ADR 0042), SSRF egress
// controls (ADR 0046), and strict multi-tenancy (ADR 0048). Each has its own
// flag and playbook; this command aggregates all four into one readiness report.
//
// For each dimension it reports the current mode (enforcing/grace), any BLOCKERS
// a static check can prove would break on flip (e.g. commingled tenant data, an
// unbound OTLP tenant, auth-off on a public bind), and WATCH items — runtime
// signals (live OpAMP connection metrics, egress shadow logs) the operator must
// confirm before flipping, which a static tool cannot assert.
//
// It MUTATES NOTHING (reuses the read-only tenant-commingling audit under a
// system context) and makes no network calls.
//
// Exit codes: 0 = every grace dimension is ready to flip (or already enforcing);
// 2 = at least one dimension has a blocker; 1 = operational error. So it doubles
// as a CI/rollout gate:  squadron-cutover --config squadron.yaml && flip.sh
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
	"github.com/devopsmike2/squadron/internal/cutover"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"

	"go.uber.org/zap"
)

func main() {
	configPath := flag.String("config", "", "path to the Squadron config file (same file the server uses)")
	asJSON := flag.Bool("json", false, "emit the report as JSON instead of a table")
	flag.Parse()

	code, err := run(*configPath, *asJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "squadron-cutover: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// run returns the process exit code (0 ready, 2 blockers) or an error (→ exit 1).
func run(configPath string, asJSON bool) (int, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return 0, fmt.Errorf("load config %q: %w", configPath, err)
	}

	// Best-effort tenant-commingling audit: it feeds the strict-tenanting
	// dimension. If the backend doesn't support it, pass nil — the evaluator
	// reports that as a blocker-to-verify rather than asserting safety.
	tenantAudit := tryTenantAudit(cfg)

	rep := cutover.Evaluate(cfg, tenantAudit, os.Getenv)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return 0, err
		}
	} else {
		printReport(rep)
	}

	if !rep.AllReady {
		fmt.Fprintln(os.Stderr, "\nRESULT: at least one dimension has a BLOCKER — resolve the blockers above before flipping enforce.")
		return 2, nil
	}
	fmt.Fprintln(os.Stderr, "\nRESULT: no static blockers — each grace dimension is ready to flip once its WATCH items are confirmed.")
	return 0, nil
}

// tryTenantAudit runs the read-only commingling audit, returning nil if the
// store can't be opened or the backend doesn't support it (both non-fatal here —
// the evaluator surfaces the gap as a blocker on the tenanting dimension).
func tryTenantAudit(cfg *config.Config) *types.TenantComminglingReport {
	factory, err := applicationstore.NewFactoryFromAppConfig(cfg)
	if err != nil {
		return nil
	}
	if err := factory.Initialize(zap.NewNop()); err != nil {
		return nil
	}
	defer func() { _ = factory.Close() }()

	store, err := factory.CreateApplicationStore()
	if err != nil {
		return nil
	}
	auditor, ok := store.(types.TenantComminglingAuditor)
	if !ok {
		return nil
	}
	ctx := identity.WithSystemContext(context.Background())
	rep, err := auditor.AuditTenantCommingling(ctx)
	if err != nil {
		return nil
	}
	return rep
}

func printReport(rep *cutover.Report) {
	fmt.Printf("Cutover readiness — generated_at=%s\n\n", rep.GeneratedAt.Format("2006-01-02T15:04:05Z07:00"))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "DIMENSION\tADR\tMODE\tREADY\tFLAG")
	for _, d := range rep.Dimensions {
		ready := "yes"
		if !d.Ready {
			ready = "NO"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", d.Dimension, d.ADR, d.Mode, ready, d.Flag)
	}
	_ = w.Flush()

	for _, d := range rep.Dimensions {
		if len(d.Blockers) == 0 && len(d.Watch) == 0 && len(d.Notes) == 0 {
			continue
		}
		fmt.Printf("\n[%s] (ADR %s, %s)\n", d.Dimension, d.ADR, d.Mode)
		for _, b := range d.Blockers {
			fmt.Printf("  BLOCKER: %s\n", b)
		}
		for _, wtc := range d.Watch {
			fmt.Printf("  watch:   %s\n", wtc)
		}
		for _, n := range d.Notes {
			fmt.Printf("  note:    %s\n", n)
		}
	}
}
