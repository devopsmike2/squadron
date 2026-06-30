// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"
	"strings"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/traceindex"
)

// discovery_resource_key_projector.go — the production ResourceKeyProjector for
// the per-resource span-quality DETAIL endpoint
// (GET /api/v1/discovery/:provider/inventory/:kind/:id/span_quality). The
// aggregate span-quality panel was activated in v0.89.326, but the detail
// endpoint 404'd every lookup because nothing satisfied ResourceKeyProjector.
//
// ProjectKey resolves a (provider, kind, id) path tuple to the same
// traceindex resource_key the OTLP receiver derives, so the handler can look up
// that resource's quality counters. It mirrors the scan-side last-seen
// projection (inventory_lastseen.go) exactly: the key components come from the
// persisted scan inventory — the resource's scope is the scan record's ScopeID,
// and the per-kind fields feed the matching traceindex.Project*Key helper (whose
// shapes are pinned to ComputeResourceKey by
// TestProjectKeys_MirrorComputeResourceKeyTier2/3/4). Slice-1 join scope: the
// three kinds with a defined trace-join key — compute, database, cluster.

// rkpScanShape is the minimal projection of a scan response JSON the projector
// needs. The resource_id / engine / name tags are consistent across all four
// clouds (GCP/Azure/OCI serialize the snapshot directly; AWS's wire rows carry
// the same tags), so one shape unmarshals every provider. Unknown fields ignored.
type rkpScanShape struct {
	Compute []struct {
		ResourceID string `json:"resource_id"`
	} `json:"compute"`
	Databases []struct {
		ResourceID string `json:"resource_id"`
		Engine     string `json:"engine"`
	} `json:"databases"`
	Clusters []struct {
		ResourceID string `json:"resource_id"`
		Name       string `json:"name"`
	} `json:"clusters"`
}

// discoveryScanResourceKeyProjector implements ResourceKeyProjector over a
// DiscoveryScanStore.
type discoveryScanResourceKeyProjector struct {
	store  DiscoveryScanStore
	logger *zap.Logger
}

// NewDiscoveryScanResourceKeyProjector builds the production projector. A nil
// store yields a projector that always returns (_, false) — the detail endpoint
// 404s, same as the unwired default (safe-degrade, no panic).
func NewDiscoveryScanResourceKeyProjector(store DiscoveryScanStore, logger *zap.Logger) *discoveryScanResourceKeyProjector {
	return &discoveryScanResourceKeyProjector{store: store, logger: logger}
}

// ProjectKey finds the resource in the most recent scan (per scope) for the
// provider and returns its traceindex key. Best-effort: a store error or
// unparseable scan is skipped, never panics. Returns ("", false) when the
// resource isn't found, the kind has no trace-join key, or a key component is
// missing.
func (p *discoveryScanResourceKeyProjector) ProjectKey(ctx context.Context, provider, kind, id string) (string, bool) {
	if p.store == nil {
		return "", false
	}
	provider = strings.TrimSpace(provider)
	id = strings.TrimSpace(id)
	if provider == "" || id == "" {
		return "", false
	}
	norm := normalizeProjectorKind(kind)
	if norm == "" {
		return "", false // kind has no slice-1 trace-join key (object store / LB / serverless / event-source).
	}

	recs, err := p.store.ListDiscoveryScans(ctx, provider, "", scanHistoryListLimit)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("resource key projector: list scans failed", zap.String("provider", provider), zap.Error(err))
		}
		return "", false
	}

	seenScope := make(map[string]bool)
	for _, rec := range recs {
		if rec == nil || seenScope[rec.ScopeID] {
			continue // latest scan per scope is the first occurrence (DESC order).
		}
		seenScope[rec.ScopeID] = true

		full, err := p.store.GetDiscoveryScan(ctx, rec.ScanID)
		if err != nil || full == nil || full.ResultJSON == "" {
			continue
		}
		var shape rkpScanShape
		if err := json.Unmarshal([]byte(full.ResultJSON), &shape); err != nil {
			continue
		}
		if key, ok := projectKeyFromShape(provider, rec.ScopeID, norm, id, shape); ok {
			return key, true
		}
	}
	return "", false
}

// normalizeProjectorKind maps the path :kind to a canonical category for the
// three trace-joinable kinds (accepting singular or plural), or "" for kinds
// without a slice-1 trace-join key.
func normalizeProjectorKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "compute":
		return "compute"
	case "database", "databases":
		return "database"
	case "cluster", "clusters":
		return "cluster"
	default:
		return ""
	}
}

// projectKeyFromShape matches id against the kind's slice and applies the
// matching traceindex.Project*Key helper (mirroring inventory_lastseen.go).
func projectKeyFromShape(provider, scopeID, kind, id string, shape rkpScanShape) (string, bool) {
	switch kind {
	case "compute":
		for _, c := range shape.Compute {
			if c.ResourceID == id {
				if k := traceindex.ProjectComputeKey(provider, scopeID, id); k != "" {
					return k, true
				}
				return "", false
			}
		}
	case "database":
		for _, d := range shape.Databases {
			if d.ResourceID == id {
				if k := traceindex.ProjectDatabaseKey(provider, scopeID, normalizeDBSystem(d.Engine), id); k != "" {
					return k, true
				}
				return "", false
			}
		}
	case "cluster":
		for _, cl := range shape.Clusters {
			if cl.ResourceID == id {
				if k := traceindex.ProjectClusterKey(provider, scopeID, cl.Name); k != "" {
					return k, true
				}
				return "", false
			}
		}
	}
	return "", false
}
