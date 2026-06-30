// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"
)

// discovery_workload_health_reader.go — the production
// ServerlessHealthInventoryReader (v0.89.132 slice 1 chunk 1 shipped the
// handler + route + UI panel but never wired a reader, so the endpoint
// returned all-zero counts and the panel permanently hid itself). This
// activates it: the reader composes per-provider workload-health counts from
// the persisted discovery scans.
//
// Source of truth: the latest persisted scan per (provider, scope). The scan
// response JSON carries the serverless rows with the three diagnostic
// annotations the panel rolls up — cold-start latency, sampling-too-aggressive,
// and error-rate spike. GCP / Azure / OCI marshal all three (the snapshot type
// is serialized directly); AWS currently carries only the cold-start flag on
// its snake_case wire row, so AWS rolls up cold-start today and the error-rate
// + sampling axes are a tracked follow-up (awsServerlessRow marshal gap). A
// missing flag is read as "not firing" — never fabricated.

// workloadHealthScanShape is the minimal projection of a scan response JSON the
// reader needs: the serverless rows and their three exceedance flags. The field
// tags match every provider's wire shape (the ServerlessInstanceSnapshot json
// tags for GCP/Azure/OCI; awsServerlessRow for the cold-start flag on AWS), so
// one shape unmarshals all four clouds. Unknown fields are ignored.
type workloadHealthScanShape struct {
	Serverless []struct {
		ColdStartExceedsThreshold *bool `json:"cold_start_exceeds_threshold"`
		SamplingExceedsFloor      *bool `json:"sampling_exceeds_floor"`
		ErrorRateExceedsThreshold *bool `json:"error_rate_exceeds_threshold"`
	} `json:"serverless"`
}

// persistedScanWorkloadHealthReader implements ServerlessHealthInventoryReader
// over a DiscoveryScanStore.
type persistedScanWorkloadHealthReader struct {
	store  DiscoveryScanStore
	logger *zap.Logger
}

// NewPersistedScanWorkloadHealthReader builds the production reader. A nil store
// yields a reader that returns empty counts (the panel hides) rather than
// panicking — same safe-degrade posture as the nil-reader default.
func NewPersistedScanWorkloadHealthReader(store DiscoveryScanStore, logger *zap.Logger) *persistedScanWorkloadHealthReader {
	return &persistedScanWorkloadHealthReader{store: store, logger: logger}
}

// workloadHealthProviders is the fixed provider set the panel rolls up, matching
// the four discovery providers.
var workloadHealthProviders = []string{"aws", "gcp", "azure", "oci"}

// WorkloadHealthCounts composes the per-provider counts from the latest
// persisted scan per scope. Best-effort: a store error or an unparseable scan
// for one provider/scope yields zero for that slice, never a panic or a
// fabricated count.
func (r *persistedScanWorkloadHealthReader) WorkloadHealthCounts(ctx context.Context) map[string]WorkloadHealthProviderCounts {
	out := make(map[string]WorkloadHealthProviderCounts, len(workloadHealthProviders))
	for _, provider := range workloadHealthProviders {
		out[provider] = r.countsForProvider(ctx, provider)
	}
	return out
}

func (r *persistedScanWorkloadHealthReader) countsForProvider(ctx context.Context, provider string) WorkloadHealthProviderCounts {
	var counts WorkloadHealthProviderCounts
	if r.store == nil {
		return counts
	}
	// List recent scans across all scopes for the provider (empty scopeID =
	// all), most-recent first. The list omits the inventory blob, so we fetch
	// the full record per scope below.
	recs, err := r.store.ListDiscoveryScans(ctx, provider, "", scanHistoryListLimit)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("workload health: list scans failed", zap.String("provider", provider), zap.Error(err))
		}
		return counts
	}

	var cold, sampling, errRate []bool
	seenScope := make(map[string]bool)
	for _, rec := range recs {
		if rec == nil || seenScope[rec.ScopeID] {
			continue // first occurrence per scope is the latest (DESC order).
		}
		seenScope[rec.ScopeID] = true

		full, err := r.store.GetDiscoveryScan(ctx, rec.ScanID)
		if err != nil || full == nil || full.ResultJSON == "" {
			continue
		}
		var shape workloadHealthScanShape
		if err := json.Unmarshal([]byte(full.ResultJSON), &shape); err != nil {
			if r.logger != nil {
				r.logger.Warn("workload health: scan JSON parse failed",
					zap.String("provider", provider), zap.String("scan_id", rec.ScanID), zap.Error(err))
			}
			continue
		}
		for _, sv := range shape.Serverless {
			counts.ServerlessResourceCount++
			c := sv.ColdStartExceedsThreshold != nil && *sv.ColdStartExceedsThreshold
			s := sv.SamplingExceedsFloor != nil && *sv.SamplingExceedsFloor
			e := sv.ErrorRateExceedsThreshold != nil && *sv.ErrorRateExceedsThreshold
			if c {
				counts.ColdStartExceededCount++
			}
			if s {
				counts.SamplingTooAggressiveCount++
			}
			if e {
				counts.ErrorRateSpikeCount++
			}
			cold = append(cold, c)
			sampling = append(sampling, s)
			errRate = append(errRate, e)
		}
	}
	// UNION semantics via the single-sourced helper (a resource firing two
	// diagnostics counts once).
	counts.AnyIssueCount = WorkloadHealthAnyIssueCount(cold, sampling, errRate)
	return counts
}
