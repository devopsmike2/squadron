// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/traceindex"
)

// inventory_lastseen.go — trace integration slice 1 chunk 4
// (docs/proposals/trace-integration-slice1.md §6 + §9 contract
// item 7, v0.89.77).
//
// Per-provider scan handlers call the three Annotate* helpers
// AFTER the scanner produces a Result, BEFORE the response is
// serialized. Each helper:
//   - Returns immediately when the supplied TraceIndexLookup is
//     nil (trace integration disabled in this deployment).
//   - Iterates the supplied snapshots, projects the inventory-side
//     resource_key via the matching traceindex.Project*Key helper,
//     and calls LastSeenAt against the index.
//   - On ok=true sets snapshots[i].LastSeenAt to the returned
//     timestamp (UTC, &-borrowed so the JSON omitempty path works).
//   - On ok=false leaves LastSeenAt nil — the UI renders "never".
//   - On error: LOGS a warning and continues. Per design doc §6
//     and constraint 4 in the brief, a flaky traceindex MUST NOT
//     break the scan endpoint; an unreachable lookup degrades to
//     the same "never" rendering as ok=false.
//
// The TraceIndexLookup interface is a slim slice of the
// traceindex.Index surface so tests can substitute a stub. The
// real *traceindex.Index satisfies it directly via its LastSeenAt
// method.

// TraceIndexLookup is the subset of *traceindex.Index used by the
// per-provider scan-response annotation helpers. Slim by design
// so test files can substitute a fake without pulling the chunk-1
// in-memory index machinery into the handler test setup.
type TraceIndexLookup interface {
	LastSeenAt(ctx context.Context, key string) (time.Time, bool, error)
}

// AnnotateComputeWithLastSeen iterates the supplied compute
// snapshots and populates LastSeenAt from the traceindex. lookup
// nil short-circuits the entire call (no-op). Lookup errors are
// logged but never returned — a flaky index degrades a row to
// "never" rather than failing the scan endpoint.
//
// The provider + scopeID pair feeds traceindex.ProjectComputeKey
// per row alongside the snapshot's ResourceID. Rows whose
// projection collapses to "" (missing provider / scope / resource)
// are skipped silently — slice 1 prefers a clean response over a
// noisy error for a degenerate inventory row.
func AnnotateComputeWithLastSeen(
	ctx context.Context,
	lookup TraceIndexLookup,
	provider, scopeID string,
	snapshots []scanner.ComputeInstanceSnapshot,
	logger *zap.Logger,
) {
	if lookup == nil || len(snapshots) == 0 {
		return
	}
	for i := range snapshots {
		key := traceindex.ProjectComputeKey(provider, scopeID, snapshots[i].ResourceID)
		if key == "" {
			continue
		}
		ts, ok, err := lookup.LastSeenAt(ctx, key)
		if err != nil {
			if logger != nil {
				logger.Warn("inventory last_seen lookup failed",
					zap.String("provider", provider),
					zap.String("scope_id", scopeID),
					zap.String("resource_id", snapshots[i].ResourceID),
					zap.String("category", "compute"),
					zap.Error(err))
			}
			continue
		}
		if !ok {
			continue
		}
		t := ts.UTC()
		snapshots[i].LastSeenAt = &t
	}
}

// AnnotateDatabaseWithLastSeen iterates the supplied database
// snapshots and populates LastSeenAt via ProjectDatabaseKey.
// Same posture as AnnotateComputeWithLastSeen — nil lookup is a
// no-op, errors are logged and continue, missing projection
// components skip the row.
//
// The db_system component of the projection key is the snapshot's
// Engine field normalized to the OTel db.system token via
// normalizeDBSystem. Slice 1 ships the normalization here so the
// scanner-side engine vocabulary (provider-typed strings like
// "aurora-postgresql") doesn't have to match the OTel SDK token
// ("postgresql") at the projection boundary.
func AnnotateDatabaseWithLastSeen(
	ctx context.Context,
	lookup TraceIndexLookup,
	provider, scopeID string,
	snapshots []scanner.DatabaseInstanceSnapshot,
	logger *zap.Logger,
) {
	if lookup == nil || len(snapshots) == 0 {
		return
	}
	for i := range snapshots {
		dbSystem := normalizeDBSystem(snapshots[i].Engine)
		key := traceindex.ProjectDatabaseKey(provider, scopeID, dbSystem, snapshots[i].ResourceID)
		if key == "" {
			continue
		}
		ts, ok, err := lookup.LastSeenAt(ctx, key)
		if err != nil {
			if logger != nil {
				logger.Warn("inventory last_seen lookup failed",
					zap.String("provider", provider),
					zap.String("scope_id", scopeID),
					zap.String("resource_id", snapshots[i].ResourceID),
					zap.String("category", "database"),
					zap.Error(err))
			}
			continue
		}
		if !ok {
			continue
		}
		t := ts.UTC()
		snapshots[i].LastSeenAt = &t
	}
}

// AnnotateClusterWithLastSeen iterates the supplied cluster
// snapshots and populates LastSeenAt via ProjectClusterKey. Same
// posture as the other two helpers.
//
// The cluster_name component of the projection key reads the
// snapshot's Name field — EKS / GKE / AKS / OKE all expose a
// single operator-visible cluster name, and the K8s detector on
// the workload SDK populates k8s.cluster.name with the same
// string, so the projection joins cleanly.
func AnnotateClusterWithLastSeen(
	ctx context.Context,
	lookup TraceIndexLookup,
	provider, scopeID string,
	snapshots []scanner.ClusterSnapshot,
	logger *zap.Logger,
) {
	if lookup == nil || len(snapshots) == 0 {
		return
	}
	for i := range snapshots {
		key := traceindex.ProjectClusterKey(provider, scopeID, snapshots[i].Name)
		if key == "" {
			continue
		}
		ts, ok, err := lookup.LastSeenAt(ctx, key)
		if err != nil {
			if logger != nil {
				logger.Warn("inventory last_seen lookup failed",
					zap.String("provider", provider),
					zap.String("scope_id", scopeID),
					zap.String("cluster_name", snapshots[i].Name),
					zap.String("category", "cluster"),
					zap.Error(err))
			}
			continue
		}
		if !ok {
			continue
		}
		t := ts.UTC()
		snapshots[i].LastSeenAt = &t
	}
}

// normalizeDBSystem folds the discovery scanner's provider-typed
// engine string into the OTel db.system token vocabulary so the
// inventory-side projection key joins cleanly against the
// receiver-side key. Slice 1 ships the minimal set of mappings the
// existing scanners produce; unknown values fall through verbatim
// (the projection still works — the receiver-side just has to
// emit with the same string).
//
// AWS RDS engine strings: "postgres" / "aurora-postgresql" both
// map to "postgresql"; "mysql" / "aurora-mysql" / "mariadb" map to
// "mysql"; "sqlserver" / "mssql" map to "mssql"; "oracle" stays.
// GCP Cloud SQL: "postgres" / "mysql" / "sqlserver" map the same
// way. Azure SQL: scanner emits "sqlserver" already; same mapping.
// OCI: Autonomous DB / DBSystem emit "oracle"; stays verbatim.
func normalizeDBSystem(engine string) string {
	switch engine {
	case "postgres", "postgresql", "aurora-postgresql":
		return "postgresql"
	case "mysql", "aurora-mysql", "mariadb":
		return "mysql"
	case "sqlserver", "mssql":
		return "mssql"
	case "oracle":
		return "oracle"
	default:
		return engine
	}
}
